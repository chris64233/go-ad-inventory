# go-ad-inventory

广告投放库存与预算管理服务：为广告活动提供**预算预占（reserve）→ 回执核销（capture）/ 取消（cancel）/ 过期（expire）**的完整生命周期，并支持**按时段节奏控制（pacing）**与**带版本的预算调整**。

开发环境：Go 1.23.0，仅依赖标准库。

## 核心语义

- **三级额度**：每个活动有总预算、按自然日（活动时区）的日上限，以及配置投放曲线后的**当前时段累计节奏额度**。一次预占必须**同时**占住三级额度，任一级不足则整体失败（错误分别带 `level=total/daily/pacing`），不留下半笔预占。
- **每日投放曲线**：24 个整点小时桶的累计目标，以 ppm（百万分之一）整数给出，必须单调非减且末点为 100%。例如第 9 桶为 `500000` 表示本地时间上午结束前最多投掉日上限的一半；目标金额在最小货币单位上**向下取整**，节奏额度永不被取整放大。未配置曲线时第三级检查退化为日预算检查。
- **凭证冻结坐标**：预占成功时按活动**当时**的时区冻结 `day_key`（自然日）、`slot`（小时桶）与 `config_version`。之后核销、取消、过期的全部额度变动都只回到这个冻结桶——**迟到回执不会被新时区或新曲线重新归类**，余额与节奏统计不重复扣减或释放。
- **配置版本**：活动初始版本为 1；切换时区/曲线（`UpdateConfig`）与每次预算调整都使版本递增。所有变更必须携带 `expected_version`（乐观锁），版本不匹配返回 `version_conflict`（带期望/实际版本）。
- **预算调整**：调整号 `adjustment_id` 幂等。提高预算立即生效；降低预算不得小于 **已核销 + 有效预占**（总预算按全局、日上限按每个仍有有效预占的自然日校验），不足时以 `budget_exceeded`（`level=total_floor/daily_floor`，body 带下限金额）**整体拒绝**，绝不静默取消凭证。
- **凭证**：预占成功返回带 `expires_at` 的凭证。未及时核销的凭证到期后自动过期，释放全部额度。
- **幂等预占**：外部请求号 `request_id` 是幂等依据——同编号同金额返回原凭证；同编号金额变化报 `idempotency_conflict`。
- **核销**：曝光回执按不高于预占额的实际费用核销，差额自动释放回三级额度与冻结时段桶。回执按 `receipt_id` 去重：重复回执返回原结果，同号不同内容报 `idempotency_conflict`。
- **终态唯一**：核销、取消、过期三者竞争时只会落入一个终态，额度绝不重复释放。迟到取消不能冲销已核销费用；失效/已取消凭证收到回执返回 `conflict`。
- **精确金额**：金额以 10⁻⁶ 元定点整数（`big.Int`）表示，节奏比例全程整数（ppm）运算，无浮点误差。
- **统一时钟**：时区归属、小时桶、日边界与过期判断都经过可注入的 `clock.Clock`，测试可确定性推进时间。
- **持久化**：所有状态与额度变化以只追加事件（JSON Lines）落盘并 Fsync，重启后由事件重放恢复；新事件对旧版事件流（无版本/时段字段）向后兼容。

## 并发与一致性

`domain.Service` 用单一互斥锁把「校验 → 事件落盘 → 更新内存投影」串成原子操作，因此：

- 并发预占时总预算、日预算与节奏额度都不会被突破（见 `TestConcurrentReserveNeverExceedsBudget`、`TestConcurrentReserveVsBudgetIncrease`）；
- 同一请求号并发重放只产生一笔预占（见 `TestConcurrentIdempotentReserve`）；
- 核销/取消/过期并发竞争只落入一个终态（见 `TestTerminalStateRace`）；
- 配置切换与迟到回执/过期并发时，费用始终落在冻结桶（见 `TestLateReceiptStaysInFrozenBucket`、`TestExpiryAfterConfigSwitchReleasesFrozenBucket`）；
- 预算调整的版本检查与下限校验在同一临界区内，拒绝调整不会改动任何凭证或版本。

## 项目结构

```
money/     精确金额类型（big.Int 定点数）
clock/     统一时间来源（System / 可推进的 Fake）
store/     事件存储：MemStore（测试）与 FileStore（JSONL 追加 + Fsync）
domain/    领域服务：活动/曲线配置、版本化预算调整、预占、核销、取消、过期、余额与节奏报告
httpapi/   JSON HTTP 适配层
cmd/server 可运行入口（含后台过期扫描）
```

