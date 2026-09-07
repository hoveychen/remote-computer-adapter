# 原生状态补丁迁移到官方最新版 Codex：最终验收

状态：DONE_WITH_CONCERNS。2026-09-07 已将 Go 原生 memory/skills 补丁迁移到官方 Codex `5ecb3afd1b` 基线，并使用真实 patched Codex、RCA trusted harness 与无宿主挂载/无网络的 Linux executor 完成 28/28 项端到端断言。concern：验收覆盖声明过的 memory/skills 语义状态与隔离执行边界，不是 Codex 进程全部 OS I/O 的形式化审计。

## 验收对象

- RCA：main `647d5dd220`，feature `prd/codex-go-native-state`，验收 HEAD `9e1f318773`。
- Codex：官方 latest main `5ecb3afd1b`，feature `prd/codex-native-state-latest`，验收 HEAD `cbc4526043`。
- trusted harness：官方 package builder 组装的本地 `codex-cli 0.0.0`，同源码 `codex-code-mode-host`。
- executor：Docker `claw-runtime:latest` 内 `codex-cli 0.153.4`；容器以 `--network none` 启动，且没有任何宿主 volume mount。
- 成功证据目录：`/private/var/folders/3_/hh7x529n3s59vbxbrn71vv7r0000gn/T/rca-native-e2e-b9j09bta`。
- Fleet 产出库：`20260907-031837`（`codex-native-state-latest-evidence.tgz`，48.2 MB）。

## 分层验证

| 层级 | 命令/范围 | 实测结果 |
|---|---|---|
| Go 全仓库 | `go test -count=1 ./...` | PASS，强制绕过缓存 |
| Go 竞态 | `go test -count=1 -race ./internal/trustedstate ./internal/codexnative ./cmd/rca` | PASS |
| Rust lint | `just fix -p codex-skills -p codex-skills-extension -p codex-core -p codex-app-server` | PASS；自动删除的 upstream 无关 import 已恢复，未纳入提交 |
| Rust memory | `just test -p codex-memories-extension -p codex-memories-write` | 73/73 PASS，8 MiB `RUST_MIN_STACK` 由 just recipe 设置 |
| Rust skills | `just test -p codex-skills -p codex-skills-extension` | 229/229 PASS |
| Rust app-server | `just test -p codex-app-server native_` | 6/6 PASS |
| Rust core usage | `just test -p codex-core native_memory` | 1/1 PASS |
| Rust state | `just test -p codex-state list_stage1_candidates_for_startup_does_not_write_memory_state` | 1/1 PASS |
| 构建 | `cargo build -p codex-cli`、官方 package builder 的 `codex-code-mode-host`、`go build ./cmd/rca` | PASS |
| 真实隔离 E2E | `scripts/test-codex-native.py` 指向官方组装包 | 28/28 PASS |

## 最新版适配与构建事实

- 最新版将 additional tools 放入 `type=namespace,name=functions`；E2E harness 递归展开 namespace 后仍确认唯一 custom tool 是 `exec`。
- 单独执行 `cargo build -p codex-cli` 不会生成相邻的 `codex-code-mode-host`，code mode 会明确 fail closed。最终验收使用仓库官方 package builder，从 `openai/codex` 的 `rusty-v8-v150.4.0` release 下载并校验 V8 archive/binding，再组装 `codex`、同源码 helper 与 rg 的正式布局。
- 直接构建 helper 时，`v8` crate 默认访问 denoland 的 v150.4.0 资产并收到 404；这不是产品测试通过。只有官方 package builder 路径构建成功后，才执行并记录最终 28/28。

## 端到端实际观察

- 通用 `exec` 与 `apply_patch` 只作用于 Linux executor；executor 无法读取或修改 trusted sentinel/journal，harness token 与 synthetic canary 没有进入 executor 环境。
- native note 只产生一条 Go receipt，list/read/search 都读回 canonical 正文；native bundled skills 只产生一条 `skills.bundled.ensure` receipt，skills list/read 读取绑定 revision 的 `imagegen` 包。
- trusted runtime 未创建 `memories/` 或 `skills/` 本地回落目录。
- revision conflict、幂等 request、路径穿越、host authority 改写、离线 executor 与 malformed exec-server 均被拒绝；后两者在模型请求前失败。
- app-server 只注册 `third-party` executor 环境，拒绝 `local` 与未知 environment ID；native memory 可读而后台生成关闭。

## 分支与产物审计

- RCA 与 Codex feature 的 merge-base 分别等于各自 main；`git diff --check main...HEAD` 双仓通过。
- 两个 feature worktree 均无未跟踪非忽略文件。Codex 仅有可重建的 ignored `target/`、ruff/venv cache 与 `scripts/codex_package/__pycache__/`。
- RCA main checkout 上既有的三个未跟踪研究文档未修改、未纳入本计划。
- 两仓均未 merge、未 push；等待 Boss 明确许可后才能分别 `merge --no-ff`，随后执行 post-merge 验证与 worktree 清理。

## 声明边界

Go 日志是原生 memory 内容、memory jobs/consolidation 水位与 skills package/revision/enabled/tombstone 的唯一事实来源。TOML 是受管导出视图；会话 SQLite、认证、插件本体、MCP/hooks/凭据等 Codex 内务仍保留各自既有存储与授权，不属于“全部 mutation 已审计”。通用 FS/exec 的安全结论依赖部署继续使用独立机器/账户或无 trusted mount 的容器；本次不覆盖恶意内核、长期负载或用户主动将已读 trusted 内容复制到 executor 的信息流。
