# Axiom 本地 Agent 产品系统设计 v0.1

状态：Draft

日期：2026-09-05

> 产品方向更新：本文的桌面客户端、Plugin Runtime、权限和存储设计继续作为
> 基础架构参考；其中固定以 `react.v1` 为中心、以 Plugin Forge 为自举终点的
> 产品假设，已由
> [`prd-recursive-bootstrap-agent-v0.1.md`](prd-recursive-bootstrap-agent-v0.1.md)
> 中的版本化 Agent Generation、递归自举和 Eval Harness 方向取代。

本文定义 Axiom 下一阶段的产品目标、系统边界和工程路线。它不是对当前
实现的描述，而是后续实现应收敛到的目标架构。已有 Plugin Runtime V2
继续保留，并作为新架构的能力底座之一。

## 1. 产品定义

Axiom 是一个完整的本地 Agent 客户端，而不是一个带聊天框的网页，也
不是一个插件管理后台。

它在用户电脑上运行可靠的 Agent Host，连接用户配置的任意受支持 LLM
API，在本地持有任务状态、工作区、权限、插件、记忆和执行证据。模型可以
更换，任务不能因此丢失；模型可以给出错误建议，Host 不能因此伪造执行
结果或破坏用户环境。

用户的主要交互入口是与 Agent 对话。缺少能力时，Agent 可以在对话中：

1. 识别能力缺口；
2. 设计 Plugin；
3. 生成前端、后端、MCP、Skill 或推理扩展；
4. 构建并执行测试和评测；
5. 向用户展示权限、风险和行为变化；
6. 在用户授权后安装；
7. 无需重启地应用新能力；
8. 发现异常时自动停用或回滚。

Plugin 是统一扩展单元。MCP、Skill、Tool、Context Provider、Verifier、
UI、后台 Service 和外部设备适配器都是 Plugin 的不同 Surface，而不是
彼此竞争的插件体系。

## 2. 核心目标与非目标

### 2.1 核心目标

- 提供 Windows 优先、后续可跨平台的完整桌面客户端。
- 提供从零实现、模型厂商无关的 Go Agent Runtime。
- 同时支持快速问答、长期任务、编码任务和现实世界交互。
- 在模型能力和成本差异很大的情况下保持一致的运行语义。
- 通过 Plugin 扩展 Agent 推理、执行、上下文、界面和外部连接能力。
- 允许 Agent 自举创建 Plugin，但永远不能绕过用户授权边界。
- 所有执行可追踪、可取消、可恢复、可验证、可回滚。
- 随着 Plugin 和历史任务增长，模型上下文不会线性膨胀。

### 2.2 非目标

- 不负责训练或本地部署 LLM。
- 不复制某个现有 Agent Harness 的完整实现。
- 不让模型直接成为操作系统权限主体。
- 不允许 Plugin 通过修改 Axiom 核心源码完成安装。
- 不承诺任意前端代码都能注入桌面 Shell 的任意位置。
- 不把“模型输出了一段计划”视为任务已经可靠执行。

## 3. 架构原则

### 3.1 Host 持有真相

LLM 是可替换、非确定性的决策建议器。以下内容只能由 Host 持有：

- Run 状态和状态迁移；
- 权限与用户批准；
- Tool 的真实执行结果；
- 文件、网络、Secret、进程和设备访问；
- Plugin 安装状态和版本；
- 重试、超时、取消、恢复和回滚；
- 证据、审计和评测结果。

模型不能通过自然语言声称某一步已经完成。只有 Host Event 可以证明它
发生过。

### 3.2 小内核，万物 Plugin

“万物 Plugin”不等于“内核没有边界”。Axiom 保留一个不可被普通 Plugin
替换的最小可信内核：

- Run 状态机；
- 调度器；
- 权限执行器；
- Plugin 生命周期；
- Event Store；
- Provider 协议；
- 资源 Broker；
- 用户批准机制。

内核之外的能力尽量 Plugin 化。Plugin 可以增加策略、验证器和上下文，
但必须通过稳定 Extension Point 注册，不能改写内核状态机或跳过权限。

### 3.3 延迟发现和最小上下文

Host 知道所有已安装 Plugin；Agent Runtime 只知道当前 Agent 可使用的
Capability Card；模型只看到当前步骤必要的少量定义。MCP Tool Schema、
Skill 正文、历史证据和 Plugin 代码均按需加载。

