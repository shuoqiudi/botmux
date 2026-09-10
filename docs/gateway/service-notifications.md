# 服务通知接入

服务通知以已鉴权的 workload 来源和完整 `fingerprint` 作为身份。`service_name` 仅用于显示；同名不同 fingerprint 是不同服务。凭据轮换不改变来源或幂等历史。首次有效通知自动登记服务。

管理员可在服务详情中选择 Bot 账号及聊天目标建立订阅，每项订阅接收该服务的全部新通知。没有订阅时不会发送 Telegram、重试或进入死信。新增订阅不会自动补发历史通知；旧的无订阅请求同键重放仍返回原结果。既有 Business Route、Telegram 兼容代理和 it_manage Adapter 接口继续使用原契约。

## 授权与地址

在 BotMux 的 Routes → Workloads 中创建 workload，保存只显示一次的凭据，再勾选“发布服务通知”和“查询服务通知”并保存。两项权限独立，默认关闭；Route grants 不授予这些权限。配置只允许管理员修改，使用 workload revision 防止覆盖并记录审计。轮换凭据后旧凭据失效。

使用现有内部 Gateway origin；不要在公网管理域名开放 `/api/v1/services/`。管理员 session 和登录密码不能替代 workload Bearer 凭据。

## 请求

```bash
export GATEWAY_ORIGIN='http://telegram-gateway.internal:8080'
# 从受管 Secret 注入 GATEWAY_WORKLOAD_CREDENTIAL，不提交到版本库。
curl -sS "$GATEWAY_ORIGIN/api/v1/services/notifications" \
  -H "Authorization: Bearer $GATEWAY_WORKLOAD_CREDENTIAL" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: alert-2026-001:firing' \
  --data '{"fingerprint":"v2:incident:example:dns","service_name":"Example DNS","text":"<b>故障</b>：example.invalid DNS 查询失败","parse_mode":"HTML"}'
```

| 字段 | 约束 |
| --- | --- |
| `fingerprint` | 必填字符串，1–512 个 Unicode 字符；保留完整值，禁止控制字符、换行和首尾空白 |
| `service_name` | 可选字符串，提供时为 1–256 个 Unicode 字符；禁止控制字符、换行和首尾空白；省略时显示 fingerprint |
| `text` | 必填字符串，不得为空或全部空白；最多 4096 UTF-8 **字节**，超限拒绝，不截断。常见汉字通常占 3 字节，不能按 4096 个汉字估算 |
| `parse_mode` | 可选字符串；省略或空字符串为普通文本，也支持 `HTML`、`Markdown`、`MarkdownV2` |

其他字段（包括 Token、Bot、Chat、订阅列表及 Route key）均拒绝。可选字段不能是 `null`。`Idempotency-Key` 为 1–128 个 ASCII 字符，首字符为字母或数字，其余可为字母、数字、`.`、`_`、`:`、`-`。

恢复通知使用同一个 fingerprint、新正文和新键，例如 `alert-2026-001:resolved`；普通通知可用 `event-2026-002:notice`。故障抑制、恢复判断、普通通知生命周期仍由 Keep/Monitor 负责。Gateway 不推断事故状态。

同一次事件重试必须保留同键和请求内容。不能只用 fingerprint 作幂等键。同键、同规范化请求返回原通知；同键、不同内容返回 409。JSON 字段顺序和省略/空的普通文本 parse_mode 不影响请求身份。重放不会更新服务名。

## 接收与查询

成功持久保存后返回 HTTP 202：

```json
{"notification_id":"opaque-id","service_id":1,"subscription_count":0,"status":"no_subscribers","received_at":"2026-09-10T00:00:00Z","delivery_summary":{"status":"no_subscribers","total":0,"succeeded":0},"deliveries":[]}
```

202 表示 Gateway 已持久接收，**不表示 Telegram 已送达**。`no_subscribers` 表示没有投递目标；`accepted` 表示通知、全部接收目标快照和独立 Delivery 已在同一个 SQLite 事务中保存，即使 Redis 暂时不可用也有持久恢复计划。

