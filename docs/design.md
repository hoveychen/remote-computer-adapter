# remote-adapter 方案纪要

> **已被取代的历史文档。** 本文记录的 syscall 拦截产品（DYLD interpose / seccomp
> + libp2p 传输 + `rca serve`）已于 2026-09-09 整体删除，代码不再存在于本仓库。
> 当前产品是 native-only 的可信 harness，见 README 与
> [`tool-boundary-implementation.md`](tool-boundary-implementation.md)。
> 保留本文是为了记录当时的取舍与 POC 证据，不描述现有实现。

> 目标：让通过 `remote-adapter` 启动的 `claude` 进程，其**工具调用的实际执行环境**（文件系统 I/O、子进程执行）落在另一台机器的 sandbox sidecar 里，而 Claude 本体（推理循环、工具 schema、transcript）与官方原生行为保持 100% 一致，模型不可感知这层分离。
>
> 状态（2026-07-08 POC 更新）：**v3 原始机制（`node --require io-shim.js cli.js`）已被 POC 证伪**——当前 claude 发行版是 Bun 编译的独立二进制，没有可注入的 Node `cli.js`。经老板拍板，POC 转向 **v3b：OS 系统调用层拦截**，并已在 **macOS（DYLD interpose）与 Linux（seccomp-user-notify）双平台实测跑通**：四个风险点全验 + 真实 Claude Code 端到端闭环（Read 读远端-only 文件、Bash 远端执行）。详见 §4（实测结论）与 §4.2（v3b 架构）。本文档是设计+POC 的落地记录，不是最终实现文档。

## 1. 背景：两个参考系统的机制调研

### 1.1 agent-workspace 的 brain/hand 分离（Pattern-2，核心已上线）

- **Brain** = 跑 `claude` 的推理容器，持有 LLM 凭证。**Hand** = 远端 sandbox 里的 `claw-executor`，只有文件系统和 shell，无任何凭证。
- 分离手法：启动 claude 时用 `--disallowedTools "Read,Write,Bash,Edit,Glob,Grep,NotebookEdit,WebFetch"` 禁掉全部本地文件/shell 工具，同时用 `--mcp-config` 注入 `claw-sandbox-mcp`，提供 `mcp__sandbox__bash/read/write/...` 等**同名替代工具**（`internal/sandboxmcp/tools.go:13-98`、`internal/runtime/claudecode/claudecode.go`）。
- 工具调用走一个极简 JSON 协议：`Request{id,tool,args}` / `Response{id,content,isError,exitCode}`（`internal/executor/protocol.go:14-37`），经 Hub WebSocket 中继（只转字节不解析，`internal/hub/executor_relay.go:99-185`）+ X25519/AES-256-GCM 端到端加密送到远端 executor 执行，结果原路返回。带 per-task token（30min TTL，`internal/hub/executor_token.go`）、断线指数退避重连（`internal/sandboxmcp/forward.go:154-174`）。
- **局限（促成本方案否掉 MCP 路线的关键点）**：MCP 工具名必带 `mcp__` 前缀，模型看到的是 `mcp__sandbox__bash` 而非 `Bash`，schema 也是自定义的——工具名和调用形态对模型可见，不是"零分布偏移"的透明替换。
- 成熟度：中心中继、E2E 加密握手、Brain pool、Branch A（compute-node 沙盒）均已合并并上线验证；Branch B（用户机注册）仍在设计阶段。

### 1.2 cc-adapter 的 wrapping（生产在用）

- 一个 Go 二进制 shim，冒充 `claude` 被官方 Agent SDK 调起，内部 spawn 真实 claude 子进程（强制 `--output-format/--input-format stream-json`），在中间做双向控制协议中继（`relay.go:122-168`）。
- 关键能力：
  - initialize 握手时把自己的 in-process MCP server（ide、claude-vscode）合并进 `sdkMcpServers`（`relay.go:201-232`）；
  - `mcp_message` 控制帧路由——目标是自己的 server 就进程内答复，否则透传（`relay.go:279-288`）；
  - `can_use_tool` 拦截（现用于 `--deny-writes` 黑名单）；
  - 环境注入（`CLAUDE_CODE_ENTRYPOINT=claude-vscode` 等计费属性伪装）与 flag 透传（`host.go:166-198`）。
