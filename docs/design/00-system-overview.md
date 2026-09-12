# 00 · 系统总览

状态：Proposed

## 目标

O 是本地运行、接入外部 LLM API 的完整 Agent 产品。用户通过客户端对话提出
目标；Agent 可以直接推理，也可以使用 Function Calling、MCP、Skill、Memory 和
Plugin 完成任务。缺失的稳定能力可以由 Agent 生成 Plugin，但安装权限仍属于用户。

## 进程

```text
Web UI / Desktop Renderer
          │ typed local API
Desktop Main（正式客户端）
          │ named pipe / Unix socket
axiomd（Go Host）
   ├─ LLM provider HTTPS
   ├─ plugin sidecar pipes
   ├─ MCP stdio / HTTP
   └─ SQLite + Artifact Store
```

开发期允许 Web UI 通过 `127.0.0.1` 访问 Host。正式桌面客户端使用本地 socket，
不要求用户启动浏览器，也不把 Host 暴露到局域网。

## 可信边界

可信内核负责 Workspace Scope、Policy、Approval、Event Store、Artifact Store、Plugin
activation、Resource Broker 和 Process Supervisor。它不决定 LLM 的具体推理内容，
但任何真实副作用都必须从内核获得 capability handle。

Agent Runtime 可以替换 Reasoning Driver、Context Policy、Provider route、Verifier
和默认能力集合。普通 Plugin 可以提供能力，但不能替换授权和审计边界。

## 一次用户请求

```text
1. Client 提交 input，并获得 receipt。
2. Agent Inbox 接纳输入并开启 Turn。
3. Context Compiler 从 Session Event 投影模型输入。
4. Provider Adapter 生成供应商请求并流式归一化响应。
5. 没有 Tool Call：提交回答并结束 Turn。
6. 有 Tool Call：Schema 校验 -> Policy -> Approval -> Execute -> Result。
7. Tool Result 写入 Session，进入下一 Step。
8. 所有 durable event 推送给 Client；断线后可从 cursor 续传。
```

## 不做的事情

- 不部署或管理本地模型；本地模型若提供兼容 API，只被视为一个 Provider。
- 不让 Plugin patch O 核心源码完成安装。
- 不把所有插件说明、Skill 正文或 MCP Schema 默认塞进上下文。
- 不用模型自然语言声明替代真实执行结果。
- 不把模块化单体拆成一组本机微服务。

## 首个可交付产品闭环

打开本地客户端 -> 配置模型 -> 新建会话 -> 流式对话 -> 模型调用内置 Function -> 查看
Tool 过程 -> 安装/启用一个 Skill 或 MCP -> Agent 使用该能力 -> 重启后恢复会话。
