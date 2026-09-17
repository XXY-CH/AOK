# AOK Toolchain 与依赖 Artifact ABI v1（草案）

开发工作负载（语言依赖、工具链、构建产物）不通过 root 安装全局软件。它们以内容寻址
artifact 进入 Application 域，获取、写入和执行各自携带独立 capability。本文件冻结这条
流水线的语义边界；它复用 [hostfs-model.md](hostfs-model.md) 的 artifact 交换、
[web-application.md](web-application.md) 的 URL 前缀约束、[acap-schema.md](acap-schema.md)
的 CapabilitySet 与 [resource-domain.md](resource-domain.md) 的预算记账，不新增内核对象。

## 原则

1. **无全局状态**：工具链和依赖不是系统对象，是 Application 域内的 artifact。不存在
   `pip install` 到全局 site-packages 或 `npm install -g` 的等价操作。
2. **内容寻址**：artifact 由内容 hash 标识，可复现、可缓存、可审计。依赖集由
   规范化解析计划确定；lockfile、目标平台、工具链、features/extras 和 resolver
   版本都是输入。
3. **最小授权**：下载、写入、执行是三个独立 capability，缺一不可组合成"安装"。
4. **污点默认**：外部下载默认携带 `external-content` taint；进入构建产物链必须经过
   策略判定。
5. **安装权与使用权分离**：root 与系统镜像只在构建期变更；工具链可在运行期进入共享
   域，但安装由受监督的 installer 执行、信任来源独立于请求方。Application 对共享域
   永远只读。Agent 运行期不存在提权路径。

## 对象模型

三类 artifact：

- **toolchain artifact**：自包含目录树（解释器、编译器、构建工具），如官方 Python
  发行版、rustup 工具链、Node tarball。解压即用，零系统状态。
- **dependency artifact**：包管理器产物（优先为 wheel、预编译 tarball 或 git tree），由
  规范化解析计划中的 URL 与 hash 唯一确定。
- **build artifact**：构建输出，提交进 LSFS，参与 Application checkpoint。

toolchain descriptor：

```json
{
  "kind": "toolchain",
  "name": "python",
  "version": "3.13.1",
  "triple": "aarch64-unknown-linux-musl",
  "linkage": "static",
  "entry": "/toolchains/python/bin/python3",
  "content_hash": "sha256:<64-hex>",
  "size_bytes": 483183820,
  "runtime_libs": []
}
```

`linkage` 为 `static` 时 `runtime_libs` 必须为空；`dynamic` 时必须列出全部所需共享库
路径，且这些库必须同为可寻址 artifact。禁止 descriptor 引用宿主或系统目录。

## Capability

manifest 扩展（示例为合成值）：

```yaml
toolchains:
  - { name: python, version: "3.13", linkage: static }
dependencies:
  fetch:
    url_prefixes: ["https://pypi.org/", "https://files.pythonhosted.org/"]
    budget_mib: 2048
  install_root: /mnt/project/.deps
fs:
  read:  [/mnt/project/in/**]
  write: [/mnt/project/out/**, /mnt/project/.deps/**]
execute:
  - /toolchains/python/bin/python3
  - /mnt/project/.venv/bin/**
net:
  prefixes: ["https://pypi.org/", "https://files.pythonhosted.org/"]
```

- `toolchain.use` 编译为 entry 路径的 execute 授权与目录树读授权；toolchain 目录本身
  对 Application 只读。这里的 execute 是 Landlock 的 OS 文件执行权，不等于批准目录
  内所有解释型代码；允许执行 Python 仍需通过读权决定哪些脚本和模块可被解释器加载。
- `dependencies.fetch.url_prefixes` 与 `net.prefixes` 必须一致或为其子集；fetch 层
  拒绝前缀外的重定向。
- `execute` 编译为 sandbox launcher 的 `--execute PATH` 规则；`/**` 只允许出现在
  终端段（与 `Policy.Args` 现有约束一致）。
- 第一版只接受明确支持的预编译 artifact；包管理器进程以普通 tool 身份在 Application
  沙箱内运行，继承同一 manifest，不持有任何附加特权。不存在 setuid、postinst 脚本或
  全局注册表写入。需要执行 build backend、npm lifecycle script、Cargo `build.rs` 或
  其他源码构建时返回 `AOK_ENOTSUP`；后续必须使用单独的 build worker，并为其声明输入、
  网络和预算。
- 当前原型 `net` 是布尔开关；URL 前缀是本草案引入的目标形态，前缀级 capability 未实现。

## 解析与获取流水线

```text
lockfile -> resolver -> fetch plan -> fetch (taint) -> verify -> install -> execute grant
```

1. **规范化解析输入**：输入包括 `uv.lock`、`package-lock.json`、`Cargo.lock` 或
   `go.sum`（`go.sum` 必须与 `go.mod` 一起使用），目标 triple、语言 ABI、features/
   extras 和 resolver 版本。resolver 输出的规范化 fetch plan 才是依赖集的身份。
   没有 lockfile 的解析（`pip install pkg` 浮动版本）产生 `unresolved` 状态的临时
   依赖集，checkpoint 必须如实标记，不得当作已解析。
2. **resolver** 是 tool 进程：读 lockfile，产出 fetch plan（URL、预期 hash、大小的
   列表）。fetch plan 必须完整后才允许任何网络动作。
3. **fetch** 按计划逐项下载到临时对象；hash 与 lockfile 预期不符即整批失败，不得
   部分采纳。全部 artifact 带 `external-content` taint。
4. **install** 仅解包已验证的预编译 artifact 到 `install_root`，拒绝安装脚本和源码
   构建，产出新增可执行路径清单；supervisor 将清单编译为 execute 授权（收窄：只授予
   清单内路径）。授权作用于随后创建的 worker；已进入 Landlock 域的进程不能事后放宽权限。
