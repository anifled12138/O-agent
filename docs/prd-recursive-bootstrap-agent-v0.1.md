# O 递归自举 Agent 产品需求文档 v0.1

状态：Draft，作为下一阶段产品与架构决策基线

日期：2026-09-05

产品代号：O

目标平台：Windows 优先，本地 Host，接入第三方 LLM API

## 1. 执行摘要

O 不是以聊天、Tool Calling 或 Plugin 数量为核心的 Agent 产品。它的
核心目标是：在不要求本地部署或训练基础模型的前提下，让 Agent 能围绕
用户的真实目标识别自身能力边界，构造新的求解系统，并通过实验 Harness
判断新系统是否确实优于当前版本。

系统中的 Plugin、Skill、Workflow、Script、MCP、多 Agent 和推理 Loop
都不是固定产品层，而是 Agent 可以选择、组合、生成和替换的能力材料。
Agent 的完整求解结构以版本化 `Agent Definition` 表示。当前稳定定义称为
一个 Generation；候选定义必须与当前稳定 Generation 进行受控评测，只有
产生可证明的能力增益且没有越过硬约束时才可以晋升。

产品最终形成如下递归闭环：

```text
现实目标
   ↓
当前 Agent 尝试解决并发现能力边界
   ↓
将能力缺口转化为 Frontier Challenge
   ↓
设计新的 Workflow / Plugin / 推理架构 / Agent 组织
   ↓
Eval Harness 执行回放、沙箱、A/B 和 Canary
   ↓
选择更优 Agent Generation
   ↓
新一代继续完成原目标并挑战更远边界
```

一句话产品定义：

> O 是一个能够针对现实任务自主设计、实验并晋升下一代求解系统的本地
> 递归自举 Agent。

## 2. 背景与问题

### 2.1 当前 Agent 的共同形态

当前主流 Agent 通常由固定 Loop、模型、Tool、上下文和人为配置的 Workflow
组成。Plugin 可以扩展工具、UI、服务和推理钩子，但多数系统仍由同一个模型
在同一条 Loop 中决定下一步。

DeepSeek Harness 已经提供高度通用的插件化和运行时自引用能力；Hermes
可以从会话中创建和更新程序性 Skill。这意味着“可以动态生成插件”或“可以
自动保存技能”本身不足以构成 O 的核心差异。

### 2.2 未解决的核心问题

现有系统善于执行一个已知结构，却缺少默认运行的能力增长过程：

- 失败后通常只是重试或重新提示，而不是诊断缺失的能力维度；
- 能生成一个插件，却不一定知道插件是否是正确的改进方向；
- 能编排 Workflow，却不主动比较不同 Workflow 或 Agent 架构；
- 模型既提出方案又评价自己，容易形成自我确认；
- 新产物是否扩大任务边界，缺少同条件基线和对照实验；
- 一次有效的改造通常不会成为下一代 Agent 的组成部分；
- Agent Loop、上下文策略和多 Agent 拓扑通常是开发者配置，而不是优化对象。

### 2.3 产品机会

O 不与通用 Plugin Runtime 比“能否表达某个功能”。任何足够通用的插件
系统都能承载任意可计算功能。O 竞争的是以下结果：

> 在相同基础模型、相近预算和相同任务条件下，O 能否比固定 Harness
> 更自主地发现有效改造，并使后继 Agent 解决前代无法解决的问题。

## 3. 产品愿景与北极星

### 3.1 产品愿景

用户始终与一个连续的 O 身份交互，但该身份背后的求解系统可以产生多个
版本和候选分支。用户无需学习如何选择 ReAct、Planner、MCP、Workflow 或
多 Agent。系统应根据目标和环境自行决定是否需要构造新的求解架构。

### 3.2 北极星指标

北极星不是安装插件数，而是 `Autonomous Capability Gain`（ACG，自主能力
增益）：

```text
ACG = 在固定模型与预算范围内，
      新 Generation 相对稳定基线新增的可自主完成任务集合，
      并扣除回归、用户干预和额外资源成本。
```

ACG 不使用单个未经校准的 LLM Judge 分数计算。它由任务成功、跨任务迁移、
成本、时延、用户干预和回归率共同构成，并保留各维度原始指标。

### 3.3 产品承诺

- O 可以承认当前能力不足，而不是无限重试；
- O 可以为能力缺口创建可运行的实验；
- O 可以生成整个求解架构，而不只生成 Tool；
- O 可以用同条件基线证明候选是否更好；
- O 可以让通过评测的后继系统继续参与下一轮自举；
- 用户始终可以看到当前使用的是哪一代 Agent 以及为何晋升。

## 4. 范围与非目标

### 4.1 产品目标

1. 提供稳定、最小、不可被普通自举产物绕过的 Seed Kernel。
2. 将完整 Agent 求解结构建模为可版本化的 Agent Definition。
3. 允许 Agent 自主修改 Workflow、推理架构、上下文策略、模型路由、
   多 Agent 拓扑和 Plugin 能力。
4. 建立 Capability Frontier Detection，识别当前失败属于何种能力缺口。
5. 建立用于候选生成、隔离执行、回放、A/B、评分和晋升的 Eval Harness。
6. 让同一稳定 Agent 可以派生多个候选 Generation，并在预算内搜索。
7. 支持第三方模型 API，不要求本地模型，不绑定单一厂商。
8. 通过完整桌面客户端呈现任务、自举、实验、成本和代际变化。
9. 继续复用现有 Plugin Runtime V2 作为一种能力实现与部署机制。

### 4.2 非目标