- 也就是说：cc-adapter 已经具备"作为 claude 的进程外壳、接管其 stream-json 控制通道"的成熟形态，这正是 remote-adapter 需要的宿主壳。
- 成熟度：生产部署，官方 SDK（Python/TS）验证通过，flag 版本每日 CI 跟踪。

## 2. 方案演进：为什么最终选择"运行时 I/O 拦截"

讨论过程中依次评估并否决了三条路线，记录取舍理由：

### 方案 A（否决）：MCP 替代工具
仿 Pattern-2，用 `--disallowedTools` + `--mcp-config` 注入 `mcp__sandbox__*` 工具。
**否决原因**：MCP 工具名带前缀、schema 自定义，模型能感知到与原生 `Bash`/`Read` 不同，不满足"训练分布不漂移"的要求。

### 方案 B（否决）：can_use_tool + updatedInput 改写 Bash
利用查证到的机制——权限回调返回 `allow + updatedInput` 只改变**实际执行**的输入，不回写模型看到的 `tool_use`（结构性事实：transcript 里的 tool_use 是模型自己生成的，CLI 无法也不会回写）。设想把 `command: "cargo build"` 在执行层改写成 `rexec -- cargo build`，模型侧完全不可见。
**否决原因**：
1. 只能覆盖 Bash 一个工具，Read/Write/Edit/Glob/Grep 仍需要单独的文件层方案；
2. 文件层最初设计的"路径镜像 + 本地临时 cwd 挂载"引入了路径不一致问题（本地 `/tmp/xxx/proj` vs 远端真实路径 `/home/dev/proj`，Bash 里写绝对路径就会对不上）；
3 即使改成"两侧路径字面相同的 NFS-over-iroh 挂载"解决了路径问题，代价是仍然需要在本地跑一个用户态 NFS server 做文件视图代理，工程上比直接拦截 I/O 层更绕；
4. 老板进一步明确的要求（"Read/Write 应该直接 proxy 到远端，读大文件的一部分不应该真的落地本地磁盘"）用挂载方案很别扭——NFS 视图仍然是"看起来像本地文件"的抽象，不是显式的按需分段读取。

### 方案 C（采纳）：运行时拦截 `fs` / `child_process`
**关键事实**：claude CLI 的 npm 发行版是一个由 Node 运行的 `cli.js`（详见 §4.1），而 remote-adapter 本来就控制 spawn 命令行。于是可以在 `cli.js` 加载**之前**，用 Node 的 `--require` 注入一个 shim，把 Node 内置的 `fs` / `fs/promises` / `child_process` 模块整体替换为远端转发版本。

这一步的意义：**工具实现完全不动**（Read/Write/Edit/Glob/Bash/Grep 全是原生代码在跑），只是它们脚下调用的 `fs.read(fd, offset)` / `child_process.spawn(...)` 被路由到远端 executor 执行。因此：
- 工具名、schema、transcript 100% 原生，模型不可感知任何差异（比方案 B 更彻底——不仅 Bash，所有工具全部原生）。
- 读大文件的一部分就只传那一段（远端 `pread(fd, offset, length)`），原始文件不落本地磁盘，直接命中老板的场景。
- Bash、Grep 的 ripgrep 子进程、harness 启动时的 `git status` 快照，全部通过 `child_process` 生成 → 全部路由到远端执行，`gitStatus` 自然按 remote 呈现（老板明确要求的行为）。
- 路径问题不复存在：不做路径映射，是"按路径决定去哪执行"，见 §3 路由表。

