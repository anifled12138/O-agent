# ADR 0003：不可变 Agent Generation 与评测晋升门

- 状态：Accepted
- 日期：2026-09-07

## 背景

如果 Agent 可以直接修改自己的 System Prompt、Workflow 或 Tool 策略，线上行为
会在没有版本、证据和回归边界的情况下漂移。仅把这些修改包装成 Plugin 也不能
解决问题，因为安装成功不等于 Agent 效果提升。

## 决策

1. 将可演化 Agent 表示为 `axiom.agent/v1` Definition；内容摘要是身份的一部分。
2. Definition 不更新。每次修改创建新的 Generation，并保留父 Digest。
3. Conversation 在创建时事务性绑定 Generation ID 与 Digest；绑定默认不可改。
4. 模型自举只能创建 `candidate`，不能写入 `stable`。
5. Candidate 与 Baseline 必须在相同 Case、Provider 和 evaluator 下成对运行。
6. Harness 保存逐 Trial 证据并产出推荐；用户通过独立晋升操作决定是否应用。
7. 晋升只影响相同 scope 的后续路由，已固定的 Conversation 不发生漂移。

## 结果

收益：自举变化可追踪、可比较、可拒绝；模型无法用文本绕过上线门；旧 Run 可
重放。代价：每次演化需要额外模型调用和本地数据；首版只支持少量 loop 结构，
后续需要 Agent Definition IR 才能表达更广的推理架构。

## 不采用方案

- 原地更新 Prompt：无法准确归因和回放。
- Candidate 自动成为 Stable：把生成模型同时变成发布权限主体。
- 只做单边评分：无法区分 Candidate 效果与 Case、模型或环境波动。
- 用 Plugin 安装状态代替效果评测：工程构建通过不代表任务能力提升。
