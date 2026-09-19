# LSFS v1 Schema

LSFS 是用户态 fsd 管理的单库存储，不是 Linux VFS 的替代 syscall。实现目标是 git 式
版本、SQLite 事务和可治理记忆；向量索引属于可探测的可选模块。

## Core tables

```sql
CREATE TABLE objects (
  sha256 BLOB PRIMARY KEY,
  kind TEXT NOT NULL CHECK (kind IN ('blob','tree','commit','entry_meta')),
  payload BLOB NOT NULL
);

CREATE TABLE refs (
  namespace TEXT NOT NULL,
  name TEXT NOT NULL,
  commit_sha256 BLOB NOT NULL REFERENCES objects(sha256),
  PRIMARY KEY(namespace, name)
);

CREATE TABLE commits (
  sha256 BLOB PRIMARY KEY REFERENCES objects(sha256),
  parent_sha256 BLOB REFERENCES objects(sha256),
  author_aid BLOB NOT NULL CHECK(length(author_aid) = 8),
  created_at INTEGER NOT NULL,
  message TEXT NOT NULL
);

CREATE TABLE entries_head (
  namespace TEXT NOT NULL,
  path TEXT NOT NULL,
  object_sha256 BLOB NOT NULL REFERENCES objects(sha256),
  meta_sha256 BLOB NOT NULL REFERENCES objects(sha256),
  PRIMARY KEY(namespace, path)
);

CREATE TABLE entry_meta (
  meta_sha256 BLOB PRIMARY KEY REFERENCES objects(sha256),
  origin_signature BLOB NOT NULL,
  access_control BLOB NOT NULL,
  lifespan_policy BLOB NOT NULL,
  version_chain BLOB NOT NULL,
  access_patterns BLOB
);

CREATE TABLE tombstones (
  namespace TEXT NOT NULL,
  path TEXT NOT NULL,
  commit_sha256 BLOB NOT NULL REFERENCES objects(sha256),
  PRIMARY KEY(namespace, path)
);
```

## Agent 与 Application registry

持久 Agent 和 Application 的 registry 与对象、ref、索引使用同一 SQLite 事务更新：

```sql
CREATE TABLE agents (
  agent_uuid BLOB PRIMARY KEY CHECK(length(agent_uuid) = 16),
  created_at INTEGER NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('active','dormant','terminated')),
  policy_sha256 BLOB NOT NULL REFERENCES objects(sha256)
);

CREATE TABLE applications (
  application_id BLOB PRIMARY KEY CHECK(length(application_id) = 16),
  owner_agent_uuid BLOB NOT NULL REFERENCES agents(agent_uuid),
  current_version INTEGER NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('requested','growing','serving','adapting','quiescent','retiring','tombstoned')),
  wake_policy BLOB NOT NULL,
  checkpoint_ref BLOB REFERENCES objects(sha256),
  created_at INTEGER NOT NULL,
  retired_at INTEGER
);

CREATE TABLE application_versions (
  application_id BLOB NOT NULL REFERENCES applications(application_id),
  version INTEGER NOT NULL,
  contract BLOB NOT NULL,
  capability_set BLOB NOT NULL,
  migration_ref BLOB REFERENCES objects(sha256),
  created_at INTEGER NOT NULL,
  PRIMARY KEY(application_id, version)
);

CREATE TABLE application_mailbox (
  application_id BLOB NOT NULL REFERENCES applications(application_id),
  message_id BLOB NOT NULL,
  payload_sha256 BLOB NOT NULL REFERENCES objects(sha256),
  status TEXT NOT NULL CHECK (status IN ('pending','claimed','acked','expired')),
  idempotency_key BLOB NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER,
  PRIMARY KEY(application_id, message_id),
  UNIQUE(application_id, idempotency_key)
);

CREATE TABLE application_wake_bindings (
  application_id BLOB NOT NULL REFERENCES applications(application_id),
  binding_id BLOB NOT NULL,
  source_kind TEXT NOT NULL CHECK (source_kind IN ('timer','port','lsfs')),
  source_ref BLOB NOT NULL,
  wake_policy BLOB NOT NULL,
  enabled INTEGER NOT NULL CHECK (enabled IN (0,1)),
  created_at INTEGER NOT NULL,
  PRIMARY KEY(application_id, binding_id)
);
```

