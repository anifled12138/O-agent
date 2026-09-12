# 04 · Function Calling 与工具执行

状态：Proposed

## 内部契约

Function Calling 是 Model Gateway 与 Capability Dispatcher 之间的标准边界，不等于
某个供应商的 `tools` JSON。

```go
type ToolDefinition struct {
    ID           CapabilityID
    ModelName    string
    Description  string
    InputSchema  json.RawMessage
    OutputSchema json.RawMessage
    Effect       EffectClass
    Execution    ExecutionPolicy
    Provenance   CapabilityProvenance
}
```

`ModelName` 是本次请求内合法且唯一的短名称；真实 Plugin/MCP/release identity 保存在
Host binding，模型不能通过参数切换目标版本。

## 调用管线

```text
normalized ToolCall
 -> resolve frozen binding
 -> parse JSON exactly once
 -> JSON Schema validation/default rejection
 -> policy and principal check
 -> approval if needed
 -> acquire resource locks
 -> invoke with deadline/idempotency key
 -> validate output schema
 -> store large/multimodal result as Artifact
 -> append ToolResult event
```

参数非法时返回结构化 Tool Error 给模型，包含可修复字段路径，但不执行 Tool。

## Effect 和重试

- `read_only`：可并行、可按策略重试；
- `idempotent_write`：必须有 idempotency key，允许安全重试；
- `reversible_write`：执行前记录补偿/快照，但回滚不是自动承诺；
- `external_side_effect`：默认要求批准，超时后状态可能 unknown；
- `irreversible`：逐次用户确认，禁止后台自动重试。

Tool 自己声明的 Effect 只是请求；Host/Plugin manifest 的已审核定义才是执行依据。

## 并发

同一模型响应中的 Tool Call 只有在以下条件全部满足时并行：

- Model Profile 支持并行调用语义；
- Tool policy 允许；
- 资源锁不冲突；
- 没有需要先完成的批准或依赖；
- 结果之间没有显式顺序关系。

执行完成事件可按真实时间发出，但写回模型的 Tool Result 保持原 call 顺序。

## 结果

结果支持 text、structured JSON、image/audio 和 Artifact reference。超过上下文预算的
内容不直接塞回模型，返回摘要、MIME、大小、digest 和可按需读取的 handle。

Tool Error 分为 `invalid_input/denied/not_found/timeout/cancelled/transient/failed/unknown`
并标记 `retryable`。异常不能伪装成普通成功文本。

## 基础内置 Function

V1 至少提供：

- `capability.search/load`；
- `skill.search/load`；
- brokered `fs.read/list/write`；
- brokered `process.run`；
- brokered `http.fetch`；
- `artifact.read`；
- `user.request_input`；
- Plugin Creator preset 下的 propose/build/request-install。

写入、进程和网络能力按 workspace/host/command 精确授权，不能以一个全局布尔值开放。

## 测试

- Schema fuzz 与超大参数；
- call name 冲突和伪造 release ID；
- 并行完成顺序；
- 取消和 deadline；
- idempotency replay；
- output schema 失败；
- 大结果 artifact 化；
- Plugin 更新期间旧 binding 固定。