- 不宣称通过 Harness 本身实现 AGI；
- 首阶段不训练或修改第三方基础模型参数；
- 不以生成更多 Plugin、Skill 或 Workflow 作为成功指标；
- 不让生产 Agent 在一次任务中无版本地覆盖自己的当前实现；
- 不把普通聊天摘要称为持续学习；
- 不要求所有任务都启动自举，已有能力足够时应直接执行；
- 不用一个综合分掩盖成本、回归或安全退化；
- 不复制 DeepSeek Harness、Hermes 或某个研究框架的完整实现。

## 5. 核心术语

### 5.1 Seed Kernel

本地可信内核。它只负责身份、权限、状态、资源隔离、Artifact、实验调度、
Provider 协议、版本和事件事实。Seed Kernel 不决定具体如何推理。

### 5.2 Agent Definition

一个完整、可执行、不可变的 Agent 版本声明。它描述模型、推理图、上下文
策略、能力依赖、验证策略、资源预算和子 Agent 组织。

### 5.3 Generation

一个已经构建并可运行的 Agent Definition 实例。状态分为 Candidate、
Experimental、Canary、Stable、Rejected 和 Archived。

### 5.4 Capability

能够改变 Agent 可解决问题集合的可组合单元。Capability 不等同于 Tool，
可以贡献观察、表示、推理、评价、行动、协作或学习能力。

### 5.5 Frontier Challenge

从失败、用户目标或主动探索中形成的“当前 Agent 尚不能稳定解决”的可评测
问题。它必须包含条件、预算和可观察结果，不能只是一句模糊目标。

### 5.6 Bootstrap Run

以产生更强 Agent Generation 为直接目标的运行。它的产物是候选定义、
实验、结果和晋升决策，而不是普通业务答案。

### 5.7 Eval Harness

负责构建公平对照环境，运行 Baseline 与 Candidate，采集客观结果，分析
不确定性、回归和资源消耗，并作出可审计晋升建议的系统。

## 6. 产品原则

### 6.1 自举对象是整个 Agent

Plugin 只是能力的一个编译目标。Agent Loop、规划器、上下文选择、多 Agent
结构和验证器必须同样可被候选 Generation 替换。

### 6.2 环境反馈高于模型自评

评价信号优先级为：

1. 确定性测试、形式约束和外部系统回执；
2. 可重置环境中的目标状态；
3. 用户明确评价或行为选择；
4. 独立模型或多模型裁判；
5. 候选 Agent 自评。

低优先级信号不能覆盖高优先级失败。

### 6.3 比较的是系统，不是提示词文案

每次实验必须记录完整 Agent Definition、模型版本、Provider 参数、能力
版本、上下文输入、环境快照和预算。不能只记录最终 Prompt。

### 6.4 先证明增益，再继承

候选可以自由实验，但不能因为“代码编译成功”或“模型认为更好”成为稳定
Generation。能力继承是一项评测决策。

### 6.5 不固定一种智能形态

`react.v1` 是 Seed Agent，不是永久核心。后续 Generation 可以采用树搜索、
状态机、程序化 Tool Calling、模型集成、专家路由或尚未预定义的结构。

### 6.6 简单任务不支付自举成本

系统先估算直接求解与自举价值。只有失败、低置信度、高复用价值或用户明确
要求扩展能力时，才启动 Bootstrap Run。

## 7. 能力边界模型

O 从七个维度描述 Agent 能力，而不是只维护 Tool Catalog：

| 维度 | 含义 | 典型自举产物 |
| --- | --- | --- |
| Observe | 能看到哪些环境状态 | Parser、Connector、Sensor、Context Provider |
| Represent | 如何表达问题和世界状态 | Schema、知识图、状态机、领域模型 |
| Reason | 如何搜索、规划和分解 | Planner、Search Policy、Reasoning Loop |
| Evaluate | 如何判断真假、进展和完成 | Verifier、Simulator、Test、Judge |
| Act | 能对环境执行什么操作 | Tool、MCP、Script、Plugin Service |
| Organize | 如何组织模型、Agent 与人 | Router、Workflow、多 Agent 拓扑 |
| Adapt | 如何从结果产生下一代 | Curriculum、Optimizer、Candidate Generator |

每个 Frontier Challenge 必须指出当前证据支持的缺口维度。可以同时存在多个
假设，但不得在没有诊断的情况下直接默认“再写一个插件”。

## 8. 产品总体架构

```text
┌──────────────────────── O Desktop ────────────────────────┐
│ Conversation  Runs  Frontier  Experiments  Generations       │
│ Capabilities  Plugins  Costs  Settings  Approvals            │
└──────────────────────────────┬─────────────────────────────────┘
                               │ authenticated local IPC
┌──────────────────────────────▼─────────────────────────────────┐
│ Stable Seed Kernel (Go)                                       │
│ Identity / Event Store / Artifact CAS / Policy / Broker       │
│ Provider Runtime / Experiment Scheduler / Generation Registry │
├────────────────────────────────────────────────────────────────┤
│ Bootstrap Plane                                                │
│ Gap Detector / Challenge Builder / Candidate Generator         │
│ Architecture Composer / Eval Harness / Selector / Promoter     │
├────────────────────────────────────────────────────────────────┤
│ Agent Execution Plane                                          │
│ Agent Definition Runtime / Workflow / Context / Model / Tool   │
│ Subagents / Verifiers / Plugins / MCP                          │
├────────────────────────────────────────────────────────────────┤
│ Isolation Plane                                                │
│ Workspace Forks / Sandboxes / Recorded Environments / Canary   │
└────────────────────────────────────────────────────────────────┘
```

### 8.1 不可自举替换的最小内核

