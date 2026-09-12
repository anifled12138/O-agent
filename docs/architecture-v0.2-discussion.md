# Axiom 系统架构 v0.2（讨论稿）

状态：Discussion Draft  
日期：2026-09-12  
范围：代码框架、Agent Runtime、Plugin Runtime、MCP/Skill、桌面客户端和工程约束  

> 本文用于在继续实现前冻结边界，不代表所有细节已经成为最终决策。
> 已接受的 ADR 继续有效；与 `system-design-v0.1.md` 冲突的内容，需要拆成新 ADR
> 逐项批准，而不是直接在代码里替换。

## 1. 先给结论

Axiom 不直接采用 DSH 或 Pi 的代码框架，而采用它们已经证明有效的两个设计：

- 从 DSH 借鉴：`Agent` 与具体 `AgentLoop` 分离、会话事件是模型上下文的事实来源、
  服务/事件/注册具有明确生命周期、作用域随 Agent 或 Plugin 一起释放。
- 从 Pi 借鉴：默认循环保持小而清楚、SDK 优先、扩展开发体验简单、工具调用和流式
  事件有稳定顺序。

但 Axiom 不照搬它们的扩展权限模型：

- DSH 的“所有部分都是同一插件树”适合高度可组合的 Harness，但我们的插件会由
  Agent 和普通用户生成，因此权限、身份、事件存储、资源 Broker、安装事务和审计
  不能让普通插件替换。
- Pi 扩展是拥有宿主完整权限的进程内 TypeScript，适合可信的个人扩展，不适合
  自动生成并长期安装的第三方全栈插件。

因此采用“可信小内核 + 可替换运行层 + 隔离能力插件”三层结构：

```text
┌──────────────────── 不可被普通插件替换的可信内核 ────────────────────┐
│ Identity / Policy / Approval / Event Store / Artifact Store         │
│ Plugin Transaction / Resource Broker / Process Supervisor / Audit   │
└──────────────────────────────┬───────────────────────────────────────┘
                               │ 受版本和权限约束的接口
┌──────────────────────────────▼───────────────────────────────────────┐
│ 可替换 Agent Runtime                                                │
│ Agent Handle / Inbox / Session Projection / Context Compiler        │
│ Reasoning Driver / Model Gateway / Capability Resolver / Dispatcher │
└──────────────────────────────┬───────────────────────────────────────┘
                               │ Surface leases
┌──────────────────────────────▼───────────────────────────────────────┐
│ Plugin 与外部能力                                                   │
│ Tool / Skill / MCP / Service / Hook / Job / UI / Reasoning Driver   │
└──────────────────────────────────────────────────────────────────────┘
```

核心原则不是“Host 限制模型怎么思考”，而是：模型和推理框架可以自由组织推理；
只有真实副作用、权限和事实提交必须经过 Host。这样不会把 LLM 降格为固定工作流，
同时也不会让一段模型文本获得操作系统权力。

## 2. 对 DSH 与 Pi 的判断

### 2.1 DSH 值得保留的部分

DSH 使用 Cordis 组织服务、类型事件和可撤销 effect。模型适配器、工具注册表、
会话日志和 Agent Loop 都能从配置组合。它最重要的设计不是“万物插件”这个口号，
而是以下四件具体事情：

1. `Agent` 是公共能力，`agent-loop` 只是其默认实现，扩展不依赖具体循环。
2. 模型可见内容必须能从 append-only Session Event 重建。
3. durable session event、live agent event、capability event 分域，不把所有事件混成
   一个总线。
4. 注册属于一个作用域，卸载时自动撤销；创建 Agent 的所有权和 disposer 明确。

这些机制直接解决替换循环、恢复会话、热卸载和依赖倒置，应该进入 Axiom。

### 2.2 DSH 不直接照搬的部分

DSH 允许从同一插件树替换几乎全部产品组成。Axiom 的威胁模型不同：插件可由
Agent 自举生成，不能让其替换自己的授权者、审计者或隔离边界。因此：

