# 01 · 代码框架与交付

状态：Proposed

## 仓库结构

核心产品使用一个 Git monorepo；Agent 生成的 Plugin Project 使用 D 盘独立 Git
仓库，避免嵌套仓库和主产品历史污染。

```text
backend/cmd/axiomd          daemon composition root
backend/internal/kernel     不可替换的可信能力
backend/internal/agent      Agent runtime 与 contracts
backend/internal/plugin     package/runtime/activation
backend/internal/integration provider/mcp/skill/memory
backend/internal/product    conversation/settings/eval/forge
backend/internal/transport  local API/plugin RPC
backend/migrations          只前向追加的 schema
desktop/main                Electron privileged main
desktop/preload             narrow context bridge
desktop/renderer            React UI
sdk/plugin-go               外部 Go Plugin SDK
sdk/plugin-ts               外部 TS Plugin/UI SDK
api                         OpenAPI 与 JSON Schema
plugins/builtin             内置 Surface 实现
docs/design                 可执行设计
docs/adr                    已冻结决策
```

## 依赖规则

- `cmd` 只负责组装，不保存领域逻辑。
- Adapter 依赖使用方定义的接口，Agent 不依赖 HTTP、SQLite 或 Plugin Forge。
- `internal` 类型不得出现在 Plugin SDK。
- Kernel 不导入 Product UI、Forge 或具体 Reasoning Driver。
- Provider Adapter 不导入 Agent Service，只处理规范化模型请求/响应。
- MCP、Skill、Memory 都通过 Capability/Context contract 接入，不直接改 Loop。

## 版本与构建

- Go 使用一个主 module；Plugin Go SDK 稳定后拆为独立 module。
- JavaScript 使用 npm workspaces、exact dependency 和提交的 lockfile。
- OpenAPI、Event Schema、Plugin Manifest Schema 的生成物提交并在 CI 检查漂移。
- Windows 首先输出 `axiomd.exe` 和独立客户端安装包。
- 开发源码、构建缓存、Plugin workspace 默认位于 D 盘。

## Git 规则

- 一个提交只完成一个可验收变化；不混入无关格式化。
- 数据迁移、依赖升级、生成文件更新分别提交。
- 禁止重写已发布 Release；Release 由 digest 寻址。
- Plugin Forge 每次生成/修改形成独立 commit，构建报告记录 source commit。
- 主干始终应可构建；实验性 Driver 通过 feature flag/Generation 隔离。

## 基础门禁

```text
go test ./...
go vet ./...
go test -race ./...          # CI/Linux 和可支持的平台
npm run typecheck
npm run lint
npm run build
contract/conformance tests
git diff --check
```

依赖更新使用独立审查，生产构建禁用未审核的 npm lifecycle script。构建输出记录
Go/Node 版本、lockfile digest、Git commit 和目标平台，保证可复现诊断。