### 3.4 Effect 必须显式

每个动作必须声明 Effect：只读、可恢复写入、外部副作用、高风险或不可
逆。调度器根据 Effect 决定是否并发、重试、请求批准或生成补偿动作。

### 3.5 失败是正常状态

每个长期 Run 都必须支持进程退出、网络中断、模型限流、电脑休眠、客户端
关闭、Plugin 崩溃和部分执行。恢复从已提交 Checkpoint 开始，而不是让
模型凭聊天记录猜测进度。

## 4. 系统拓扑

```text
┌──────────────────────── Axiom Desktop ────────────────────────┐
│ Native Shell                                                   │
│ Window / Tray / Update / Notification / File Picker           │
│                                                                │
│ Product UI                                                     │
│ Chat / Runs / Workspace / Diff / Terminal / Approval / Plugin │
└──────────────────────────────┬─────────────────────────────────┘
                               │ local authenticated IPC
┌──────────────────────────────▼─────────────────────────────────┐
│ Axiom Host (Go)                                                 │
│                                                                │
│ API Gateway       Identity        Event Store      Scheduler   │
│ Run Engine        Context Engine  Policy Engine    Eval Engine │
│ Provider Router   Capability Hub  Plugin Manager   Brokers     │
└─────────────┬─────────────────┬──────────────────┬──────────────┘
              │                 │                  │
       HTTPS Provider API   Plugin Sidecars    MCP Processes
       OpenAI / Anthropic   Service / Tool     stdio / HTTP
       DeepSeek / others    Context / Verify   remote / local
```

### 4.1 进程模型

正式客户端采用至少两个进程：

- `axiom-desktop.exe`：桌面 Shell 和用户界面；
- `axiomd.exe`：Go Agent Host，可在窗口关闭后继续执行授权的长期任务。

Plugin backend 和本地 MCP server 使用独立受限子进程。UI Plugin 运行在
隔离 WebView 中。桌面 Shell 崩溃不应导致 Run 丢失；Host 崩溃后应通过
Event Store 恢复。

首版 Windows 客户端建议使用 Wails/WebView2 作为窗口与打包基础设施，
React/TypeScript 作为渲染层。Wails 只承担桌面外壳和 IPC，不承载 Agent
框架逻辑。未来如需更强隔离，可将 UI 与 Host 完全拆成独立安装组件。

## 5. 完整客户端设计

### 5.1 客户端不是简单聊天页

客户端至少包含以下一级能力：

- 对话与流式输出；
- 多 Run 管理、暂停、继续、取消和分支；
- 计划和执行图查看；
- Tool 调用、输入、输出、耗时和证据查看；
- 文件树、编辑器、Diff 和变更批准；
- 受控终端和后台进程状态；
- Provider、模型、成本和路由设置；
- Plugin 安装、权限、版本、健康与回滚；
- MCP 连接和 Skill 管理；
- 通知、系统托盘和长期任务；
- 日志、诊断包和恢复入口。

### 5.2 对话是一等控制面

所有常用控制都可以在对话里完成，但高风险行为不使用含糊的文字确认。
Host 在消息流中生成原生交互卡片：

- 权限批准卡；
- 文件变更 Diff 卡；
- Plugin 安装卡；
- 外部发送确认卡；
- 计划分支选择卡；
- 失败恢复卡。

卡片携带 Host 签发的短期 Action Token，绑定用户、Run、操作摘要、
Plugin Release 和过期时间。模型无法伪造有效 Token。

### 5.3 客户端扩展

Plugin 不能 patch 主客户端源码。前端扩展通过声明式 Slot 和隔离 UI
Bundle 完成，例如：

- `chat.message.renderer`；
- `tool.result.renderer`；
- `workspace.panel`；
- `settings.section`；
- `run.inspector.tab`；
- `composer.action`。

Plugin UI 通过受类型约束的 Client Bridge 调用已授权 Service。需要操作
系统通知、剪贴板、文件选择器或设备时，调用 Native Broker，而不是直接
获得 Shell 权限。

## 6. Agent 推理内核

### 6.1 V1 默认核心：可替换的 ReAct 式 AgentLoop

