# Codex 原生工具收口原型

状态：DONE_WITH_CONCERNS，原型实现与验收完成，尚未合并；范围限制见末节。独立入口，不改变 legacy `rca codex ...`。

完整 Codex harness、历史、认证和可信状态留在宿主；通用文件/执行工具使用唯一的 `third-party` 原生 exec-server。memory/skills 的原型对象通过语义 MCP 修改，日志同时承载对象版本和审计。

## 使用

构建 `go build -o /absolute/path/rca ./cmd/rca`。准备可信 JSON 配置：

```json
{
  "binary": "/absolute/path/to/codex",
  "runtime_home": "/absolute/private/native-home",
  "state_root": "/absolute/private/trusted-state",
  "remote_cwd": "/srv/project",
  "exec_program": "/usr/bin/ssh",
  "exec_args": [
    "-T", "-a", "-o", "BatchMode=yes", "-o", "ForwardAgent=no",
    "-o", "SendEnv=-*", "executor-host",
    "cd /srv/project && exec env -i PATH=/usr/local/bin:/usr/bin:/bin HOME=/srv/executor-home CODEX_HOME=/srv/executor-home /opt/codex exec-server --listen stdio"
  ],
  "model": "gpt-5.6-sol"
}
```

远端目录与 exec-server 二进制需要事先准备。上述 SSH 地址和路径是示例；SSH 传输命令由可信操作者配置，原型不创建远端账户、不安装二进制。请关闭 SSH agent forwarding、SendEnv 和已有 SSH 配置中的凭据转发。

```sh
/absolute/path/rca codex-native --config /absolute/prototype.json -- \
  exec --json -- '读取远端项目，并用 memory_put 保存结论。'
```

`exec` 是 RCA 原型的单回合入口；内部实际驱动 `codex app-server`。`--json` 输出原生 app-server 通知（`method` / `params`），**不是** `codex exec` 的 `type` 事件格式。不带 `--json` 时输出助手文本。prompt 放在第二个 `--` 后；省略或使用 `-` 时读 stdin，限制 4 MiB。当前不支持 TUI、多回合、resume、fork、profile、`-c`、feature override、本地输出文件等透传。`--color auto|always|never` 接受但当前输出没有颜色控制序列。

`runtime_home` 与 `state_root` 必须是绝对路径、私有目录（0700）。运行目录首次必须不存在或为空，之后由 `.rca-native` 标记识别；不会接管现有 `.codex`。运行目录和状态目录分别独占，禁止并行写者。runtime 里的生成配置由 RCA 管理，不应手改。

认证留可信侧：`OPENAI_API_KEY` / `CODEX_API_KEY` 可提供给 harness；也可在初次启动生成带标记的 runtime 后，以该目录为 `CODEX_HOME`，用普通 `codex login` 完成可信侧登录。登录命令同时设置 `HOME=<runtime_home>/user-home` 以保持运行环境一致。不自动复制真实用户 HOME、auth.json、历史或 skills。端到端验收使用模拟 provider，**没有实测真实账户的登录/刷新流程**。

可选 `provider_url` 为可信操作者指定的 Responses API 地址，主要用于离线测试。设置后使用自定义 provider、`requires_openai_auth=false`；它不是模型可修改的参数。生产账户场景省略该字段。

## 为什么经 app-server

已安装 Codex 0.153.4 的 `codex exec --cd` 会先在本机规范化目录；Linux 的 `/workspace` 在 Mac 宿主不存在时，尚未调用模型就报 ENOENT。用同机同路径探针无法发现这个问题。

最终实现直接使用原生 app-server：

1. 用私有可信目录加载 harness 配置。
2. `environments.toml` 设置 `default="third-party"`、`include_local=false`，只注册指定 exec-server。
3. 用 `thread/start.environments` 绑定远端 cwd；不在宿主伪造远端目录。
4. 在任何模型调用前验证远端可连接、`local` 返回 unknown environment id，以及 MCP 清单只有 `rca_state` 的八个预期工具。失败即退出。
5. 通过 `turn/start` 开始单回合；交互审批/elicitation 请求被拒绝，不暴露通用 RPC 透传入口。

可信状态 MCP 由 RCA 父进程持有，只监听 `127.0.0.1` 随机端口；每次启动生成 256-bit bearer token，放入可信 harness 环境，不写入配置。HTTP 仅支持带认证的单请求 POST；拒绝 Origin、批量/多行请求、GET 流和审计写接口。

原因：参考源码在移除 local environment 后也拒绝 local stdio MCP，而 local HTTP MCP 仍可使用宿主 HTTP client。最终 HTTP 路径已由真实 0.153.4 harness 验证；源码检出的新版本不被当作已安装版本等价证明。`rca _state-mcp --root /absolute/state` 仍可供可信客户端单独使用 stdio，不能与正在持锁的原型会话并开。

传输前经过 `_native-transport`：清空环境，仅留固定 PATH，再执行可信配置 argv。harness 自身只接收 PATH、私有 HOME/CODEX_HOME、状态令牌、显式认证变量和 TERM；不继承其他 ambient secrets 或 DYLD/LD 注入变量。远端 shell policy 为 `inherit="none"`，不继承可信 harness 环境。

