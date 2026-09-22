# 上下文压缩调研与 Axiom 优化方案

更新日期：2026-09-22

## 结论

上下文压缩不是简单删除最早消息。可靠实现需要同时处理三层状态：

1. 持久化会话历史：保留原始记录，发送给模型时构造有界视图；
2. 单轮 Agent 工具轨迹：优先压缩旧工具输出，并保留最近的完整调用/结果对；
3. Provider 原生状态：若使用 OpenAI Responses 原生 compaction，必须原样传递 compaction item，不能把它解析成普通文本或只保留其中一部分。

当前 Axiom 的通用 Provider 抽象以 `ChatMessage` 为核心，适合先实现 Provider 无关的本地压缩。OpenAI 原生 compaction 需要进一步扩展类型系统以保存 opaque response items；仅在请求中加入 `compact_threshold`、但丢弃响应里的 compaction item，是错误实现。

## 官方机制

OpenAI 提供两种方式：

- 服务端自动压缩：在 Responses create 请求中设置 `context_management` 与 `compact_threshold`。超过阈值后，响应流会包含加密的 compaction item。
- 独立压缩：调用 `/responses/compact`，把返回的完整窗口作为下一次请求输入。官方明确要求不要自行裁剪该返回值。

对于 stateless input-array chaining，可以在收到新的 compaction item 后丢弃它之前的旧输入；使用 `previous_response_id` 时则不应手动裁剪。compaction item 是 opaque 数据，不应解析或依赖内部结构。

资料：

- [Compaction guide](https://developers.openai.com/api/docs/guides/compaction)
- [Compact conversation API](https://developers.openai.com/api/reference/java/resources/responses/methods/compact)
- [Responses create API](https://developers.openai.com/api/reference/cli/resources/responses/methods/create)

## 项目审计发现

优化前存在这些问题：

- `axiom_compact_context` 只返回成功文案，未改变下一次模型请求；
- `ContextManagerPlugin` 虽已存在，但执行链路只产生临时副本，压缩结果不会成为本轮后续步骤的 canonical transcript；
- 历史预算按字符估算，并用 `3.5 chars/token` 换算，中文会被严重低估；
- 省略历史的“摘要”逐条保留最多 600 字符，没有全局上限，消息越多摘要越大；
- 字节切片可能截断 UTF-8；工具输出只保留开头，容易丢失末尾错误、退出状态和总结；
- 文案宣称“100% 保持”，与有损压缩的事实不符；
- 上下文窗口由模型名猜测，尚未真正采用设置界面的显式 `contextWindow` 配置。

## 本轮实现

- 使用面向中英文混合文本的保守 token 估算；持久化消息约使用窗口的 55%，为工具定义、推理和输出预留空间；
- 旧历史摘要设置硬上限：保留初始用户目标和最近的被省略轮次，并明确标注为有损摘要；
- 超长文本和工具结果采用 UTF-8 安全的头尾保留；
- `axiom_compact_context` 成为可直接调用的基础工具，调用后在下一次模型请求前执行真实压缩；
- 压缩后的消息数组替换本轮 canonical transcript，后续步骤不再重复发送已丢弃内容；
- 产生包含压缩前后字符数、step 和是否主动触发的 `context.compacted` trace；
- 增加中文 UTF-8、摘要上限、主动压缩和 canonical transcript 的测试。

## 后续建议

1. 把 `contextWindow` 作为 Provider 的持久化字段，由用户配置覆盖模型名启发式；模型名猜测只作为 fallback。
2. 恢复并升级 OpenAI Responses adapter 后，为 response item 建立独立类型，完整保存 `compaction`、reasoning、function call/output 等 item，再接入服务端自动压缩。
3. 增加基于真实 tokenizer 或 Provider usage 的校准：记录估算 token 与实际 `prompt_tokens` 的误差，按模型族调整安全系数。
4. 做长会话回归评测：目标/约束召回率、工具调用正确率、压缩率、首 token 延迟、总 token 成本、context overflow 率。
5. 若要生成语义摘要，应由单独的低温度结构化请求完成并持久化版本与来源范围；失败时继续使用当前确定性的有界 fallback，不能阻塞主任务。