V1 不自创未经验证的推理范式。默认采用经过广泛实践的 ReAct 式循环：模型
读取当前上下文，决定回答或调用 Tool，Host 执行 Tool 并把观察结果追加到会话，
模型继续推理，直到完成、取消、需要批准或达到预算。

ReAct 在这里是执行模式，不是外部框架依赖。Host 自己持有会话事实、权限、
预算、取消、重试和 Tool 调度；具体 AgentLoop 通过稳定接口替换：

```text
Prompt Inbox -> Context Compile -> Model Stream
                                  |          |
                                  | answer   | tool calls
                                  v          v
                              Complete <- Guarded Tool Dispatch
                                              |
                                              v
                                      Append Observation
                                              |
                                              +----> next model step
```

首个实现命名为 `react.v1`。AgentLoop 只决定“下一步做什么”，不能绕过
Capability Registry、Policy、Approval、Resource Broker 和 Event Store。

### 6.2 未来候选：ADG 节点

Adaptive Deliberation Graph（ADG，自适应推理执行图）保留为复杂长任务的
研究草案，不是 V1 前置条件，也不默认进入生产路径。只有在核心运行稳定并且
证据表明 ReAct 式循环无法满足任务需求后，才通过新的 AgentLoop 实现引入。

若未来启用 ADG，一个 Run 可由 Host 管理的节点组成：

```go
type Node struct {
    ID           string
    Kind         NodeKind
    Goal         string
    Inputs       []ArtifactRef
    Dependencies []NodeID
    Capability   CapabilityRef
    Effect       EffectClass
    State        NodeState
    Budget       Budget
    RetryPolicy  RetryPolicy
    VerifyPolicy VerifyPolicy
}
```

首批 Node Kind：

- `answer`：基于已知证据生成回答；
- `discover`：搜索 Capability、Skill、MCP 或资料；
- `inspect`：读取环境和收集证据；
- `transform`：生成代码、文档或结构化数据；
- `execute`：调用有副作用的能力；
- `verify`：运行测试、检查事实或交叉验证；
- `approval`：等待用户授权；
- `checkpoint`：提交可恢复状态；
- `recover`：处理失败和重新规划。

### 6.3 ADG 候选的决策协议

模型不直接控制调度器。每轮模型输出结构化 `DecisionEnvelope`：

```json
{
  "mode": "execute",
  "objective": "验证并修复构建失败",
  "assumptions": [],
  "proposedNodes": [],
  "completionClaim": null,
  "needsUserInput": false
}
```

Host 校验节点、依赖、预算、权限和 Capability 合约后才提交到 Run Graph。
不支持可靠结构化输出的模型，由 Provider Adapter 使用受限 Tool Calling
协议转换，而不是解析任意 Markdown。

### 6.4 ADG 候选的自适应推理深度

Run Engine 计算 `DeliberationProfile`：

- `fast`：稳定事实、低风险、无 Tool 或单个只读 Tool；
- `standard`：短计划、有限 Tool、多步验证；
- `deep`：未知环境、跨模块修改、复杂故障；
- `critical`：高风险 Effect，独立验证器和用户批准。

升级条件包括连续失败、证据冲突、计划漂移、预算异常和高风险 Effect。
降级条件包括目标已满足、证据充分或后续节点独立且可并发。这样简单任务
不会被沉重规划拖慢，复杂任务也不会被单一 ReAct 链限制。

### 6.5 执行与验证分离

每个可验证节点都定义成功条件。执行者不能仅凭自己的自然语言宣布成功。
验证可以由以下来源完成：

- 确定性程序，例如测试、Schema、文件 Hash、HTTP 状态；
- Plugin Verifier；
- 不同上下文或不同模型的审查步骤；
- 用户确认；
- 外部系统回执。

只有 Evidence Gate 通过，节点才进入 `committed`。否则进入 `retryable`、
`needs_replan`、`needs_approval` 或 `failed`。

### 6.6 并发和长期任务

调度器只并发执行满足以下条件的节点：

- 依赖已完成；
- Effect 互不冲突；
- Workspace Lock 不重叠；
- Provider 和 Tool 并发预算允许；
- 没有未决用户批准。

所有节点使用可传播的 `context.Context`、Deadline 和 Cancellation Token。
长任务按节点写入 Checkpoint。系统休眠或重启后，调度器根据 Event Log
重建未完成图，并重新确认不可安全重试的外部操作。

