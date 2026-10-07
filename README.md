# go-ad-inventory

广告投放库存与预算管理服务：为广告活动提供**预算预占（reserve）→ 回执核销（capture）/ 取消（cancel）/ 过期（expire）**的完整生命周期，并支持**按小时时段的投放节奏（pacing）控制**与**带配置版本的预算/时区/曲线调整**。

开发环境：Go 1.23.0，仅依赖标准库。

## 核心语义

- **三级额度**：每个活动有总预算、按自然日（活动时区）的日预算，以及可配置的**当日逐小时累计目标**。一次预占必须**同时**占住三级额度，任一级不足则整体失败，不留下半笔预占。
- **投放曲线**：活动可配置 24 个非负整数权重 `curve_weights`。到小时时段 `h` 结束时的累计目标为
  `ceil(daily_cap * prefix[h] / sum(weights))`，目标随时间单调不减，最后一个时段恰为日预算。
  权重为 0 的时段不解锁新额度（曲线开头的零权重时段目标为 0）。不配置曲线时只做总/日两级校验。
- **凭证**：预占成功返回带 `expires_at` 的凭证，并在凭证上**冻结**创建时刻的日期（`day_key`）、小时时段（`slot`）与配置版本（`config_version`）。未及时核销的凭证到期后自动过期，释放全部额度。
- **迟到回执不重新归类**：无论回执何时到达、期间活动是否切换过时区或曲线，每笔费用始终归入预占时冻结的日期与时段；核销差额与取消/过期释放也回到冻结桶，余额与节奏统计绝不重复扣减或释放。
- **幂等预占**：外部请求号 `request_id` 是幂等依据——同编号同金额返回原凭证；同编号金额变化报 `idempotency_conflict`。
- **活动状态**：活动有 `active` / `paused` 两种投放状态。暂停后**一切新预占都被拒绝**（`conflict`），不落事件、不留半条消耗记录；已确认的回执核销仍正常入账（真实消耗必须记录）。恢复后从**当前已确认的消耗**继续按现行配置版本放行，暂停期间失败的请求不会被补记成有效曝光。暂停/恢复都是幂等操作，重复调用不重复落事件；状态变化以 `campaign.paused` / `campaign.resumed` 事件持久化，重启重放后保持一致。
- **调整原因**：每次预算调整、配置切换、暂停/恢复都可携带 `reason`（原因与依据），随事件持久化；预算/配置调整的 `reason` 还会出现在配置版本流中，同调整号重放时原因不一致报 `idempotency_conflict`。
- **核销**：曝光回执按不高于预占额的实际费用核销，差额自动释放回三级预算。回执按 `receipt_id` 去重：重复回执返回原结果，同号不同内容报 `idempotency_conflict`。
- **终态唯一**：核销、取消、过期三者竞争时只会落入一个终态，额度绝不重复释放。迟到取消不能冲销已核销费用；失效/已取消凭证收到回执返回 `conflict`。
- **精确金额**：金额以 10⁻⁶ 元定点整数（`big.Int`）表示，累计目标用向上取整的整数除法，全程无浮点误差。
- **统一时钟**：日期边界、小时时段与过期判断都经过可注入的 `clock.Clock`，测试可确定性推进时间。
- **配置版本**：活动创建时版本为 1；每次预算调整、时区/曲线切换都产生新版本，事件流中保留完整版本历史。
- **预算调整**：
  - 必须带 `adjustment_id`（幂等键）与 `expected_version`（乐观锁）；
  - **提高**预算立即生效；
  - **降低**总预算不得小于 `已核销 + 全部有效预占`（必然也不小于已核销额）；降低日预算不得小于**任何一天**（含历史日）的已占用；
  - 条件不满足时**整体拒绝**（`budget_exceeded`，body 带 `limit`/`committed`/`day_key`），绝不静默取消既有凭证；
  - 期望版本过期返回 `version_conflict` 并带 `current_version`；同调整号重放返回该调整生效时的版本快照，同号不同内容报 `idempotency_conflict`。
