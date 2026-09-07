# Agent Evolution Runtime 实现说明

状态：M0 完成，M1 最小纵向切片完成（2026-09-07）

本文记录当前代码已经实现的事实；目标架构和后续里程碑见
[`prd-recursive-bootstrap-agent-v0.1.md`](prd-recursive-bootstrap-agent-v0.1.md)。

## 1. 当前纵向闭环

```text
Stable Generation
       │
       ▼
Frontier Challenge ──► Bootstrap Candidate Generator
                              │
                              ▼
                     immutable Candidate(s)
                              │
                              ▼
                 paired A/B Eval Harness
                       │             │
                  reject/inconclusive│ promote recommendation
                                     ▼
                              user promotion
                                     │
                                     ▼
                           new Stable Generation
```

Agent 只能生成 Candidate，不能直接覆盖 Stable。Harness 只给出建议，最终晋升
通过独立 API 发生，并再次校验实验归属、Candidate ID、实验状态和推荐结果。

## 2. 已实现组件

- `runtime.evolution.v1`：持久化不可变 Agent Definition、Generation 谱系、
  Frontier Challenge 和会话绑定。
- `runtime.agent.v1`：根据会话固定的 Definition Digest 执行 loop。已有
  `react.v1` 与 `plan-react.v1` 两种结构；后者先生成执行简报，再进入独立的
  Tool loop。
- `runtime.bootstrap.v1`：把 Challenge、失败证据、成功条件和 Baseline
  Definition 提交给用户选择的模型，解析有界 JSON 候选并进行 Host 端校验。
- `runtime.eval-harness.v1`：在受限只读 Capability Scope 内交替运行 Baseline
  与 Candidate，保存每个 Trial，使用确定性 evaluator 评分并生成配对报告。
- Evolution Lab：创建 Challenge、选择模型生成候选、配置测试用例、启动 A/B、
  查看成功率/Token/配对回归并显式晋升。

## 3. 关键不变量

1. Definition 内容参与 SHA-256 Digest，Generation 引用不可变 Digest。
2. 会话创建与 Generation 绑定在同一 SQLite 事务提交；旧会话不会因 Stable
   升级而漂移。
3. 普通对话可发现 Plugin Creator；评测运行不暴露 Creator，并过滤掉未声明
   为 `workspace-readonly` 的 Tool。
4. Baseline/Candidate 的运行顺序按 Case 与 repetition 交替，减少固定顺序偏差。
5. Candidate 只有在“无配对回归的能力增益”或“能力不降且 Token 至少下降
   15%”时才得到 `promote` 建议。
6. 晋升会原子地将同 scope 的旧 Stable 标记为 `superseded`；已绑定旧版本的
   会话继续使用旧版本。

## 4. HTTP API

| Method | Path | 作用 |
| --- | --- | --- |
| GET | `/api/v1/evolution/generations` | Generation 谱系 |
| POST | `/api/v1/evolution/generations/candidates` | 手工提交候选定义 |
| POST | `/api/v1/evolution/generations/{id}/promote` | 使用实验凭证晋升 |
| GET/POST | `/api/v1/evolution/challenges` | 查询/创建 Frontier |
| POST | `/api/v1/evolution/challenges/{id}/bootstrap` | 让模型生成候选 |
| GET/POST | `/api/v1/evolution/experiments` | 查询/启动 A/B |
| GET | `/api/v1/evolution/experiments/{id}` | 报告和 Trial 明细 |

## 5. 当前限制与下一阶段

当前是 M1 最小纵向切片，不应被描述为已经完成的递归 AGI 系统。下一阶段按
以下顺序推进：

1. Agent Definition IR：把固定的两种 Go loop 扩展为受 Schema 约束的
   Workflow/Branch/Verify/Delegate 图，并实现 Definition Diff。
2. Replay 与密封测试：记录 Tool Observation，支持确定性 Replay、Transfer
   Suite 和不可见 Holdout Case，避免自评过拟合。
3. 可恢复实验：实验队列、取消、预算、Host 重启续跑、Provider 限流与成本模型。
4. Frontier 自动捕获：从失败 Trace 建 Challenge，并把成功 Generation 路由回
   原 Task/Workspace，而不是只影响后续新会话。
5. 完整桌面客户端：Wails/WebView2 Shell、流式 Run Event、托盘与长期任务。
6. TODO（有统一任务集后）：比较 `react.v1`、Plan→ReAct、图式 Workflow 与
   其他 harness；在数据出来前不宣称某一推理架构更优。