- 用户身份与本地登录；
- Provider Secret 和凭据边界；
- Event Store 和 Artifact 完整性；
- 权限、批准与资源 Broker；
- 实验隔离与取消；
- Generation 签名、激活和恢复；
- A/B 分流和实验数据真实性。

### 8.2 可自举替换的执行层

- Agent Loop；
- Prompt 与上下文编排；
- Planner 和搜索策略；
- Workflow 与状态表示；
- 模型选择、并行和聚合；
- 子 Agent 数量、角色与通信；
- Tool、Skill、MCP 和 Plugin；
- Verifier、Judge 和完成策略；
- 能力缺口诊断器与候选生成策略本身。

最后一项允许 Bootstrap Plane 产生后继优化器，但其晋升仍由 Seed Kernel
执行既定的实验与权限规则。

## 9. Agent Definition

### 9.1 逻辑结构

```yaml
apiVersion: axiom.agent/v1
kind: AgentDefinition
metadata:
  id: agent-general
  generation: 12
  parent: agent-general@g11
spec:
  objectiveClasses: [general]
  models:
    - ref: provider-model://primary
      role: proposer
    - ref: provider-model://critic
      role: verifier
  cognition:
    entrypoint: strategy://adaptive-router@3
    strategies:
      - strategy://react@1
      - strategy://workflow-planner@4
      - strategy://candidate-search@2
  context:
    policy: context://task-state-first@5
  capabilities:
    - capability://workspace-edit@2
    - capability://capability-discovery@1
  evaluators:
    - evaluator://task-contract@2
  budgets:
    modelCalls: 40
    tokens: 200000
    wallTime: 45m
```

### 9.2 不可变与派生

- 已运行的 Generation Definition 不可原地修改；
- Candidate 必须引用 Parent Generation；
- 一次候选只需记录相对 Parent 的结构化 Diff；
- 构建后生成完整 Definition Digest；
- Run 始终 pin 到一个 Digest，运行中不能被全局升级改变；
- 新 Generation 可以从多个 Parent 合并，但必须记录来源。

### 9.3 Agent Definition IR

首版采用声明式有向图 IR，节点类型保持少而稳定：

- `model.invoke`；
- `capability.call`；
- `workflow.call`；
- `branch`；
- `parallel`；
- `aggregate`；
- `verify`；
- `checkpoint`；
- `delegate`；
- `finalize`。

自举系统可以生成新的节点实现 Plugin，但不能在未注册 Schema 的情况下向
Seed Kernel 注入任意状态转换。

## 10. 自举生命周期

### 10.1 触发条件

满足任一条件可提出 Bootstrap Run：

- 当前任务在受控重试后仍失败；
- Agent 明确检测到所需能力不存在；
- 当前方案成功概率或验证覆盖不足；
- 同类任务重复出现且成本长期偏高；
- 用户直接要求 Agent 获得一种新能力；
- Frontier Explorer 在空闲预算中提出有价值挑战。

用户可以关闭主动探索，但不能关闭失败记录。

### 10.2 生命周期状态

```text
Detected
  → Scoped
  → BaselineCaptured
  → Designing
  → CandidatesBuilt
  → Evaluating
  → Selected
  → Canary
  → Promoted

终止状态：NoGap / Inconclusive / Rejected / BudgetExhausted / Blocked
```

### 10.3 Frontier Challenge

Challenge 至少包括：

- 原始目标及其用户价值；
- 当前 Stable Generation；
- 已观察失败和环境证据；
- 一个或多个能力缺口假设；
- 初始状态或可重置环境；
- 成功条件与禁止条件；
- 模型、Token、时间和外部成本预算；
- 训练/生成任务与保留测试任务的边界；
- 是否允许产生现实副作用。

### 10.4 候选生成

Candidate Generator 不能只生成一个方案。它应在预算内保持结构差异，例如：

- 复用现有能力的新 Workflow；
- 新的状态表示配合现有 Tool；
- 新 Plugin 或外部程序；
- 单 Agent 的新推理结构；
- 多 Agent 分工和聚合；
- 不同模型角色或路由；
- Verifier 或 Simulator 优先的结构；
- 上述方案的组合。

候选数量由预算和预计信息增益决定，不设置永远固定的 N。

### 10.5 后继生成

通过评测的 Candidate 可以成为：

- 仅对当前任务有效的 Task Generation；
- 对某个 Workspace 有效的 Workspace Generation；
- 某一类任务的 Specialist Generation；
- 默认 General Generation 的后继版本；
- Bootstrap Optimizer 自身的新版本。

局部成功默认不晋升为全局 General Generation。

## 11. Workflow 与推理架构自举

### 11.1 Workflow 是可执行认知结构

Workflow 不只是 Tool 顺序。它可以描述：

- 领域状态和阶段；
- 分支、循环与停止条件；
- 何时重新规划；
- 哪些步骤由程序确定执行；
- 哪些步骤调用模型；
- 哪些步骤需要独立验证；
- 中间 Artifact 和跨 Agent 通信；
- 失败恢复与替代路径。

### 11.2 推理架构是一级产物

Candidate 可以改变：

- 先规划后执行，还是边观察边行动；
- 单轨采样，还是并行候选搜索；
- 使用树搜索、辩论、反思或程序合成；
- 何时分配更多 test-time compute；
- 何时把模型调用编译为确定性程序；
- 何时召回 Specialist Agent；
- 由谁验证、聚合和宣布完成。

### 11.3 从 Workflow 到 Plugin

Workflow 可以直接作为版本化 IR 运行。只有满足以下条件才编译为 Plugin：

- 需要新增运行时接口、后台 Service、UI 或外部连接；
- 需要降低重复模型调用成本；
- 需要长期监听或响应事件；
- 已有稳定接口和足够实验覆盖；
- 预计跨任务复用价值高于维护成本。

