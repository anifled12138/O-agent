# 10 · 存储、安全与可观测性

状态：Proposed

## SQLite

SQLite 使用 WAL、foreign keys 和显式事务。主要数据域：

- identity/session；
- provider config 和 secret reference；
- agent/session event/inbox；
- artifact metadata；
- plugin project/release/grant/installation/activation；
- capability projection；
- MCP/Skill/Memory；
- eval/generation；
- audit/diagnostic。

Event 表 append-only；Capability index、会话列表和搜索索引是 projection，可重建。
Migration 使用单调版本、事务和启动前备份策略，已应用 migration 不修改。

## Artifact Store

大 Tool 结果、模型原始诊断、Plugin package、Diff、图片和快照写内容寻址仓库：

```text
artifacts/sha256/ab/cd/<digest>
```

Metadata 记录 owner、MIME、size、source、retention 和 encryption。写入使用临时文件、
fsync、digest 校验和原子 rename；数据库只在 Artifact 安全落盘后引用。

## Secret

- 生产使用 OS credential vault；Windows 首先 DPAPI/Credential Manager；
- SQLite 只保存 opaque secret reference；
- Provider 请求由 Host 注入 credential；
- Plugin 优先获得“使用 Secret 的能力”，而不是 Secret 明文；
- 日志、event、trace、HTTP error 和 crash report 统一 redaction；
- Secret 更新不改变 Provider ID，但产生新的 credential version。

现有 AES-GCM 本地 Vault 作为迁移兼容，不作为最终跨用户密钥管理方案。

## Principal 与 Grant

所有调用带 `host/user/ui/agent/job/plugin/mcp` Principal。Grant 精确绑定 user、release、
surface、resource scope 和 permission digest。Policy 默认拒绝；Plugin 只能请求，不能
自行扩权。

## 审计

必须记录：登录/失败、Grant、Plugin activation、Secret use（不含值）、外部副作用、
模型 route、Tool source、Memory commit/revoke 和系统策略变化。普通 Token delta 不进入
审计库。

Audit event 使用稳定 code 和结构化字段，文本只用于展示。诊断 ID 将用户可见错误与
本地详细日志关联。

## Telemetry

默认仅本地：

- model latency/token/cache/cost；
- tool queue/run/result size；
- plugin health/crash/restart；
- context segment/token；
- event projection lag；
- client reconnect。

任何远程 telemetry 必须 opt-in，并通过独立 sink Plugin；发送前删除 prompt、Tool
内容、路径、Secret 和用户标识。

## 备份和恢复

- SQLite online backup + Artifact manifest；
- Release Store 不可变，可单独校验；
- 恢复后重建 projection，不相信持久化的“进程正在运行”；
- Plugin activation journal 按 desired/observed state reconcile；
- unknown external side effect 显示给用户，不自动假设失败并重放。

## 测试

- migration from every released schema；
- WAL/crash/partial artifact write；
- user scope isolation；
- redaction golden tests；
- corrupt release/artifact；
- activation recovery；
- backup/restore/rebuild projection。