- Policy、Approval、Identity、Event Store、Artifact Store、Plugin Coordinator、
  Broker 和 Supervisor 属于可信内核；
- Agent Loop、Context Policy、Model Adapter、Verifier、Tool、UI 等可以替换；
- 普通能力插件不能通过 event hook 获得与系统插件相同的拦截权；
- 替换 Reasoning Driver 属于 Agent Generation 变更，要经过评测和显式启用。

### 2.3 Pi 值得保留的部分

Pi 的 Agent Core 是一个容易理解的工具循环：一次模型响应和对应工具执行构成一轮，
工具存在时继续调用模型；它支持流式事件、并行或串行工具执行、steering/follow-up、
上下文转换和 SDK 嵌入。它的优势是小、透明、扩展作者容易理解。

Axiom 默认 Driver 也应保持这种规模。复杂规划、多 Agent、Verifier 或 Workflow
应该由可替换 Driver/Plugin 组合，而不是把所有策略硬编码进唯一主循环。

### 2.4 Pi 不直接照搬的部分

Pi 的扩展可直接使用 Node 内置模块并拥有宿主进程权限，热重载主要是开发体验。
Axiom 的生产插件不能采用这一信任模型。Pi Package 的资源筛选和固定 Git/npm 版本
值得参考，但 Axiom Release 还必须增加：内容摘要、权限绑定、进程隔离、原子激活、
前后端同版本切换、调用租约和持久化恢复。

## 3. 产品进程拓扑

```text
 Axiom Desktop (Electron)
 ┌─────────────────────────────────────────────────────────────┐
 │ Main: window/tray/update/custom protocol/local socket proxy │
 │ Preload: very small typed bridge                            │
 │ Renderer: React product UI                                  │
 │ Plugin UI: sandboxed, release-bound iframe                  │
 └──────────────────────────┬──────────────────────────────────┘
                            │ per-user named pipe / Unix socket
                            │ HTTP/1.1 JSON + event stream
 ┌──────────────────────────▼──────────────────────────────────┐
 │ axiomd (Go)                                                 │
 │ trusted kernel + agent runtime + product application layer │
 └──────────────┬──────────────────────────────┬───────────────┘
                │ framed JSON-RPC              │ MCP protocol
        Plugin sidecars / WASM          local stdio / remote HTTP
```

最少两个长期进程：

- `axiom-desktop` 只负责完整客户端、系统集成和安全桥接；
- `axiomd` 负责用户、会话、Agent、插件和长期任务，窗口关闭后可继续运行。

插件 backend 和本地 MCP Server 不是微服务，而是由 Host 管理生命周期的子进程。
核心后端仍是一个模块化单体，避免在本机产品里引入分布式系统复杂度。

## 4. 技术选型及原因

| 层 | 选择 | 没选其他方案的原因 |
| --- | --- | --- |
| Host | Go | 并发、取消、子进程管理、单文件部署和跨平台都成熟；Agent 的主要延迟在模型/API/工具，不需要用 Rust 换取有限的 CPU 收益。 |
| Desktop | Electron | 对复杂桌面 UI、自定义协议、多窗口、自动更新、Renderer sandbox、`contextIsolation` 和崩溃隔离最成熟；全栈插件 UI 比安装包小几十 MB 更重要。 |
| Renderer | React + TypeScript + Vite | 本地客户端不需要 SSR；Vite 比当前 Next/vinext 链路更少层，插件 SDK 也能共享 TypeScript 类型。 |
| Desktop IPC | HTTP/1.1 over named pipe/Unix socket | 保留标准 HTTP 请求、取消和流式语义，不开放本机 TCP 端口；Electron Main 代理后，Renderer 无法直接接触 socket。 |
| Host API contract | OpenAPI 3.1 + JSON Schema | 用户/API/插件输入本来就是 JSON；可生成 TypeScript client，并作为兼容性测试输入。 |
| Plugin backend IPC | JSON-RPC 2.0 + `Content-Length` framing over pipes | 跨 Go/TS/Python、可调试、无端口；比逐行 JSON 更能处理大内容，比为本地 sidecar 引入 HTTP/2/gRPC 更简单。 |
| Storage | SQLite WAL + `database/sql` | 本地事务、崩溃恢复和备份成熟；单用户/少量并发无需外部数据库。 |
| Query layer | 手写迁移 + sqlc 生成查询 | 不引入会隐藏事务和查询行为的 ORM，同时减少手写扫描字段错误。 |
| Artifact store | 内容寻址文件仓库 | Plugin bundle、工具结果和证据可去重、校验且不可静默覆盖。 |
| MCP | 官方 `modelcontextprotocol/go-sdk` | MCP 不需要自行创新协议；官方 SDK 已覆盖当前 2026-07-28 规范及旧版本兼容。 |
| Secret | OS credential vault；Windows 首先使用 DPAPI/Credential Manager | API Key 不进入普通 SQLite 字段、日志、Plugin 环境变量或前端状态。 |
| JS workspace | npm workspaces + lockfile + exact versions | 当前仓库已使用 npm；先减少工具链变化。生产安装禁用未审核 lifecycle scripts。 |

