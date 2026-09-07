# Go 原生状态最终验收

状态：DONE_WITH_CONCERNS。2026-09-07 使用真实 patched Codex、RCA trusted harness 与无宿主挂载/无网络的 Linux executor 完成 28/28 项端到端断言；分层测试全部通过。concern：验收证明的是声明过的 memory/skills 语义状态和隔离执行边界，不是 Codex 进程全部 OS I/O 的形式化审计。

## 验收对象

- RCA 分支：`prd/codex-go-native-state`，P7 验收前基线 `12ea5ad`。
- Codex 分支：`prd/codex-go-native-state`，验收基线 `1dd9e5f416`。
- trusted harness：本地源码构建 `codex-cli 0.0.0`。
- executor：Docker `claw-runtime:latest` 内 `codex-cli 0.144.4`，容器以 `--network none` 启动且没有任何宿主 volume mount。
- 原始证据目录：`/private/var/folders/3_/hh7x529n3s59vbxbrn71vv7r0000gn/T/rca-native-e2e-8khyhqud`；另已打包进入 Fleet 产出库。

## 实际运行结果

| 层级 | 命令/范围 | 结果 |
|---|---|---|
| Go 全仓库 | `go test ./...` | PASS |
| Go 竞态 | `go test -race ./internal/trustedstate ./internal/codexnative ./cmd/rca` | PASS |
| Rust lint | `just fix -p codex-skills-extension -p codex-core-skills -p codex-app-server` | PASS |
| Rust skills | `just test -p codex-skills-extension -p codex-core-skills` | 153/153 PASS |
| Rust memory | `just test -p codex-memories-extension -p codex-memories-write` | 69/69 PASS |
| Rust app-server | `just test -p codex-app-server native_skill` | 2/2 PASS，951 项按过滤条件未运行 |
| 构建 | `cargo build -p codex-cli` 与 `go build ./cmd/rca` | PASS |
| 真实隔离 E2E | `scripts/test-codex-native.py` | 28/28 PASS |

端到端实际观察到：通用 exec 与 apply_patch 只作用于 Linux executor；executor 无法读取或修改 trusted sentinel/journal；harness token 与 canary 没有出现在 executor 环境；native note 仅产生一条 Go receipt，list/read/search 均读回 canonical 正文；native bundled skills 仅产生一条 Go `skills.bundled.ensure` receipt，`skills.list/read` 读取绑定 revision 的 `imagegen` 包；trusted runtime 未创建 `memories/` 或 `skills/` 本地回落目录；executor 离线和 malformed exec-server 都在模型请求前失败。

## P6/P7 关键故障语义

- bundled 包的稳定 request ID 在 store 重启后重放，不追加重复审计。
- authority、package ID、revision 与资源路径由 provider 和 Go 服务双向核对；旧 revision 返回 `snapshot_expired`。
- native enable 先提交 Go；Go 请求失败时不写 TOML。Go 已提交而 TOML 导出目标故障时返回明确失败，修复目标后可重试恢复，Go 仍是 enabled 状态的事实来源。
- bundled 首装、升级、保留用户停用、移除 tombstone、重新引入，以及 v1 有效/无效导入均有测试覆盖。
- model、background、installer 使用不同 token；只有 installer 获得 `native_skills_packages` 并能调用 package lifecycle HTTP API。

## 声明边界

Go 日志是原生 memory 内容、memory jobs/consolidation 水位与 skills package/revision/enabled/tombstone 的唯一事实来源。TOML 是受管导出视图；会话 SQLite、认证、插件本体、MCP/hooks/凭据等 Codex 内务仍保留各自既有存储与授权，不在本报告的“全部 mutation 已审计”表述内。插件包导入不能隐式扩大 MCP、hook 或凭据能力。

通用 FS/exec 的安全结论依赖实际部署继续使用独立机器/账户或无 trusted mount 的容器；RCA 不会仅凭一个 cwd 自动提供 OS 隔离。本次测试不覆盖真实公网 SSH、恶意内核、长期负载或用户主动把已读 trusted 内容复制到 executor 的信息流。
