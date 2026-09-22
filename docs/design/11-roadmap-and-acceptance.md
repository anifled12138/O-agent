# 11 · 实施路线与验收

状态：Accepted for sequencing

## 里程碑

### M1 · Provider 与 Function Calling 契约

- OpenAI-compatible、OpenAI Responses、Anthropic Messages、DeepSeek preset；
- 规范化 Message/Tool/Usage/Error；
- Provider capability profile 和用户 override；
- scripted fake provider 与 wire fixture tests。

验收：同一个 Agent Loop fixture 在至少三种 wire format 下得到相同的规范化 Tool Call
和最终回答；HTML/空响应/错误 JSON 不会泄漏成前端解析错误。

### M2 · Event-sourced Agent Loop

- Agent/ReasoningDriver/Inbox；
- Turn/Step/Attempt durable events；
- 流式 live event；
- cancel、retry、resume 和 deterministic replay。

验收：Host 在任意一个 Step 后退出，重启可判断继续、重试或等待用户，而不是重复
已知外部副作用。

### M3 · 基础能力

- 内置文件、进程、HTTP Function；
- Skill metadata/body lazy load；
- MCP Gateway；
- Run/Workspace/User Memory 基础实现。

验收：模型初始上下文不含完整目录；搜索并加载后才能调用精确能力，所有来源可追踪。

### M4 · Plugin Runtime V3

- framed sidecar RPC、并发与取消；
- immutable routing snapshot 和 Surface Lease；
- activation journal、drain、rollback；
- Broker 和 Windows containment。

验收：在 backend/UI/tool 调用中间升级，永远不发生 release 混用；Host 在每个事务
阶段崩溃都能收敛。

### M5 · 独立客户端

- 开发 Web UI；
- Electron Main/Preload/Renderer；
- local socket、事件续传、设置、会话和 Tool 展示；
- Plugin UI Slot 与 release-bound bridge。

验收：关闭窗口不终止授权的长期 Run；重新打开从 cursor 恢复；Renderer 无 Node
权限，Plugin iframe 无宿主权限。

### M6 · Agent 自举 Plugin

- PluginSpec、独立 Git workspace、生成、构建、conformance；
- 权限/Surface diff、安装时 Grant、Canary、activate；
- Skill/MCP/UI/backend/Tool 组合模板。

验收：用户只描述能力，Agent 能准备可安装 Release；“安装并启用”明确展示并授予
当前 Release 的权限，失败 Release 不影响当前版本。

### M7 · Generation Harness

- Driver/Context/Prompt 的候选 Generation；
- replay、paired A/B、canary 和反馈；
- promotion/rollback barrier。

## 端到端完成定义

产品达到第一版完整状态必须同时满足：

1. 外部 LLM API 可配置、测试和切换；
2. Agent Loop 有流式、Tool、取消、恢复和事件重放；
3. MCP、Skill、Memory 和 Function Calling 均通过统一 Capability 边界工作；
4. Web UI 和独立桌面客户端共享同一 Host API；
5. 全栈 Plugin 可以生成、构建、授权、安装、热更新和回滚；
6. Secret 不进入前端、普通日志或 Plugin 环境；
7. 每次模型调用、上下文注入和真实副作用都有来源和版本；
8. 所有核心路径具备确定性测试，不依赖付费 API 才能通过 CI。

## 当前执行位置

M1 的四种首批协议适配和 Function Calling 归一化已实现；model profile override、
streaming adapter 和完整 capability probe 仍待完成。M2 已实现 durable Turn/Step/Attempt
journal、异步 Receipt、可重连 SSE、取消和保守重启分类；下一项是 Provider delta
streaming、Inbox 和恢复执行策略。