```bash
curl -sS "$GATEWAY_ORIGIN/api/v1/services/notifications/opaque-id" \
  -H "Authorization: Bearer $GATEWAY_WORKLOAD_CREDENTIAL"
```

查询返回相同通知身份、接收结果、投递汇总和子投递列表，不返回正文或物理目标。不存在和其他 workload 的通知统一返回 404。

400 表示字段/JSON/幂等键无效；401 表示凭据缺失、失效或 workload 禁用；403 表示没有该操作权限；409 表示幂等冲突；503 表示持久接收暂不可用。遇到响应丢失或 503 时保留原键和请求重试，不把 202 当成需要重发的新事件。

## 网页验收

打开“通知服务”入口，按来源、名称、fingerprint、订阅数和最近接收时间识别服务。进入详情查看通知历史和“无订阅，未投递”结果；管理员可以查看正文，operator 只查看不含正文的运行记录，普通用户没有入口权限。页面沿用英语/俄语和主题设置。

接入开发任务模板：为业务事件确定稳定 fingerprint；从受管 Secret 读取 workload 凭据；保留 Keep 的正文渲染及通知条件；按事件身份和阶段生成幂等键；提交一次通知；验证 202 与查询；在网页确认来源和无订阅记录。记录“Keep 接收”“Gateway 持久接收”“Telegram 送达”三个阶段，不能互相替代。