## 7. Context Engine

Context Engine 负责为每次模型调用编译最小、可信、可追踪的上下文，不是
简单截断最近消息。

### 7.1 上下文分层

1. Kernel Policy：固定且短的内核规则；
2. Run Contract：当前目标、约束、用户决定和完成条件；
3. Working Set：当前节点直接需要的状态；
4. Evidence：带来源的 Tool 输出和文件片段；
5. Capability Definition：当前已加载 Tool/Skill 的精确合约；
6. Episodic Memory：与当前目标相关的历史摘要；
7. Conversation Tail：必要的近期对话。

每段上下文都有来源、版本、时间、可信级别、Token 成本和失效条件。

### 7.2 Context Plan

Context Compiler 在调用模型前产生可观测的 `ContextPlan`：

```text
预算 = 模型窗口 - 输出预留 - Tool 预留 - 安全余量

硬保留：Policy + Run Contract + 当前 Node
按需保留：直接证据 + 已加载 Capability
竞争预算：历史、搜索结果、辅助说明
```

超出预算时优先做来源保留的结构化压缩，而不是直接丢弃最旧消息。压缩
结果必须保存 `derivedFrom`，需要细节时可重新展开原始 Event 或 Artifact。

### 7.3 Memory

记忆分为三类：

- Run Memory：当前任务状态，任务完成后归档；
- Workspace Memory：项目约定、架构和已验证事实；
- User Memory：用户明确允许长期保存的偏好。

模型不能直接写永久记忆，只能提出 `MemoryCandidate`。Host 去重、标记来源
并根据用户策略提交。过期事实需要重新验证。

## 8. Provider Runtime

Provider Adapter 将不同模型 API 映射为统一能力，而不是假设所有模型都
完整兼容 OpenAI Chat Completions。

```go
type ModelCapabilities struct {
    ToolCalling       bool
    StrictJSONSchema  bool
    Streaming         bool
    ParallelToolCalls bool
    ReasoningControl  bool
    PromptCaching     bool
    MaxInputTokens    int
    MaxOutputTokens   int
}
```

首批 Provider：

- OpenAI-compatible；
- OpenAI Responses；
- Anthropic Messages；
- DeepSeek API；
- 用户自定义兼容端点。

Provider Router 根据任务配置、模型能力、延迟、错误率、上下文大小和用户
预算选择模型。默认不在未授权情况下把同一任务内容发送给多个厂商。

Provider 层必须处理流式事件、限流退避、幂等请求、取消、用量统计、响应
格式修复和 API 错误归一化。原始 Provider 响应可选择性加密保存用于诊断，
不能进入普通日志。

## 9. Capability Hub：Plugin、MCP 与 Skill 的统一关系

### 9.1 统一能力模型

```text
Plugin Package
├── Agent Surfaces
│   ├── Tool
│   ├── Skill
│   ├── Context Provider
│   ├── Planner Strategy
│   └── Verifier
├── Runtime Surfaces
│   ├── Service
│   ├── Hook
│   ├── Job
│   └── Device Adapter
├── Integration Surfaces
│   └── MCP Connection / Server
└── Client Surfaces
    ├── UI Slot
    └── Result Renderer
```

Plugin 是安装、版本、权限和生命周期单位。Capability 是调用单位。Surface
是面向某类调用方的注册单位。

### 9.2 MCP

MCP 是外部工具和资源的兼容协议，不是 Agent Runtime 本身。一个 MCP
Plugin 声明连接方式、服务器版本、Tool/Resource/Prompt 映射和权限。

为避免上下文污染：

- 安装时只索引名称、摘要、标签、风险和来源；
- 不在每轮启动所有 MCP server；
- 搜索命中后才连接或唤醒目标 server；
- 只加载被选中 Tool 的完整 Schema；
- MCP Resource 先返回 Handle 和摘要，正文按需读取；
- server 的日志、健康和崩溃与普通 Plugin Sidecar 一样管理。

### 9.3 Skill

Skill 是惰性加载的策略或知识包，可以包含说明、模板和只读资源，但没有
独立系统权限。Skill 如需执行现实操作，必须调用已授权 Tool。Skill 摘要
进入 Capability Index，正文只在匹配当前节点后加载。

### 9.4 推理扩展

Plugin 可以通过受约束扩展点优化 Agent：

