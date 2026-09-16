# acap v1 Schema and Enforcement Boundary

## 分层

```
kernel syscall -> capability table / taint gate -> acapd verdict
                                      |
                       Landlock + seccomp + unotify
                                      |
                              audit event / Sigsum log
```

`acapd` 是被 supervisor 管理的 Rust 策略进程。kernel 是唯一强制入口：acapd 不可直接
授予 fd，CLI/GUI/ACP 也不可直接修改 capability table。acapd 无法访问时默认 deny 或
挂起等待，不得 fail open。

## CapabilitySet

manifest 编译为稳定的 CapabilitySet：

```json
{
  "issuer": "aok:aid:1234",
  "subject": "aok:aid:5678",
  "object": "tool:web.fetch",
  "actions": ["invoke"],
  "constraints": {"url_prefix": "https://example.com/"},
  "not_before": 1760000000,
  "expires": 1760000300,
  "revocation_id": "r-9f2c",
  "delegation_depth": 0,
  "sealed": true
}
```

Biscuit block/check 用于令牌携带和可收窄委托；Cedar policy 用于 principal、object、action
决策；两者任一拒绝，最终结果为 deny。`sealed=true` 禁止再委托。撤销由
`revocation_id` 和短 TTL 双重控制。

## Taint

每个可外流数据块携带不可伪造的 taint metadata：`origin`、`source_aid`、`turn_id`、
`labels`、`chain_hash`。标签只能增加或由明确的 sanitizer capability 清除。外发类工具
必须经过 taint policy；无法判断时转为 human confirmation，而不是自动放行。

## OS mapping

- Landlock 用于文件树和访问权；seccomp 用于 syscall 形状；unotify 只用于受控慢路径。
- capability fd 关闭或 revoke 后，kernel 立即拒绝新的调用；正在执行的外部调用按工具
  可取消能力处理，不能假定能回滚。
- Unix domain socket、网络、文件和 terminal 权限分别编译，不能用一个 host 级开关替代。
- Linux `root`/`CAP_*` 仅在可选的 legacy adapter 或内部实现域中存在；它们不是 Agent-native
  权限证明。AOK 对象和工具操作必须经过 AOK gate，不能把 manifest 字段本身当作强制证据。

## Audit

每次 grant/use/revoke/deny 记录：`event_hash`、`key_id`、`aid`、`action`、`object`、
`taint_digest`、`decision`、`time`、`prev_hash`。Sigsum 式 witness 是可选增强；本地
链必须先保证顺序、持久化和独立验证，不能以“写入日志”代替 Merkle 证明。