### 4.1 为什么不优先 Wails

Wails 与 Go 很搭，安装包也小；但我们的 `axiomd` 必须独立于窗口长期运行，因此
“Go 直接绑定到 WebView”并不能减少核心边界。Wails v3 当前仍是 Beta，v2 到 v3
又存在迁移成本；不同操作系统 WebView 的行为差异也会增加 Plugin UI 的兼容矩阵。
它适合普通 Go 桌面应用，不是本项目当前最优的插件 UI 容器。

### 4.2 为什么不优先 Tauri

Tauri 体积小且权限系统优秀，但会增加 Rust 主壳和相应构建链。后端已经由 Go
承担，继续增加一个特权语言层并不能改善 Agent 或 Plugin 的核心能力。若未来
Electron 的资源占用成为经过测量的主要问题，桌面壳可以替换，Host API 不变。

### 4.3 Electron 的边界

选择 Electron 不意味着 Plugin 获得 Node：

- Product Renderer 和所有 Plugin Renderer 均关闭 Node integration；
- 全局启用 Chromium sandbox 和 context isolation；
- Preload 只暴露窄的、逐方法校验的 bridge，不暴露通用 IPC；
- Plugin UI 使用独立 origin、严格 CSP、默认拒绝浏览器权限；
- Agent/Plugin 的文件、网络、进程权限仍由 Go Host Broker 决定。

## 5. Go 代码框架

建议保留单仓库，但把“内核、运行时、产品功能、适配器”分开：

```text
D:\agent-harness\
├─ backend/
│  ├─ cmd/
│  │  ├─ axiomd/                 # daemon composition root
│  │  └─ axiomctl/               # diagnosis/admin CLI
│  ├─ internal/
│  │  ├─ kernel/
│  │  │  ├─ identity/ policy/ approval/
│  │  │  ├─ eventstore/ artifact/
│  │  │  ├─ broker/ supervisor/
│  │  │  └─ lifecycle/
│  │  ├─ agent/
│  │  │  ├─ session/ inbox/ runtime/
│  │  │  ├─ driver/ context/ model/
│  │  │  └─ capability/ dispatch/
│  │  ├─ plugin/
│  │  │  ├─ manifest/ package/ install/
│  │  │  ├─ activation/ registry/ lease/
│  │  │  ├─ sidecar/ wasm/ ui/
│  │  │  └─ forge/ conformance/
│  │  ├─ integration/
│  │  │  ├─ mcp/ skill/ provider/
│  │  │  └─ filesystem/ process/
│  │  ├─ product/
│  │  │  ├─ conversation/ settings/ eval/
│  │  │  └─ plugincenter/
│  │  └─ transport/
│  │     ├─ localapi/ stream/ openapi/
│  │     └─ pluginrpc/
│  └─ migrations/
├─ desktop/
│  ├─ main/ preload/ renderer/
│  └─ package.json
├─ sdk/
│  ├─ plugin-go/                 # 独立 Go module，不导入 internal
│  └─ plugin-ts/
├─ plugins/builtin/              # 通过正常 Surface 注册的内置能力
├─ api/openapi.yaml
└─ docs/adr/
```