5. **幂等键** = `sha256(canonical fetch plan || toolchain descriptors)`。解析器、目标
   平台和 features 等差异必须反映在 plan 中。重复安装不得重复副作用；同键请求返回缓存结果。

## 缓存与共享

- 下载缓存是 LSFS 内容寻址存储，跨 Application 按内容 hash 去重。缓存命中不重复
  计费，但记录 `cache_hit_kind`。
- 下载流量计入 Application budget（对齐 resource-domain 记账）；module cache 与
  `install_root` 大小受资源域约束，超限走 throttle/quiesce，不得静默删除。
- 工具链缓存（LSFS 层）与项目依赖（mount 层）分离：工具链属于 supervisor 域的共享
  存储，Application 只拿 execute 授权，不能写。共享域支持运行期扩展，见下一节。
  checkpoint、休眠 Application 和审计记录中的引用同样阻止回收；不能只保护当前活跃的
  `toolchain.use`。

## 运行期工具链获取（toolchain.request）

纯构建期预置与"Application 按需生长"矛盾：Agent 遇到镜像中没有的工具链时不应等待
人类重建镜像。共享工具链域因此支持运行期扩展，但安装权与使用权分离：

1. Application 通过 control API 发起
   `toolchain.request(name, version_spec, triple, linkage)`；`version_spec` 必须是精确
   版本或可复核范围，不得是裸浮动标签（如 `latest`）。
2. 安装由 supervisor 域的受监督 installer 进程执行（gofer 模式：不信请求方，同
   fsd/ainf backend 的既有拓扑）。installer 持有 registry 下载 capability，独立于
   任何 Application 的 net 前缀。
3. **信任来源独立**：预期 hash 与签名取自官方 release 的 checksum/签名文件，验证
   密钥预置于 toolchain-bundle manifest。请求方提供的 hash 只作交叉校验，不是信任
   依据。验证失败拒绝安装并写审计事件；installer 不可用时 request 挂起或拒绝，
   不得 fail open，也不得由 Application 自行替代安装。
4. 安装成功后发布 descriptor 与 `toolchain.ready` 事件；请求方获得 `toolchain.use`
   capability（读 + entry execute）。共享域对 Application 仍然只读。
5. 预算归属：下载流量记入请求方 Application 的 budget（谁申请谁付费）；共享域存储
   由全局配额约束；回收采用引用计数 + LRU，被活跃 `toolchain.use` 引用的条目不可
   回收。
6. 宿主缓存层（可选加速）：aok-host 维护内容寻址 store。VM 内 installer cache miss
   时先经 vsock 向宿主取，宿主未命中才出网。网络出口集中在宿主，跨 VM 去重并提供
   单一审计点。

Application 也可以用自身 web capability 把工具链下载到自己域内私用（带
`external-content` taint，execute 仍需授权）；该副本永不进入共享域、不能被其他
Application 引用。私用自由，共享验证。

## 系统级依赖与 legacy 栈

需要 C 库、apt 包或完整 Debian 用户态的场景只有三条显式出路：

1. **预置镜像**：常用 dev stack 在构建期进入 VM 镜像/initfs，`toolchain-bundle`
   manifest 记录全部预置项、hash 与验证密钥，写入 boot 审计。运行期只有读和执行。
   预置是冷启动优化，不是获取工具链的唯一入口；缺失的工具链经
   `toolchain.request` 在运行期补齐。
2. **静态链接优先**：musl/静态 toolchain 优先于动态系统依赖；sandbox 对动态可执行
   文件要求显式授予每个运行时库的读权。
3. **`adapters/legacy/`**：需要真实 apt 的域在镜像构建期完成安装（apt 不存在于运行期）；
   该域通过 AOK control API 获取受限 capability，不得成为 PID1、内核 ABI 或默认
   control 面（对齐 ARCHITECTURE 的 legacy adapter 冻结决议）。

## 检查点与恢复

- Application checkpoint 至少提交 canonical fetch plan hash、已导入依赖 artifact 集合、
  toolchain descriptor 集合和 execute 授权版本号。若声明 `install_tree_exact`，还必须
  提交保存了内容、权限和链接的不可变安装树引用。
- 恢复分为两种明确语义：`install_tree_exact` 直接引用保存了内容、权限和链接的不可变
  安装树；`deps_replayed` 从相同 fetch plan 重新解包并逐项校验 hash。只有计划或 artifact
  不可用时才是 `refetch_required`，重新安装不得伪装成原样恢复。
- hostfs mount 上未显式导入的写入不能计入可恢复状态（对齐 hostfs-model）。

## 审计与 taint

- 每个 artifact 记录：source URL、content hash、size、import 时间、taint 标签、
  owner `application_id`。
- `dep_fetch`、`dep_install`、`dep_execute` 三类事件进入 audit chain，复用
  `event_hash`/`prev_hash` 链式结构。
- Application `adapting` 升级依赖集时换 `contract_version`；依赖变更事件必须携带
  新旧 lockfile hash，可回滚到旧版本继续服务已有 turn。

## 实现状态

本文件为设计草案，全部未实现。已有可复用件：sandbox `Policy.Args` 的 `--execute`
与路径收窄、manifest→Landlock/seccomp 编译、LSFS 设计（SQLite/git 语义）。缺口：
URL 前缀网络 capability、`artifact.import`、`toolchain.request` 与受监督 installer
（含签名验证信任锚）、toolchain-bundle 构建协议、缓存与 budget 记账接线。第一实现
切片建议：静态 Python toolchain 进 initfs + 预置依赖 + execute 授权闭环（零网络），
打通最小依赖工作负载后再开 fetch 层；installer 可作为第二切片，复用 vsock 与
ProcessProvider 监督原语。