- `context.provider`：提供相关上下文片段；
- `strategy.proposer`：针对特定任务提出执行图节点；
- `verifier`：验证某类 Artifact 或 Tool 结果；
- `policy.guard`：收紧权限，不能放宽内核策略；
- `memory.extractor`：提出 Memory Candidate；
- `result.renderer`：改善结果展示。

推理扩展输出都是建议或证据，最终状态迁移仍由 Host 完成。普通 Plugin
不能替换 Scheduler、Approval Engine 或 Event Store。

## 10. Plugin 自举闭环

### 10.1 何时创建 Plugin

Agent 不能遇到一次性问题就创建 Plugin。Capability Gap Detector 只有在
满足以下条件时才建议创建：

- 现有 Tool、MCP、Skill 和组合工作流无法合理完成；
- 能力预计会重复使用，或必须长期运行；
- 需要新的外部系统、设备、UI、验证器或推理策略；
- 新能力可以定义稳定输入、输出和权限边界。

一次性代码优先作为 Run Artifact 执行；稳定后可由用户选择提升为 Plugin。

### 10.2 自举流水线

```text
Gap Detected
    │
    ▼
PluginSpec ──► Threat Model ──► Scaffold
    │                              │
    │                        isolated Git repo
    ▼                              ▼
Contract Tests ◄── Generate ◄── Coding Agent
    │
    ▼
Build + Static Policy + Unit Tests + Evals
    │
    ▼
Permission / Surface / Behavior Diff
    │
    ▼
User Approval Card
    │
    ▼
Stage ──► Probe ──► Canary ──► Atomic Activate
                         │
                         └── failure ──► Rollback
```

### 10.3 PluginSpec

生成代码前必须先有机器可验证的规范：

- 解决的能力缺口；
- Surface 和调用方；
- 输入输出 Schema；
- Effect 和幂等语义；
- 权限及理由；
- 数据生命周期；
- 依赖；
- 测试、评测和完成条件；
- UI Slot 和交互协议；
- 更新与迁移策略。

Agent 只能在 PluginSpec 范围内生成代码。新增权限、Surface 或依赖会使
原批准失效。

### 10.4 用户控制

Agent 可以自动完成提案、编码、测试、构建和安装准备，但不能自我批准
新的权限。用户可以配置 Standing Policy，例如允许某个工作区中的只读
Tool 自动升级；Policy 仍由 Host 执行，并绑定明确上限。

批准卡至少展示：

- Plugin 要解决什么问题；
- 新增哪些 Agent 和 Client 能力；
- 会访问哪些文件、网络、Secret、进程或设备；
- 是否有后台运行和自动更新；
- 测试与 Eval 结果；
- 与当前版本的权限和行为差异；
- 回滚版本。

## 11. Plugin 安装与热拔插

### 11.1 安装不修改 Host 源码

一个 Release 是不可变、内容寻址、可签名的 Bundle：

```text
release/
├── plugin.json
├── integrity.json
├── backend/
├── ui/
├── skills/
├── mcp/
├── migrations/
└── evals/
```

安装只会写入 Plugin Store、Installation State 和 Plugin Data，不会 patch
Axiom 二进制或前端源代码。

### 11.2 激活事务

1. 校验签名、Digest、兼容版本和用户 Grant；
2. 解析依赖并构造 Activation Plan；
3. 在 staging 启动 backend/MCP 并握手；
4. 注册候选 Service、UI、Tool、Skill、Context、Verifier 等 Surface；
5. 执行健康检查和最小 Canary；
6. 在单次 Registry Epoch 中原子切换；
7. 旧版本继续服务已 pin 的 Run；
8. 排空后停止旧进程；
9. 持久化 observed state 和审计 Event。

任一步失败都不发布部分注册表。新版本异常时自动恢复上一稳定 Epoch。

### 11.3 前后端 Plugin

同时包含前后端的 Plugin 不把代码动态编译进客户端。后端以 Sidecar 或
WASM 组件运行，前端以隔离 Bundle 运行，两者通过 release-bound Service
Contract 通信。UI 热拔插通过 Slot Registry 增删视图，不要求重启 Shell。

只有真正需要新的原生窗口能力或内核权限时，才属于 Axiom 核心升级，不
伪装成普通 Plugin 热更新。

## 12. 权限和安全模型