当前用户态 v0 使用 `state.db` 的 supervisor JSON snapshot 保存 LSFS artifact binding
及 scan cursor，尚未实现上述关系表。源 commit 位于 `contexts/contexts.db`；扫描只读该
commit log，mailbox 记录与对应 cursor 一次写入 supervisor state。两个数据库不需要
联合写事务，崩溃时未扫描 commit 保留，已扫描事件通过持久幂等键去重。实际契约和限制见
[control-api.md](control-api.md#当前用户态-lsfs-wake-v0)。

## Route、HostFS 与消息网关

路由决策、宿主挂载授权、artifact 传输和外部消息投递必须持久化，才能在 worker、VM 或
channel 重启后继续对账和 replay。以下表属于同一 SQLite 事务边界：

```sql
CREATE TABLE route_records (
  route_id BLOB PRIMARY KEY,
  application_id BLOB NOT NULL REFERENCES applications(application_id),
  turn_id BLOB NOT NULL,
  policy_version INTEGER NOT NULL,
  backend TEXT NOT NULL,
  model_id TEXT NOT NULL,
  compatibility_key BLOB NOT NULL,
  reason_code TEXT NOT NULL,
  usage BLOB,
  cache_hit_kind TEXT,
  fallback_count INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);

CREATE TABLE hostfs_mounts (
  mount_id BLOB PRIMARY KEY,
  application_id BLOB NOT NULL REFERENCES applications(application_id),
  host_path BLOB NOT NULL,
  guest_path TEXT NOT NULL,
  mode TEXT NOT NULL CHECK (mode IN ('ro','rw','append-only','dropbox-in','dropbox-out')),
  limits BLOB NOT NULL,
  capability_ref BLOB NOT NULL,
  version INTEGER NOT NULL,
  enabled INTEGER NOT NULL CHECK (enabled IN (0,1)),
  expires_at INTEGER
);

CREATE TABLE artifacts (
  sha256 BLOB PRIMARY KEY,
  size_bytes INTEGER NOT NULL,
  mime TEXT,
  origin TEXT NOT NULL,
  taint BLOB NOT NULL,
  manifest BLOB NOT NULL,
  created_at INTEGER NOT NULL
);

CREATE TABLE artifact_transfers (
  transfer_id BLOB PRIMARY KEY,
  artifact_sha256 BLOB NOT NULL REFERENCES artifacts(sha256),
  direction TEXT NOT NULL CHECK (direction IN ('import','export')),
  owner_application BLOB NOT NULL REFERENCES applications(application_id),
  idempotency_key BLOB NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('pending','committed','failed','expired')),
  created_at INTEGER NOT NULL,
  UNIQUE(owner_application, idempotency_key)
);

CREATE TABLE message_channels (
  channel_id BLOB PRIMARY KEY,
  kind TEXT NOT NULL,
  config_ref BLOB NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('active','paused','revoked')),
  created_at INTEGER NOT NULL
);

CREATE TABLE message_identities (
  channel_id BLOB NOT NULL REFERENCES message_channels(channel_id),
  external_principal TEXT NOT NULL,
  principal_ref BLOB NOT NULL,
  PRIMARY KEY(channel_id, external_principal)
);

CREATE TABLE message_conversations (
  conversation_id BLOB PRIMARY KEY,
  channel_id BLOB NOT NULL REFERENCES message_channels(channel_id),
  external_id TEXT NOT NULL,
  application_id BLOB REFERENCES applications(application_id),
  last_sequence INTEGER NOT NULL DEFAULT 0,
  UNIQUE(channel_id, external_id)
);

CREATE TABLE message_inbox (
  message_id BLOB PRIMARY KEY,
  conversation_id BLOB NOT NULL REFERENCES message_conversations(conversation_id),
  external_principal TEXT NOT NULL,
  payload_sha256 BLOB NOT NULL REFERENCES objects(sha256),
  sequence INTEGER NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('pending','claimed','acked','dead-letter')),
  received_at INTEGER NOT NULL,
  UNIQUE(conversation_id, sequence)
);

CREATE TABLE message_outbox (
  request_id BLOB PRIMARY KEY,
  conversation_id BLOB NOT NULL REFERENCES message_conversations(conversation_id),
  payload_sha256 BLOB NOT NULL REFERENCES objects(sha256),
  idempotency_key BLOB NOT NULL,
  status TEXT NOT NULL CHECK (status IN ('pending','sent','failed','cancelled')),
  created_at INTEGER NOT NULL,
  UNIQUE(conversation_id, idempotency_key)
);

CREATE TABLE message_delivery_attempts (
  request_id BLOB NOT NULL REFERENCES message_outbox(request_id),
  attempt INTEGER NOT NULL,
  status TEXT NOT NULL,
  provider_receipt BLOB,
  error BLOB,
  created_at INTEGER NOT NULL,
  PRIMARY KEY(request_id, attempt)
);
```

`host_path`、channel config 和 provider receipt 必须按部署策略加密或以受保护引用存储；
密钥、cookie、authorization header 和 webhook secret 不进入普通对象 payload。消息正文、
附件 artifact 和路由 usage 仍受 ACL、taint、TTL 和可达性 GC 规则约束。

route 记录当前同样落在 supervisor JSON snapshot（`route_records`，全局上限 512 条、
新的逐出旧的，字段为 policy 版本/backend/compat key/fallbacks/cache_hit_kind/token
三项/理由码）；本节的 SQL `route_records` 表属于后续迁移。

`generation` 和当前 AID 属于运行时映射，不覆盖持久身份。worker 重建或 VM 重启时，
supervisor 为同一个 `application_id` 创建新的 aproc/AID，并从 `checkpoint_ref` 和
`pending` mailbox 恢复。退役先写入 `state='retiring'` 和 tombstone commit；只有没有
审计或 capability 引用的对象才能由可达性 GC 回收。

唤醒绑定与 Application registry 在同一事务中创建、禁用或删除。事件本身先进入 durable
mailbox，再由 runtime 按 `wake_policy` 尝试恢复或创建 aproc；客户端是否在线不影响事件
保留和 replay。

`author_aid` 是 kernel 生成的 64 位 AID 的固定 8 字节 big-endian 编码。fsd 必须在写入
commit 前验证引用对象的 `kind='commit'`，并在启用 `PRAGMA foreign_keys=ON` 的事务中执行
所有引用检查；SQLite 外键本身不会检查 `kind`。

启动 probe 决定是否启用 FTS5 和 vector index。启用时，索引更新与 object/ref/head 更新
必须在同一个 `BEGIN IMMEDIATE` 事务中完成；不可用时退化为词法检索或外部索引，不能在
查询结果中绕过 ACL、taint 或 TTL 过滤。

## Commit and concurrency

一次写入事务同时完成新 object、新 commit、ref 前移和当前视图更新；失败则全部回滚。
每个 aproc 使用独立 ref namespace，merge 显式解决冲突。读使用 WAL snapshot；fsd 作为
独立受监督进程，客户端按 fd 语义访问，不能用路径重走替代 capability 检查。

## Forgetting and GC

遗忘顺序为：查询过滤 -> tombstone commit -> reachability GC -> 可选冷归档。GC 只能回收
不可达 object；审计对象和仍被 capability 引用的对象必须保留。硬删除属于慢路径，须有
明确 policy 和审计事件。
