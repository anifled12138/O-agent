# 03 · 模型供应商兼容

状态：Proposed，M1 implementation target

## 问题

“OpenAI-compatible”只表示大致相似，不表示请求、Tool Calling、reasoning、stream、
usage 或错误完全一致。仅通过 Base URL 拼接 `/chat/completions` 会在真实工程中失败。

## 两层适配

### Provider Protocol Adapter

负责 wire format：

- OpenAI Chat Completions；
- OpenAI Responses；
- Anthropic Messages；
- DeepSeek Chat preset；
- Generic OpenAI-compatible；
- 后续 Google Gemini。

### Model Profile

负责具体模型能力：

```go
type ModelCapabilities struct {
    Tools                 SupportLevel
    ParallelToolCalls     SupportLevel
    Streaming             SupportLevel
    Vision                 SupportLevel
    StrictJSONSchema      SupportLevel
    ReasoningControl      SupportLevel
    PromptCaching         SupportLevel
    MaxInputTokens        int
    MaxOutputTokens       int
}
```

`SupportLevel` 使用 `yes/no/unknown`，不把未探测到误判为不支持。能力来源按优先级：

1. 用户显式 override；
2. 成功的 capability probe；
3. 内置、带版本的 model catalog；
4. adapter 保守默认值。

每次 Run 记录实际 profile snapshot，catalog 更新不能改变已运行记录的解释。

## 规范化请求

内部 `ModelRequest` 表达 system/user/assistant/tool result、多模态 content、Tool Schema、
输出预算和 reasoning preference。Adapter 只能降级自己声明支持的字段：

- 不支持独立 system role：按明确策略合并到首条 user；
- 不支持 parallel tools：Dispatcher 串行；
- 不支持 strict schema：仍由 Host 对参数和结果二次校验；
- 不支持 tools：该模型不能运行要求 Function Calling 的 Driver，不能用文本正则模拟；
- 不支持某种 image/audio：在调用前返回可解释的 capability error。

## 规范化响应流

所有 Adapter 输出同一事件：

```text
response.started
content.delta
reasoning.delta           # 仅供应商明确提供时
tool_call.started
tool_call.arguments.delta
tool_call.completed
usage.updated
response.completed | response.failed
```

供应商隐藏的 chain-of-thought 不请求、不持久化。可见 reasoning summary 作为独立内容
类型处理，不能混入最终回答。

## Tool Calling 差异

- OpenAI Chat：`message.tool_calls[].function.arguments` 通常是 JSON 字符串；
- OpenAI Responses：`output` 中的 `function_call` 和 `function_call_output`；
- Anthropic：content block 中的 `tool_use`，结果以 `tool_result` 返回；
- 部分兼容服务返回 object arguments、空 call ID 或 legacy `function_call`。

Adapter 可做有界兼容：object arguments 规范化为 JSON、空 ID 生成本 Attempt 内 ID、
legacy 单调用映射到 Tool Call。任何修复都产生 compatibility warning event。

## 错误模型

```go
type ProviderError struct {
    Class      ErrorClass // auth, rate_limit, unavailable, invalid_request,
                          // context_overflow, unsupported, protocol, cancelled
    StatusCode int
    RetryAfter time.Duration
    SafeDetail string
    Cause      error
}
```

UI 只展示 `SafeDetail` 和诊断 ID。HTML、代理登录页和整个响应体不作为 JSON 返回；
原始响应最多受限保存到加密诊断 Artifact。

## 连接测试

连接测试分三档：

1. Transport：认证与 models/discover endpoint；
2. Minimal inference：最小无 Tool 请求，可能产生费用，需用户触发；
3. Capability probe：最小 Tool、stream、JSON schema 测试，结果按 model/profile 缓存。

不能因为 `/models` 不存在就判定 Anthropic 或私有兼容端点不可用；每个 Adapter 定义
自己的测试策略。

## 首批实现

- 拆分现有 `provider.Service` 与 wire adapter；
- 支持四个 kind preset；
- 新增 Provider kind metadata API；
- 前端允许选择协议，自动给出默认 Base URL；
- fixture 覆盖 content 为 null、arguments 为 object、无 choices、HTML、429 和工具调用。