依赖只能朝内：

```text
transport/adapters -> product composition -> agent/plugin use-cases
                                      -> kernel interfaces
```

接口放在使用方所在包，不创建一个装满所有接口的 `common` 或 `core` 包。`cmd/axiomd`
是唯一生产 composition root。跨 Plugin SDK 的契约放在 `sdk/`，不能导出 `internal/`
类型。

## 6. Agent Runtime

### 6.1 公共 Agent 与可替换 Driver

`Agent` 是稳定的宿主能力，`ReasoningDriver` 是可替换策略：

```go
type Agent interface {
    ID() AgentID
    Session() SessionView
    Send(ctx context.Context, input InputEnvelope) (Receipt, error)
    Cancel(cause CancelCause)
    WhenIdle(ctx context.Context) error
}

type ReasoningDriver interface {
    Run(ctx context.Context, turn TurnHandle) error
}
```

Driver 不直接拿数据库、Secret 或文件系统。`TurnHandle` 提供：

- 编译模型请求并调用 `ModelGateway`；
- 搜索/加载 Capability；
- 请求执行 Tool；
- 追加经过校验的 Session Event；
- 请求用户输入或批准；
- 创建子 Agent/分支（如果当前 Generation 允许）。

这样 Driver 可以实现直接工具循环、Plan + Execute、多 Agent、Tree Search、Verifier
Loop 或特定 Workflow；Host 仍掌握真实副作用和记录。

### 6.2 默认 Driver：Direct Tool Loop v1

首个生产 Driver 不强迫模型生成自定义计划 JSON，也不在文本推理外再套一层固定
状态图。它直接使用模型厂商的原生流式输出和 Tool Calling：

```text
claim inbox -> compile context -> call model
                                 ├─ final text -> close turn
                                 └─ tool calls -> guarded dispatch
                                                   -> append results
                                                   -> next step
```

定义：

- Turn：从一条会唤醒 Agent 的输入开始，直到没有后续工作；
- Step：一次模型请求，加上该响应产生的工具调用；
- Attempt：一次可能成功、失败、取消或重试的 Provider 请求；
- Run：产品层对一个目标生命周期的聚合，不等同于一次 LLM 调用。

工具默认可并发，但有写冲突、声明为 sequential、共享同一外部事务或要求用户输入
时串行。最终写入 Session 的 Tool Result 顺序保持模型原调用顺序，保证重放稳定。

### 6.3 会话事实与实时事件分离

持久事件是唯一事实来源：

```text
turn.started
input.accepted
context.projected
model.requested
assistant.committed | assistant.attempt_failed
tool.requested
tool.completed
approval.requested | approval.resolved
turn.completed | turn.interrupted
```

Token delta、进度百分比和临时 UI 状态属于 live event，不逐 token 写 SQLite。完整
Assistant 响应提交后写一个 durable event。任何进入模型请求的内容都要能由事件和
Artifact 引用重建；否则不允许发送。这一点直接决定恢复、分支、评测和调试质量。

### 6.4 Context 不是会话真相

`ContextCompiler` 只是事件到某次模型请求的投影器：

```text
Session Events + Artifacts + Agent Generation + loaded capabilities
                              -> ContextPlan -> provider messages
```

默认策略只做可靠的预算、裁剪、压缩和来源保留。未来可以替换 Context Policy，
但不能改写原始事件。Skill 正文、MCP Schema、Plugin 代码和全部历史不会默认进入
上下文。

### 6.5 Agent Generation

一个运行中的 Agent 固定到不可变 Generation：

- Reasoning Driver release；
- Context Policy release；
- System prompt sections；
- 默认 Capability policy；
- Provider route policy；
- Budget、并发和停止策略。