- **持久化**：所有状态与额度变化以只追加事件（JSON Lines）落盘并 Fsync，重启后由事件重放恢复。

## 并发与一致性

`domain.Service` 用单一互斥锁把「校验 → 事件落盘 → 更新内存投影」串成原子操作，因此：

- 并发预占时总预算、日预算与时段节奏目标都不会被突破
  （见 `TestConcurrentReserveNeverExceedsBudget`、`TestConcurrentReserveNeverExceedsSlotTarget`）；
- 同一请求号并发重放只产生一笔预占（见 `TestConcurrentIdempotentReserve`）；
- 暂停与预占并发竞争时，要么预占先于暂停成功占住额度，要么被整体拒绝且不留记录，
  预算不变量恒成立（见 `TestConcurrentPauseAndReserve`）；
- 核销/取消/过期与预算调整并发竞争时凭证只落入一个终态、预算不变量恒成立
  （见 `TestTerminalStateRace`、`TestConfigSwitchAndTerminalRace`）。

## 关键状态变化

- **活动**：`active` ⇄ `paused`。`POST /pause` 与 `POST /resume` 触发，均幂等；
  暂停只阻断新预占，不影响在途凭证的核销/取消/过期，也不改变配置版本号。
- **凭证**：`reserved` → `captured` / `cancelled` / `expired`（三选一终态）。
  凭证创建时冻结 `day_key`/`slot`/`config_version`，之后的计划修改（预算、曲线、时区）
  不会把已确认的曝光重新归类。
- **配置版本**：创建为版本 1，每次预算/配置调整 +1；每次调整记录 `reason`，
  版本流（`GET /versions`）完整可追溯。

## 项目结构

```
money/     精确金额类型（big.Int 定点数）
clock/     统一时间来源（System / 可推进的 Fake）
store/     事件存储：MemStore（测试）与 FileStore（JSONL 追加 + Fsync）
domain/    领域服务：活动配置、投放曲线、预占、核销、取消、过期、
           预算/配置调整、版本流与节奏报告、余额查询
httpapi/   JSON HTTP 适配层
cmd/server 可运行入口（含后台过期扫描）
```

## HTTP 接口

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/v1/campaigns` | 创建活动（总预算、日上限、时区、默认 TTL、可选 24 时段曲线） |
| GET | `/v1/campaigns/{id}` | 查询活动配置（含 `config_version` 与曲线） |
| GET | `/v1/campaigns/{id}/balance` | 总预算/当日预算/当前时段节奏余额 |
| POST | `/v1/campaigns/{id}/reservations` | 预占（幂等键 `request_id`） |
| POST | `/v1/campaigns/{id}/budget-adjustments` | 带版本的预算调整（幂等键 `adjustment_id`） |
| POST | `/v1/campaigns/{id}/config-adjustments` | 切换时区/投放曲线/移除曲线（带版本） |
| POST | `/v1/campaigns/{id}/pause` | 暂停投放（幂等；body 可带 `reason`） |
| POST | `/v1/campaigns/{id}/resume` | 恢复投放（幂等；body 可带 `reason`） |
| GET | `/v1/campaigns/{id}/versions` | 配置版本流（创建 + 每次调整） |
| GET | `/v1/campaigns/{id}/pacing?day=YYYY-MM-DD` | 逐时段目标、实际核销、有效预占、偏差 |
| GET | `/v1/reservations/{id}` | 查询凭证状态（含冻结的 `day_key`/`slot`/`config_version`） |
| POST | `/v1/reservations/{id}/capture` | 回执核销（去重键 `receipt_id`） |
| POST | `/v1/reservations/{id}/cancel` | 主动取消 |
| POST | `/v1/admin/expire` | 触发一次过期扫描 |

金额线格式：`{"amount": "12.34", "currency": "CNY"}`。

### 错误分类

| code | HTTP | 含义 |
|---|---|---|
| `invalid_argument` | 400 | 参数错误（金额非法、超过预占额、币种不符、曲线权重非法等） |
| `not_found` | 404 | 活动或凭证不存在 |
| `budget_exceeded` | 422 | 额度不足：预占时 body 带 `level`（`total`/`daily`/`slot`）、`requested`、`available`；调整被拒时带 `limit`、`committed`（日预算另带 `day_key`） |
| `conflict` | 409 | 状态冲突（已终态、迟到取消、失效凭证收到回执、活动已暂停） |
| `idempotency_conflict` | 409 | 同一请求号/回执号/调整号但内容变化 |
| `version_conflict` | 409 | `expected_version` 与当前版本不一致，body 带 `current_version` |

### 示例

```bash
# 创建活动：总预算 1000 CNY，日上限 200，凭证默认 1 小时有效
curl -X POST localhost:8080/v1/campaigns -d '{
  "name": "spring-promo",
  "total_budget": {"amount": "1000", "currency": "CNY"},
  "daily_cap":    {"amount": "200",  "currency": "CNY"},
  "timezone": "Asia/Shanghai",
  "default_ttl": "1h"
}'