> ⚠️ **POC 证伪（2026-07-08）**：本节的实现手法（`node --require` patch `cli.js`）**对当前发行版不成立**——当前 claude 是 Bun 编译的独立二进制，没有 Node `cli.js`，`--require`/`--preload` 对编译后的独立二进制无效（实测确认）。**但"运行时拦截 fs/child_process、工具实现不动"这个核心思路本身是对的**，只是拦截点从 Node 模块层下沉到了 OS 系统调用层（DYLD interpose）。§3 的路由表、"读大文件只传一段""子进程路由到远端"等目标全部保留，实现载体换成 §4.2 的 v3b。

## 3. 最终架构（v3）

### 3.1 三个组件

```
┌─────────────────────────────┐        libp2p (Noise/TLS, PeerID=公钥)        ┌──────────────────────────┐
│   remote-adapter (Go)     │◀───────────── P2P 直连/打洞/relay 兜底 ─────────▶│   executor sidecar (Go)   │
│                              │                                                │   (远端 sandbox 内)       │
│ - spawn: node --require      │                                                │                          │
│   io-shim.js cli.js ...      │        unix socket (本机 IPC)                  │ - fs 操作服务             │
│ - unix socket 侧 IO RPC 服务  │◀──────────────────────────────────────────────│ - 子进程执行服务          │
│ - libp2p endpoint            │        (io-shim.js ↔ adapter)                  │ - libp2p endpoint         │
└──────────────┬───────────────┘                                                └──────────────────────────┘
               │ spawns
               ▼
┌─────────────────────────────┐
│  node --require io-shim.js   │
│  cli.js (真实 claude 进程)    │
│                              │
│  io-shim.js: patch fs /      │
│  fs.promises / child_process │
│  → 转发到 unix socket         │
└─────────────────────────────┘
```

1. **remote-adapter**（Go，参考 cc-adapter 的 stream-json shim 形态）
   - 作为 claude 的进程外壳，spawn `node --require io-shim.js cli.js ...`（而不是直接 spawn 原生二进制——用 npm 发行版是因为它的 fs/child_process 调用经过标准 Node 模块，可 patch；原生二进制版做不到这一层拦截）。
   - 在本机监听一个 unix socket，作为 io-shim.js 的 IO RPC 服务端。
   - 持有 libp2p endpoint，与远端 executor sidecar 建立 P2P 连接（穿墙细节见 §3.3）。
   - 把 unix socket 收到的 IO 请求按路由表（见 §3.2）转发到本地处理或经 libp2p 转发给远端 executor。

2. **io-shim.js**（小体积 JS，随 adapter 一起注入）
   - 在 `cli.js` 加载前 patch 掉 Node 内置的 `fs`、`fs/promises`、`child_process` 模块。
   - 每个被 patch 的函数把参数（路径、模式、数据、子进程命令等）序列化后经 unix socket 发给 adapter，等待响应后返回给调用方——对 `cli.js` 表现为普通的同步/异步返回值。
   - 不做任何业务判断（路由表逻辑在 adapter 侧，io-shim 只是转发层），保持 shim 尽量薄。

3. **executor sidecar**（Go，远端 sandbox 内，协议设计参考 agent-workspace 的 `internal/executor`）
   - fs 操作服务：对等实现 open/read/write/stat/readdir/mkdir/rm/rename/... 等，支持按 offset+length 的分段读取（满足"大文件只传需要的那一段"）。
   - 子进程执行服务：spawn/exec 语义代理，含持久 shell 会话（cwd/env 跨调用保留，agent-workspace 的 executor 已验证这个设计可行）、流式 stdout/stderr、信号转发、退出码回传、后台任务支持。
   - libp2p endpoint，接受 adapter 侧发起的连接。

### 3.2 路由表（方案核心）

io-shim 拦截到的每次 fs/child_process 调用，adapter 按**路径**决定去哪执行：

