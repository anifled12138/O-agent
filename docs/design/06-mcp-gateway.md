# 06 · MCP Gateway

状态：Proposed

## 边界

MCP 是连接外部 Tool、Resource 和 Prompt 的标准协议，不是 O 内部 Plugin ABI，
也不负责 Agent Loop、用户权限或 Plugin 安装。O 使用官方 Go SDK，不重写协议。

## 连接模型

```go
type MCPConnection struct {
    ID, UserID, Name string
    Transport        TransportConfig // stdio | streamable-http
    ProtocolPolicy   VersionPolicy
    CredentialRef    SecretRef
    GrantID          GrantID
    DesiredState     string
}
```

当前规范以每请求自包含的 2026-07-28 协议为首选；SDK 负责 2025-11-25、2025-06-18
等旧服务器兼容。Host 记录协商结果，不能把旧 session 语义硬编码到 Agent。

## 生命周期

- 本地 stdio：Supervisor 启动受限子进程，stderr 进入诊断流，stdout 只承载 MCP；
- Remote HTTP：Host 管理 OAuth/API credential、超时、重试、Origin/URL 校验；
- 连接按需启动，可设置 keep-warm；
- 健康失败只降级该连接，不拖垮 Agent Registry；
- Plugin Release 可以声明 MCP 连接模板，但用户 credential 和 Grant 独立保存。

## 能力映射

| MCP | O |
| --- | --- |
| Tool | discoverable Tool Surface |
| Resource | Resource handle/provider，不自动注入正文 |
| Prompt | Skill/prompt candidate，由用户或 Agent 显式加载 |
| InputRequiredResult | 原生用户输入/批准请求 |
| progress | Tool live progress |
| list TTL/cache scope | Capability index cache policy |

工具真实 ID 使用 connection UUID + server-local name；模型名称是本次请求内生成的短名，
避免多个 Server 的 `search` 冲突。

## 惰性发现

Host 可以缓存 Tool Card：ID、title、短描述、风险、server source、schema digest。模型
搜索命中后，Resolver 才读取或刷新完整 Schema。MCP Resource 默认返回 handle、摘要、
MIME 和大小，正文由 `resource.read` 按需获取。

## 安全

- Server 不读取完整会话，只得到本次调用所需参数；
- Server annotations 视为不可信，Host risk policy 优先；
- stdio process 不继承全部环境和 Provider secrets；
- Remote URL 固定 scheme/host，重定向重新校验；
- Sampling/roots/logging 等旧能力只做兼容，不成为新架构依赖；
- Tool 调用仍经过 O Policy、Approval、Schema 和审计。

## 实现

1. 引入并固定官方 `modelcontextprotocol/go-sdk` 版本；
2. 实现 Connection Store、credential reference 和 lifecycle；
3. 实现 stdio/HTTP adapter 与 compatibility fixtures；
4. 将 tools/resources/prompts 投影进各自 Registry；
5. 接 Capability Resolver 和 Function Dispatcher；
6. Client 提供连接、权限、健康和来源页面。

## 测试

官方 conformance + fake MCP server；覆盖协议版本、缓存、名称冲突、InputRequired、取消、
server crash、恶意 schema、超大内容和 remote auth。

