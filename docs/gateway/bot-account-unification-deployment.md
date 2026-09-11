# Bot 与通知账号统一配置：真实环境部署

2026-09-11 05:25 UTC（北京时间 13:25）已部署到 https://telegram-gateway.uulong.net/。

- 部署提交：`277b8f76e50b74a0993cb517508de7124acd32f7`，独立分支 `fix/bot-account-unification`。
- 镜像：`it_manage_telegram_gateway:277b8f7`。
- 镜像 ID：`sha256:6e91dba81a9264ae27847ac59d570d9a59109a82bd315d0977b960063df01632`。
- 原版本：`e38933a5c7ea335056961f4ad6c671be08290856`。
- 使用仓库 `ee/telegram_gateway/build.sh`，从干净的独立提交构建，未包含原工作区其他未提交文档。

部署保持原 Compose 项目、域名、网络别名、监听端口、数据卷及凭据。停止 Gateway 与其 Redis 后，备份 SQLite、加密密钥、Redis 持久化数据和挂载配置；启动原 Redis，仅更换 Gateway 镜像。

受保护的备份和部署证据位于部署主机 `/opt/telegram_gateway/acceptance-1238/bot-unification-277b8f7/`。其中 `stopped-state.tar.gz` 为停止状态备份，`rollback.compose.json` 保存原镜像与配置。回滚镜像时保留当前持久化数据；如需恢复备份，必须先核对部署后新增投递，避免覆盖新数据。

线上验证通过：

- Bot 列表从 1 项变为 2 项：`@botmux_test_1_bot` 与 `@cafe_trans_bot`。
- 两个 Account 均关联对应 Bot，名称与用户名一致。
- 备用 Bot 保持仅发送模式，管理和代理均未启用；原 Bot 保持运行。
- 原有聊天目标、订阅、投递接收关系和路由修订记录逐行保持。
- SQLite 完整性检查通过；Gateway 与 Redis 健康，观察约 112 秒后重启次数仍为 0，无轮询所有权冲突。
- 公网管理 API、服务详情、聊天选项接口返回正常；未登录访问 Bot API 仍返回 401。
- 真实浏览器确认侧栏与订阅选项一一对应，旧 Account 表单已移除，共用 Bot 编辑窗口可以打开与关闭，JavaScript 错误数为 0。
- 部署后出站投递仍为 42 项成功。本次未发送新的真实测试告警。

本地会话验证用的临时凭据文件已删除；原有线上凭据文件保持不变。


## 后续决定：审批 Bot 退出 Gateway 测试

2026-09-11 05:44 UTC，用户确认 `@cafe_trans_bot` 仅用于原审批业务。此前该 Bot 在 Gateway 开启管理轮询后，与本机既有审批服务争用 `getUpdates`，产生 409。审批服务容器从 2026-09-04 起运行，未在本次操作中停止或重启。

已通过正常管理 API 关闭该 Bot 在 Gateway 的管理、代理及长轮询选项，并设为停用。检查确认没有有效测试订阅或业务路由；在受保护备份后，以受约束的 SQLite 事务停用其唯一测试聊天目标、清除已停止轮询对应的历史 409 状态，并追加 `bot.gateway_retire` 审计事件。2 条成功投递及所有历史引用均保留。

状态检查由 FAIL 转为 PASS：审批 Bot 在 Gateway 已停用，主测试 Bot 正常运行，原审批服务仍运行且重启次数为 0。停用账号为保留历史记录仍显示在列表中。操作备份和结果位于部署主机 `/opt/telegram_gateway/acceptance-1238/approval-only-20260911T054451Z/`。
