# Go 统一原生状态协议 v2

状态：P1–P7 已实现并完成分层测试与真实 patched Codex 隔离验收，等待 Boss 的跨仓库合并许可。Boss 已选择 Go 服务作为原生 memory/skills 的唯一事实来源；不实施 Rust SQLite 审计表方案。源码证据见 `docs/codex-native-state-integration.md`，最终验收见 `docs/codex-go-native-state-validation.md`，其中“最终选择”优先于保留的未采用备选。

## 代码归属与实施顺序

RCA 工作区 `.worktrees/codex-go-native-state`，分支 `prd/codex-go-native-state`。P2/P3/P5 的服务实现属于 `internal/trustedstate`，新增原生领域逻辑放相邻文件或子包，避免继续扩大当前 `store.go`。P4/P6 的 Rust 适配在 `/Users/hoveychen/workspace/codex` 的独立 worktree 中开发，不能改其主 checkout。

宏观任务以 root TASKS.md plan `codex-go-native-state` 为准：P2 服务事务、P3 语义 API、P4 原生 read/note、P5 后台 jobs/consolidation、P6 skills lifecycle、P7 跨系统验收。每片对应真实能力握手；未完成的入口拒绝或不注册，不能误开后台路径。

## 单一事实日志

复用现有 `journal.jsonl` 的独占写者与持久化失败停服语义。v1 行保持可重放；新增 v2 envelope，按 `format_version` 分派。sequence 在 v1/v2 之间连续递增，request_id 在整个 root 唯一。新代码必须能读取旧日志；旧程序遇到 v2 行应拒绝，不允许误重放后继续写入。

v2 的一条成功记录包含本次事务的全部资源变更、package manifest、job 更新、migration alias 与审计结果，不分成“对象日志”和“审计日志”两次写入。内存视图仅在完整写入+fsync 后更新。重启从日志恢复；坏行/截断拒绝，不能静默丢弃。

```json
{
  "format_version": 2,
  "kind": "native_transaction",
  "sequence": 10,
  "request_id": "stable-trusted-call-id",
  "actor": {"kind": "model_tool", "thread_id": "thread-id", "call_id": "call-id"},
  "operation": "memory.note.create",
  "request_digest": "sha256-of-canonical-request",
  "expected": [{"domain": "memory.note", "key": "2026-09-06T15-00-00-note.md", "revision": 0}],
  "changes": [{"domain": "memory.note", "key": "2026-09-06T15-00-00-note.md", "revision": 1, "content_base64": "bm90ZQ==", "deleted": false}],
  "jobs": [],
  "result": {"status": "committed", "commit_sequence": 10}
}
```

示例展示字段语义，不是现有可调用 API。实现时 digest 由规范化结构计算，不能接受客户端自报值；内容 hash 由服务计算。日志必须保存可重放的结果/变更，不仅保存 hash。每个拒绝结果同样带 request_id 和 reason；解析/认证阶段非法输入可直接拒绝且没有变更，不能制造部分提交。

域固定为 `memory.note`、`memory.stage1`、`memory.artifact`、`memory.extension`、`skills.package` 和内部 `maintenance` / `migration`。模型工具不能自行选择内部域。native note filename 保留现有时间戳 Markdown 约束；其它 key 按对应领域验证，不把 v1 的扁平 ID 规则硬套到所有原生文件。

资源和 package 删除保留 tombstone、revision 单调增加。批量提交必须先验证全部 CAS，再一次提交，禁止逐个 put 后留下半包。禁止相同事务出现重复 key 或归一化后重复路径。

初始硬限制：单资源 1 MiB、单包最多 512 文件/合计 16 MiB、单事务编码后最多 32 MiB；这些是明确的原型策略，不是测得的用户数据分布。超限整体拒绝。日志读写均按限制增量解码，不能无限 ReadBytes 分配；blob/base64 编码膨胀计入事务限制。读接口分页，单页正文最多 32 KiB，native 适配继续应用原生 token/line 截断规则，不能把 byte 上限宣称为实测 token 数。

## API 与权限

新增 `/native/v2` 可信语义 API，与现有 `/mcp` 同进程、同 store、同锁；仅回环监听并认证。普通 MCP 保留 list/read/put/delete 语义，不暴露任意 URL、宿主路径或 shell。原生 Rust 客户端通过固定 endpoint 与临时凭证访问。

分开模型工具凭证与可信后台/安装器凭证。actor/thread/call/job 信息由可信适配层附加，模型工具 schema 不接受 actor、host root 或管理员能力。后台 token 只用于固定语义端点，不能成为通用文件权限。Rust 的 capability handshake 必须确认 protocol=2、所需能力和存储 identity；失败不得回落 LocalMemoriesBackend。

```json
{
  "protocol": 2,
  "store_id": "persisted-server-generated-id",
  "capabilities": ["native_memory_read", "native_memory_note"],
  "limits": {"resource_bytes": 1048576, "page_bytes": 32768}
}
```

