# 原生 memory/skills 审计接入

状态：研究进行中。本文以 Codex 源码 `315195492c80fdade38e917c18f9584efd599304` 为基线，不声称它等价于已安装 0.153.4，也没有将源码推断冒充运行实测。RCA 已合并的原型在 `4351e77`，现有跨系统验收见 Fleet wiki `remote-adapter/codex-tool-prototype`。

本轮产出是源码接入设计及验收规范；未修改或编译 Codex Rust。仍沿用 Boss 的单一可信 harness + 不可信通用执行端约束。所有下列源码路径相对 `/Users/hoveychen/workspace/codex/codex-rs`。

## P1：memory 的真实写入链

| 入口 / 路径 | 已核查实现 | 接入含义 |
|---|---|---|
| `ext/memories/src/backend.rs:12` | `MemoriesBackend` 包含 add_ad_hoc_note/list/read/search，当前无 CAS、request_id | 可复用工具接口；并非全部 memory 存储接口 |
| `ext/memories/src/extension.rs:112` | tools 直接实例化 `LocalMemoriesBackend` | 需要后端 factory/枚举选择，不能只在 RCA 注入同名 MCP |
| `ext/memories/src/local/ad_hoc_note.rs:30` | `create_new` 创建笔记，随后 `write_all`，未统一版本/审计 | create-only 语义、原生时间戳 `.md` 文件名必须保留 |
| `ext/memories/src/prompts.rs:35` | summary 通过 `tokio::fs::read_to_string` 读取 | 替换 tools backend 后，prompt 仍会走旧磁盘副本；必须同时修改 |
| `memories/write/src/phase1.rs:383` | 提取结果交给 `state_db.memories().mark_stage1_job_succeeded` | 与 extension backend 无关的第二个写入入口 |
| `state/src/runtime/memories.rs:827` | 同一个 SQLite tx 更新 jobs、upsert stage1_outputs、排队 phase2 | 可在原有事务内追加语义审计；不要改成 SQLite 提交后再独立写 JSONL |
| `state/src/runtime/memories.rs:47` / `:55` / `:380` | 清空、引用 usage 更新、retention prune 都会修改 memory 状态 | 审计不能只覆盖新建笔记与 extraction 成功 |
| `memories/write/src/storage.rs:13` / `:25` | raw_memories.md 与 rollout_summaries 由 DB 结果派生；会覆写和删除文件 | 文件应标明 projection 身份、来源 revision、内容 hash |
| `memories/write/src/phase2.rs:316` | consolidation cwd 设为可信 memory root，关闭其 memory 自引用，并清空 MCP | 仅把可信 MCP 加到普通线程不覆盖 consolidation |
| `memories/write/src/runtime.rs:320` | consolidation 使用 default_environment_selections 选择执行域 | 保留原型的 remote-only registry 时不能假定可信 root 可通过通用工具访问；本条是源码推断，未实跑开启后的 consolidation |
| `memories/write/src/phase2.rs:410` / `:452` / `:457` | 先检查文件，再 reset Git baseline，再写 SQLite job succeeded | Git 文件状态、数据库 job 和语义审计没有现成单事务边界 |
| `memories/write/src/workspace.rs:13` / `:32` / `:81` | 准备目录、Git baseline、生成/删除 diff 文件 | scratch / projection 的维护事件需与用户语义内容区分 |
| `memories/write/src/start.rs:53` 与 `extensions/ad_hoc.rs:8` | 创建 root、seed instructions；之后 prune 再跑 phase1/phase2 | 初始化也是实际写入，关闭生成模型调用不代表没有文件维护 |
| `extensions/prune.rs:78`（位于 memories/write/src） | 删除到期 extension resources | 删除必须与版本/审计一致，不可只在 install 时记日志 |
| `state/src/runtime.rs:241` | StateRuntime 打开 memories SQLite | DB 建库/WAL/迁移是存储引擎维护，不是模型获准的任意 FS 能力 |

复核命令：

```sh
cd /Users/hoveychen/workspace/codex
rg -n 'LocalMemoriesBackend::from_codex_home|build_memory_tool_developer_instructions' codex-rs/ext/memories/src
rg -n 'create_new|write_all' codex-rs/ext/memories/src/local/ad_hoc_note.rs
rg -n 'mark_stage1_job_succeeded|tx.commit|enqueue_global_consolidation_with_executor' codex-rs/state/src/runtime/memories.rs
rg -n 'mcp_servers =|agent_config.cwd|reset_memory_workspace_baseline|job::succeed' codex-rs/memories/write/src/phase2.rs
rg -n 'default_environment_selections' codex-rs/memories/write/src/runtime.rs
```

预期分别定位硬编码后端、即时笔记写入、原生 SQLite 事务、阶段二的 MCP 清空/文件提交及执行域选择。上述命令已在该基线运行并人工核对函数体。

结论：工具后端、prompt summary 读路径、后台生成与 pruning 必须同时纳入接入范围。不能因为 `MemoriesBackend` 已存在就声称全部原生 memory 只改一个接口即可接入。