因此 Plugin 是成熟能力的部署形态之一，不是所有自举结果的默认终点。

## 12. Eval Harness

### 12.1 角色

Eval Harness 不是外围测试工具，而是递归自举的选择机制。没有 Harness，
Candidate Generation 只能算模型提出的修改，不能称为能力增长。

Harness 必须回答：

1. Candidate 是否完成了 Baseline 无法完成的任务？
2. 增益是否来自 Agent Definition 差异而非更多预算或更强模型？
3. 增益能否迁移到未参与生成的任务？
4. 是否导致旧能力、成本、时延或用户干预退化？
5. 结果的不确定性是否足以支持晋升？

### 12.2 Eval Suite 类型

每个 Frontier Challenge 形成一个 Eval Suite：

- `frontier`：直接验证新增目标；
- `transfer`：验证相似但未见任务；
- `regression`：验证父 Generation 已有能力；
- `robustness`：扰动输入、顺序、环境或 Tool 错误；
- `efficiency`：Token、模型调用、工具调用、时延和费用；
- `autonomy`：用户澄清、纠正、批准之外干预的次数；
- `constraint`：权限、副作用和禁止行为硬门槛。

### 12.3 环境类型

Harness 支持四级环境：

1. `Recorded Replay`：使用保存的 Observation 和 Tool Result 重放；
2. `Deterministic Sandbox`：可重置文件、进程、浏览器或模拟服务；
3. `Shadow`：读取真实环境但禁止写入，比较决策；
4. `Canary`：在明确限制的真实环境和小流量上运行。

Recorded Replay 成本最低，但不能证明 Candidate 能处理未记录的新状态；
Sandbox 是主要对照环境；Shadow 用于检查真实分布；Canary 是晋升前最后阶段。

### 12.4 A/B Test 设计

#### 配对对照

同一个 Eval Case 同时运行：

- A：当前 Stable Generation；
- B：Candidate Generation。

两组应尽量固定：

- 相同模型与模型版本；
- 相同 Provider 参数；
- 相同初始环境快照；
- 相同资源上限；
- 相同 Tool/Plugin 外部版本；
- 相同超时和完成定义。

若模型 API 支持 Seed，则记录并轮换 Seed；不支持时，进行多次重复并报告
方差。不能只选取 Candidate 成功的一次与 Baseline 对比。

#### 随机化与盲评

- Case 执行顺序随机化，避免外部服务时间偏差；
- LLM Judge 看不到 A/B 身份、Parent 关系和生成理由；
- 主观输出交换展示顺序；
- Candidate Generator 不读取保留测试集答案；
- 同一 Agent 不能修改它正在参加的 Eval Suite。

#### Live A/B

真实用户任务只有在以下条件同时满足时才允许分流：

- 用户开启实验；
- Effect 为只读或可恢复；
- Candidate 权限不高于当前授权；
- 有即时回退路径；
- 两组不会重复执行外部副作用。

有写入副作用的任务优先使用 Shadow Decision：B 生成决策但不执行，由 A
正常执行。只有 Canary 才允许 B 独立产生受限副作用。

### 12.5 指标

每个 Case 保存以下原始指标：

| 指标 | 说明 |
| --- | --- |
| Task Success | 是否满足环境可验证的完成条件 |
| Quality | 输出质量，多维评分而非单个 Judge 结论 |
| Frontier Win | B 成功且 A 在同预算下失败 |
| Regression | A 成功而 B 失败 |
| Transfer | B 在未参与生成的相似任务上的增益 |
| Intervention | 非权限类用户干预次数和强度 |
| Token/Cost | 输入、输出、缓存、Tool 与外部服务成本 |
| Latency | 首响应、完成和关键路径时延 |
| Robustness | 对扰动、失败和环境变化的成功率 |
| Bootstrap Cost | 产生 Candidate 和评测本身的总成本 |

### 12.6 统计与决策

首版采用配对试验和区间估计，不强制所有任务使用同一种统计模型：

- 确定性 Case 可以一次判定，但必须重置环境并验证无隐藏状态；
- 非确定性 Case 至少重复到达到预设预算或置信门槛；
- 报告 B 优于 A 的后验概率或置信区间；
- 小样本结果只能晋升为 Task/Workspace Generation；
- General Generation 需要独立保留集和回归集；
- 结果不确定时状态为 Inconclusive，不以平均分微弱领先强行晋升。

### 12.7 晋升规则

Candidate 必须同时满足：

- 所有硬约束通过；
- Frontier Suite 出现有意义的新增成功；
- 保留集存在可重复增益；
- 回归不超过目标 Scope 的允许上限；
- 成本增量与能力增益匹配；
- Definition、依赖、实验和结果可完整重放；
- Canary 未发现新的高严重度失败。

Selector 默认采用约束下的 Pareto 选择，不把质量、成本和时延强行压成一个
不透明总分。若两个候选各有优势，可以保留为不同 Specialist，而不是只留
一个“全局最佳”。

### 12.8 评价器防污染

- 确定性评价器代码与 Candidate 工作区分离；
- Candidate 无权修改 Eval Case、评分器和历史结果；
- LLM Judge 的 Prompt、模型和版本被记录；
- 对主观任务使用多个独立 Judge 或用户偏好校准；
- Judge 只提供软证据，不能掩盖测试失败；
- 检测 Candidate 对 Benchmark 文本或答案的硬编码。

## 13. 自举策略

### 13.1 能力缺口诊断

Gap Detector 输出多个可证伪假设，而不是唯一标签：

