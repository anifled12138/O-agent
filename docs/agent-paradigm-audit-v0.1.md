# Axiom Agent 范式审计 v0.1

状态：Research Draft，不代表最终产品方向  
日期：2026-09-07

## 1. 审计目的

本文件不从 Axiom 已有实现或 DeepSeek Harness 的功能缺口出发。它先回答三个
更基础的问题：

1. Codex、Claude Code、Manus、OpenClaw 和 DeepSeek Harness 的共同运行假设
   是什么；
2. 哪些看似新的方向已经被这些产品或现有研究覆盖；
3. 在只使用第三方 LLM API、本地 Go Runtime、有限工程资源的条件下，哪些
   架构变化仍值得制作纵向原型。

通过标准不是“DSH 当前有没有这个功能”，而是：

- 缺陷是否在插件数量、模型能力和上下文长度增加后仍然存在；
- 解决后是否扩大用户可以交给 Agent 的任务集合；
- 是否改变 Agent 的主要工作单位或用户与 AI 的关系；
- 能否先实现一个有独立价值的有限闭环；
- 是否依赖尚不存在的可靠评价器、基础模型训练或任意代码正确性保证。

## 2. 五类系统的共同最小模型

把产品 UI、插件格式和部署方式拿掉以后，它们都可以还原为：

```text
User Goal
   ↓
LLM Controller(current context, available actions)
   ↓
text | tool calls | delegation
   ↓
Runtime executes actions
   ↓
observations are converted back to model input
   └───────────────────────────────┐
                                   ↓
                         repeat until model stops
```

差异主要发生在循环周围：

| 系统 | 主要执行环境 | 扩展机制 | 并行/多 Agent | 主要持久对象 |
| --- | --- | --- | --- | --- |
| Codex | 本地/云端工作区、终端、应用工具 | Skill、MCP、Plugin、工具 | 子任务、工作树、线程 | 对话、任务、文件和 Git 状态 |
| Claude Code | 本地/云端 Shell 与项目 | Skill、MCP、Hook、Plugin | Subagent、Agent Team | Session、项目文件、任务列表 |
| Manus | 每任务云 VM、本地电脑连接 | Connector、Skill、工具 | Wide Research 全功能子 Agent | Task、Sandbox、Artifact |
| OpenClaw | 常驻 Gateway + Agent Runtime | Tool、Skill、Plugin、Channel | Session/Subagent | Session、Workspace、消息路由 |
| DSH | Cordis Host、Worker、浏览器客户端 | 一切均可 Plugin | Subagent、Workflow | Append-only Session Event |

Claude Code 官方文档对共同循环给出了最直接的定义：模型接收 Prompt、工具定义
和历史，产生工具调用，Runtime 执行，再把结果交回模型，直到模型输出不含工具
调用的最终回复。DSH 的默认 `ReactLoopAgent`、OpenClaw 的 Agent Runtime 和
Manus 的 Sandbox Agent 仍属于同一结构。

这意味着 Tool、MCP、Skill 和 Plugin 通常扩大的是循环的输入、动作空间或
执行环境，并不自动改变“LLM 是逐步中央控制器”这一事实。多 Agent 通常复制
多个相同循环，再由主 Agent、共享任务表或 Workflow 聚合。

## 3. 已被覆盖的方向

### 3.1 万物插件化

DSH 已经允许模型、工具、Skill、Session、Sandbox、Storage、Loop、Schedule
和 UI 成为插件，并允许 Creator Mode 在内存中检查、组合和试验 Cordis
Plugin。Axiom 不应再把“Loop 也能替换”或“Plugin 可以同时包含前后端”描述为
差异。

### 3.2 动态 Harness 生成与架构搜索

现有 Axiom PRD 把完整 Agent Definition 的生成、A/B、选择和代际晋升作为
核心。这个方向已经有明确研究先例：

- ADAS / Meta Agent Search 让 Meta Agent 用代码发明和组合 Agent 系统；
- JIT-Agent 使用专门训练的 Harness Intelligence Model，为每个任务即时生成、
  修复和演化 Memory、Planning、Action 与 Capability Orchestration 模块；
- 相关 Test-Time Harness Evolution 工作把可执行控制程序作为搜索对象。

因此“Agent 自动设计 Agent”不是空白。更重要的是，JIT-Agent 的效果依赖专门
训练的 27B Harness 模型和训练数据，而 Axiom 当前约束是不训练本地或自有基础
模型。仅用通用 API 模型加 Prompt 重做同一方向，在成本和效果上都没有合理的
领先假设。

结论：保留 Agent Definition、实验和版本能力作为 Harness 基础设施；暂停把
“递归 Agent 架构搜索”作为已经成立的产品核心。

