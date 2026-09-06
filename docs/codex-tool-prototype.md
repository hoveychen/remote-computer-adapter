# Codex 工具收口原型

状态：开发中，不能作为已完成的安全沙盒使用。

本原型实现 Boss 指定的模型：完整 harness 留可信侧；通用文件/执行工具强制使用独立 exec-server；memory/skills 用语义工具修改，所有状态变更有版本和审计。研究证据见 Fleet wiki `remote-adapter/tool-boundary-implementation`。

## 开发接口

原型使用显式子命令，不改变现有 `rca codex ...` 的 legacy hijack 路径：

```text
rca codex-native --config <trusted-prototype.json> -- <codex arguments>
rca _state-mcp --root <trusted-state-root>
```

可信配置包含 Codex binary、独立 runtime CODEX_HOME、可信状态目录、远端 cwd，以及 exec-server 的 program/args。传输命令是 argv 数组，由启动器绑定；不接受模型选择任意后端。用 SSH 时关闭 agent forwarding。

runtime home 必须由原型显式管理，不覆盖用户现有 `.codex/config.toml` 或 `environments.toml`。凭据、历史、既有 skills 的导入不自动发生；需要在最终原型说明里写清如何使用可信侧的认证和状态，不能为了方便复制整个 HOME 到执行端。

代码应生成 `include_local=false` 的 environment 配置，关闭原生 memory 自动生成与导入路径，显式注册状态 MCP。禁止用户传入的透传 flag 意外覆盖原型保证的后端与状态策略；不支持的调用需要明确报错。

原型阶段可以通过自定义 MCP 注入状态后端、暂停原生后台 memory 合并，不要求先构建整个 Codex Rust fork。若这条路径无法确保工具/后台范围，必须报告具体缺口，不能用提示词模拟约束。

## 状态服务

提供 memory/skills 两个逻辑对象集合，各自的 list/read/put/delete 工具。参数只允许逻辑 ID、内容、expected_revision、request_id；不提供宿主绝对路径或通用 shell。

状态内容和审计应共享一个原子提交机制。可使用单写者 append-only journal 作为事实来源并重放得到当前对象视图，避免分别写 state.json 和 audit.jsonl 产生不一致。记录 request_id、对象、操作、before/after revision 和结果；CAS 防覆盖，请求幂等，拒绝路径穿越和跨集合混用。

单个服务占有存储锁；持久化失败不得声称成功或继续无审计写入。审计日志不得向模型提供删改操作。宿主 root 由可信启动参数指定，不由模型输入提供。

## 验收

- Go 状态服务测试：创建、读取、更新、删除、版本冲突、重放幂等、目录/ID 攻击、审计重启恢复、并发写者拒绝及持久化错误。
- MCP 测试：initialize/tools/list/tools/call，只有注册工具可调用，非法参数拒绝且无状态修改。
- 启动测试：仅注册指定 exec-server，没有 local，原生 memory 自动写入被禁用，状态 MCP 保持可信侧；注入环境与用户 secret 不转发到远端。
- 真实 Codex + 模拟模型：文件/exec 落在执行域，未知/local 后端被拒绝；memory/skill mutation 落在状态服务且有审计；尝试用通用文件工具改可信状态不能成功。
- 远端失联和协议错误不回落本地。

已安装 Codex 0.153.4 的 standalone exec-server 能通过 stdio 连接，不需要厂商云注册。注意 `--exit-on-stdin-close` 在该版本要求 `--remote/--environment-id`，不能用于 standalone stdio 探针；测试应自己管理 stdin EOF 与子进程回收。

外部现有 harness 所有内部 I/O 的形式化证明不属于原型完成声明。必须逐条报告通过的验证和剩余的内部插件/维护路径，不能将控制了 memory 自动生成外推成接管全部系统写入。