```json
{
  "challenge": "frontier-123",
  "hypotheses": [
    {"dimension": "represent", "claim": "缺少稳定页面状态模型"},
    {"dimension": "evaluate", "claim": "无法判断提交是否成功"}
  ],
  "evidence": ["event://run-7/step-18"],
  "nextProbe": "构造只读探索任务并比较两种状态表示"
}
```

### 13.2 架构搜索

首版不实现无限开放搜索，采用受预算约束的候选生成：

- 从稳定 Agent Definition 派生局部变体；
- 从 Capability Hub 组合已有构件；
- 允许模型生成新 Workflow 和 Plugin；
- 允许多个模型独立提出架构；
- 根据历史实验结果分配后续预算；
- 低信息增益分支提前停止；
- 允许保留多个互补 Specialist。

后续可增加进化搜索、Bandit、MCTS 或学习型架构优化器，但它们本身也必须
通过同一 Harness 与当前 Optimizer 做 A/B。

### 13.3 自动课程

主动自举不能无目标随机生成插件。Frontier Explorer 从以下来源生成课程：

- 用户反复提出但 Agent 失败的目标；
- 已完成任务中的脆弱步骤；
- 用户明确希望获得的能力方向；
- 相邻能力组合后自然出现的新挑战；
- 已有 Specialist 的边界和反例；
- 真实环境允许的低风险探索空间。

课程优先选择“完成后能解锁更多后续任务”的挑战，而不是最容易刷分的任务。

## 14. 能力产物与复用

### 14.1 统一 Capability Descriptor

```text
Capability
├── semantic contract：解决什么问题
├── preconditions：何时可用
├── effects：对环境产生什么影响
├── implementation：Workflow / Plugin / Skill / Script / Agent
├── evidence：哪些 Eval 支持它有效
├── scope：Task / Workspace / User / Global
├── dependencies：模型、服务、其他能力
└── versions：不可变版本及后继关系
```

### 14.2 懒加载

普通 Agent 只看到相关 Capability Card，不读取全部实现。选定能力后才加载
Agent Definition 片段、Workflow、Schema 或 Skill 内容。纯 UI、后台维护和
不面向模型的 Plugin 不进入 Agent 上下文。

### 14.3 复用不等于记忆注入

实验轨迹、历史消息和构建日志保存在 Event/Artifact Store。未来任务通过
Capability Index 找到已验证能力，而不是把过去全部对话塞回上下文。长期
记忆只负责少量用户偏好、环境事实和检索线索，不承担能力增长核心职责。

## 15. 用户体验

### 15.1 普通任务

用户直接描述目标。O 优先使用当前 Stable Generation 完成任务，不展示
内部架构细节。只有检测到能力缺口或用户打开高级视图时，才展示自举过程。

### 15.2 能力不足

Agent 应明确表达：

- 当前卡在哪个能力边界；
- 是否可以启动自举；
- 需要多少时间、模型预算和环境权限；
- 自举期间原任务是否可以继续；
- 最终会形成临时能力还是候选 Generation。

低风险且在 Standing Policy 内的实验可以自动开始。

### 15.3 自举中心

桌面客户端新增 `Evolution` 一级界面：

- Frontier Challenge 列表；
- 当前 Stable Generation 和谱系图；
- Candidate Agent Definition Diff；
- A/B 任务、进度和资源使用；
- Frontier Win、Regression 和 Transfer 结果；
- Candidate 运行轨迹对比；
- 晋升、拒绝、保留为 Specialist 或继续实验；
- 当前 Agent 为什么选择某个 Generation。

### 15.4 对话中的控制

用户可以直接说：

- “想办法让自己学会完成这一类任务”；
- “不要只写插件，比较几种推理方案”；
- “给这个候选更多实验预算”；
- “只把它用于这个项目”；
- “把当前版本与上一代做 A/B”；
- “这次结果不好，恢复使用上一代”；
- “解释这一代 Agent 比上一代强在哪里”。

### 15.5 代际连续性

升级 Agent Definition 不创建新的用户人格。对话、用户配置、授权和任务历史
属于 O Identity；Generation 只是其当前求解系统。用户始终面对一个产品
主体，而不是被迫管理大量机器人实例。

## 16. 功能需求

### 16.1 Seed Kernel

- `FR-KERNEL-001`：所有 Run 必须 pin 到不可变 Agent Definition Digest。
- `FR-KERNEL-002`：Seed Kernel 必须能取消、暂停和恢复普通 Run 与 Bootstrap Run。
- `FR-KERNEL-003`：Candidate 无权修改实验定义、权限系统和自身评分记录。
- `FR-KERNEL-004`：所有 Model、Tool、Plugin、Workflow 和 Judge 调用写入统一 Event。
- `FR-KERNEL-005`：Host 重启后可以恢复实验状态，不依赖模型聊天记忆。

### 16.2 Agent Definition Runtime

- `FR-AGENT-001`：支持注册、校验、构建和执行 `axiom.agent/v1`。
- `FR-AGENT-002`：支持 Parent、Diff、Digest、Scope 和 Generation 状态。
- `FR-AGENT-003`：首个 Seed Definition 为 `react.v1`。
- `FR-AGENT-004`：运行时支持分支、并行、验证、委派和 Workflow 调用。
- `FR-AGENT-005`：Candidate 可以改变推理结构但不能改变 Seed Kernel 规则。

### 16.3 Frontier 与 Bootstrap

- `FR-BOOT-001`：系统能从失败 Run 创建 Frontier Challenge。
- `FR-BOOT-002`：Challenge 必须包含 Baseline、成功条件、预算和环境策略。
- `FR-BOOT-003`：系统能为一个 Challenge 生成多个结构不同的 Candidate。
- `FR-BOOT-004`：Candidate 可以包含 Workflow、Plugin 和 Agent Definition Diff。
- `FR-BOOT-005`：Bootstrap Optimizer 自身可以产生候选后继版本。
- `FR-BOOT-006`：预算耗尽时保存所有有效实验状态并停止继续生成。