| 路径匹配 | 执行位置 | 理由 |
|---|---|---|
| `~/.claude/**`（全局 CLAUDE.md、skills、memory、settings） | 本地 | 老板明确要求：agent 更新全局配置必须落在本地真实 HOME，不能被路由到远端 |
| CLI 自身安装目录、凭证文件、会话 jsonl 等 CLI 内务路径 | 本地 | CLI 自身运转依赖，且这些路径通常在 `~/.claude` 或系统临时目录下，天然与上一条一致 |
| 其余所有路径（包括工作目录、`/tmp`、系统目录等） | **默认远端** | 老板明确要求："只有特定地址会路由到本地，其他时候一概直接在远端操作" |

默认远端意味着连 `/tmp` 都不再是"两个世界"——这是相对 v1/v2 方案（路径镜像、NFS 挂载）的额外收益：只要不在白名单里，本地进程视角里"这个路径不存在"，所有操作都经 RPC 打到远端。

### 3.3 穿墙：go-libp2p

- 选型结论（已与老板确认）：**go-libp2p**，优先纯 P2P 打洞（DCUtR），失败降级到 circuit relay（relay 可自建，不依赖第三方账号体系）。
- Noise/TLS 做端到端加密，**PeerID 即公钥**——配对时交换 PeerID 即完成身份认证，天然解决了 agent-workspace v1 "只防被动审查、不防主动 MITM" 的问题。
- fs RPC 和子进程 RPC 复用同一条 libp2p 连接的不同 stream，一次配对全部搞定。

## 4. POC 实测结论（2026-07-08，`/tmp/rcc-poc` 内自包子实验）

### 4.0 首要发现：原始注入路线（v3）被证伪

`node --require io-shim.js cli.js` 依赖"npm 发行版是 Node 跑的 `cli.js`"。实测三处，前提不成立：

| 来源 | 实测形态 |
|---|---|
| 本机 `~/.local/share/claude/versions/2.1.181` | 215MB **Bun 编译的 Mach-O 独立二进制**（`Bun/1.4.0`, JavaScriptCore + WebKit） |
| npm 平台包 `@anthropic-ai/claude-code-darwin-arm64`@2.1.204 | 44MB 同款 Bun 编译二进制 |
| npm `bin/claude` / `cli-wrapper.cjs` | 都只是 `spawnSync` 那个原生二进制，**没有任何 cli.js** |

两个硬堵点（均实测确认）：① 当前发行版没有 cli.js；② 用 `bun build --compile` 复现——**编译后的 Bun 独立二进制忽略运行时 `--preload`/`--require`**（preload 被当普通参数吃掉，shim 完全不触发）。

补充实测：拿到裸 JS 包、用 `bun --preload shim.js <裸JS>` 跑时，`fs` 同步接口 / `fs/promises` / `child_process` 全部可被 monkeypatch 拦截，连 `Bun.file` 手动 patch 也能拦——**机制本身成立，但官方只发编译二进制、不发裸包**。故 v3 的 Node/Bun 模块层注入对当前发行版走不通，拦截点下沉到 OS 系统调用层（下述 v3b）。

### 4.1 v3b 采纳路线：DYLD interpose（macOS）实测全部跑通

链路：**拷贝 claude 二进制 → ad-hoc 重签（`codesign -f -s -`，去掉 hardened runtime）→ `DYLD_INSERT_LIBRARIES` 注入 interpose dylib → 截到它真实的 libSystem 文件/子进程调用**。（全程只动 `/tmp` 里的拷贝，不碰真实安装。）

四个风险点的实测结论：