# 创建带投放曲线的活动（24 个权重；夜间权重为 0，白天逐步放量）
curl -X POST localhost:8080/v1/campaigns -d '{
  "name": "daytime-only",
  "total_budget": {"amount": "1000", "currency": "CNY"},
  "daily_cap":    {"amount": "200",  "currency": "CNY"},
  "timezone": "Asia/Shanghai",
  "default_ttl": "1h",
  "curve_weights": [0,0,0,0,0,0,0,1,2,3,4,5,5,4,3,2,1,1,0,0,0,0,0,0]
}'

# 预占 50（幂等键 req-001）；凭证返回冻结的 day_key/slot/config_version
curl -X POST localhost:8080/v1/campaigns/cmp_xxx/reservations -d '{
  "request_id": "req-001",
  "amount": {"amount": "50", "currency": "CNY"}
}'

# 曝光回执：实际费用 42.5，差额 7.5 自动释放（费用归入冻结时段）
curl -X POST localhost:8080/v1/reservations/rsv_xxx/capture -d '{
  "receipt_id": "rcpt-001",
  "amount": {"amount": "42.5", "currency": "CNY"}
}'

# 查询余额（含当前 slot 的累计目标 slot_target 与可用节奏额度 slot_available）
curl localhost:8080/v1/campaigns/cmp_xxx/balance

# 查询当天逐时段目标/核销/预占/偏差（variance = 累计已核销 - 累计目标）
curl 'localhost:8080/v1/campaigns/cmp_xxx/pacing'

# 预算调整：基于版本 1 提高到总 2000 / 日 300（幂等键 adj-001）
curl -X POST localhost:8080/v1/campaigns/cmp_xxx/budget-adjustments -d '{
  "adjustment_id": "adj-001",
  "expected_version": 1,
  "total_budget": {"amount": "2000", "currency": "CNY"},
  "daily_cap":    {"amount": "300",  "currency": "CNY"},
  "reason": "双十一加投：依据昨日 ROI 2.3"
}'

# 暂停投放（此后新预占返回 409 conflict；已确认核销仍入账）
curl -X POST localhost:8080/v1/campaigns/cmp_xxx/pause -d '{"reason": "预算超投，临时止损"}'

# 恢复投放（从已确认消耗继续；暂停期间失败的请求不会被补记）
curl -X POST localhost:8080/v1/campaigns/cmp_xxx/resume -d '{"reason": "止损完成，恢复投放"}'

# 切换时区与曲线（只传需要改的字段；curve_weights 传 null 表示移除节奏限制）
curl -X POST localhost:8080/v1/campaigns/cmp_xxx/config-adjustments -d '{
  "adjustment_id": "cfg-001",
  "expected_version": 2,
  "timezone": "UTC",
  "curve_weights": [1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1,1]
}'

# 查询配置版本流
curl localhost:8080/v1/campaigns/cmp_xxx/versions
```

## 运行

```bash
go run ./cmd/server -addr :8080 -event-log data/events.jsonl
```

## 测试

```bash
go test ./...          # 全部测试
go test -race ./...    # 含竞态检测
```
