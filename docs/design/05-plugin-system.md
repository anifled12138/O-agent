# 05 · Plugin 系统

状态：Proposed；热切换细节以 ADR 0004 为准

## 目标

Plugin 是能力的构建、分发、权限和生命周期单位。它可以只有 Skill，也可以同时包含
后端、Agent Tool、MCP、Memory Provider 和前端页面。安装 Plugin 不代表模型必须
感知它。

## 四个平面

1. Package：源码、构建、Release、digest、签名和依赖锁；
2. Runtime：sidecar/WASM/UI、进程、健康、激活和排空；
3. Agent Capability：Tool、Skill、Context、Memory、Verifier、Driver；
4. Presentation：UI Slot、result renderer 和用户动作。

## Surface

```text
components: static | wasm | sidecar | ui
exports:
  agent: tool | skill | context | memory | verifier | driver
  host: service | hook | job | model_adapter
  integration: mcp_connection | mcp_server
  client: panel | renderer | settings | composer_action
```

每个 Surface 声明调用者、contract、executor、权限、生命周期和可见性。纯 UI、Service、
Hook、Job 默认不进入模型上下文。

## Release Manifest

Manifest 至少记录：

- spec/host API/SDK version；
- plugin identity、semantic version、description；
- artifact path + digest；
- exports/imports 和 versioned contracts；
- filesystem/network/secret/process/UI/event 权限；
- state schema/migration/rollback barrier；
- readiness/drain/health；
- conformance suite version。

解析器拒绝未知的安全相关字段。Manifest 规范化后参与 Release digest，Grant 同时绑定
digest、Surface set 和 permission set。

## 运行级别

- Static：无代码执行，适合 Skill/模板/Schema；
- WASM：适合纯计算和 Verifier，默认零系统能力，通过 Host import 使用 Broker；
- Sidecar：适合设备、SDK、长连接和复杂服务，独立进程；
- System Plugin：Driver/Model Adapter 等高影响扩展，仍尽量 sidecar 化，但需要更高
  审查和 Generation gate。

不使用 Go shared plugin；Windows 难以可靠卸载，也会把第三方代码放进 Host 地址空间。

## 生命周期

```text
source -> built -> verified -> awaiting_grant -> installed
       -> preparing -> ready -> active -> draining -> inactive
                                  \-> degraded -> rollback/recover
```

Install 只是期望状态；Activate 才发布 Surface。Coordinator 先私下准备所有组件，再以
一个 immutable routing snapshot 和 epoch 提交。调用取得 release-bound Lease；旧版本
只停止接收新 Lease，已有调用排空。

## 全栈一致性

UI、backend、Tool 和 Skill 属于同一 MountedRelease。UI instance 绑定 release/epoch，
不能在更新后根据 plugin ID 自动访问新 backend。新 UI 隐藏预载并完成 handshake，
与 backend 一起提交后再替换旧 UI。

## 依赖

Plugin 只依赖 versioned Service Contract，不依赖另一 Plugin 的源码路径。激活前解析
完整 release graph 并持久化 lock。Breaking contract 需要协调激活计划，不能按方法名
猜兼容。

## 数据

Host 提供按 user/plugin 隔离的 storage namespace。进程和 UI 都不是数据所有者。
迁移使用 expand/migrate/switch/contract；不可逆 migration 必须标记 rollback barrier。

## 自举

```text
Capability gap -> PluginSpec -> threat model -> isolated Git repo
 -> generate -> tests/conformance -> build immutable Release
 -> behavior/permission diff -> install grant -> canary -> activate
```

本地单用户产品不再暴露独立 approve 步骤。“安装并启用”是唯一显式授权边界：
用户执行安装即确认当前 Release 摘要绑定的权限契约。Agent 不能绕过安装边界或直接写活动路由。

## PluginSpec

生成代码前必须包含：目标、非目标、输入输出、Surface、Effect、权限理由、依赖、数据、
失败语义、测试、UI Slot 和升级策略。实现新增 Surface/权限/依赖时，既有安装授权失效，
必须针对新的 Release 再次执行“安装并启用”。

## Conformance

- Manifest/schema/digest；
- hello/describe/ready/drain/shutdown；
- request concurrency/cancel/deadline；
- Broker allow/deny 和路径/重定向逃逸；
- crash/restart/resource cleanup；
- UI CSP/bridge/origin；
- upgrade/rollback/migration；
- Agent Surface lazy visibility。