1. **同步 RPC 阻塞：风险蒸发**。这在 JS 层是最头疼的点（要 worker thread + `Atomics.wait`，即 `synckit` 思路）；下沉到 syscall/C interpose 层后，**拦截函数本身就是同步阻塞语义**，直接同步 socket 读写即可，根本不需要 Atomics 体操。原风险点 1 消解。
2. **覆盖面：已映射，且发现 `$NOCANCEL` 变体承重**。一次真实 `claude -p`（强制 Read+Grep+Bash）截到 1714 次调用：`stat`×696、`openat$NOCANCEL`×362、`lstat`×351、`open`×158、`open$NOCANCEL`×67、`readlink`×33、`posix_spawn`×31、`access`×14。**关键：`$NOCANCEL` 变体承载约 73% 的文件 open（429/589）**；只 `--version` 时全走普通变体、完全看不到这个。**完整 hook 清单（macOS）**：元数据 `stat/lstat/fstatat/access/readlink`；open 族 `open + open$NOCANCEL + openat + openat$NOCANCEL`；fd 读写 `read/pread/write/pwrite/close/lseek`（**含各自 `$NOCANCEL` 变体——实测 `pread$NOCANCEL`、`close$NOCANCEL` 都是承重的，漏 hook 分别导致读返回 0 / 写回不 flush**）、目录 `__getdirentries64`；子进程 `posix_spawn`。`$NOCANCEL` 用 asm label 绑定（`__asm("_pread$NOCANCEL")`）即可 interpose——已验证能截到。子进程面截到的真实目标印证了纪要预期：`/bin/sh`、`/bin/zsh`（Bash 工具）、`/usr/bin/git`（gitStatus）、`/usr/bin/security`（读 keychain 凭证）、`/bin/ps`。
3. **fake-fd 重定向（读远端不落地）：已验**。一个本地根本不存在的路径（无 interpose 时 `stat` 直接 ENOENT），在 interpose 下伪造完整 fd 生命周期 `stat/lstat→open(假fd)→fstat→lseek→read/pread(切片)→close`：从 10MB 远端后备文件读 offset=5MB 的 25 字节，**fetch 账 `total_fetched=25`——只拉了请求那一段，全文不落本地磁盘**。在 Bun 二进制消费端同样跑通。**实证教训**：第一版只 hook 普通 `pread`，`readSync` 静默返回 0 字节（拉到 `/dev/null` 占位 fd）；补上 `pread$NOCANCEL` 才好——坐实了 `$NOCANCEL` 漏 hook 会**静默写坏读取**。
4. **child_process 转发（posix_spawn）：已验**。interpose `posix_spawn` 把命令转到一个本机 unix-socket 假 executor 执行（同一份 consumer 代码，完全不知道自己的 `/bin/sh` 跑在远端）：✅ stdout 透明穿回父进程原生管道；✅ 退出码保真（7 / 42）；✅ 信号转发（父发 SIGTERM → 代理 → 远端子进程 trap 触发 → exit 42）；✅ 流式（逐字节 pump）；✅ 远端执行已证（`REMOTE_EXECUTOR=1` + 远端 cwd）。原理：interpose 时保留原 `file_actions`（管道 dup2 不变）、只把目标二进制换成代理，故父进程的 pipe 照旧收到 stdout。
5. **性能（原风险点 4）：已量化，结论是"必须分层"**。微基准：raw `stat()` 1.05µs/op、本机 unix-socket 往返 2.58µs/op。
   - **executor 若在 brain 本机**（unix socket，无网络）：一轮 1714 个 fs op 共 ~4.4ms，可忽略。
   - **若每个 op 都是一次阻塞的远端网络往返**：灾难——20ms RTT 下一轮 =34s，光 Grep 的元数据风暴（stat 696+lstat 351+... = 1094 op）就 22s。
   - **关键缓解（机制已验）**：Grep/Glob 的密集 fs 遍历来自 ripgrep 子进程——把该子进程用 posix_spawn 转发到远端（§4.1 点 4 已验），让 ripgrep 的 1094 次 stat 在远端本地跑完、只回传一次结果；fs-interpose 层则只承载少数 Read/Write（正是 fake-fd 切片按需读的用武之地）。**这解释了为什么两层拦截都需要**：fs-interpose 管 Read/Write/Edit（少而大、要切片），subprocess-forward 管 Bash/Grep/Glob（会 spawn 遍历型工具，让它们在远端做密集 IO）。