### 16.4 Eval Harness

- `FR-EVAL-001`：支持 Baseline/Candidate 配对运行。
- `FR-EVAL-002`：支持 Replay、Sandbox、Shadow 和 Canary 四类环境。
- `FR-EVAL-003`：记录模型、参数、输入、依赖、环境和预算，保证可比较。
- `FR-EVAL-004`：支持确定性评分、环境评分、用户评分和盲化 LLM Judge。
- `FR-EVAL-005`：自动生成 Frontier、Transfer、Regression、Robustness 和 Efficiency 报告。
- `FR-EVAL-006`：支持重复试验、方差、置信区间和 Inconclusive 结论。
- `FR-EVAL-007`：没有 Eval Evidence 的 Candidate 不能成为 Stable。
- `FR-EVAL-008`：支持对 Bootstrap Optimizer 本身做 A/B。

### 16.5 Generation Registry

- `FR-GEN-001`：保存 Agent Generation 谱系和不可变 Definition。
- `FR-GEN-002`：支持 Task、Workspace、Specialist 和 General Scope。
- `FR-GEN-003`：支持 Canary、Promote、Reject、Archive 和恢复上一稳定版本。
- `FR-GEN-004`：并存 Specialist 由 Runtime Router 按任务选择。
- `FR-GEN-005`：晋升不会改变已运行或已恢复 Run 所 pin 的 Generation。

### 16.6 Capability 与 Plugin

- `FR-CAP-001`：Capability Descriptor 必须声明能力维度和 Evidence。
- `FR-CAP-002`：Capability 搜索只返回 Card，具体实现按需加载。
- `FR-CAP-003`：Workflow 可以独立版本化运行，不强制封装为 Plugin。
- `FR-CAP-004`：Plugin Forge 输出可以作为 Candidate 的一部分进入 Harness。
- `FR-CAP-005`：Plugin 的测试结果不能代替整个 Agent Candidate 的端到端 Eval。

### 16.7 Provider

- `FR-PROVIDER-001`：支持 OpenAI、Anthropic、DeepSeek 和自定义兼容 API。
- `FR-PROVIDER-002`：A/B 默认使用相同模型；使用不同模型时必须标记为系统组合实验。
- `FR-PROVIDER-003`：记录 Provider 返回的模型标识、用量、缓存和采样参数。
- `FR-PROVIDER-004`：Provider 错误率和限流不能被误记为 Candidate 推理失败。

### 16.8 客户端

- `FR-UI-001`：提供完整登录、Provider 配置、对话、Run 和设置体验。
- `FR-UI-002`：提供 Generation、Experiment 和 Frontier 可视化。
- `FR-UI-003`：支持查看 A/B 两侧轨迹和 Agent Definition Diff。
- `FR-UI-004`：支持在对话中启动、限制和终止 Bootstrap Run。
- `FR-UI-005`：实验在后台运行时通过系统通知报告关键状态。

## 17. 数据模型

首批核心实体：

- `agent_definitions`：完整 Definition、Digest、Parent 和 Scope；
- `agent_generations`：Candidate/Stable 状态和激活范围；
- `frontier_challenges`：能力缺口、目标、环境和预算；
- `bootstrap_runs`：一次自举过程；
- `candidate_variants`：Definition Diff 和构建 Artifact；
- `eval_suites`：Case 集、划分和评分规则；
- `eval_cases`：输入、环境、成功条件和密封数据；
- `eval_trials`：一次 A/B 侧的执行；
- `eval_observations`：原始指标和 Evidence；
- `promotion_decisions`：规则、报告、Actor 和理由；
- `capability_descriptors`：语义合约、实现和 Scope；
- `generation_capabilities`：Generation 与 Capability 的固定依赖；
- `model_invocations`：模型、参数、Token、费用和 Trace；
- `environment_snapshots`：可重置状态或引用；
- `artifacts`：内容寻址产物。

Eval Case 的密封答案、Judge 配置和评分器不进入 Candidate 可读工作区。

## 18. 事件模型

关键事件包括：

```text
frontier.detected
frontier.scoped
bootstrap.started
candidate.proposed
candidate.definition_built
eval.suite_created
eval.trial_started
eval.trial_completed
eval.observation_recorded
eval.comparison_completed
candidate.selected
generation.canary_started
generation.promoted
generation.rejected
generation.routed
generation.regression_detected
```

事件只记录事实。模型生成的解释保存为带 Actor 的 Claim，不能覆盖环境结果。

## 19. API 与扩展点

首批内部服务接口：

```go
type AgentDefinitionRegistry interface {
    Resolve(ctx context.Context, ref AgentRef) (AgentDefinition, error)
    RegisterCandidate(ctx context.Context, draft DefinitionDraft) (AgentDefinition, error)
    Promote(ctx context.Context, candidate Digest, scope Scope, evidence EvalReportRef) error
}

type FrontierService interface {
    Detect(ctx context.Context, runID string) ([]GapHypothesis, error)
    CreateChallenge(ctx context.Context, spec ChallengeSpec) (Challenge, error)
}

type CandidateGenerator interface {
    Propose(ctx context.Context, challenge Challenge, budget Budget) ([]CandidateDraft, error)
}

type EvalHarness interface {
    BuildSuite(ctx context.Context, challenge Challenge) (EvalSuite, error)
    Compare(ctx context.Context, baseline AgentRef, candidate AgentRef, suite EvalSuite) (EvalReport, error)
}

type GenerationRouter interface {
    Select(ctx context.Context, task TaskFingerprint, candidates []AgentRef) (RoutingDecision, error)
}
```