### 3.3 多 Agent、并行搜索和独立上下文

Manus Wide Research 用多个完整 Manus 实例处理可并行子任务；Claude Code
Agent Teams 提供独立上下文、共享任务和点对点通信；Codex 和 DSH 也具备子任务
或 Workflow。增加角色、投票、辩论或并发数量不能单独形成新方向。

### 3.4 持久任务、记忆与上下文管理

持久化执行、断点恢复、显式状态、上下文压缩和长期记忆都有成熟产品或研究实现。
它们是生产 Agent 必需的底座，但除非引出新的用户工作单位，否则仍然属于 Runtime
或 Context/Harness Engineering。

### 3.5 自动生成任意 Plugin

DSH 已具备运行模型生成代码、动态加载 Cordis Package 和 Creator Mode 的基础，
但明确把第三方插件与模型代码视为可信宿主风险。把任意前端、后端、迁移、依赖、
凭据、热升级和回退同时纳入自动安装，需要解决开放软件工程问题，而不仅是 Agent
问题。

结论：不能把“一句话可靠生成并安装任意 Plugin”作为第一个可交付差异。它只能是
建立在受限产物、隔离运行和渐进发布之上的长期能力。

## 4. 共同结构性假设

目前可确认的共同假设有五项：

### H1：一次任务的默认产物是结果

系统完成回答、代码、文件、网页或外部操作后结束。任务过程中形成的求解结构即使
有效，通常不会自动成为一个有明确接口、生命周期和复用范围的运行能力。

### H2：LLM 默认参与每个不确定步骤

即使大量步骤已经程序化，Agent 的常见退路仍是让通用模型读取 Observation 并决定
下一步。Code Mode 和 Workflow 可以批处理动作，但通常是显式调用的执行模式，而
不是任务完成后形成的长期能力边界。

### H3：能力由开发者生产，Agent 负责消费

Plugin、Skill、MCP 和 Hook 的格式都在快速成熟，但正常产品路径仍然是人或开发者
创建、测试、发布，Agent 在会话中选择和使用。Creator Mode 证明 Agent 可以参与
开发，却没有把“用户需求到可复用能力”变成普通用户默认获得的产品结果。

### H4：能力复用以代码包或文本说明为中心

市场分发 Plugin，Skill 分发说明，Workflow 分发控制代码。它们很少同时携带：适用
条件、可观察效果、已验证任务、失败边界、成本分布和需要保留的 LLM 决策点。系统
因此知道“它存在”，但不真正知道“什么时候值得信任它”。

### H5：一次 Agent 运行承担发现、决策和执行

现有 Agent 通常在同一条认知轨迹中理解目标、探索环境、决定实现并执行。失败后会
重试、反思、分支或换 Agent，但“这次任务是否暴露了一个应当被永久软件化的重复
结构”不是默认运行分支。

H1-H5 指向的不是 Context 大小，而是 Agent 的产出模型：当前产品主要生产结果，
而不是生产可继续工作的能力。

## 5. 当前唯一保留的产品级候选：任务到能力的闭环

本轮审计暂时只保留一个候选，后续仍需证伪：

> 用户提出一次真实需求后，Agent 不仅完成本次任务，还能把其中已经证明稳定、
> 预计会复用的部分变成一个可独立运行的能力；下次同类需求直接由该能力承担，
> 只有新的语义判断和未知情况回到 LLM。

这不是要求每次任务都生成 Plugin，也不是让 Agent 自主学习。触发来源仍然是用户
需求；系统只在出现重复价值、确定边界和成功证据时提出能力化。

其工作单位不是一个 Tool，也不是完整对话轨迹，而是 `Capability Capsule`：

```text
Capability Capsule
├── intent contract      它接受哪一类用户目标
├── preconditions        运行前必须成立的事实
├── input/output         稳定的数据接口
├── deterministic body   已可程序化的步骤
├── LLM slots            必须保留开放推理的位置
├── effects              可能发生的外部变化
├── evidence             哪次真实任务和哪些测试支持它
├── failure boundary     何时必须退出并交还给通用 Agent
└── presentation         可选 UI，而非必需组成
```

关键变化在于，LLM 不再永久承担已经稳定的软件步骤，也不会被强制退出开放推理：
能力遇到声明外情况时，重新把控制权交给通用 Agent。Plugin 是 Capsule 需要后台服务、
UI、连接器或长期运行时的一种编译目标；简单 Capsule 可以只是版本化脚本或 Workflow。

### 与 DSH 的区别假设

DSH 提供“所有部件都可以插件化”和“Agent 可以在 Creator Mode 中制作 Runtime”的
机制。这里研究的是另一条默认产品链：