### 4.1.1 端到端（真实 claude）已验闭环

把 fake-fd VFS + posix_spawn 转发合成一个 dylib，注入 ad-hoc 重签的 claude 副本，跑真实 `claude -p`（haiku，allowedTools=Read,Bash），路由保持外科手术级（只有 VFS 前缀 + sentinel 命中的 Bash 命令走远端，其余透传本地，故 claude 自身启动/凭证/配置照常）：

- **Read 工具读远端-only 文件**：Read `/tmp/rcc-poc/e2e/vfs/secret.txt`（本地不存在），返回了只存在于远端 store 的 `magic-token: ZEBRA-4417-QUASAR`。dylib 日志：`STAT(virt) → OPEN(virt) fd=19 → FETCH store/secret.txt len=109`。
- **Bash 工具远端执行**：Bash 跑 `echo RCC_REMOTE_MARK=$REMOTE_EXECUTOR`，输出 `RCC_REMOTE_MARK=1`（证明跑在设了该 env 的远端 executor）。dylib 日志：`SPAWN-ROUTE /bin/zsh (sentinel hit)`。
- claude 全程正常完成一轮，对底层 syscall 被重定向毫无感知——**整套 v3b 在真实 Claude Code 上闭环成立**，且印证了 §3.2「按路径决定去哪执行、其余本地透传」的路由表可让 claude 照常运转。

### 4.1.2 健壮性尾巴已补（Bun 消费端实测）

- **write/pwrite 写回**：`writeFileSync(vfs/newfile.txt)` → close 时 flush 到远端 store（`FLUSH-W bytes=33`），文件落远端、本地 vfs 路径始终不存在；再读回一致。**又一个 `$NOCANCEL` 承重发现**：写回不触发是因为 Bun 用 `close$NOCANCEL`——补上它 flush 才生效。至此已确认 open/openat/read/pread/write/close 六族都要连 `$NOCANCEL` 变体一起 hook。
- **getdirentries 虚拟目录**：`readdirSync(vfs/existing-dir)` 正确返回 `["a.txt","b.txt"]`。readdir 在 Bun 下走 `__getdirentries64`（实测确认，非 `getdirentries`/`getattrlistbulk`）；interpose 里按 macOS dirent64 记录格式（21B 头 + name + 4B 对齐的 d_reclen）打包合成，Bun 一次解析成功。
- **stderr 分流**：假 executor 改为 stdout/stderr 分开 PIPE、按 `O`/`E` 打标回传，代理分别写 fd1/fd2；命令同时写两条流 + `exit 3`，两条流各自捕获正确、退出码保真。

### 4.1.3 Linux brain：route C 目前是 macOS-only（实测）

在 Docker（`oven/bun:debian`, aarch64）里交叉编译一个 `LD_PRELOAD` .so 拦截 glibc `open/openat/open64`：

- **阳性对照**：`cat` / `head`（走 glibc）被截到（`[LD_PRELOAD open HIT]`）——证明 .so 加载正常、拦截机制有效。
- **Bun**：`readFileSync` 正常读到内容，但 **interpose HIT = 0**——裸 JS 与 `bun --compile` 独立二进制都一样。

结论第一步：**Linux 上 Bun 绕过 glibc、直接发裸 syscall（Zig std 内联 `syscall(SYS_openat,...)`），`LD_PRELOAD` 符号拦截截不到它的文件 I/O**（阳性对照 cat/head 走 glibc 能截，Bun HIT=0）。这与 macOS 形成鲜明对比（macOS 上 Bun 走 libSystem，DYLD interpose 干净生效）。所以 macOS 的 `LD_PRELOAD` 等价手法在 Linux 行不通。