普通 Tool/UI 插件可按租约热更新；Reasoning Driver/Context Policy 不在一个进行中的
Turn 中间切换。升级先生成候选 Generation，经过回放、对照评测、Canary 和用户确认，
再成为新会话默认。旧会话可继续固定旧 Generation 或显式迁移。

## 7. Capability 模型

Plugin 是安装和生命周期单位；Surface 是给某类调用方的导出；Capability 是实际
可搜索或调用的能力。三者不能混为一个“插件列表”。

| Surface | 调用者 | 默认是否模型可见 |
| --- | --- | --- |
| Tool | Agent | 可发现，按需加载完整 Schema |
| Skill | Agent | 只索引摘要，正文惰性加载 |
| Context Provider | Context Compiler | 否，只有选中结果可进入模型 |
| Service | Host/UI/其他 Plugin | 否 |
| Hook | 指定事件域 | 否 |
| Job | Scheduler | 否 |
| UI | 用户 | 否 |
| MCP Connection | MCP Gateway | 否；其 Tool/Resource 分别投影 |
| Reasoning Driver | Agent Runtime | 否，属于 Generation 配置 |
| Model Adapter | Model Gateway | 否 |
| Verifier | Harness/Driver | 仅结果可能成为证据 |

Host 感知全部安装和健康状态；Agent 只感知自己有资格使用的 Agent Surface；模型只
看到本步已加载的精确定义。这同时避免上下文污染和“纯前端插件为什么要让 Agent
知道”的错误。

## 8. Plugin 框架

### 8.1 Package 与 Release

源码不是安装物。流水线为：

```text
PluginSpec -> isolated Git workspace -> build/test -> package
           -> digest/sign -> immutable Release Store
           -> permission review -> installation desired state
           -> prepare -> atomic activate -> observe/reconcile
```

Release 是不可修改的目录或 `.axp` 包，摘要覆盖规范化 Manifest 和全部声明 Artifact。
开发中的 Plugin Project 使用独立 Git 仓库；不能嵌套写入 Axiom 主仓库。Agent 可以
修改 Project，但没有写 Release Store、Grant Store 或活动路由表的权限。

### 8.2 Manifest

现有 `axiom.plugin/v2` 的 Surface 思路保留，并补充以下字段：

- `hostApi`：兼容的 Host API 范围；
- `components`：sidecar、WASM、UI、static resources；
- `exports`：Tool/Skill/Service/Hook/Job/Driver 等；
- `imports`：按 contract 声明的 Service 依赖；
- `permissions`：资源和调用权限，不只 OS 权限；
- `state`：数据 schema、迁移和 rollback barrier；
- `activation`：ready probe、drain deadline、是否允许热切换；
- `conformance`：必须通过的契约测试集版本。

Manifest 声明是请求，不是授权。Grant 绑定：

```text
user + plugin id + release digest + surfaces digest + permission digest
```

Release 新增 Tool、UI Bridge method、Hook、Secret、域名或目录时，旧 Grant 自动失效。

### 8.3 三种执行形态

| 形态 | 适用场景 | 隔离与热拔插 |
| --- | --- | --- |
| Static | Skill、模板、Schema、静态 UI 资源 | 无执行权限，按 Release 卸载 |
| WASM | 纯计算、转换、Verifier、轻量 Tool | 默认无系统能力，只能调用显式 Host import；实例可快速替换 |
| Sidecar | 设备 SDK、长连接、原生库、复杂后台服务 | 独立进程 + OS containment + Broker；停止新租约后排空进程 |

首个实现应先完成 Sidecar，因为它覆盖面最广；Manifest 同时预留 WASM component，
等 sidecar 契约稳定后使用 wazero 接入。第三方原生库永不动态加载进 `axiomd`。

### 8.4 Sidecar 协议

当前逐行 JSON RPC 只允许单飞调用，并且取消时直接杀进程。V3 协议需要：

