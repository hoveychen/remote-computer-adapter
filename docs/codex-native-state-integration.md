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

## P2：skills 是版本包与配置，不只是一个 SKILL.md

| 入口 / 路径 | 已核查实现 | 接入含义 |
|---|---|---|
| `ext/skills/src/provider.rs:59` | SkillProvider 只有 list/read/search，要求 authority 保持一致 | 这是读接口；没有 install/update/delete 事务 |
| `ext/skills/src/provider/host.rs:47` | host read 必须命中已加载的 skill；随后交给 HostSkillsSnapshot | 不应把宿主路径当成任意文件接口开放 |
| `core-skills/src/model.rs:152` | snapshot 固定 metadata，但 read_skill_text 仍从映射 FS 或 LOCAL_FS 读正文 | 不能把该 snapshot 误认作内容版本快照；读取需要绑定 package revision/hash |
| `core-skills/src/service.rs:81` / `:100` | 构造服务时安装 bundled skills；关闭 bundled 时调用 uninstall | 仅关 feature 也可能产生删除维护写入 |
| `skills/src/lib.rs:32` / `:48` / `:121` | marker fingerprint 不匹配时删整个 `.system` 后重写嵌入资源 | 当前 marker 用 DefaultHasher，是缓存指纹，不能直接当作加密内容校验 |
| `core-skills/src/system.rs:6` | bundled uninstall 直接 remove_dir_all，忽略错误 | 审计模式应记录版本停用，清理失败不能伪称物理删除完成 |
| `skills/src/assets/samples/skill-installer/scripts/install-skill-from-github.py:172` | Python 直接 copytree 到 `CODEX_HOME/skills` | 在 remote-only 下这是执行端写入，不会自动安装到可信 host；需新的语义 package import |
| `app-server/src/request_processors/catalog_processor.rs:667` | skills/config/write → ConfigEditsBuilder → clear skills/plugin cache | enabled 配置也是持久状态，应与 skill revision / audit 对齐 |
| `core/src/config/edit.rs:453` | 按 name/path 修改 skills 配置 TOML | 启用状态不能遗漏；外部手改配置需显式导入或标为未审计外部修改 |
| `core-plugins/src/store.rs:288` / `:327` | 按版本 install/uninstall plugin；插件可以带 skills | PluginStore 是另一条持久化入口，不能仅拦单独 skill 安装器 |
| `core-plugins/src/manager.rs:1463` / `:1513` | 先 store.install，再 set_user_plugin_enabled | package 激活与配置分属不同提交；接入时必须处理失败恢复 |
| `core-plugins/src/manager.rs:1566` | 先 store.uninstall，再 clear_user_plugin | 删除同样有跨存储状态，需要 tombstone/恢复语义 |
| `core-plugins/src/remote_bundle.rs:484` | 暂存解包目录 rename 成正式目录 | 原生已存在 staging，可复用校验与原子目录发布，不能据此宣称和审计已原子 |

复核命令：

```sh
cd /Users/hoveychen/workspace/codex
rg -n 'install_system_skills|uninstall_system_skills' codex-rs/core-skills/src
rg -n 'remove_dir_all|fs::write|DefaultHasher' codex-rs/skills/src/lib.rs
rg -n 'read_skill_text|LOCAL_FS' codex-rs/core-skills/src/model.rs
rg -n 'skills_config_write_response_inner|ConfigEditsBuilder|clear_cache' codex-rs/app-server/src/request_processors/catalog_processor.rs
rg -n 'install_resolved_plugin|set_user_plugin_enabled|uninstall_plugin_id|clear_user_plugin' codex-rs/core-plugins/src/manager.rs
```

结论：需要保留 `Host / Executor / Orchestrator` 读域，并新增可信 package 生命周期服务；不能让模型把 executor authority 的路径升格为 host，也不能把现有 `skills_put(id, content)` 当成完整的原生技能包安装。
