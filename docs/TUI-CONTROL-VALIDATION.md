# TUI 与 Control 验证

验证日期：2026-09-16。

## 已验证

- Swift host 源码使用 `swiftc -warnings-as-errors` 直接编译通过。
- `aok settings show`、`aok tui` 的无 supervisor 路径可运行，未配置状态会明确显示。
- 启动真实 `aok-supervisor` Unix control socket 后，TUI 能读取 `health` 和 `application.list`，并展示 Application 状态、owner、token 用量和 failures。
- control API 新增只读 `application.list`、`mailbox.list`、`event_source.list`；Go control socket 测试覆盖创建、查询、timer、mailbox 和权限校验。

## 当前边界

TUI 通过 `aokctl` 作为 control API 适配器，不直接持有 kernel root capability。Application 的创建、冻结、恢复和 token limit 操作已接入；timer 与 mailbox 当前提供快照查看。vsock、完整 permission broker、aproc 级操作和实时事件订阅仍未实现。
