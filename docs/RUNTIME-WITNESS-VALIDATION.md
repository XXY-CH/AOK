# Witness 联签验证

2026-09-19，P3 末片：`PLAN-AOK-DEEP` P3 的"Sigsum 审计链 + witness 联签"。

## 动机

本地审计链是无密钥 hash chain：能写 state 的人可以整条自洽重造（重算每个 hash），
`VerifyAudit` 无法发现。检查点经**独立 witness 密钥**联签后历史被外部锚定——重写
者没有 witness key 就无法为旧检查点重新生成有效签名。

## 实现

- `witness.head`：返回当前链头（最新记录 hash + 条数）供外部 witness 签名。
- `witness.cosign`：记录 `WitnessCheckpoint{HeadHash, Count, Time, PublicKey,
  Signature, Signer}`。签名必须验证、检查点必须与链上该位置一致（否则
  `-32008 ErrWitnessMismatch`，且**不入库**——被拒的检查点正是要抓的攻击）、
  必须推进已见证历史。上限 256 条、联签入审计。
- `witness.verify`：`VerifyAudit` + 逐检查点比对链头 + 验证已记录签名。重写
  （哪怕本地自洽）在检查点位置必然暴露。
- 规范签名消息：`aok-audit-checkpoint:v1|<head>|<count>|<time>`（Sigsum 形状的
  无歧义 canonical bytes）。

## 证据

```sh
cd runtime
go test -race ./...   # TestWitnessCosignAndTamperDetection / TestWitnessPersistenceAndGrowth
go vet ./... && GOOS=linux GOARCH=arm64 go build ./... && GOOS=linux GOARCH=arm64 go vet .
python3 scripts/core-smoke.py
```

全部通过。测试以独立密钥扮演外部 witness：正常联签+验证、伪造链头被拒且不入库、
错误密钥被拒、不推进的检查点被拒、**自洽重造链**通过 `VerifyAudit` 但被
`VerifyWitness` 语义覆盖（检查点头不匹配即 `-32008`）、跨重启保留两个检查点。

## 安全条件（独立审查后明确）

本地 `VerifyWitness` 只比对 supervisor 自己持有的检查点——与审计链同在一个可重写的
state blob 里。因此锚定的安全条件是**外部 witness 自留其签名记录**并在验证时与其
比对：重写者没有 witness key，无法为旧检查点重新签名；但若只信 supervisor 本地
状态，能整库重写的人也能换掉检查点列表。supervisor 侧提供的是协议与验证语义，
不是单机信任锚。已由 `TestWitnessDetectsOnDiskRewrite` 证明：自洽重造落盘链通过
`VerifyAudit`、被 `VerifyWitness` 在锚定位置拒绝。

## 尚未完成

- 真正的外部 witness 服务（独立进程/主机、定时拉取 head 并联签）与 Sigsum log 的
  gossip/透明树——签名验证与锚定语义完整（安全条件见下节），witness 是本地测试中的独立密钥。
- acapd（Rust）与 Biscuit token 的完整迁移（当前 token 为 ed25519 衰减链）。
