# go-ad-inventory

广告投放预算的**预占（reserve）与核销（capture）**服务。每个广告活动同时受
**总预算**和**按自然日设置的日预算上限**两级约束；一次投放请求必须同时占住两级
额度，成功后返回带失效时间的凭证，之后凭曝光回执按实际费用核销。

开发环境：Go 1.23.0，无第三方依赖。

## 核心语义

### 两级额度与原子预占

- 活动配置包含 `total_budget`（总预算）和 `daily_budget`（每自然日上限）。
- 一次预占同时进入两级台账，恒等式为：

  ```
  reserved + captured <= budget
  available          = budget - reserved - captured
  ```

- 任何一级额度不足，整笔请求失败，**不会留下半笔预占**：余额判断与状态写入在
  同一临界区内完成，且先写持久化日志、成功后才更新内存。
- 自然日按 **UTC** 划分，日键为 `YYYY-MM-DD`。跨日后日预算独立计算，总预算持续累计。

### 金额精确表示

- 金额类型 `Money` 为不可变十进制定点数（`big.Int` 系数 + 10 进制 scale），
  不使用 `float64`，杜绝 `0.1 + 0.2 != 0.3` 类误差，支持任意精度。
- JSON 中金额始终序列化为字符串，如 `"12.30"`、`"0.0001"`。

### 统一时钟

- 日期边界与凭证过期一律通过 `Clock` 接口判断；生产用系统时钟（UTC），
  测试可注入固定时钟确定性地模拟“跨日 / 过期”。

### 幂等

- **预占幂等键 = 外部请求号 `request_no`**。
  - 编号相同且内容（活动、金额、载荷）完全一致：返回原凭证，不重复占额。
  - 编号相同但内容变化：返回幂等冲突（`idempotency_conflict`）。
- **回执幂等键 = 外部回执号 `receipt_no`**。
  - 重复或乱序投递的相同回执：返回首次核销结果，不重复入账。
  - 同回执号内容变化，或用于另一张凭证：幂等冲突。

### 核销、取消与过期

凭证是一个三终态状态机，终态只可能落入一次：

```
                 capture(实际费用 ≤ 预占额)
   reserved ───────────────────────────────► captured（终态）
      │                                         差额预占立即释放
      ├── cancel ───────────────────────────► cancelled（终态，全额释放）
      │
      └── 到达 expires_at（后台扫描 / 任意接口惰性触发）► expired（终态，全额释放）
```

- 核销时预占额全部从 `reserved` 释放，实际费用计入 `captured`，差额自动回到可用额度；
  实际费用为 0 表示全额释放。实际费用不得高于预占额。
- 迟到取消不能冲销已核销费用：`captured` 后再取消返回状态冲突。
- 已失效凭证收到回执：先惰性置为过期，再明确返回状态冲突（`state_conflict`）。
- 核销 / 取消 / 过期在同一把锁内竞争，配合事件日志保证额度不会被重复释放。

### 持久化

- 所有状态及额度变化以**追加式预写日志（WAL，`wal.jsonl`，每行一个 JSON 事件）**落盘，
  每条事件 `fsync`（可用 `-no-fsync` 关闭，仅供测试）。
- 事件类型：`campaign_created`、`reservation_created`、`reservation_finalized`
  （核销 / 取消 / 过期共用，以 `status` 区分）。
- 内存台账不做独立持久化，启动时重放事件流完整重建（含幂等索引与回执去重索引）。
- 进程崩溃若留下写了一半的尾行，重放时自动截断损坏尾行，已确认事件不丢。

## 错误模型

| Kind | HTTP 状态码 | 含义 |
| --- | --- | --- |
| `invalid_argument` | 400 | 参数错误（字段缺失、金额非法/超预占额、预算配置非法等） |
| `not_found` | 404 | 活动或凭证不存在 |
| `budget_exceeded` | 422 | 预算不足，错误体 `scope` 为 `total` 或 `daily` |
| `state_conflict` | 409 | 状态冲突（凭证已核销/取消/过期、迟到取消、失效凭证收回执） |
| `idempotency_conflict` | 409 | 幂等冲突：请求号/回执号相同但内容不一致 |

