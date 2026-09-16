# AOK HostFS 与 Artifact ABI v1

宿主文件交换由 hostfs bridge 提供，类似虚拟机共享文件夹，但共享范围必须由宿主显式
授权。guest 永远不能通过路径字符串访问宿主任意目录。

## 两种数据面

- `hostfs.mount`：virtiofs live mount，适合频繁、小文件和双向协作。
- `artifact.import/export`：按内容 hash 传输的不可变对象，适合大文件、跨 VM、离线传递
  和需要审计的结果。

mount 是 live view，不是 checkpoint。Application checkpoint 只提交 LSFS 和 mount 中已
显式导入的 artifact；未提交的 hostfs 写入不能被宣称为可恢复状态。

## Mount 请求

```json
{
  "host_path":"/Users/name/project/inbox",
  "guest_path":"/mnt/project",
  "mode":"rw",
  "owner_application":"app-uuid",
  "max_file_bytes":1073741824,
  "expires_at":1770000000
}
```

mode 可为 `ro`、受限 `rw`、`append-only`、`dropbox-in` 或 `dropbox-out`。默认推荐
`ro` 加明确的 `dropbox-out`；双向 `rw` 只对指定目录开放。capability 包含 host path、
guest path、文件类型/大小限制、owner 和过期/撤销状态，内核和 bridge 都必须检查。

## 冲突、事件与安全

默认采用 single-writer 或 version-conflict；`last-writer-wins` 不是默认语义。共享目录
事件包括 create、modify、rename、delete，进入 event source 并以 `file_event_id` 去重。
宿主导入的数据带 `hostfs` 和 `external-content` taint；导出必须经过 destination
capability、taint policy、内容 hash 和审计检查。

`artifact.import` 先写临时对象，校验 hash/大小/类型后原子提交；`artifact.export` 生成
不可变 manifest 和 delivery record。断线可重试，重复 idempotency key 不得重复副作用。