```text
DSH:  开发/组合 Plugin → 配置 Runtime → Agent 使用 → 得到任务结果
Axiom: 用户需求 → Agent 完成任务 → 提取可复用能力 → 下次直接调用/继续扩展
```

DSH 理论上可以通过插件实现这条链。任何图灵完备插件系统都能实现普通软件功能，
所以“DSH 永远做不了”不是合理标准。真正需要验证的是：把 Capability Capsule 作为
核心持久对象和默认任务分支，是否能比以 Session/Plugin 为核心的产品显著降低重复
推理，并扩大用户无需开发即可获得的稳定能力集合。

## 6. 可实现性边界

第一原型不能生成任意 Plugin，也不修改 Agent Loop。只实现一个窄闭环：

1. 用户让 Agent 完成一个包含多次工具调用的本地任务；
2. Agent 成功完成并取得可检查结果；
3. Agent 生成一个带类型接口的 Capsule Candidate；
4. Runtime 在隔离工作区重放 Candidate；
5. Candidate 通过后只在当前 Workspace 注册；
6. 第二个参数不同但结构相同的任务直接调用 Capsule；
7. 遇到未覆盖条件时 Capsule 明确失败并交回通用 Agent。

首版允许的实现：

- 只读文件处理、数据转换、命令组合和无副作用 API 查询；
- Go Host 调度，脚本子进程执行；
- 固定 Capsule Schema 和不可变版本；
- 用户确认注册，不自动全局晋升；
- 只比较重复任务的成功率、模型调用次数和人工干预。

首版禁止：

- 自动修改稳定 Agent Loop；
- 自动安装第三方依赖或扩大权限；
- 生成并热更新任意前端/后端 Plugin；
- 根据模糊现实结果宣称能力已验证；
- 跨用户自动传播或后台自主寻找训练任务。

这使原型成为普通软件工程问题：类型化任务、脚本生成、隔离执行、重放和注册，而不
要求先解决开放世界评价与安全自举。

## 7. 必须先通过的否证实验

这个候选只有满足以下条件才值得升级为产品方向：

1. 在至少三类不同任务中，第二次使用显著减少 LLM 决策轮数；
2. 参数和输入变化后仍能完成，不是把原轨迹硬编码成宏；
3. 超出边界时能退出，不会静默产生错误结果；
4. 创建与验证 Capsule 的成本能被少量复用回收；
5. 与“保存一段 Skill/脚本然后让 Agent 调用”相比有明确增益；
6. 用户不需要理解 Plugin、Workflow 或 Harness 才能使用。

如果第 5 项不成立，这个方向只是更复杂的 Skill/Workflow 管理器，应当淘汰。

## 8. 对现有 PRD 的即时影响

- Plugin Runtime V2、Agent Definition 和 Eval 记录继续保留；
- Capability Search/Load 继续作为底座，不再作为产品核心；
- `prd-recursive-bootstrap-agent-v0.1.md` 保持历史 Draft，不立即删除或覆盖；
- 暂停 Frontier Explorer、全 Agent 架构搜索和自动 Generation 晋升的扩展；
- 下一份 ADR 应只定义 Capsule 原型的类型边界和否证实验，实验通过前不进行完整
  前后端产品化。

## 9. 主要证据

- OpenAI 官方模型与 Agent 能力文档：
  https://developers.openai.com/api/docs/guides/latest-model
- Claude Code Agent Loop：
  https://code.claude.com/docs/en/agent-sdk/agent-loop
- Claude Code 扩展结构：
  https://code.claude.com/docs/en/features-overview
- Manus Sandbox：
  https://manus.im/blog/manus-sandbox
- Manus Wide Research：
  https://manus.im/blog/introducing-wide-research
- OpenClaw Tools / Skills / Plugins：
  https://github.com/openclaw/openclaw/blob/main/docs/tools/index.md
- DeepSeek Harness 官方介绍：
  https://www.deepseek.com/harness/en/
- DeepSeek Harness Agent Loop：
  https://github.com/deepseek-ai/deepseek-harness/blob/master/packages/core/agent-loop/README.md
- Automated Design of Agentic Systems：
  https://arxiv.org/abs/2408.08435
- JIT-Agent：
  https://arxiv.org/abs/2608.25593
- SPACE / Adaptive Action Chunking：
  https://arxiv.org/abs/2609.02042
- SKILL.state：
  https://arxiv.org/abs/2608.26263

## 10. 尚未完成

本审计只完成了架构同构分析和第一轮方向淘汰。下一步必须完成：

1. 用具体任务比较 Capsule、DSH Code Mode、普通脚本和 Skill 的差异；
2. 检查 DSH Creator Mode 是否已经能完整复现原型闭环；
3. 设计三个最小实验及成本上限；
4. 在写代码前给出继续或淘汰该候选的结论。