完整设计和后续多目标验收/Monitor 迁移范围见 [规格 #12](https://github.com/shuoqiudi/it_telegram/issues/12)。登记接口见 [任务 #13](https://github.com/shuoqiudi/it_telegram/issues/13)，订阅和单目标可靠投递见 [任务 #14](https://github.com/shuoqiudi/it_telegram/issues/14)。


## 添加订阅和管理目标

1. 用管理员账号打开 **Notification services / 通知服务**，选择已自动登记的服务。首次没有接收方是正常状态。
2. 在 **Add subscription / 添加订阅** 表单选择 Bot account 和 Chat target。只显示该账号下有效的目标。保存后列表显示账号、目标及 Telegram 聊天名称。
3. 新接收方可在同一页面展开 **Configure or validate Bot accounts and chat targets**：先创建账号并输入 Token，再创建聊天目标并输入 Chat ID。保存账号会验证 Telegram `getMe`；保存目标会验证该 Bot 对聊天的访问。Token 只保存在账号配置中，不放进订阅或业务请求。
4. 用新的幂等键提交一条通知。点详情页 Refresh，查看每个 Delivery 的 ID、状态、尝试次数和安全错误类别。正文按文字展示，不执行 HTML。
5. 点击 Cancel subscription 取消接收后续新通知。已经持久接收的 Delivery 继续使用原计划，取消不会删除历史。

一个服务可以添加多个订阅；不存在“最多一个订阅”的限制。同一实际 Telegram Bot 和同一 Chat 不可对同一服务重复有效订阅，即使配置了两个逻辑账号。不同服务可以订阅同一个 Chat。账号和聊天变更也执行重复检查。

聊天目标变更：在配置表单选择已有目标、修改 Chat ID 并保存。之后新通知使用新 Chat，旧 Delivery 的 Chat 保持不变。故障发给 A 后改成 B，新的恢复通知发给 B；Gateway 不识别故障/恢复类别，也不保存事故周期订阅快照。

Token 轮换：选择已有 Bot 账号并输入新 Token；留空表示保留现有 Token。属于同一实际 Bot 的新 Token 可用于旧 Delivery。如果账号改成另一个实际 Bot，旧 Delivery 进入死信，错误为 `service_bot_identity_changed`，不会发给新 Bot。新通知使用最新账号配置。

只有管理员能修改订阅、账号、目标；operator 可看状态和处理死信，不能修改配置。订阅变更使用服务的 `revision`，账号和目标变更使用各自的 `revision`；冲突返回 409，应刷新、核对再提交。变更保留审计记录，审计不记录 Token、正文或 Chat ID。

## 管理 HTTP 契约

这些接口使用现有管理员 session，不能使用 workload Bearer 凭据配置订阅：

| 操作 | 接口 | JSON |
| --- | --- | --- |
| 查看服务、有效订阅和通知 | `GET /api/gateway/v1/services/{service_id}` | 无 |
| 添加订阅 | `POST /api/gateway/v1/services/{service_id}/subscriptions` | `{"destination_id":7,"expected_revision":1}` |
| 取消订阅 | `DELETE /api/gateway/v1/services/{service_id}/subscriptions/{subscription_id}` | `{"expected_revision":2}` |
| 创建 Bot 账号 | `POST /api/gateway/v1/bot-accounts` | `{"name":"Example Bot","token":"<受管 Token>"}` |
| 更新 Bot 账号 | `PUT /api/gateway/v1/bot-accounts/{id}` | `{"name":"Example Bot","token":"<新 Token>","expected_revision":1}` |
| 创建聊天目标 | `POST /api/gateway/v1/destinations` | `{"name":"Example Operations","bot_account_id":3,"chat_id":-100123456}` |
| 更新聊天目标 | `PUT /api/gateway/v1/destinations/{id}` | `{"name":"Example Operations","bot_account_id":3,"chat_id":-100654321,"expected_revision":1}` |

添加返回 201 和 `id`、新的服务 `revision`；取消返回 200 和相同字段。订阅重复返回 `409 duplicate_subscription`，旧版本返回 `409 revision_conflict`，省略版本返回 400。订阅引用目标，目标引用 Bot 账号，调用方无需重复传 Bot 或 Token。

## 投递状态与恢复

有订阅的接收结果示例：

```json
{"notification_id":"notification-example","service_id":1,"subscription_count":1,"status":"accepted","received_at":"2026-09-10T00:00:00Z","delivery_summary":{"status":"pending","total":1,"succeeded":0},"deliveries":[{"delivery_id":"delivery-example","status":"pending_enqueue","attempt_count":0}]}
```

`status` 是稳定的接收结果；`delivery_summary` 和子 Delivery 状态随发送更新。同键重放保留原通知 ID、接收结果和 Delivery ID，同时可显示最新发送状态。

| 汇总 | 含义 |
| --- | --- |
| `no_subscribers` | 接收时无订阅，没有发送计划 |
| `pending` | 已有持久计划，等待入队、发送或有限重试 |
| `succeeded` | 全部 Delivery 已确认成功 |
| `failed` | 至少一个 Delivery 进入死信、结果不确定或被丢弃；不表示其他目标也失败 |

子状态包括 `pending_enqueue`、`accepted`、`processing`、`retrying`、`succeeded`、`dead-lettered`、`reconciling`、`replay_pending`、`discarded`。限流期限写入 SQLite，重启后继续等待；可确定的临时失败使用既有有限重试。发送结果不确定时进入 `reconciling`，不会自动盲目重发。Gateway 不承诺 Telegram 端 exactly-once。

SQLite outbox 由现有出站 worker 持续恢复。初始入队失败、入队响应丢失和进程重启均复用原 Delivery；Redis 使用 Delivery ID 去重。运行环境必须启用现有 Gateway 出站服务并使用持久 Redis/AOF。接收后查看 `pending` 不能当作 Telegram 成功。

死信可从服务详情或 Routes 的 DLQ 操作入口重放/丢弃。操作需要 operator 或管理员权限并记录审计。重放始终使用原 Bot 账号、实际 Bot 身份和 Chat；不会重新展开当前订阅。每次明确请求的死信重放有持久 generation，即使 Redis 重放响应丢失，worker 仍能完成该次入队。结果不确定的 `reconciling` 不提供自动重放按钮，需要先人工核对 Telegram 的实际结果。
