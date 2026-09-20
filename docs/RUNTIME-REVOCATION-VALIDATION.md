# Runtime 撤销链验证

2026-09-19，P3 切片：验收项"撤销两级生效"的 runtime 侧。

## 两级结构

1. **内核级**（已有）：0006 的 `cap.Revoke()` 撤销 capability 并拒绝全部后代会话
   （probe 验证：revoke 后 descendant 的 client/result 均 `EACCES`）。
2. **runtime 级**（本切片）：ed25519 capability token 的撤销注册表。

## 实现

- 撤销按 token digest 登记（`sha256(tokenBytes||signature)`），持久化于
  supervisor state（上限 1024 条，达到上限返回 `ErrRevocationCapacity`，保留已有撤销），
  带时间与理由，允许和容量拒绝均审计入 hash chain。当前不自动清理过期项；容量回收仍待实现。
- **链式波及**：衰减用同一 issuer key 重签、只追加 caveat，因此后代的 caveat 列表
  必然以祖先为前缀——撤销条目匹配"同 key 且条目 caveat 是 token caveat 前缀"的
  一切 token。撤销中间节点 ⇒ 叶子死亡、根仍可用、撤销后新派生的后代先天无效。
- `capability.check`（含 token 路径）在策略评估后加查注册表，撤销以
  `capability revoked` 理由拒绝；控制面新增 `capability.revoke`（token 文本 +
  理由），只接受**仍可验证**的 token——撤销移除权威，不伪造权威；幂等。

## 证据

```sh
cd runtime
go test -race ./...   # TestRevocationCoversAttenuationChain / TestRevocationRequiresVerifiableToken
go vet ./... && GOOS=linux GOARCH=arm64 go build ./... && GOOS=linux GOARCH=arm64 go vet .
python3 scripts/core-smoke.py
```

全部通过。链测试覆盖：撤销前三级全过、撤销中间节点（叶子拒绝/根存活/新派生后代
无效）、重启后注册表保留、审计含 `capability.revoke`；不可验证 token（过期）被
拒绝登记。

## 链匹配语义（独立审查后收紧）

撤销匹配 = 同 issuer key + **同 Subject** + 精确 digest 或**严格** caveat 前缀。
- Subject 约束：同 key 不同 subject 的独立 token 互不波及。
- 严格前缀：完全相同 caveat 列表的 token 是字节级同一授权（同 digest），不存在
  "巧合兄弟"；共享早期 caveat、之后分叉的兄弟链互不波及，撤销任一根仍级联其真正
  后代。`TestRevocationSiblingsDoNotCrossRevoke` 钉死全部三类。
- 持有 issuer key 者可全新铸造绕过注册表——注册表防的是**被盗 token**，不是被盗
  key；生产 key 托管在 supervisor，属部署边界。

## 尚未完成

- unotify 人工确认慢路径（`confirm` 仍为同步标志）、Sigsum/witness 联签、
  撤销的实时下发（当前为检查时失效，无主动推送）。