结论第二步（**已实测跑通**）：改用 **`seccomp-bpf` user-notify**（syscall 边界拦截，与 libc 无关，故直发裸 syscall 也逃不掉）。最小 PoC：launcher 装 filter（openat→`SECCOMP_RET_USER_NOTIF`）经 `NEW_LISTENER` 拿 listener fd、用 `SCM_RIGHTS` 交给 supervisor 父进程，再 exec 目标；supervisor 循环 `NOTIF_RECV`，读 `/proc/pid/mem` 取路径，路由命中就 open 远端 store 并用 `NOTIF_ADDFD`（`FLAG_SEND`）把 fd 注入目标、非命中 `FLAG_CONTINUE` 放行真实 openat。实测：
- **裸 syscall 程序**（`syscall(SYS_openat,...)`，LD_PRELOAD 截不到的那种）→ 被截 + 重定向，读到远端内容。
- **真实 Bun**（`readFileSync` 一个本地不存在的路由路径）→ supervisor 截到 openat、注入远端 store 的 fd，Bun 读出 `token: LINUX-9931-FALCON`。

所以 **route C 在 Linux 也可行，只是拦截载体从 DYLD interpose 换成 seccomp-user-notify**。诚实标注的 Linux 特有代价：
- 需要权限（`NEW_LISTENER`+`ADDFD`）：PoC 用了容器 `--privileged --security-opt seccomp=unconfined`；生产里 adapter 亲自 launch claude、可给它装 filter，但需 CAP_SYS_ADMIN 或 user-ns 配置。
- **fd 生命周期语义与 macOS 不同**：seccomp 只 trap 了 openat，注入的是**真实内核 fd**，其后的 read/lseek/fstat/close 都是真 syscall、**不再经过我们**。要做 macOS 那样的"切片按需读、不落地"，要么把 read/lseek 也纳入 seccomp trap（每 syscall 一次用户态往返，开销远大于 macOS 的函数 interpose，叠加 §4.1 点5 的元数据风暴会更糟），要么给注入的 fd 背一个 FUSE/socket 后端。故 Linux 实际设计更可能是 **FUSE 挂载做 fs 视图 + seccomp 只做子进程/特殊路由**，而非纯 seccomp 全拦。

### 4.2 v3b 架构要点（相对 §3 的调整）

- **io-shim.js 不再存在**；取而代之是注入进 claude 进程的拦截层：**macOS = interpose dylib（重签去 hardened runtime）；Linux = seccomp-user-notify supervisor（+ 可能 FUSE 背 fs 视图），均已 PoC 验证**。unix socket + 路由表 + libp2p 三件套（§3.1/§3.2/§3.3）保留不变。
- **adapter 需管理一份自己 ad-hoc 重签的 claude 副本**（它本来就 spawn claude，spawn 副本即可）——重签抖掉了 Anthropic 签名，仅供本地拦截用；`disable-library-validation` 已在原签名里开着，故重签去掉 hardened runtime 后能加载外来 dylib。
- 路由表（§3.2）语义不变：`~/.claude/**`、CLI 内务路径走本地（interpose 里 route() 返回 NULL 即透传真实 syscall），其余默认远端。

### 4.3 尚未验证（诚实标注）

- **流式已验、run_in_background 完整 detach-poll 未验**：命令按 0.5s 节奏输出，代理逐行实时穿回（21.30/21.82/22.32），流式/长任务保活成立；但 Bash `&` 后台任务"脱离本次调用、之后再轮询取输出"的完整语义还没端到端验。
- **占位 fd 用 `/dev/null` 的健壮性边界（已定性）**：实测 Bun 会对 fd 调 `fcntl(F_GETPATH)`（cmd 50）与 `mmap`——`/dev/null` 占位对 F_GETPATH 返回 `/dev/null`（非虚拟路径）、且不能按文件内容 mmap，是**潜在保真缺口**（小文件 read/pread 路径没踩到，故 §4.1.1/4.1.2 的读写照常）。修法：换更保真的占位（shm/临时 fd）或对 slot fd 补 interpose `fcntl(F_GETPATH)`+`mmap`。
- **Linux fd-after-open 的落地策略**：§4.1.3 已证 seccomp 能拦+重定向，但注入 fd 之后的切片/不落地需 FUSE 或把 read/lseek 也纳入 trap，具体选型未定。