### 12.1 Principal

每次调用携带不可由 Plugin 自行修改的 Principal：

- `user`：用户直接操作；
- `agent`：某个 Run 中已加载的 Capability；
- `ui`：某个 Release 的 UI；
- `job`：某个后台任务；
- `plugin`：Sidecar 内部调用；
- `host`：仅内核生命周期操作。

Grant 绑定：

```text
user + plugin digest + surface digest + permission digest + workspace scope
```

### 12.2 Broker

Plugin 默认没有 Host 资源权限，必须使用 Broker：

- Filesystem Broker；
- Network Broker；
- Secret Broker；
- Process Broker；
- Native UI Broker；
- Notification Broker；
- Device Broker。

每个 Broker 统一实现 Scope、配额、超时、审计、取消和返回值限制。

### 12.3 隔离

Windows 生产版目标：

- Job Object 限制进程树、内存和关闭行为；
- AppContainer 或受限 Token；
- 独立临时目录和 Plugin Data；
- 网络默认拒绝；
- WebView 独立 profile、CSP 和消息 Schema；
- Secret 仅按名称临时解封，不写入环境变量和日志；
- 本地 API 使用 Named Pipe 或每次启动随机认证凭据；
- Release 签名、来源和构建证明可审计。

## 13. 状态、存储和事件

### 13.1 Event-sourced Run

Run 的事实以追加 Event 保存，当前视图是 Projection：

- `run.created`；
- `objective.compiled`；
- `node.proposed`；
- `node.committed`；
- `capability.loaded`；
- `tool.started/completed/failed`；
- `approval.requested/resolved`；
- `artifact.created/verified`；
- `checkpoint.created`；
- `run.completed/failed/cancelled`。

Event 包含单调 Sequence、时间、Actor、来源、Trace ID 和可选 Artifact
引用。大内容存入 Content-addressed Artifact Store，Event 只保存引用。

### 13.2 数据分层

- SQLite WAL：用户、Run、Event、Projection、Grant、Installation；
- CAS：Tool 大输出、文件快照、构建产物、上下文快照；
- Secret Vault：Provider Key 和 Plugin Secret；
- Plugin Store：不可变 Release；
- Workspace Metadata：锁、变更集和恢复点。

数据库迁移只做向前兼容的分阶段变更。关键写入使用事务和 Outbox，避免
状态已提交但 Event 丢失，或 Event 已发布但状态未提交。

## 14. 故障与恢复

统一错误分类：

- `invalid_request`：模型或 Plugin 违反合约；
- `permission_denied`：Grant 不满足；
- `transient`：限流、短暂网络、进程忙；
- `dependency_failed`：前置节点或 Service 失败；
- `conflict`：文件、版本或外部状态改变；
- `unknown_effect`：请求可能已执行但未收到回执；
- `terminal`：不可恢复错误。

自动重试只适用于幂等或有 Idempotency Key 的操作。`unknown_effect` 必须先
查询外部状态，不能盲目重放。连续失败触发 Circuit Breaker，并将 Run
转入 Recover 节点。Plugin 崩溃只摘除依赖它的 Surface，不污染其他
Plugin 和 Run。

## 15. 性能设计

关键手段：

- Fast Path 避免所有请求都进入规划；
- Capability Card 索引和延迟 Schema 加载；
- MCP/Sidecar 按需唤醒和空闲回收；
- Context Plan 精确预算和 Provider Prompt Cache；
- 无冲突节点并发执行；
- Tool 输出存 Artifact，模型只读取摘要或切片；
- Provider 流式传输与客户端增量事件；
- SQLite 单写者队列和只读 Projection；
- Plugin Registry 使用 Epoch 快照，Run 无需持有全局锁。

首阶段性能目标：

- 本地客户端冷启动 P95 小于 2.5 秒；
- 打开已有会话 P95 小于 200 毫秒；
- Host 本地控制 API P95 小于 50 毫秒；
- 不含模型延迟的 Tool 调度开销 P95 小于 20 毫秒；
- 安装 100 个 Plugin 时，空闲内存不随 Sidecar 数线性增长；
- 每轮默认只暴露发现工具和已加载 Capability。

## 16. 可观测性与 Eval

每个 Run 使用统一 Trace：

```text
Run → Node → Model Call / Tool Call / Plugin RPC / Broker Call
```