- `Content-Length` framed JSON-RPC 2.0；
- 并发 request id；
- `$/cancelRequest`；
- progress/event channel；
- `plugin.describe` 返回实际导出并与 Manifest 对照；
- `plugin.ready`、`plugin.drain`、`plugin.shutdown`；
- Host Broker 反向调用使用独立 ID 空间；
- frame 大小、并发数、时间和内存上限；
- 协议握手包含 Host API、Plugin SDK 和 Release digest。

性能上不使用二进制 RPC，因为模型与外部 I/O 才是主要延迟；跨语言可生成、可诊断
和协议兼容性更重要。

### 8.5 权限不是字符串提示

Plugin 子进程不直接继承 Host 的 API Key、完整环境变量或工作目录。默认环境只有
Plugin/Release identity 和 IPC handle。所有访问走资源 Broker：

- `fs.read/write/list/watch` 使用 capability handle，而不是任意路径；
- `net.fetch/connect` 检查规范化域名、端口、协议和重定向；
- `secret.use` 优先让 Host 代为签名/发请求，避免返回明文；
- `process.spawn` 只执行 Grant 中的 toolchain/profile；
- `ui.call`、`agent.invoke`、`job.run` 使用不同 Principal；
- Windows 用 Job Object 管理进程树，后续增加受限 Token/AppContainer。

### 8.6 全栈插件 UI

插件不能 patch Product Renderer。它声明 Slot，例如：

- `conversation.message.renderer`；
- `tool.result.renderer`；
- `workspace.panel`；
- `settings.section`；
- `run.inspector.tab`；
- `composer.action`。

每个 UI instance 绑定 `user/plugin/release/epoch/slot/nonce`。Host 将专用
`MessagePort` 交给完成 nonce handshake 的 iframe；iframe 只能调用 Manifest 和
Grant 允许的 service method。UI 没有资格自己提交 plugin id、release id 或 principal。

更新时使用双缓冲：后台预载 B 的 iframe 和 backend，全部 ready 后在一个 registry
epoch 提交；新 UI 与新 backend 同时可见，旧 A 的 pending RPC 继续固定 A，完成后
排空。详细语义沿用 ADR 0004。

### 8.7 原子激活与热拔插

“热拔插”不是覆盖目录或重启进程，而是不可变 Release 间的事务切换：

```text
A active -------------------------- draining -> retired
             B prepare -> ready -> active ---------------->
                                  ^ one commit point
```

所有路由通过不可变 `MountedSnapshot` 读取；调用先获得包含 release/epoch/surface 的
Lease。激活事务持久记录 `requested/preparing/ready/committing/active/draining`，Host
崩溃后根据日志收敛。卸载先停止新 Lease，再排空旧 Lease；“停用”与“物理删除”分离。

不同 Surface 的切换边界不同：

- Tool：固定到当前 Step/Turn 的 release；
- Skill：已进入模型上下文的来源保持旧 release；
- Service：固定到一次调用；
- UI：固定到 iframe 生命周期；
- Hook：固定到事件被接纳时；
- Job：持久记录 claim 时的 release；
- Driver：固定到 Agent Generation，不做 Turn 中间热切换。

### 8.8 依赖与数据迁移

Plugin 不直接 import 另一 Plugin 的源代码。依赖面向版本化 Service Contract；激活
生成完整 release lock graph。旧 Release 排空期间可与新 Release 并存。

Plugin 数据属于 Host 管理的独立 keyspace，不属于某个 UI 或进程。迁移遵循
expand -> migrate -> switch -> contract。不可逆迁移不能标记为 hot-swappable；它要
进入维护模式、建立快照并明确越过 rollback barrier。

### 8.9 Agent 自举插件

Agent 可以完成：发现能力缺口、写 PluginSpec、创建 Git Project、生成代码、运行
测试、构建 Release、生成权限/Surface diff、申请安装和观察 Canary。

Agent 不能完成：批准自己的新增权限、伪造测试通过、修改 Release、绕过签名/摘要、
直接替换稳定 Generation。用户提供需求和最终权限决定，而不是手写能力实现。

