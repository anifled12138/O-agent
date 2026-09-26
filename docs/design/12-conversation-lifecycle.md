# Conversation and Turn lifecycle implementation plan

状态：Checkpoint resume implemented; backend build verified; tests left for local trial
更新时间：2026-09-26

## 目标与边界

把用户输入、Agent Turn、取消、后续消息排队、进程重启恢复、重试和编辑变成一套由 SQLite 驱动的生命周期。UI 只展示后端读回的持久状态；浏览器关闭或 SSE 断线不改变 Host 中的 Turn。

“恢复”分成两种能力：重连 SSE 并按 durable sequence 补事件，以及在 Host 重启后恢复执行。Host 启动时会自动续跑绑定仍一致、检查点可读且没有不确定工具副作用的 Turn；未闭合的 `tool.started` 一律进入人工核对。Provider 已返回但响应尚未与检查点事务提交时，续跑会再次请求 Provider，因此不承诺 Provider 请求 exactly-once。

## 状态与不变量

### Turn

活动状态为 `running`、`cancelling`、`awaiting_approval`，同一 Conversation 同一时刻最多一个。终态包括 `completed`、`incomplete`、`failed`、`cancelled`、`interrupted` 和 `needs_reconciliation`。每个状态变化追加有序 durable event；消息、Turn、首/末事件和关联记录使用一个事务提交。

`recovery_class` 的定义：

- `safe_to_retry`：该输入关联的所有尝试都没有 `tool.started`；可在原会话安全重跑。
- `checkpoint_resumable`：Host 已验证加密检查点、运行绑定和工具事件账本；启动后从该检查点自动续跑。
- `not_replayable`：Turn 已完成并执行过工具；重跑可能重复外部影响。
- `unknown_external_effect`：Host 中断或 Turn 失败时至少有一个 Tool 已开始；不能自动重放。
- `manually_reconciled`：用户记录了外部影响核对说明；之后可显式重试或创建分支。

检查点保存已提交的模型消息、pending tool-call 索引、累计调用/Token 指标和最终回答。完整转录用本机 `master.key` 加密后存入 SQLite；这些数据不受 Run Inspector 开关影响。任一工具结果仅在 `tool.completed` 与新检查点同事务提交后才可续跑；工具产生 run-scoped artifact、fragment 状态或更改活动插件路由的 Turn 暂不自动续跑。崩溃时存在未闭合工具调用、检查点不兼容或绑定改变时，系统停止续跑并保留人工核对状态。

### Inbox

消息先以 `queued` 持久化；与新 Turn、用户消息和首事件在同一个事务中 claim。Turn 结束后按 FIFO 启动下一项。Host 启动时先将遗留活动 Turn 分类；有可续检查点的 claimed 项先恢复原 Turn，不可续项进入 `interrupted` 或 `needs_reconciliation`。Conversation 内存在未核对的工具影响时，新 Turn 与后续队列保持等待；核对事务完成后才继续队列。

停止单个 Turn 只取消当前执行。停止整个 Conversation 同时取消活动 Turn 并将所有未 claim Inbox 项标记 `cancelled`；已 claim 的 Turn 按普通取消处理。暂停、恢复都追加 Conversation lifecycle event；暂停期间拒绝新排队输入，显式发送新消息才恢复会话。

### 重试、编辑与分支

- 安全重试保留原用户消息，创建新的 Turn，并记录 `retry_of_turn_id`。
- 安全重试中的编辑、消息 revision、新 Turn 与首事件原子提交；只允许改写没有成功回答的最新用户消息。
- 编辑或重跑已完成对话中的历史输入时创建新 Conversation 分支。分支复制目标输入之前的消息、复用 Provider/项目/权限/Generation 绑定，并从修改后的输入开始；原会话保持只读历史不变。
- 有 Tool 外部影响的失败/取消/中断/未完成 Turn 必须先提交人工核对说明，之后才允许开始同一 Conversation 的新 Turn 或消费排队项。核对记录只允许授权用户创建，追加 `turn.reconciled`；重试和分支事务会再次检查 Conversation 中所有不确定的工具影响。

## API 与 UI

- `POST /api/v2/agent/turns/{id}/cancel`：取消单个 Turn。
- `POST /api/v2/agent/conversations/{id}/cancel`：停止整个 Conversation 并持久暂停、清空未 claim Inbox；回执包含取消中的 Turn 状态与暂停读回结果。
- `GET/POST /api/v2/agent/conversations/{id}/inbox`：读取/排队后续输入。
- `POST /api/v2/agent/turns/{id}/retry`：安全重跑或编辑重跑。
- `POST /api/v2/agent/turns/{id}/reconcile`：提交必填核对说明；不自动重放，核对成功后按 FIFO 恢复排队输入。
- `POST /api/v2/agent/turns/{id}/branch`：从已完成/已核对的用户输入创建分支并重跑，可提交编辑内容。

UI 为用户消息呈现 Turn 状态；为安全终态提供重跑、编辑重跑；为带外部影响的 Turn 提供核对说明表单；对历史输入提供分支编辑入口。停止整个会话时明确展示会清空的排队数量。所有成功操作后重新读取 Conversation、Turn、Inbox 和事件投影。

## 事务与失败处理