客户端可查看耗时、Token、费用、上下文组成、Capability 加载、权限判断、
重试和证据。默认日志脱敏，不记录 Secret 和未经用户允许的完整文件内容。

Eval 分为：

- Kernel Eval：状态机、恢复、权限和调度正确性；
- Provider Contract Eval：不同厂商模型协议一致性；
- Plugin Contract Eval：Schema、权限和生命周期；
- Agent Task Eval：完成率、误执行率、工具选择和上下文效率；
- Regression Replay：用保存的事件和模拟 Tool 重放历史 Run。

Agent 生成的 Plugin 必须携带最小 Eval。更新后若关键指标退化，不进入
稳定激活通道。

## 17. 最难的问题

### 17.1 模型差异远大于 API 格式差异

不同模型在 Tool Calling、长上下文、并行调用、结构化输出和错误恢复上
差异明显。只做 OpenAI-compatible URL 配置无法保证统一行为。需要能力
协商、Provider Contract Test 和按模型降级策略。

### 17.2 Agent 自己写的 Plugin 不能天然可信

代码能编译不代表行为正确。必须在生成前约束 Spec，生成后执行静态策略、
测试、Eval、权限 Diff、隔离运行和 Canary。用户批准的是不可变 Release，
不是一句“相信这个 Agent”。

### 17.3 任意前端扩展与稳定客户端之间存在冲突

允许 Plugin 修改主 React 树会让热更新、权限和版本兼容失控。必须坚持
Slot、隔离 WebView 和 typed bridge。客户端扩展能力取决于稳定 SDK，而
不是任意源码注入。

### 17.4 长期任务的真实状态不能只放在消息里

电脑休眠、进程退出或上下文压缩后，聊天记录不足以恢复执行。必须把计划、
节点、Effect、回执、Artifact 和 Checkpoint 建模为 Host 数据。

### 17.5 推理扩展可能破坏推理

Context Provider、Strategy 和 Memory Plugin 可能注入低质量或恶意内容。
必须限制其预算、标记来源、使用结构化输出，并允许 Policy 和 Eval 将其
隔离。推理 Plugin 不能拥有比普通 Tool 更模糊的权限。

### 17.6 自动重试可能重复现实副作用

发送邮件、下单、设备控制和支付等操作不能因网络超时自动重复。Tool
Contract 必须声明幂等性、查询回执方式和补偿动作，Host 需要保存
Idempotency Key。

### 17.7 本地并不等于安全

Plugin 仍可能读取用户文件、窃取 Key 或滥用登录态。进程隔离、最小 Grant、
Broker、UI 隔离和供应链验证都是必需项，不能依赖静态代码扫描单独兜底。

## 18. 当前实现与目标架构的差距

现有工程已经具备：

- Go Host、用户登录和 Provider 配置；
- 基础持久化对话和 Tool Loop；
- Plugin Manifest V2、五类插件形态和不可变 Release；
- 分离的 UI、Service、Tool、Skill Registry；
- 延迟 Capability Search/Load；
- Resource Broker、Windows Job Object、热替换、恢复和 Trace。

下一阶段主要差距：

| 领域 | 当前状态 | 目标状态 |
| --- | --- | --- |
| 客户端 | 浏览器单页 | 安装型桌面客户端、托盘、通知、长期任务 |
| Agent | 固定 12 步 Tool Loop | 可恢复的 `react.v1`、稳定 AgentLoop 接口、预算和取消 |
| 上下文 | 最近消息字符截断 | Context Plan、Artifact、来源和结构化压缩 |
| Provider | OpenAI-compatible | 多协议 Adapter、能力协商和路由 |
| MCP | 尚未成为一等 Surface | 延迟启动、发现、Resource 和 Tool Bridge |
| Skill | 可延迟加载 | 资源、版本、预算和适用性评测 |
| Plugin 自举 | 可生成参考模板 | Coding Agent、Spec、Eval、Diff、Canary |
| 推理 Plugin | 未定义 | Context、Strategy、Verifier、Policy Extension |
| 执行安全 | Broker + Job Object | AppContainer、幂等 Effect、Native Broker |
| 可观测性 | Turn Trace | Run/Node/Model/Tool/Plugin 全链路 Trace |

## 19. 推荐实施顺序

### Phase A：桌面产品壳与 Host IPC