这些接口首先在 Go Host 内实现，成熟后再暴露受约束 Plugin Extension Point。

## 20. 权限与资源边界

权限不是产品核心卖点，但它是开放自举得以运行的必要边界：

- Candidate 默认只能在实验 Workspace 和模拟环境运行；
- 自举不能扩大用户已授予的权限；
- 新权限必须绑定 Candidate Digest 单独批准；
- A/B 两侧共享同等级权限；
- Candidate 不能读取 Provider Secret 明文；
- Canary 必须声明最大副作用和恢复方式；
- 多 Agent 子进程继承收窄后的 Scope；
- 生成的 Plugin 继续使用现有 Release、Broker 和 Sidecar 隔离机制。

## 21. 非功能需求

### 21.1 可恢复性

- 所有实验步骤可从 Event Store 重建；
- Host 退出不能丢失已完成 Trial；
- 非幂等外部动作不会在恢复时自动重放；
- Stable Generation 始终存在可启动版本。

### 21.2 可复现性

- Definition、依赖和 Eval Suite 内容寻址；
- 记录模型实际标识和采样参数；
- 可复现不等于结果完全一致，非确定性必须记录分布；
- Replay 报告需标明哪些外部 Observation 是录制值。

### 21.3 性能

- 普通任务不经过 Bootstrap Plane 时，额外本地调度 P95 小于 30ms；
- 100 个 Candidate Definition 不使普通模型上下文线性增长；
- Eval Worker 支持至少 4 个互不冲突 Trial 并行；
- Artifact 去重，A/B 共享只读基础快照；
- Bootstrap Run 必须有独立 Token、费用、时长和并发上限。

### 21.4 可解释性

每次晋升必须能回答：

- 改了什么；
- 为何提出这项修改；
- 在哪些任务上胜出；
- 在哪些任务上退化；
- 增加了多少成本；
- 将在哪个 Scope 生效。

## 22. 成功指标

### 22.1 产品指标

- 用户发起自举后得到至少一个可运行 Candidate 的比例；
- 用户接受或持续使用晋升 Generation 的比例；
- 自举后同类任务的用户干预下降；
- 自举成本被后续复用收益覆盖的时间；
- 用户主动恢复上一代的比例；
- 用户能够理解代际变化的定性反馈。

### 22.2 Agent 指标

- Frontier Win Rate；
- Generalization/Transfer Gain；
- Regression Rate；
- Bootstrap Depth：连续有效后继 Generation 数；
- Capability Reuse Rate；
- Intervention-adjusted Success；
- Cost-adjusted Success；
- Optimizer Yield：评测 Candidate 中产生有效增益的比例。

### 22.3 不采用的虚荣指标

- Plugin 总数；
- Memory 条目数；
- 单次任务 Tool Call 数；
- Agent 自称学会的能力数；
- 没有 Baseline 的 Judge 平均分。

## 23. 里程碑

### M0：方向冻结与 Agent Definition 基础

交付：

- 本 PRD 评审完成；
- `axiom.agent/v1` Schema 和 ADR；
- Seed Kernel 与 Mutable Agent 的代码边界；
- `react.v1` 被包装为首个不可变 Agent Definition；
- Run pin Agent Digest。

验收：同一会话可以明确选择两个 Definition 运行，旧 Run 不受升级影响。

### M1：Eval Harness 最小纵向切片

交付：

- Eval Suite/Case/Trial 数据模型；
- Recorded Replay 和 Deterministic Sandbox；
- Baseline/Candidate 配对运行；
- 确定性评分、Token、时延和成本报告；
- 客户端 Experiment 页面。

验收：手工创建一个 Workflow Candidate，可以与 `react.v1` 对同一组任务做
A/B，并生成可重放报告。

### M2：Workflow 与推理架构候选

交付：

- Agent Definition IR 执行器；
- Workflow、Branch、Parallel、Verify 和 Delegate；
- Candidate Definition Diff；
- 候选生成器；
- Regression 和 Transfer Suite。

验收：Agent 自主生成至少两种结构不同的求解架构，Harness 能选出有效候选，
且未通过候选不会影响 Stable。

### M3：Frontier Detection 与自动自举

交付：

- 从失败 Run 创建 Challenge；
- 多假设能力缺口诊断；
- 有预算的架构搜索；
- Task/Workspace Generation 自动路由；
- Inconclusive 与预算停止。

验收：面对 Seed Agent 失败的保留任务，系统能够从失败开始自主产生、评测并
使用一个成功 Candidate 完成原任务。

### M4：Plugin Forge 与完整能力编译

交付：

- Workflow/Agent Candidate 可生成 Plugin Surface；
- Plugin 端到端 Eval，而不只做单元测试；
- Shadow 与 Canary；
- 前后端 Plugin Candidate 的隔离 A/B；
- Specialist Generation。

验收：一个需要新前后端能力的目标可以从对话进入自举，经过 A/B 后作为
Workspace Specialist 使用，不修改 Host 源码。

### M5：递归优化器与主动课程

交付：

- Bootstrap Optimizer Definition 版本化；
- Optimizer A/B；
- Frontier Explorer 和自动课程；
- 多代谱系、合并与互补 Specialist 路由；
- 长周期能力增益报告。

验收：后继 Optimizer 在密封 Challenge 集上以相同预算产生更多有效 Candidate，
并能继续生成下一代 Optimizer。

## 24. V1 发布验收标准

首个可对外称为“自举 Agent”的版本必须满足：