- 队列插入与 Conversation 更新时间原子提交；队列 claim、user message、Turn 和首事件原子提交。新 Turn 开始事务会重新检查同会话的不确定工具影响，防止恢复时越过人工核对。
- 编辑 revision、消息改写和新 Turn 原子提交；分支记录、消息前缀、分支输入、generation binding、Turn 和首事件原子提交。
- 核对记录、recovery class 和 `turn.reconciled` 事件原子提交。
- Conversation 暂停事件、活动 Turn 的 `cancelling` 状态及未 claim Inbox 的取消原子提交；随后再触发进程内取消信号。已 claim Inbox 的终态与 Turn 终态/重启分类同事务提交。
- 任一查询、RowsAffected 检查或 Commit 失败都返回错误；异步终态写入失败记结构化错误日志，数据库状态不得伪装为完成。

## 验证方案与验收

### Storage

1. Turn 首事件、输入、assistant 输出和末事件都能从数据库读回；错误的活动 Turn 冲突会回滚输入。
2. 安全重试/编辑在关闭并重新打开 Store 后保留新 Turn、消息 revision 和事件顺序；诱发 revision 或 Turn 写入失败时原消息不变。
3. 多个 Inbox 项按 FIFO claim；Host 重启后未 claim 项仍可读取，存在不确定工具影响时保持排队，核对后继续执行；已 claim 项不重复插入。
4. 恢复测试覆盖 `checkpoint_resumable`、无检查点的 `interrupted/safe_to_retry`、未闭合工具调用的 `needs_reconciliation/unknown_external_effect`，并验证已完成工具不会重复执行、事件 cursor 增长。
5. 取消整个 Conversation 后，当前 Turn 为 `cancelling`、排队项为 `cancelled`、Conversation 为暂停状态；关闭并重开 Store 后仍可读回暂停事件，显式新 Turn 会原子恢复执行状态；失败事务不得留下半套状态。
6. 分支只包含目标消息之前的上下文；原会话不变；分支 start 失败时不残留分支 Conversation 或消息。
7. 未核对的 Tool 影响无法重试；无论 Turn 以完成、失败、取消或 Host 中断结束都可先记录核对说明；所有相关 Tool 尝试都必须核对，事件、说明与 recovery class 可读回。

### API / UI

1. API 检查所有 mutation 返回读回的 authoritative receipt/detail，并拒绝跨用户 Conversation/Turn。
2. `npm run typecheck`、`npm run lint`、`npm run build` 通过；UI 状态、编辑器保留失败内容、核对流程及分支切换完成手工验证。
3. Backend 运行 `go test ./internal/storage ./internal/agent ./internal/httpapi` 与 `go test ./...`，覆盖固定 SQLite fixture，不依赖外部 LLM。

## 实施顺序

1. 固化并迁移状态、分支、核对和 Inbox schema；补 Store 原子接口和存储回归测试。
2. 接通 Service/API：单 Turn/Conversation 取消、安全重试、人工核对、分支重跑、Host 启动 Inbox 恢复。
3. 接通 UI 状态/编辑/分支/核对/清队列流程，所有写操作后读回。
4. 执行后端测试、前端检查和构建，更新 `02-agent-loop.md` 与路线图的已实现/未实现状态。

## 临时命令与脚本隔离

Agent turn 的 run scope 拥有脚本源文件、搜索结果等可按 ID 读取的临时 artifact，并在 turn 结束时统一清理。每条 `exec_script` / `exec_command` 在独立 AppContainer profile 下运行；Windows 的 profile 专属 `AC\Temp` 承载该命令的 TEMP、Go cache 和脚本工作目录，命令退出后删除目录并删除 profile。AppContainer 只获得工作区和必要运行时目录的只读 ACL；项目文件由宿主文件工具修改，命令本身不能写工作区。两种命令都不授予网络 capability，进程树由 Job Object 限制。为恢复宿主异常退出时遗留的只读 ACL 授权，恢复记录保存在 runfiles 根目录；Agent 服务启动时读回记录，撤销授权并清除失效 profile。若当前进程无法恢复 ACL，沙箱会阻止后续命令，直到重启服务触发恢复。turn 级脚本/artifact 仍由 run scope 保留至 turn 结束后统一清理；命令 scratch 的生命周期更短，在命令结束时清理。

当前只有 Windows AppContainer 后端；其他平台在 native sandbox 接入前会拒绝这些命令，避免退回宿主权限直接执行。Windows 运行时的文件 ACL、网络隔离、Job Object 限制及恢复路径仍需在目标 Windows 环境完成验证。

## 本次验证记录

- `frontend/npm run typecheck`、`frontend/npm run lint`、`frontend/npm run build` 均通过。
- SQLite schema DDL、恢复分类迁移 SQL 与 82 条 Turn/Inbox lifecycle SQL 已在本地 SQLite parser 中执行/解析通过。
- 新增的 Go storage regression tests 覆盖暂停/重启/恢复、已 claim Inbox 分类、手工核对后编辑重跑、历史输入分支及取消事务回滚；当前执行环境没有 `go`/`gofmt`，因此尚未运行 `go test ./internal/storage ./internal/agent ./internal/httpapi` 或 `go test ./...`。
