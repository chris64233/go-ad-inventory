# go-ad-inventory

广告投放库存与预算管理服务：为广告活动提供**预算预占（reserve）→ 回执核销（capture）/ 取消（cancel）/ 过期（expire）**的完整生命周期。

开发环境：Go 1.23.0，仅依赖标准库。

## 核心语义

- **两级预算**：每个活动既有总预算，也有按自然日（活动时区）设置的上限。一次预占必须**同时**占住两级额度，任一级不足则整体失败，不留下半笔预占。
- **凭证**：预占成功返回带 `expires_at` 的凭证。未及时核销的凭证到期后自动过期，释放全部额度。
- **幂等预占**：外部请求号 `request_id` 是幂等依据——同编号同金额返回原凭证；同编号金额变化报 `idempotency_conflict`。
- **核销**：曝光回执按不高于预占额的实际费用核销，差额自动释放回两级预算。回执按 `receipt_id` 去重：重复回执返回原结果，同号不同内容报 `idempotency_conflict`。
- **终态唯一**：核销、取消、过期三者竞争时只会落入一个终态，额度绝不重复释放。迟到取消不能冲销已核销费用；失效/已取消凭证收到回执返回 `conflict`。
- **精确金额**：金额以 10⁻⁶ 元定点整数（`big.Int`）表示，全程无浮点误差。
- **统一时钟**：日期边界与过期判断都经过可注入的 `clock.Clock`，测试可确定性推进时间。
- **持久化**：所有状态与额度变化以只追加事件（JSON Lines）落盘并 Fsync，重启后由事件重放恢复。

## 并发与一致性

`domain.Service` 用单一互斥锁把「校验 → 事件落盘 → 更新内存投影」串成原子操作，因此：

- 并发预占时总预算与日预算都不会被突破（见 `TestConcurrentReserveNeverExceedsBudget`）；
- 同一请求号并发重放只产生一笔预占（见 `TestConcurrentIdempotentReserve`）；
- 核销/取消/过期并发竞争只落入一个终态（见 `TestTerminalStateRace`）。

## 项目结构

```
money/     精确金额类型（big.Int 定点数）
clock/     统一时间来源（System / 可推进的 Fake）
store/     事件存储：MemStore（测试）与 FileStore（JSONL 追加 + Fsync）
domain/    领域服务：活动配置、预占、核销、取消、过期、余额查询
httpapi/   JSON HTTP 适配层
cmd/server 可运行入口（含后台过期扫描）
```

## HTTP 接口

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/v1/campaigns` | 创建活动（总预算、日上限、时区、默认 TTL） |
| GET | `/v1/campaigns/{id}` | 查询活动配置 |
| GET | `/v1/campaigns/{id}/balance` | 查询总预算与当日预算余额 |
| POST | `/v1/campaigns/{id}/reservations` | 预占（幂等键 `request_id`） |
| GET | `/v1/reservations/{id}` | 查询凭证状态 |
| POST | `/v1/reservations/{id}/capture` | 回执核销（去重键 `receipt_id`） |
| POST | `/v1/reservations/{id}/cancel` | 主动取消 |
| POST | `/v1/admin/expire` | 触发一次过期扫描 |

金额线格式：`{"amount": "12.34", "currency": "CNY"}`。

### 错误分类

| code | HTTP | 含义 |
|---|---|---|
| `invalid_argument` | 400 | 参数错误（金额非法、超过预占额、币种不符等） |
| `not_found` | 404 | 活动或凭证不存在 |
| `budget_exceeded` | 422 | 预算不足，body 带 `level`（`total`/`daily`）、`requested`、`available` |
| `conflict` | 409 | 状态冲突（已终态、迟到取消、失效凭证收到回执） |
| `idempotency_conflict` | 409 | 同一请求号/回执号但内容变化 |

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

# 预占 50（幂等键 req-001）
curl -X POST localhost:8080/v1/campaigns/cmp_xxx/reservations -d '{
  "request_id": "req-001",
  "amount": {"amount": "50", "currency": "CNY"}
}'

# 曝光回执：实际费用 42.5，差额 7.5 自动释放
curl -X POST localhost:8080/v1/reservations/rsv_xxx/capture -d '{
  "receipt_id": "rcpt-001",
  "amount": {"amount": "42.5", "currency": "CNY"}
}'

# 查询余额
curl localhost:8080/v1/campaigns/cmp_xxx/balance
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