1. Agent Loop 已成为版本化 Agent Definition，而不是 Go 代码中的固定流程；
2. Agent 可以从一次失败建立可评测 Frontier Challenge；
3. Agent 可以生成至少 Workflow 和 Plugin 两类 Candidate；
4. Candidate 与 Stable 在相同条件下完成配对 A/B；
5. Harness 同时报告成功、成本、时延、干预和回归；
6. 至少一个保留任务证明 Candidate 完成了 Baseline 无法完成的目标；
7. Candidate 可以仅在当前 Task/Workspace 生效；
8. 晋升后 Agent 能使用新 Generation 继续原任务；
9. Host 重启后 Generation、实验和原任务均可恢复；
10. 用户可在完整客户端中查看差异、证据、成本和代际谱系。

如果缺少第 4、6 或 8 项，产品只能称为“Agent 自动生成扩展”，不能称为
“递归自举 Agent”。

## 25. 主要风险

### 25.1 自我评价闭环

模型可能生成容易被自己判高分的 Candidate。解决方向是环境结果优先、盲评、
密封测试和多来源评价，而不是增加更多反思 Prompt。

### 25.2 Benchmark 过拟合

Candidate 可能记住 Eval Case。必须区分生成集、开发集和保留集，并持续加入
真实任务扰动。General 晋升不能只依赖生成过程中可见的 Case。

### 25.3 搜索成本超过收益

自举可能比直接解决任务更昂贵。Challenge Builder 必须估计未来复用价值，
Harness 报告 Bootstrap Cost，普通任务默认不启动架构搜索。

### 25.4 能力碎片化

大量 Specialist 可能使路由困难。系统允许能力组合和谱系合并，但不能为了
目录整洁强行合并行为不同的 Agent。

### 25.5 伪递归

如果每一代都只是当前模型改写几句 Prompt，能力不会持续增长。Bootstrap
Depth 必须要求新增可验证任务成功，不能只要求 Definition 发生变化。

### 25.6 基础模型上限

使用冻结 API 模型意味着自举主要发生在 test-time compute、外部程序、
环境反馈和系统组合层。O 不应把系统放大误称为基础模型参数能力突破。

## 26. 当前工程迁移

现有 O 能力继续保留：

- Go Host、SQLite、身份和 Provider 配置；
- Plugin Manifest V2 与不可变 Release；
- UI/Service/Tool/Skill 分离；
- Capability Search/Load；
- Broker、Sidecar、热更新和恢复；
- Agent Turn Trace。

需要调整的产品假设：

- `react.v1` 从“核心推理框架”降为 Seed Agent Definition；
- Plugin Forge 从“自举终点”降为 Candidate Compiler 之一；
- Eval 从 Plugin 发布检查升级为 Generation 选择机制；
- Context、Workflow、Verifier 和多 Agent 拓扑进入 Agent Definition；
- Run Trace 升级为支持 A/B 对照和完整 Definition Pin 的 Event；
- 客户端新增 Frontier、Experiment 和 Generation 三个产品概念。

不应立即重写现有 Plugin Runtime。第一项代码工作必须是 Agent Definition
与 Eval Harness 的最小纵向切片，因为没有 Baseline/Candidate 对照，后续
任何“自举效果提升”都无法被判断。

## 27. 开放问题

- General Generation 的首批密封任务集如何构建并防止泄露；
- 不同 Provider 无法固定 Seed 时采用何种默认重复次数和统计方法；
- 用户真实任务如何在不泄露隐私的情况下转化为可复现 Eval；
- Specialist Router 是规则、检索、模型还是可自举 Agent；
- Candidate 是否允许选择更贵模型，以及如何分离模型增益和架构增益；
- 何时允许 Optimizer 修改自己的 Candidate Generator；
- 主动课程的空闲预算由用户、系统还是收益回收策略决定；
- 后续是否接入 Provider Fine-tuning，将模型参数版本纳入 Agent Definition。

这些问题不阻塞 M0/M1。首要事实是：O 的核心对象从 Plugin 转为完整
Agent Generation，核心闭环从“生成并安装”转为“提出、实验、选择、晋升并
递归产生后继”。

## 28. 研究基线

本 PRD 将以下现有方向视为设计基线，而不是 O 独有能力：

- [DeepSeek Harness](https://www.deepseek.com/harness/en/)：万物 Plugin、
  Creator Mode、程序化 Tool Calling 和可追踪 Session；
- [DeepSeek Dynamic Cordis Plugin](https://github.com/deepseek-ai/deepseek-harness/blob/master/packages/extensions/tool-cordis/README.md)：
  运行时检查、Host/Client 动态包、Tool 注册与热更新；
- [Hermes Agent](https://hermes-agent.nousresearch.com/docs/)：后台学习循环、
  程序性 Skill 和跨会话记忆；
- [Voyager](https://arxiv.org/abs/2305.16291)：自动课程、可执行 Skill Library
  和开放式能力获取；
- [Agent Workflow Memory](https://arxiv.org/abs/2409.07429)：从历史轨迹中
  归纳可复用 Workflow；
- [Automated Design of Agentic Systems](https://arxiv.org/abs/2408.08435)：
  由 Meta-Agent 搜索和编写 Agent 架构；
- [FlowEvo](https://arxiv.org/abs/2607.21596)：Workflow 与可执行 Skill 的
  共同演化。

O 的产品假设不是上述任一构件首次出现，而是把真实用户目标、完整 Agent
Definition、隔离实验、A/B 选择、分 Scope 晋升和递归后继放入同一个持续运行
的本地产品闭环。该假设最终必须通过 M1-M5 的对照实验验证，不能只由设计文档
宣称成立。
