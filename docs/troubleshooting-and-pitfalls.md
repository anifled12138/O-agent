# 工程踩坑与实战解决记录 (Engineering Pitfalls & Solutions)

本文档系统记录了在构建单体桌面 Agent 系统 `O` 过程中，关于 **大模型接入层改造（New-API）**、**Agent 主循环与步数配额限制（MaxSteps）**、**长任务软着陆机制（Soft Landing）** 以及 **桌面客户端打包构建（Electron Packager）** 中遇到的真实技术踩坑点、底层根因分析与最终解决方案。

---

## 目录
1. [踩坑一：大模型接入层重构与架构选型（New-API 方案 A vs 方案 B）](#1-踩坑一大模型接入层重构与架构选型new-api-方案-a-vs-方案-b)
2. [踩坑二：Agent 主循环步数配额超限（Step budget exhausted / 12 步锁死）](#2-踩坑二agent-主循环步数配额超限step-budget-exhausted--12-步锁死)
3. [踩坑三：软着陆总结失败导致生硬错误暴露（Role 'system' 序列非法）](#3-踩坑三软着陆总结失败导致生硬错误暴露role-system-序列非法)
4. [踩坑四：大模型长程工程任务下的注意力分散与步数膨胀（80 步仍然超限）](#4-踩坑四大模型长程工程任务下的注意力分散与步数膨胀80-步仍然超限)
5. [踩坑五：桌面客户端打包在受限环境下的缓存与临时目录拒绝访问](#5-踩坑五桌面客户端打包在受限环境下的缓存与临时目录拒绝访问)

---

## 1. 踩坑一：大模型接入层重构与架构选型（New-API 方案 A vs 方案 B）

### 1.1 问题背景
系统原先在 `backend/internal/provider` 维护了多个非标适配器（如 `anthropic.go`, `openai_responses.go`, `openai_chat.go`），由于各厂商在流式 SSE、Tool Calls 协议和鉴权机制上差异极大，自行手写适配脆弱且难以跟进社区生态更新。团队希望引入开源成熟的 `New-API` 进行彻底改造。

### 1.2 架构权衡对比
* **方案 A（独立网关中继）**：
  * **机制**：外部独立运行 New-API 进程（作为代理网关），系统内部仅保留标准 OpenAI 客户端。
  * **弊端**：违背了 `O` 作为**单桌面免安装便携软件**的定位，用户需要单独部署并配置 New-API Web 控制台，使用门槛高。
* **方案 B（内嵌轻量转换器 - 最终采纳）**：
  * **机制**：吸纳 New-API 的核心架构思想，将请求/响应归一化模型和思考流解析内嵌到 Go 进程中。
  * **优势**：保持单二进制零外部环境依赖分发，同时抹平厂商差异。

### 1.3 核心改造与落地
1. **协议规范统一**：在 `contracts.go` 中注册 `KindNewAPI` (`new-api`) 与 `KindOneAPI` (`one-api`)，设为默认首选提供商。
2. **支持 DeepSeek / 思考模型**：在 `openai_chat.go` 与 `stream.go` 中增加对 `reasoning_content` 和 `reasoning` 字段的解析与流式事件透传，当主回复为空时自动平滑回退，杜绝“空回复”崩溃。
3. **免密钥鉴权支持**：优化 Bearer 鉴权，当对接本地无鉴权 Ollama/vLLM 时自动省略 Authorization 请求头。

---

## 2. 踩坑二：Agent 主循环步数配额超限（Step budget exhausted / 12 步锁死）

### 2.1 现象与报错
运行稍复杂的编码任务时，Agent 频繁中断报错：
```text
Step budget exhausted before the agent loop reached a natural stopping point.
```
即便在代码中将默认步数修改为 80 步，重新运行后依然报错：
```text
Step budget reached maximum limit (12 steps)...
```

### 2.2 根本原因深度分析
* **硬限制默认值过小**：原先代码写死默认值为 `12` 步，而真实软件工程任务（搜索、读文件、改代码、测试）通常需要几十步交互。
* **SQLite 历史种子持久化锁死**：系统首次启动时，会将初始种子规范 `Axiom Seed`（包含 `maxSteps: 12`）持久化写入 SQLite 数据库（`data/axiom.db` 中的 `agent_definitions` 表）。代码运行时优先读取数据库中绑定的定义，而旧代码仅在 `maxSteps < 1` 时才应用默认值，导致数据库内的历史值 12 一直生效。
* **后端参数校验硬编码截断**：`evolution/service.go` 中校验规则硬编码了 `spec.MaxSteps > 64` 则拒绝保存，导致上层无法调大步数。

### 2.3 解决方案
1. **自动平滑升级旧数据**：在 `backend/internal/agent/loop.go` 中添加兼容升级逻辑，当检测到 `maxSteps < 1 || maxSteps == 12 || maxSteps == 80` 时，自动升级至最新标准步数。
2. **放宽校验区间**：将 `evolution/service.go` 的合法性校验上限从 `64` 扩充放宽至 `500`。
3. **数据库订正**：直接同步更新 SQLite 数据库内已持久化的 `agent_definitions` 记录。

---

## 3. 踩坑三：软着陆总结失败导致生硬错误暴露（Role 'system' 序列非法）

### 3.1 现象
为解决步数超限直接硬报错的问题，我们在循环结尾引入了软着陆机制（Soft Landing）：当步数达到最大配额时，请求大模型基于现有上下文进行总结。但实际运行后，依然抛出了生硬的错误提示：
```text
Step budget reached maximum limit (80 steps). Completed observations are preserved; please continue or refine your request.
```
软着陆代码似乎完全没有生效。

### 3.2 根本原因
软着陆初版实现如下：
```go
summaryMessages := append(messages, provider.ChatMessage{
    Role:    "system", // <--- 致命错误！
    Content: "Your step budget has been reached. Do not call any tools. Summarize what you have completed...",
})
```
**协议规范约束**：在标准 OpenAI Chat Completions、New-API 及 Anthropic 协议规范中，消息序列如果以 `role: "tool"` 结尾，下一条合法消息的角色**只能是 `user` 或 `assistant`，绝不能在末尾插入 `role: "system"`**。
各大 LLM 网关与提供商会直接抛出 `400 Bad Request: Invalid message role sequence`。该错误导致模型的总结请求失败，系统只得被迫降级返回原有的纯文本超限通知。

### 3.3 解决方案
将注入的角色更改为合规的 `user` 角色，并辅以系统通知前缀：
```go
summaryMessages := append(messages, provider.ChatMessage{
    Role:    "user",
    Content: "[System Notice] Your step budget has been reached. Do not call any tools. Summarize what you have completed so far, any findings or obstacles, and provide your final response to the user with the information gathered.",
})
```
修正后，无论底层接哪家模型（Claude/OpenAI/DeepSeek），都能合规返回高质量的阶段性总结。

---

## 4. 踩坑四：大模型长程工程任务下的注意力分散与步数膨胀（80 步仍然超限）

### 4.1 现场还原
用户输入了全仓代码重构任务（*“开始改造吧，然后mcp使用成熟的开源方案...用git进行保存”*）。
数据库跟踪记录显示：`gemini-3.8-flash-high` 在单个轮次内真实执行了 **80 次工具调用**（57 次 `exec_command`，15 次 `fs_read`，7 次 `grep_search`，1 次 `fs_list`）。
每一步都在真实执行命令和探查，但因为工程任务本身庞大，80 步仍然没能走到终点。

### 4.2 业内成熟实践对比（Codex / Claude Code）
* **Codex**：单次任务通常允许 100~200 步，同时具备滑动窗口上下文折叠（Context Compaction），且模型在完成阶段目标时有明确的“主动汇报机制（Proactive Yield）”。
* **Claude Code**：长任务不设置机械硬限制，而是采取步数提醒与用户中断确认，保持任务上下文持续推进。

### 4.3 解决方案
1. **步数预算提升**：默认步数正式扩充到 **`150 步`**，上限放宽到 **`500 步`**，以应对复杂架构改造。
2. **系统提示词注入终止准则**：在 `DefaultSystemPrompt` 中严格要求模型：*“一旦收集到足够信息或达成阶段目标，立即停止调用工具，直接给出清晰结构化的答复；禁止做多余的验证性探查”*。
3. **保留现场支持无缝接力**：步数用尽时自动输出详尽进度与后续待办，用户回复“继续”即可在同一个会话中继续推进，不再丢失进度。

---

## 5. 踩坑五：桌面客户端打包在受限环境下的缓存与临时目录拒绝访问

### 5.1 现象
在执行 `npm --prefix desktop run package` 构建桌面客户端时发生崩溃：
```text
Error: EPERM: operation not permitted, stat 'C:\Users\lx'
```
或者在 Go 构建时抛出：
```text
Access is denied. (open C:\Users\lx\AppData\Local\go-build\...-a)
```

### 5.2 原因分析
* 操作系统环境或开发沙箱对跨盘符访问（尤其是跨到 `C:\Users\<user>` 目录下的用户文件夹）进行了读写拦截。
* `@electron/packager` 默认将下载的 Electron 模板解压到 `os.tmpdir()`（即 `C:\Users\<user>\AppData\Local\Temp`），并且 Electron 下载缓存默认存放在用户目录。
* Go 构建工具链默认使用 `%LOCALAPPDATA%\go-build` 作为构建缓存。

### 5.3 解决方案
在打包脚本 `desktop/scripts/package.mjs` 和构建脚本中将所有临时目录、缓存目录**内敛到当前工程工作区（Workspace）**中：
1. **Electron 打包重定向**：
   ```javascript
   const appPaths = await packager({
     dir: desktopDir,
     out: outputRoot,
     tmpdir: path.join(repositoryRoot, '.tmp', 'packager'),
     download: {
       cache: path.join(repositoryRoot, '.cache', 'electron'),
       unsafelyDisableChecksums: true,
     },
     ...
   });
   ```
2. **Go 构建重定向**：
   传递环境变量：`GOCACHE=D:\agent-harness\.gocache`、`GOPATH=D:\agent-harness\.gopath`、`CGO_ENABLED=0`。
3. 改造后整个编译构建与 Electron 打包完全局限在工作区内部，实现 100% 独立可复现打包。
