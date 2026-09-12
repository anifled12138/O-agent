# 02 · Agent Loop

状态：Accepted；durable journal/async receipt/cancel/event resume implemented，token streaming/inbox pending

## 目标

默认循环足够小，可以可靠实现和测试；复杂推理通过可替换 Reasoning Driver 增加，
不修改 Agent、Session、权限和工具执行的基础语义。

## 核心接口

```go
type Agent interface {
    ID() AgentID
    Send(context.Context, InputEnvelope) (Receipt, error)
    Cancel(CancelCause)
    WhenIdle(context.Context) error
}

type ReasoningDriver interface {
    Run(context.Context, TurnHandle) error
}

type TurnHandle interface {
    NextInput(context.Context) (InputBatch, error)
    CompileContext(context.Context, ContextRequest) (ModelRequest, error)
    CallModel(context.Context, ModelRequest) (ModelStream, error)
    DispatchTools(context.Context, []ToolCall) ([]ToolResult, error)
    RequestInput(context.Context, InputRequest) (InputResponse, error)
}
```

`TurnHandle` 是能力接口，不暴露数据库和 Broker。Host 记录每次调用产生的 durable
event，Driver 不直接伪造完成状态。

## 默认 Direct Tool Loop

1. 从 Inbox 原子 claim 一条 waking input 和已经排队的 context input；
2. 开启 durable Turn；
3. 编译上下文并冻结本次请求的 Generation、Provider route 和 Capability lease；
4. 流式调用模型；
5. 提交完整 assistant attempt；
6. 若包含 Tool Call，执行并按模型原始顺序提交 Tool Result；
7. 下一 Step 重新投影上下文；
8. 没有 Tool Call、Driver 主动停止、等待用户、取消或达到预算时关闭 Turn。

## Inbox

输入必须有稳定 ID、来源和目标边界：

- `followup`：排到下一个 Turn，并唤醒 Agent；
- `steer`：尽可能进入下一个 Step；
- `context`：不唤醒，只在后续 Step 被 claim；
- `approval/input response`：恢复对应等待点；
- `system continuation`：由 Host retry/recovery 产生。

Claim 和 discard 都记录事件。取消正在运行的 Turn 不应吞掉取消后提交的新 waking
input。

## Event

持久事件和实时事件分离：

| Durable | Live |
| --- | --- |
| turn/step start/end | status changed |
| accepted input | token/content delta |
| context projection ref | tool progress |
| model request metadata | transient retry countdown |
| assistant committed/failed attempt | UI waiting indicator |
| tool call/result | process health |
| approval request/result | |

完整模型可见内容必须先能由 durable event + artifact 重建。Live event 带单调 cursor，
客户端断线后先取 durable snapshot，再接续 live stream。

## Provider 错误和重试

- 连接失败、429、明确 5xx：在没有副作用时按 Provider policy 重试；
- context overflow：触发一次 Context Policy 收缩，再创建新 Attempt；
- schema/tool protocol 错误：只允许经过记录的兼容修复，不能静默猜测；
- authentication、model not found、permission：立即停止并要求配置；
- 已经开始的 Tool 外部副作用未知：不得自动重放，进入 `needs_reconciliation`。

## Generation 与升级

Agent 创建时固定 Reasoning Driver、Context Policy、系统提示段、Provider route policy
和预算。Tool/Skill 可以按 Turn lease 更新；Driver 不在活动 Turn 中间替换。候选 Driver
通过 Harness 验证后只影响新 Agent，旧 Agent 明确迁移。

## 测试

- scripted provider：text -> tool -> result -> text；
- 多 Tool 并行完成顺序与持久顺序不同；
- streaming 中取消；
- Tool 中取消；
- context overflow 后压缩重试；
- Host 在每个 durable event 后重启；
- steering 与 followup 竞争；
- Provider/Plugin 更新时 lease 固定。

## 当前实现（2026-09-12）

- `agent_turns`、`agent_steps`、`agent_model_attempts` 保存执行投影；
- 用户输入、Turn 创建和首事件在一个事务中提交；最终 assistant 消息、终态和末事件也
  在一个事务中提交；
- `model.requested` 和 `tool.started` 必须先持久化，失败时禁止越过边界继续调用模型或
  执行工具；
- 同一 Conversation 只允许一个 running/cancelling Turn；
- 取消先写 `turn.cancel_requested`，再触发进程内 `CancelCauseFunc`；
- Host 启动时把遗留活动 Turn 分类为 `interrupted/safe_to_retry`，存在未闭合 Tool Call
  时分类为 `needs_reconciliation/unknown_external_effect`，绝不自动重放；
- API 已提供 Turn 列表和取消，Web UI 在运行中显示 Stop。
- V2 提交接口在 durable start 后返回 Receipt，执行绑定 Host 生命周期而不是浏览器请求；
- SSE 使用 durable sequence 作为 cursor，内存 Broker 只发送唤醒信号，断线重连始终从
  SQLite 补齐，因此慢客户端不会成为运行时状态源。

下一小步是 Provider token/tool-call delta streaming、Inbox，以及客户端打开已有运行中
Turn 时自动重连。旧 V1 同步消息接口仍会随 HTTP 请求取消；Web UI 已切换到 V2 异步
Receipt 接口。
