# 服务通知接入

服务通知以已鉴权的 workload 来源和完整 `fingerprint` 作为身份。`service_name` 仅用于显示；同名不同 fingerprint 是不同服务。凭据轮换不改变来源或幂等历史。首次有效通知自动登记服务。

本阶段支持服务登记、查询和无订阅记录。没有订阅时不会发送 Telegram、重试或进入死信。后续订阅功能只作用于新通知，历史通知不会自动补发。既有 Business Route 和 it_manage Adapter 接口继续使用原契约。

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

202 表示 Gateway 已持久接收，**不表示 Telegram 已送达**。本阶段 `no_subscribers` 表示没有投递目标。

```bash
curl -sS "$GATEWAY_ORIGIN/api/v1/services/notifications/opaque-id" \
  -H "Authorization: Bearer $GATEWAY_WORKLOAD_CREDENTIAL"
```

查询返回相同通知身份、接收结果、投递汇总和子投递列表，不返回正文或物理目标。不存在和其他 workload 的通知统一返回 404。

400 表示字段/JSON/幂等键无效；401 表示凭据缺失、失效或 workload 禁用；403 表示没有该操作权限；409 表示幂等冲突；503 表示持久接收暂不可用。遇到响应丢失或 503 时保留原键和请求重试，不把 202 当成需要重发的新事件。

## 网页验收

打开“通知服务”入口，按来源、名称、fingerprint、订阅数和最近接收时间识别服务。进入详情查看通知历史和“无订阅，未投递”结果；管理员可以查看正文，operator 只查看不含正文的运行记录，普通用户没有入口权限。页面沿用英语/俄语和主题设置。

接入开发任务模板：为业务事件确定稳定 fingerprint；从受管 Secret 读取 workload 凭据；保留 Keep 的正文渲染及通知条件；按事件身份和阶段生成幂等键；提交一次通知；验证 202 与查询；在网页确认来源和无订阅记录。记录“Keep 接收”“Gateway 持久接收”“Telegram 送达”三个阶段，不能互相替代。

完整设计和后续订阅/Monitor 迁移范围见 [规格 #12](https://github.com/shuoqiudi/it_telegram/issues/12)。本接口首期范围见 [任务 #13](https://github.com/shuoqiudi/it_telegram/issues/13)。
