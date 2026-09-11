# Bot 与通知发送账号统一配置验证

## 根因与修复

普通 Account 创建只写入 `gateway_bot_accounts`，Bot 管理读取 `bots`；只有业务路由的一站式创建会同时登记两者。原 `NativeBotIDsForGatewayAccount` 按凭据指纹查询，既不补齐历史数据，也不能维持换凭据后的稳定关联。

现在 `bots` 管理名称、用户名、凭据与运行模式，Account 通过 `native_bot_id` 稳定关联 Bot，保留作为 Gateway API 和历史投递的兼容记录。Bot/Account 新增均补齐另一端；重复登记同一凭据复用 Bot，不重置其运行模式。修改名称或凭据在同一事务中同步所有 Account 别名、推进版本与关联路由修订，并验证订阅接收方唯一性。

订阅页面使用同一个 Bot 编辑窗口，选项按 Bot ID 去重，历史别名下的聊天目标仍然可选。保存订阅、取消和刷新保留当前选择的 Bot。更新凭据时，两个 API 入口均验证该 Bot 所有别名下的已登记聊天。

删除仍被聊天目标、路由修订或历史投递引用的 Bot 返回 409，保留其运行状态。无引用的 Bot 和兼容 Account 一起删除。

## 历史数据库

启动时在凭据加密迁移之后执行幂等补齐：

- 已有相同凭据的 Bot 继续使用原 ID、运行配置和名称。
- 仅存在于 Account 的身份补为 Bot，保持仅发送通知模式，不自动开启轮询或代理。
- Account、目标、订阅和历史投递 ID 保持不变，现有别名继续引用同一 Bot。
- 数据迁移不调用 Telegram，也不发送通知。

历史初次匹配仍依赖相同凭据指纹；无法仅凭用户名安全合并以前已更换为不同凭据的两条记录。建立稳定关联后，后续凭据轮换不会再使关联丢失。Gateway Account API 为兼容旧调用仍可返回历史别名；订阅界面按关联 Bot 展示一次。

## 回归验证

新增 `tests/e2e_bot_account_unification_test.go`，通过实际 HTTP API 和独立 SQLite 数据库验证：

- Account 已登记但 Bot 列表缺失的原始症状。
- Bot 新增后自动进入订阅选项。
- 一个已有 Bot、两个不同身份的 Account 和一个历史别名的迁移，连续两次启动保持一致。
- 双向改名、凭据轮换、过期版本冲突、全部别名的聊天验证、已知聊天保留。
- 删除保护不停止正在运行的 Bot；无引用删除不留下孤立 Account。
- 订阅接收关系保持不变，列表不泄露凭据。

原始红灯命令为 `go test ./tests -run '^TestE2E_BotAccountAppearsInBotList$' -count=2`，修复前连续两次失败，单次约 0.43 秒：Account 已存在，Bot 数量为 0。

验证命令：

```bash
go test ./...
go build -o /tmp/botmux-unification .
```

最终全量测试通过（`tests` 包耗时约 117 秒），构建成功；新增登记、迁移与生命周期回归连续两次通过。

宿主机没有 Go，使用已有 `golang:1.26-alpine` 容器及 Go 缓存执行。浏览器通过编译后的测试程序，使用已有 Node、Puppeteer 和 Chrome 依赖执行 `TestE2E_ServiceChatPickerBrowser` 与 `TestE2E_ServiceSubscriptionBrowser`。两项浏览器验收通过，覆盖选项与 Bot 列表对应、别名去重、群聊选择、订阅发送与取消、部分失败、历史接收方及 EN/RU。截图更新至 `screenshots/service-subscriptions-{en-dark,ru-light}.png`，全部使用合成数据。

本次验证不连接生产站点，不修改线上数据库，不发送真实告警；线上修复需要部署包含本变更的版本。
