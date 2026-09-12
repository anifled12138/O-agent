# 08 · Memory

状态：Proposed

## 原则

Memory 不是把全部历史塞回 Prompt，也不是让模型自动改写长期人格。它保存可追踪、
可撤销、按作用域检索的信息；模型可以提出候选，Host 决定存储语义。

## 四类状态

1. Session State：当前会话事件，是事实来源，不叫长期记忆；
2. Run Memory：目标、约束、决定、待办和证据索引，Run 结束后归档；
3. Workspace Memory：项目约定、结构和经验证事实；
4. User Memory：用户明确允许保留的偏好。

## Memory Record

```go
type MemoryRecord struct {
    ID, UserID, Scope, ScopeKey string
    Kind, Content, Status       string
    SourceRefs                  []ArtifactRef
    Confidence                  float64
    ValidFrom, ExpiresAt        time.Time
    Supersedes                  []MemoryID
    CreatedBy                   Principal
}
```

永久记录必须有来源；推断和用户声明分开标记。旧记录不覆盖，使用 supersede/revoke，
保留审计链。

## 写入

- 工具产生的确定事实可按 policy 自动提交；
- 模型产生 `MemoryCandidate`，经过去重、敏感信息检查和 scope policy；
- 用户偏好默认要求明确确认；
- API Key、密码和原始 credential 永不进入 Memory；
- 临时任务细节默认只进入 Run Memory。

## 检索

首版采用结构化 filter + SQLite FTS，不先引入向量数据库。排序考虑 scope、关键词、
recency、confidence、expiry 和 source validity。只有选中的短片段进入 ContextPlan，
需要细节时读取 source artifact。

Semantic index 以后作为可替换 Memory Index Plugin；索引是 projection，可以重建，
不是事实来源。

## 失效

涉及外部状态的记录带验证时间和 TTL。工具发现冲突时追加新记录并 supersede；模型
不能只因为“感觉旧了”删除事实。用户可以查看、修正、撤销和禁用某个 scope。

## 测试

- scope 隔离；
- source/revoke/supersede；
- 敏感信息过滤；
- TTL 与重新验证；
- FTS 确定性；
- Context budget；
- Index Plugin 移除后仍保留原记录。