能力逐片增加：P4 只认 read/note；P5 才有 `native_memory_jobs` / `native_memory_consolidation`；P6 才有 `native_skills_packages`。客户端不因版本号等于 2 就假定全部实现。

| 语义操作 | 输入与结果约定 |
|---|---|
| memory.note.create | 原生 filename/note，可信 call identity；create-only；同一 request 重试返回原结果 |
| memory.list/read/search | 保留原生相对 path、line_offset、max_lines、query/cursor 语义；绑定 snapshot revision，游标不能跨版本复用 |
| memory.summary.read | 从同一 canonical memory.artifact revision 读取 summary；没有本地磁盘 fallback |
| resources.batch | 内部事务原语：expected version 向量 + 全量变化集；MCP 不直接暴露 |
| skills.package.replace | package_id、expected revision、全部文件；服务验证 SKILL.md frontmatter 和依赖资源，原子激活 |
| skills.package.enable/delete | 启停/删除与配置视图、tombstone、审计同一提交 |
| skills.package.list/read | authority、package_id、revision、域内 resource key；禁止 executor→host 升格 |
| skills.bundled.ensure | 内置包首装、升级、移除与 maintenance manifest 在一个事务提交；稳定版本 request_id 跨重启重放 |
| maintenance.materialize | 可信侧按 commit revision 生成原生文件视图，报告每次成功/失败；无任意宿主路径参数 |
| migration.import_v1 | source fingerprint、固定原 revision、目标映射；一次原子导入，保留原日志 |

v1 `skills_put` 不保证原生 frontmatter 有效；不得自动把已有任意字符串变成 active native skill。迁移某对象后，用同一条 v2 事务记录 alias/freeze 标记：v1 写请求要么进入同一 native 对象的适配器，要么明确报 migrated_read_only，不能更新留存副本造成双主。未迁移 v1 对象仍按原规则工作。失败迁移不冻结源对象。

## 后台 jobs 与 consolidation

原生 SQLite 的历史与线程数据库仍由可信 harness 管理。**memory 的成功状态、lease、输入 selection、水位与 canonical 内容必须由 Go 同一事务管理**；不能先在 SQLite 标记 success 再调用服务写内容。若需兼容旧数据库界面，只能把相应行作为由 commit_sequence 驱动的派生视图，重放幂等。

Go job 契约：`enqueue`、`claim`、`heartbeat`、`fail`、`stage1.commit`、`phase2.begin`、`phase2.commit`。lease token、server time、expiry、输入版本向量由服务生成和校验，模型不能自报；lease 失败意味着拒绝提交，不是提示性检查。

- stage1.commit 同一事务保存 raw_memory/rollout_summary、job 成功和 phase2 排队/水位。
- phase2.begin 返回服务签发 transaction_id，绑定 lease、memory generation、选中 stage1 revisions。
- phase2.commit 一次保存 MEMORY、summary、其它允许的 artifacts、selected inputs 与 job success；任何 note/输入/lease 冲突均整体拒绝。
- retention、clear、引用 usage、extension seed/prune 也调用服务。SQLite schema 初始化、WAL、临时 projection 不被包装成模型可用的文件能力。
- 原生 consolidation 使用专用语义工具 contributor，不依赖 get_config 中被清空的 MCP 列表；不注册可信侧通用 shell/apply_patch。普通工具的唯一执行域仍是 third-party。

P4 开原生 read/note 时，必须在 `start_memories_startup_task` 入口检查 capabilities，在 create/seed/prune 之前返回。不能单用 `generate_memories=false`。skills 未适配前也不能以关闭 bundled feature 后执行本地 remove_dir_all 作为“已审计禁用”。

## 原生目录视图与快照

服务 canonical 记录 resource bytes 和 package manifest/hash；宿主目录只作不可变 revision 的派生视图。Rust SkillsService/HostSkillsSnapshot 与 memory prompt 从服务或该已校验 revision 读取。列表拿到 A 后，即使 B 激活，A 的读取必须仍是 A 或明确返回过期；不能读可变路径而悄悄拿到 B。

视图先暂存、校验 hash/路径，再发布 immutable generation。active revision 由 canonical 日志决定；磁盘链接/TOML 导出失败时状态是 committed_but_unmaterialized，客户端读取 canonical 或拒绝该视图，不伪称全部安装完成。旧 generation 的回收必须考虑活跃 snapshot；记录 GC 维护结果。

不自动监控宿主目录并把任何编辑当作可信提交。外部编辑只能通过显式 import 和 source fingerprint 进入事实日志。插件里的 MCP/hooks/凭据配置仍走既有独立授权，技能包导入不能隐式扩大能力。

## 对应验收

P2 必测：混合 v1/v2 重放、所有 key CAS 全有/全无、包重建 tombstone、request_id 参数冲突、短写/fsync 失败、重启幂等、坏尾拒绝、数量/体积上限、重复路径、两个服务争锁。