- 建立 `desktop/`，实现 Wails/WebView2 客户端；
- 将现有 Web UI 迁入桌面 Shell；
- 拆分 `axiom-desktop` 和 `axiomd`；
- 实现单实例、托盘、通知、窗口恢复、自动启动和诊断页；
- 使用本地认证 IPC，禁止裸露无凭据控制接口。

验收：关闭窗口后长期 Run 继续；重新打开可恢复完整状态。

### Phase B：Event Store 与 Run Runtime

- 定义 Run、Step、Effect、Artifact、Evidence 和 Checkpoint；
- 建立追加 Event 与 Projection；
- 将现有 Turn Trace 迁移到 Run Event；
- 实现 `react.v1`、取消、暂停、恢复和步骤级重试；
- 提供稳定 AgentLoop 接口，但首阶段不实现图调度器。

验收：任意步骤杀死 Host，重启后从最后安全 Checkpoint 恢复。

### Phase C：Context Engine 与 Provider Runtime

- 实现 Token-aware ContextPlan；
- 建立 Artifact Store 和来源保留压缩；
- Provider 能力协商与 Contract Test；
- 增加 OpenAI Responses、Anthropic 和 DeepSeek Adapter；
- 实现流式事件、用量、限流和取消。

验收：同一个 Run 可在兼容能力范围内切换 Provider，状态和证据不丢失。

### Phase D：MCP、Skill 与 Capability Hub

- MCP 作为 Plugin Surface；
- 延迟进程启动、Tool Schema 和 Resource 加载；
- Skill 资源、版本和预算；
- Capability 搜索改为结构化索引；
- 加入 Context Provider 和 Verifier Extension。

验收：安装大量 MCP/Skill 后，初始模型上下文保持近似常量。

### Phase E：Agent Plugin Forge 2.0

- PluginSpec 和 Threat Model；
- 专用 Coding Agent 与隔离工作区；
- 合约测试、Eval、权限和行为 Diff；
- 对话内原生批准卡；
- Staging、Canary、自动回滚；
- 支持 MCP、推理和客户端 Surface 生成。

验收：用户只通过对话描述能力，即可得到可审查、可安装、可回滚的 Plugin。

### Phase F：执行与安全强化

- Effect/Idempotency/Compensation；
- AppContainer 或受限 Token；
- Native、Device、Notification Broker；
- Plugin 签名、来源和更新通道；
- 全链路审计与 Regression Replay。

验收：高风险动作无法被模型文本、Plugin UI 或崩溃重试绕过批准。

## 20. 首个工程里程碑建议

下一里程碑不应继续堆 Plugin 页面，而应完成“桌面客户端 + 可恢复 Run
内核”的最小纵向切片：

1. 新建桌面 Shell 并复用现有 UI；
2. 将对话请求改为流式 Run Event；
3. 建立 Run、Step 和追加事件模型；
4. 实现可替换的 `react.v1` AgentLoop；
5. 将 Tool 调用包装为可审计 Step，并保存结果证据；
6. 支持暂停、取消、Host 重启恢复；
7. 在客户端展示执行时间线和证据；
8. 保持现有 Plugin Runtime 和延迟 Capability 兼容。

完成这一里程碑后，Axiom 才从“能调用 Plugin 的聊天应用”进入“可靠运行
任务的本地 Agent 客户端”。之后再接 MCP、自举 Plugin 和更多推理扩展，
不会反复推翻底层状态模型。

## 21. 待确认但不阻塞首阶段的问题

- 桌面 Shell 最终采用 Wails 还是自维护 WebView2 Host；
- `axiomd` 默认随用户登录启动，还是只在客户端启动后驻留；
- 多 Provider 自动路由是否默认启用；
- Plugin 市场是否进入首个公开版本；
- WASM 是否作为 Sidecar 之外的第二种 backend runtime；
- 用户永久记忆默认关闭还是首次引导时选择；
- 高风险 Tool 是否支持组织级 Policy 和远程审批。
- TODO（核心稳定后）：用同一任务集比较 `react.v1`、Eino ReAct 和图式
  Planner；在有证据前，不把 ADG 或多 Agent 编排设为默认内核。

这些选择不改变核心不变量：Host 持有状态和权限、执行以证据提交、能力按需
进入上下文、扩展通过不可变 Plugin Release 安装。