错误响应体形如：

```json
{ "error": { "kind": "budget_exceeded", "code": "budget_exceeded",
             "scope": "daily", "message": "reserve 15 would exceed ..." } }
```

## HTTP 接口

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| `POST` | `/v1/campaigns` | 创建活动配置 |
| `GET` | `/v1/campaigns/{id}` | 查询活动 |
| `GET` | `/v1/campaigns/{id}/balance?day=YYYY-MM-DD` | 查询两级余额（`day` 可省，默认当日） |
| `POST` | `/v1/reservations` | 创建预占（带 `request_no` 幂等） |
| `GET` | `/v1/reservations/{token}` | 查询凭证 |
| `POST` | `/v1/reservations/{token}/capture` | 回执核销 |
| `POST` | `/v1/reservations/{token}/cancel` | 主动取消 |
| `POST` | `/v1/maintenance/sweep` | 立即扫描并过期失效凭证 |
| `GET` | `/healthz` | 健康检查 |

### 典型流程

```bash
# 1. 建活动：总预算 100，日预算 30
curl -s -X POST localhost:8080/v1/campaigns \
  -d '{"id":"c1","total_budget":"100.00","daily_budget":"30.00"}'

# 2. 预占 20（request_no 为幂等键）
curl -s -X POST localhost:8080/v1/reservations \
  -d '{"campaign_id":"c1","request_no":"req-1","amount":"20.00","payload":"placement-A"}'
# -> {"token":"...","status":"reserved","expires_at":"...", ...}

# 3. 回执核销实际费用 12.30（差额 7.70 立即释放）
curl -s -X POST localhost:8080/v1/reservations/<token>/capture \
  -d '{"receipt_no":"rcpt-1","actual_cost":"12.30"}'

# 4. 查询余额
curl -s localhost:8080/v1/campaigns/c1/balance
```

## 运行

```bash
# 启动服务
go run ./cmd/adinventoryd -addr :8080 -data-dir ./data \
  -reservation-ttl 5m -sweep-interval 30s

# 参数
#   -addr               监听地址（默认 :8080）
#   -data-dir           WAL 目录（默认 ./data）
#   -reservation-ttl    凭证有效期（默认 5m）
#   -sweep-interval     后台过期扫描间隔（默认 30s；读接口也会惰性过期）
#   -no-fsync          跳过每条事件的 fsync（不安全，仅供测试）
```

## 代码结构

| 文件 | 职责 |
| --- | --- |
| `money.go` | 高精度十进制金额类型 |
| `clock.go` | 统一时间来源（含测试用固定时钟） |
| `errors.go` | 业务错误类型与错误分类 |
| `model.go` | 活动 / 凭证 / 余额等领域模型 |
| `store.go` | WAL 事件日志接口与 JSONL 文件实现 |
| `service.go` | 核心状态机：预占、核销、取消、过期、幂等、台账 |
| `server.go` | HTTP/JSON 接口与错误码映射 |
| `cmd/adinventoryd/main.go` | 服务入口（含后台过期扫描） |

## 测试

```bash
go test ./...            # 全部测试
go test -race ./...      # 带竞态检测
go test -run TestConcurrent ./...
```

测试覆盖：

- 高精度金额解析 / 运算 / 序列化往返；
- 两级额度原子预占、任一额度不足整体失败且不落事件、跨自然日日预算重置；
- 请求号幂等（同号同内容返回原凭证 / 同号异内容冲突）；
- 200 并发预占绝不突破两级预算；
- 核销释放差额、零费用全额释放、超预占额拒绝；
- 回执按回执号去重、乱序重放、异内容冲突、已核销凭证再收回执冲突；
- 取消全额释放且幂等、迟到取消不冲销已核销费用；
- 过期全额释放、失效凭证收回执状态冲突；
- 核销/取消/过期高并发竞争下每张凭证只落入一个终态、台账恒等式成立；
- WAL 写入失败不留半成品、崩溃重放重建全部状态（含幂等/去重索引）、
  损坏尾行截断、并发追加；
- HTTP 端到端与四类错误到状态码的映射。