P3 必测：原生 note 文件名、list/read/search 的原生响应格式与截断、snapshot cursor 绑定、MCP 无管理员操作、错误 token/actor/域/绝对路径拒绝、summary 与 read 同源、legacy alias 防双主。

P4 必测：patched Codex 原生 note/read/search/summary 产生唯一 Go receipt；remote-only backend 下成功；停止服务即拒绝；不能落到 LocalMemoriesBackend；未审计 startup 完全不运行。使用 Codex 原生 mock Responses/app-server 集成测试，不能只以 Go 单测替代。

P5 必测：stage1 output + job + enqueue 原子性；phase2 同时发布两个必需产物；失租/新增 note/输入变化拒绝旧 commit；retention/clear/usage 可对账；重启恢复未完成 lease 与 projection。

P6 必测：原生包首装/升级/停用/删除、SKILL.md 与 assets 一致、authority 绑定、旧 snapshot 版本一致、plugin store/config 导出中断恢复、v1 导入重试或无效正文拒绝。

P7 必测：真实 patched Codex + 无宿主挂载执行域；通用 FS/exec 无 local，原生 memory/skills 全部声明覆盖的 mutation 在 Go 中可审计；故障时没有 local fallback。最终报告逐项区分已覆盖语义状态和内部存储维护，不宣称 OS 级全 I/O 审计。

验证运行规则：RCA 按改动运行 `go test -race ./internal/trustedstate ./internal/codexnative ./cmd/rca`；Codex 按其 AGENTS.md 使用 `just test -p codex-memories-extension`、`just test -p codex-memories-write`、`just test -p codex-core-skills`、`just test -p codex-skills-extension` 等实际受影响 crate。新增 config/schema/dependencies 时执行该仓库对应生成流程。没有实现的测试不得写成 PASS。

## P2 实际落地格式

实现位于 `internal/trustedstate/native.go`。v2 envelope 将可信 actor、operation、request_id 和 expected revision 放入 `request`，其 `changes` 保存完整正文与包 manifest；`request_digest` 基于服务规范化后的请求计算。`result` 仅持久化 status、commit_sequence、error，返回的 resources 从同条请求确定性重建并在重放时校验，避免正文在日志中重复编码导致 16 MiB 包无法容纳。此节为上方示意 JSON 的具体字段布局。

原生批次仍是内部 Go API，不向 HTTP 暴露任意 actor/domain。受限的 skills package replace/enable/delete/list/read 与 bundled ensure 已通过 installer-only HTTP 端点开放；服务完成路径/大小/hash/CAS、SKILL.md frontmatter、authority 与快照过期验证。旧版本读取请求明确返回 snapshot_expired；没有默默读新版本。每批最多 1024 个资源变化，包最多 512 文件。读取日志用带硬上限的增量 scanner，缺失最终换行视为截断拒绝。

## P3 原生 memory HTTP 契约

`NativeHTTPHandler` 由可信 owner 配置独立 token、role 和 thread_id；生产启动器为 model、background、installer 分配不同凭证。与 v1 的 `HTTPHandler` 分开构造，再由可信 loopback server 分流路径。所有端点用 POST 与 Bearer 凭证，拒绝 Origin、未知字段、任意 domain/actor/root 和批量事务端点。model_tool 可 read/note，background 可执行受限 memory jobs/consolidation，installer 可执行受限 skills 生命周期；握手按角色声明能力。installer authority 由凭证绑定，不能由请求提供或改写。

`/native/v2/handshake` 以空对象请求返回 protocol、持久化 store_id、按角色能力及 limits。首次启用 native HTTP 时，store identity 作为 maintenance/store-id 提交到同一个日志。其余端点为 `/native/v2/memory.note.create`、`memory.list`、`memory.read`、`memory.search`、`memory.summary.read`，字段命名与 Codex backend 对应。note 请求额外带可信 call_id，request_id 由服务对 thread_id/call_id 确定性生成，响应是原生空对象，提交序号在 `X-RCA-Commit-Sequence`。

list/read/search 的响应 JSON 保留原生字段；`X-RCA-Snapshot` 返回该响应所用提交序号，read 的可选 expected_sequence 可绑定此前列表快照，否则读取当前快照。游标绑定提交序号和全部查询参数，任何提交均保守地使旧游标失效。list 最多 2000 项、search 最多 200 项，另受 32 KiB 正文预算限制。Go read 接受 line_offset/max_lines，不接受 max_tokens；Rust P4 必须继续调用原生 token 截断逻辑。单个搜索匹配正文超 32 KiB 时明确报错，要求减小 context 或用 read，不伪造完整匹配。

`ImportLegacyMemory` 与 `ImportLegacySkill` 是可信内部 API：检查固定源版本/hash，在一个批次创建 native 对象和 migration alias。skill 导入额外验证 frontmatter、name 与包内资源引用。v1 对已迁移对象的后续写入持久化 migrated_read_only 拒绝结果，导入失败不冻结源对象。