## 9. MCP 的位置

MCP 是外部能力协议，不是 Axiom Plugin ABI，也不是 Agent Loop。实现方式：

1. `MCP Gateway` 使用官方 Go SDK；
2. 每个连接是独立 Host client，保留服务器身份和来源；
3. 支持当前 2026-07-28 stdio/Streamable HTTP，并通过 SDK 兼容旧版本；
4. 远程认证 Secret 由 Host 管理，MCP Server 看不到其他连接或完整会话；
5. Tool、Resource、Prompt 映射成内部不同 Surface，不强制都变成 Tool；
6. Tool ID 使用 `connection identity + server tool name` 消除重名；
7. `tools/list` 的 TTL/cache scope 和 list-changed 用于维护索引；
8. `InputRequiredResult` 映射为原生用户输入/批准卡，再重试原调用；
9. 模型先看到 Capability Card，命中后才加载完整 MCP Tool Schema；
10. 本地 stdio Server 由 Supervisor 管理并走独立 Grant，不继承 Host Secret。

一个 Plugin 可以声明并配置 MCP Connection，也可以自带 MCP Server，但安装单位仍是
Plugin Release，协议交互仍严格遵循 MCP。这样复用生态，同时不让 MCP 决定内部权限
和生命周期模型。

## 10. Skill 的位置

兼容常见 `SKILL.md` 目录结构，但内部作为 `skill` Surface 管理：

- 安装时只解析名称、摘要、标签、版本和资源索引；
- 匹配任务后才读取完整正文；
- Skill 资源按需读取并保留来源；
- Skill 自身不授予文件、网络、进程或 Secret 权限；
- 需要执行时只能调用当前 Agent 已获授权的 Tool；
- Skill 文本是非可信指令输入，不能覆盖 Kernel Policy 或 Grant。

Skill 可以单独打包，也可以和 Tool/UI 放在同一 Release 中；它与代码组件遵循同一
版本和安装流程，但具有更低的执行权限。

## 11. 用户、登录和 Secret

本地登录不是装饰性页面。V1 使用本机用户库：

- Password 使用 Argon2id；
- Session 使用短期 access token + 可撤销本地 refresh record；
- Desktop 登录凭证存 OS credential vault；
- SQLite 中所有资源带 `user_id`，仓库查询默认要求 user scope；
- Plugin Grant、MCP connection、Provider credential 和 Agent Generation 都属于用户；
- 后续加入云账户或同步时实现新的 Identity Provider，不改变领域 ID。

开发环境可以提供显式的单用户 dev bootstrap，但不能让生产 API 在“未配置时自动
成为管理员”。

## 12. 当前原型的处理

### 保留

- Go Host、SQLite、Provider adapter 的方向；
- `axiom.plugin/v2` 的显式 Surface；
- Capability summary search -> exact load；
- immutable release、Grant、sidecar、Broker、Turn lease；
- ADR 0004 的全栈原子热切换语义；
- Agent Generation 和 Eval Harness 的数据模型方向。

### 重构

- `agent.Service` 不再直接依赖 `pluginforge.Service`；只依赖 Capability Resolver；
- `executeLoop` 变为一个 `ReasoningDriver` 实现，不读取具体 Evolution 类型；
- `Conversation messages` 迁移为 append-only Session Event + projection；
- `core.Plugin Manager` 与外部 `pluginruntime.Supervisor` 合并为一套 lifecycle/registry
  抽象，内置能力和第三方能力共享 Surface contract，但信任级别不同；
- sidecar 从单飞逐行 RPC 升级为可取消、可并发的 framed protocol；
- frontend 从 Web demo 迁入 Electron renderer，去掉不需要的 SSR 框架层；
- HTTP API 按 identity/product/plugin/agent 分域并有统一错误 envelope，避免再次出现
  HTML 错误页被当作 JSON 解析的问题。

### 不立即做