## HTTP 接口

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/v1/campaigns` | 创建活动（总预算、日上限、时区、默认 TTL、可选 `curve`） |
| GET | `/v1/campaigns/{id}` | 查询活动配置（含 `config_version` 与 `curve`） |
| PUT | `/v1/campaigns/{id}/config` | 切换时区/投放曲线（带 `expected_version`） |
| POST | `/v1/campaigns/{id}/budget-adjustments` | 预算调整（`adjustment_id` 幂等 + `expected_version`） |
| GET | `/v1/campaigns/{id}/budget-adjustments` | 预算版本与调整历史 |
| GET | `/v1/campaigns/{id}/balance` | 总预算、当日预算与当前时段节奏余额 |
| GET | `/v1/campaigns/{id}/pacing?day=YYYY-MM-DD` | 逐时段目标/核销/有效预占/偏差（`day` 缺省为今天） |
| POST | `/v1/campaigns/{id}/reservations` | 预占（幂等键 `request_id`） |
| GET | `/v1/reservations/{id}` | 查询凭证状态（含冻结的 `day_key`/`slot`/`config_version`） |
| POST | `/v1/reservations/{id}/capture` | 回执核销（去重键 `receipt_id`） |
| POST | `/v1/reservations/{id}/cancel` | 主动取消 |
| POST | `/v1/admin/expire` | 触发一次过期扫描 |

金额线格式：`{"amount": "12.34", "currency": "CNY"}`。曲线为 24 个 ppm 整数数组，例如
`[41666,83333,…,1000000]`（均匀投放），缺省或 `null` 表示不做节奏限制。

### 错误分类

| code | HTTP | 含义 |
|---|---|---|
| `invalid_argument` | 400 | 参数错误（金额非法、超过预占额、币种/曲线/日期格式不符等） |
| `not_found` | 404 | 活动或凭证不存在 |
| `budget_exceeded` | 422 | 额度不足，body 带 `level`（`total`/`daily`/`pacing`/`total_floor`/`daily_floor`）、`requested`、`available` |
| `conflict` | 409 | 状态冲突（已终态、迟到取消、失效凭证收到回执） |
| `idempotency_conflict` | 409 | 同一请求号/回执号/调整号但内容变化 |
| `version_conflict` | 409 | 期望版本与当前版本不一致，body 带 `expected_version`、`actual_version` |

### 示例

```bash
# 创建活动：总预算 1000 CNY，日上限 200，凭证默认 1 小时有效，上午投 50% 的曲线
curl -X POST localhost:8080/v1/campaigns -d '{
  "name": "spring-promo",
  "total_budget": {"amount": "1000", "currency": "CNY"},
  "daily_cap":    {"amount": "200",  "currency": "CNY"},
  "timezone": "Asia/Shanghai",
  "default_ttl": "1h",
  "curve": [500000,500000,500000,500000,500000,500000,500000,500000,500000,500000,500000,500000,
            1000000,1000000,1000000,1000000,1000000,1000000,1000000,1000000,1000000,1000000,1000000,1000000]
}'

# 预占 50（幂等键 req-001）
curl -X POST localhost:8080/v1/campaigns/cmp_xxx/reservations -d '{
  "request_id": "req-001",
  "amount": {"amount": "50", "currency": "CNY"}
}'

# 切换时区（必须带当前版本号；成功后版本变为 2）
curl -X PUT localhost:8080/v1/campaigns/cmp_xxx/config -d '{
  "expected_version": 1,
  "timezone": "UTC"
}'

# 提高预算到 1500（调整号幂等）
curl -X POST localhost:8080/v1/campaigns/cmp_xxx/budget-adjustments -d '{
  "adjustment_id": "adj-20260928-01",
  "expected_version": 2,
  "total_budget": {"amount": "1500", "currency": "CNY"}
}'

# 查询逐时段节奏报告（目标、核销、有效预占、偏差）
curl "localhost:8080/v1/campaigns/cmp_xxx/pacing?day=2026-09-28"

# 查询余额（含当前时段 pacing_target/pacing_available）
curl localhost:8080/v1/campaigns/cmp_xxx/balance
```

节奏报告每行形如：

```json
{
  "hour": 10,
  "cumulative_target_ppm": 500000,
  "target":   {"amount": "100", "currency": "CNY"},
  "spent":    {"amount": "30",  "currency": "CNY"},
  "reserved": {"amount": "0",   "currency": "CNY"},
  "cumulative_spent": {"amount": "30", "currency": "CNY"},
  "variance": {"amount": "-70", "currency": "CNY"}
}
```

`variance = 累计实际核销 − 累计目标`：正数超投、负数欠投。即使配置已经切换，
历史日期各行的 `spent`/`reserved` 仍来自凭证创建时冻结的小时桶。

## 运行

```bash
go run ./cmd/server -addr :8080 -event-log data/events.jsonl
```

## 测试

```bash
go test ./...          # 全部测试
go test -race ./...    # 含竞态检测
```
