# 09 · 客户端与本地 API

状态：Proposed

## 两种客户端形态

- 开发 Web UI：React/Vite，通过受限 loopback HTTP 访问 Host，便于调试；
- 正式独立客户端：Electron Main 连接 local socket，Renderer 只通过 Preload bridge。

两者共享 OpenAPI 和事件协议，不复制业务逻辑。正式客户端可以在没有开发服务器和
默认浏览器的情况下运行。

## 客户端功能

- 单用户本地工作区，启动后直接进入对话界面；
- Provider 协议、模型、能力测试和费用提示；
- 多会话、分支、取消、恢复；
- 流式消息、reasoning summary、Tool 调用和用户输入卡；
- 文件/Artifact/Diff/Terminal 面板；
- MCP、Skill、Memory、Plugin 管理；
- Plugin Grant、升级、健康和回滚；
- Run trace、诊断和成本；
- 系统托盘、通知和后台任务。

## API

命令使用普通 request/response；运行事件使用带 cursor 的 stream：

```text
POST /v1/agents/{id}/inputs       -> 202 Receipt
POST /v1/agents/{id}/cancel
GET  /v1/sessions/{id}/events?after=cursor
GET  /v1/agents/{id}/live?after=cursor
```

开发 Web 使用 SSE；Desktop Main 可将同一事件转发到 Renderer。断线恢复先读取 durable
event，再接 live stream。发送输入不等待整个 Agent Turn 完成。

## 错误 Envelope

```json
{
  "error": {
    "code": "provider.authentication_failed",
    "message": "The provider rejected this API key.",
    "retryable": false,
    "diagnosticId": "diag_...",
    "details": {}
  }
}
```

API 的所有成功和错误响应都声明 Content-Type。反向代理 HTML、空 body 和非 JSON
响应在 Go Provider 层转为安全错误，前端 `request()` 先检查 status/content-type，
不能直接无条件 `response.json()`。

## Desktop 安全

- Renderer：`nodeIntegration=false`、`contextIsolation=true`、sandbox；
- Preload 逐方法暴露 API，不暴露 `ipcRenderer.send`；
- Main 验证 channel、参数、窗口 origin 和本地进程身份；
- local socket 使用当前 OS 用户 ACL；
- 自定义 Plugin scheme 不映射任意本机路径；
- 浏览器 permission 默认拒绝；外链交给系统浏览器并检查 scheme。

## Plugin UI

Plugin UI 运行在独立 origin 的 sandboxed iframe。Host 创建 release-bound UIInstance 和
MessagePort，允许的方法来自 Manifest + Grant。UI 不能直接访问 local API、Cookie、
Node、文件系统或其他 Plugin iframe。

## 前端状态

服务端 durable state 以 API snapshot/event 为准；React 只保留选择、输入框、布局等
临时状态。不要在多个组件复制 Agent/Plugin 状态机。事件 reducer 必须支持重复 cursor
和重连重放。

## 测试

- API schema/错误 content-type；
- SSE 断线续传与重复 event；
- 未携带 Cookie 时所有本地 API 仍可正常工作；
- Renderer/Preload 权限；
- Plugin iframe origin 和 MessagePort 伪造；
- Host/客户端分别崩溃重启；
- Playwright 完整对话与 Tool 卡流程。

## 当前实现（2026-09-12）

Web UI 使用 V2 async Turn Receipt；durable event 通过 SSE sequence cursor 接续，Stop 先
持久化取消意图再触发执行上下文。当前流中已经包含 Turn、Model、Tool 和终态事件，
打开已有活动 Turn 时客户端会自动重新订阅；切换会话只关闭旧 EventSource，不取消
后台执行。Provider token delta 仍待实现。V1 同步接口暂时保留用于兼容，不作为客户端
默认路径。产品使用单用户本地工作区，不提供注册、登录、登出或 Cookie session；
数据库中的 `user_id` 暂作为兼容性工作区作用域键，启动时绑定已有数据所有者。