## 状态模型

提供以下八个工具：

| 集合 | 查询 | 修改 |
|---|---|---|
| memory | `memory_list`、`memory_read` | `memory_put`、`memory_delete` |
| skills | `skills_list`、`skills_read` | `skills_put`、`skills_delete` |

list 无参数；read 只接受 `id`。put 需要 `id`、`content`、`expected_revision`、`request_id`；delete 除 content 外相同。ID 仅允许 1–128 个 ASCII 字母、数字、下划线和连字符，首字符必须为字母或数字。内容最多 256 KiB。额外参数、绝对路径、路径穿越和缺失/null 版本均被拒绝。

对象初始 revision=0；成功 put/delete 加一。删除保留 tombstone，list 返回其版本，重建必须提交该版本，不能用 0 重新覆盖。list 不返回正文，read 返回完整对象（包括 tombstone）。不同集合的同名对象互不影响。

`request_id` 在整个 state root 内唯一。相同请求重试返回首次结果，不重复追加日志；跨集合、操作或参数复用会失败。CAS 冲突也会持久化为带结果的审计记录；重试该 request_id 仍返回原冲突，新的尝试需新 request_id。

`journal.jsonl` 是唯一事实来源：sequence、UTC 时间、集合、操作、请求内容、before revision、结果与 after revision 在同一条记录里。写入和 fsync 成功后才更新内存视图并答复成功。独占 flock、私有文件权限与 journal symlink 拒绝保护存储入口。

持久化失败会使服务停止读写，不能继续生成无审计状态。fsync 失败的提交结果可能不确定：关闭后重开，以**同一个 request_id** 重试，由完整日志决定是否已经提交。截断或不一致日志拒绝启动，不自动丢弃尾部；需可信操作者保留原始日志后离线核对。原型不提供自动修复、压缩、加密或抗可信宿主篡改机制；日志包含正文，按敏感状态管理。

## 验证

```sh
go test -race ./internal/trustedstate ./internal/codexnative ./cmd/rca
go test ./...
go build -o /tmp/rca-codex-native ./cmd/rca
python3 scripts/test-codex-native.py --rca /tmp/rca-codex-native --image claw-runtime:latest
```

最后一项要求本机 Docker、真实 Codex binary，以及包含 `/usr/local/bin/codex` 的 Linux 镜像。镜像名可替换；脚本不会安装依赖或访问真实模型账户。脚本创建唯一命名的临时容器，无宿主挂载、无网络，退出时只清理该容器；证据保留在系统临时目录，包含版本、模型请求、工具结果、RPC、日志与断言。

2026-09-06 最终构建通过全部 21 项端到端断言、全仓库 Go 测试与新增模块竞态测试。跨系统验收使用 macOS harness 0.153.4 与 Linux exec-server 0.144.4。覆盖实际 Linux exec 输出、远端 apply_patch 文件、宿主路径读写失败、可信日志 shell/patch 旁路失败、凭据 canary 不出现在执行端环境、memory/skills 写入与读回、幂等、CAS 冲突审计、路径穿越拒绝、local/unknown environment 拒绝，以及断线/协议错误在模型调用前终止。版本组合兼容仅以此测试覆盖为准。

Go 测试另覆盖创建/更新/删除与 tombstone 重建、重启重放、审计损坏、重复写者、短写/fsync 故障、MCP initialize/list/call、HTTP 认证/Origin/请求边界、配置覆盖拒绝、非受管目录拒绝和状态工具未就绪时禁止开始模型回合。

## 明确边界

- 这是**工具收口原型**。SSH 执行端必须是真正隔离的机器/账户，或采用无可信目录挂载的容器。仅把 exec-server 起在同一宿主同一账户的另一个 cwd，不能阻止其访问宿主文件。启动器不会自动验证任意传输命令是否提供了 OS 隔离。
- 关闭 `features.memories`、`memories.generate_memories`、`memories.use_memories`，原生 memory 后台生成/导入被停用。原型 memory/skills 是独立语义对象库，不是原生 `.codex/memories` / `.codex/skills` 的透明兼容后端；已有内容不自动迁移。
- Codex 仍会在可信 runtime 初始化内部 SQLite、保存会话和安装 bundled `.system` skills。这些内部维护写入未接入语义日志；不能声称“全部 harness 内部写入均已审计”。native skills 生命周期完整替换与长期 memory consolidation 属于后续源码接入工作。
- 原型仅注册自己的状态 MCP；没有自动导入用户既有授权 MCP/插件。加入其他授权 MCP 需要扩展可信 inventory 配置与验证，不接受运行时随意加 server。
- 关闭 apps、hooks、remote plugin、multi-agent、shell snapshot 与 skill MCP 自动安装。没有做外部 harness 全部内部 I/O 的形式化证明；也不承诺模型主动把已读入的可信内容写到远端时仍能保密。
- 未覆盖真实 SSH 跨公网、真实模型账户、TUI、会话恢复、长期运行、恶意 OS 内核或可信宿主本身被攻破。故障/坏日志使用 fail-closed 行为，可能需要人工恢复。

官方配置参考：https://learn.chatgpt.com/docs/config-file/config-reference 。厂商未文档化的接线以记录的真实进程验收为依据。