- 不先实现新的复杂推理算法；
- 不在 sidecar 稳定前引入 WASM；
- 不先做 Plugin Marketplace；
- 不先做分布式多机 Agent；
- 不在没有契约测试前让 Agent 自动激活 Driver 更新。

## 13. 工程和版本控制

- 主仓库保持 monorepo；Agent 生成的 Plugin Project 是 D 盘独立 Git repo；
- 每个架构阶段一个 ADR、一个可验证提交，不混入无关格式化；
- 依赖使用 exact version，lockfile 必须提交；依赖更新单独 PR/commit；
- schema migration 只前向追加，删除/收缩进入后续明确维护版本；
- OpenAPI、Plugin Schema、Event Schema 和 SDK 生成物必须在 CI 检查无漂移；
- `go test ./...`、`go vet ./...`、race test、前端 typecheck/lint/build 是基础门禁；
- Plugin conformance suite 覆盖握手、取消、Broker 越权、崩溃、超时和升级；
- scripted fake model 用于确定性测试 Agent Loop，不依赖真实 API；
- 热切换使用故障注入覆盖 prepare/commit/drain 每个崩溃点；
- 所有开发源码、构建缓存和 Agent 生成工程默认放在 D 盘；不在 C 盘创建工程副本。

## 14. 建议实施顺序

每一步都应可独立运行和回退：

1. **冻结核心契约**：Event vocabulary、Agent/Driver、Capability、Surface Lease、
   Plugin RPC、Host API；先写 ADR 和 contract tests。
2. **会话与 Agent Runtime**：建立 append-only Session Event、projection、Inbox、
   Direct Tool Loop、取消/恢复；暂时复用现有 Provider。
3. **统一 Plugin Runtime**：合并两套插件抽象，完成 snapshot/lease、framed RPC、
   activation journal 和 sidecar containment。
4. **MCP/Skill adapter**：接官方 MCP Go SDK，实现 lazy capability projection；完成
   Skill metadata/body 分离。
5. **Desktop shell**：Electron Main/Preload/Renderer，通过 local socket 接 Host；
   迁移现有 UI，不修改 Agent 语义。
6. **全栈 Plugin UI**：Slot、custom protocol、MessagePort、双缓冲和 release-bound RPC。
7. **自举闭环**：PluginSpec -> Git -> build -> conformance -> grant -> canary -> activate。
8. **Generation Harness**：Driver/Context 更新的 replay、A/B、Canary、反馈和推广门禁。

第一阶段完成前不继续扩展产品功能。否则当前原型里的耦合会被更多代码固化。

## 15. 下一批 ADR

建议按顺序评审：

1. ADR 0005：Trusted Kernel 与可替换 Agent Runtime 边界；
2. ADR 0006：Session Event、Live Event 和 Projection；
3. ADR 0007：Agent/ReasoningDriver/Turn/Step/Attempt 契约；
4. ADR 0008：Plugin RPC V3 与 sidecar containment；
5. ADR 0009：Desktop Electron 与本地 socket API；
6. ADR 0010：MCP Gateway 和 Skill adapter；
7. ADR 0011：Plugin data migration 与 rollback barrier。

## 参考资料

- [DeepSeek Harness architecture](https://github.com/deepseek-ai/deepseek-harness/blob/master/docs/architecture.md)
- [DeepSeek Harness core subsystem](https://github.com/deepseek-ai/deepseek-harness/blob/master/docs/subsystems/core.md)
- [Pi Agent Core](https://github.com/earendil-works/pi/blob/main/packages/agent/README.md)
- [Pi extensions](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/extensions.md)
- [Pi packages](https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/packages.md)
- [MCP 2026-07-28 architecture](https://modelcontextprotocol.io/specification/2026-07-28/architecture)
- [Official MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk)
- [Electron process model](https://www.electronjs.org/docs/latest/tutorial/process-model)
- [Electron security checklist](https://www.electronjs.org/docs/latest/tutorial/security)
- [Wails v3 status](https://v3.wails.io/status/)
