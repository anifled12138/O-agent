# Axiom 设计文档索引

状态：Active Design  
更新时间：2026-09-12

本目录把总体架构拆成可以实现和验收的独立部分。每个文档都必须同时回答：边界
是什么、为什么这样选、接口如何落地、失败时怎么办、怎样测试。总体方向见
[`../architecture-v0.2-discussion.md`](../architecture-v0.2-discussion.md)。

## 文档

1. [`00-system-overview.md`](00-system-overview.md)：系统边界、进程和核心数据流。
2. [`01-codebase-and-delivery.md`](01-codebase-and-delivery.md)：代码目录、依赖规则、Git 和发布。
3. [`02-agent-loop.md`](02-agent-loop.md)：Agent、Turn、Step、事件、取消和恢复。
4. [`03-model-provider-compatibility.md`](03-model-provider-compatibility.md)：不同 LLM API、能力协商和降级。
5. [`04-function-calling.md`](04-function-calling.md)：Tool Schema、调用归一化、并发、重试和结果。
6. [`05-plugin-system.md`](05-plugin-system.md)：Package、Surface、权限、隔离、自举和热拔插。
7. [`06-mcp-gateway.md`](06-mcp-gateway.md)：MCP Client、连接生命周期和内部映射。
8. [`07-skills.md`](07-skills.md)：Skill 发现、惰性加载、资源和安全。
9. [`08-memory.md`](08-memory.md)：运行记忆、工作区事实、用户记忆和检索。
10. [`09-client-and-local-api.md`](09-client-and-local-api.md)：独立客户端、Web UI、流式协议和错误模型。
11. [`10-storage-security-observability.md`](10-storage-security-observability.md)：存储、Secret、审计和可观测性。
12. [`11-roadmap-and-acceptance.md`](11-roadmap-and-acceptance.md)：实施顺序和端到端验收。

## 决策状态

- `Accepted`：可以直接成为实现依据；修改需要 ADR。
- `Proposed`：已有推荐方案，可以实现不影响外部契约的部分。
- `Exploratory`：只允许原型或测试，不进入默认运行路径。

实现提交必须在说明中引用对应文档或 ADR。文档与代码不一致时，先修正文档状态，
不能让实现悄悄成为架构决定。