（已闭环并移出本清单的项：真实 claude 端到端 →§4.1.1；write/pwrite+getdirentries+stderr →§4.1.2；性能量化 →§4.1 点 5；Linux 是否可 LD_PRELOAD →§4.1.3。）

## 5. POC 产物清单（`/tmp/rcc-poc/`）

- `syscall-probe/`：`interpose.dylib`（open/openat/stat 观察）、`audit2.dylib`（含 `$NOCANCEL` 的全量审计）、`claude-copy`（ad-hoc 重签的 2.1.181 副本）、`cov.log`（1714 次调用审计原始数据）。
- `fakefd/`：`fakevfs.dylib`（fake-fd 重定向 + 切片读，含 `pread$NOCANCEL`）、`remote-store/huge.dat`（10MB 后备）、`consumer.c` / `bun-consumer.js`（C + Bun 消费端）、`vfs.log`（fetch 账）。
- `spawn/`：`exec_server.py`（unix-socket 假 executor，流式+信号+退出码）、`remote_run.py`（代理）、`spawnfwd.dylib`（interpose posix_spawn 转发）、`spawn_consumer.c` / `sig_consumer.c`。
- `bun-probe/`：`bun --preload` monkeypatch 覆盖面验证（含 `bun build --compile` 忽略 preload 的复现）。
- `e2e/`：合并 dylib（`e2e.dylib`）+ 远端-only `store/secret.txt`，真实 claude 端到端闭环（§4.1.1）；`e2e.log` 为路由证据。
- `robust/`：`robust.dylib`（read + write-back + `__getdirentries64` 虚拟目录，含 `close$NOCANCEL`）、`rtest.js`、`fdops.dylib`（fcntl/dup/mmap 边界探针）；§4.1.2 写回/readdir + fd-ops 边界。`spawn/` 的 executor 已升级为 stdout/stderr 分流。
- `linux/`：`LD_PRELOAD` 探针（证明 Bun 绕 glibc，§4.1.3 第一步）。`seccomp/`：`sec.c`（seccomp-user-notify launcher+supervisor）、`rawcat.c`（裸 syscall 测试）、`app.js`，Docker 内实测拦+重定向真实 Bun（§4.1.3 第二步）。
- `perf/`：`bench.c` fs op / 本机 socket 往返微基准（§4.1 点5）。

## 6. 后续步骤

1. ~~真实 claude 端到端闭环~~ ✅ 已完成（§4.1.1）。
2. ~~补齐 fake-fd 的写路径与 getdirentries~~ ✅ 已完成（§4.1.2，含 stderr 分流）；剩 run_in_background 后台任务保活未验。
3. ~~性能量化（risk 4）~~ ✅ 已完成（§4.1 点 5）：结论是必须分层——Read/Write 走 fs-interpose，Grep/Glob/Bash 走 subprocess-forward 让遍历在远端本地跑。
4. ~~Linux brain~~ ✅ 已实测（§4.1.3）：LD_PRELOAD 截不到（Bun 直发 syscall），但 **seccomp-user-notify 能拦+重定向真实 Bun**——route C 在 Linux 也可行，载体换成 seccomp（+可能 FUSE）。
5. 剩余小项（已定性/部分验，非阻塞）：run_in_background 完整 detach-poll、`/dev/null` 占位 fd 的 F_GETPATH/mmap 保真、Linux 注入 fd 之后的落地策略（FUSE vs 全 trap）。
6. 结论：**route C 在 macOS 与 Linux 均已 PoC 验证可行**，可进入正式实现（Go adapter + macOS interpose dylib / Linux seccomp supervisor + go-libp2p executor sidecar）。
