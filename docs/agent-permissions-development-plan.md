# Agent 会话权限与审批开发任务

> **策略已更新：** 本文后续阶段清单是早期方案草稿，不再作为权限行为的依据；尤其是“未知/MCP/插件逐个询问”和“完全自动仍拒绝未分类操作”已被用户明确否定。当前行为以本段为准：
>
> | 会话档位 | 运行规则 |
> | --- | --- |
> | 只读 | 所有工具统一按实际影响判断，只允许读取与会话内临时操作；拒绝变更。 |
> | 工作区自动 | 已启用的 MCP、插件工具可直接调用；常规操作不逐项询问。删除/覆盖数据，以及构建/安装插件代码前确认。联网读取不询问。 |
> | 完全自动 | 不进行逐项权限拦截，包括未知影响的工具调用。执行沙箱仍独立生效。 |
>
> `ask_on_sensitive` 是旧会话的兼容值，按“工作区自动”处理。启用 MCP/插件决定工具是否可用，不按工具来源额外设一套权限。当前 `exec_command` 和 `exec_script` 的 Windows AppContainer 工作区只读、无网络限制仍适用；启用的 MCP 服务本身不由该 AppContainer 隔离。

## 当前进度

- [x] 将调研结果拆成阶段任务并记录本文件。
- [x] 会话权限等级的 domain 类型、SQLite 持久字段和更新 API。
- [x] Agent turn 创建时保存会话权限等级快照。
- [x] Agent loop 执行前授权判断；核心文件、Shell、插件 Creator、插件能力、MCP/未知工具有宿主分类或保守默认。
- [x] 工具审批请求持久化、turn 等待授权状态、按次批准/拒绝、超时、取消和重启时安全中断。
- [x] 会话权限选择器和单次工具审批表单 UI（包含插件安装 release 权限摘要）。
- [x] Agent 插件安装审批绑定不可变 release ID；批准与安装使用同一 release，避免审批等待期间 latest release 变化。
- [x] 提供最近 1–365 天的工具使用、上下文压缩和插件生命周期聚合指标 API，并开放只读 Agent 工具读取调用、失败、授权、审批、耗时、版本构建/启停/回退和压缩前后字符数。
- [x] 在应用内提供工具/release 观测面板；安装和版本回退需提交包含固定 release、权限摘要和显式同意的表单，HTTP 路径不再后台自动批准或解析 latest。
- [x] MCP 新建、启动、移除及项目设置移除有明确表单确认；MCP 状态/删除结果会读回核验，UI 不再把仅重载 Skills 描述成热重载所有插件。
- [x] 插件构建二进制临时目录退出即清理；内容寻址 bundle 打包后重新计算摘要，重复 release 仅在数据库读回匹配同项目与 digest 后复用。
- [x] 插件生命周期审计写入失败显式返回；运行时故障状态/审计写入失败记录操作日志，持久审计只存错误摘要；旧的隐式 latest 审批/安装接口已禁用，审批请求绑定确切 release ID。
- [x] 插件工坊显示经路径和符号链接检查的 release bundle 实际占用、去重包数和缺失包数。
- [x] release 默认永久保留；用户通过含原因和确认框的表单标记确定错误/不可用版本后，立即禁止批准、安装和回退，下次启动才删除 bundle；release 元数据与两阶段审计记录保留。活动安装引用不能标记，停用引用保留元数据但允许清理 bundle。
- [x] 重启后恢复同一 turn 的加密消息/工具调用 checkpoint；只恢复已验证的运行绑定和已提交工具结果，未闭合副作用仍要求人工核对。Host 重启时未决审批重新询问，精确匹配的已批准/拒绝请求复用原决定。
- [ ] 有范围的会话级后续授权；当前审批仅支持“仅此次允许”或拒绝。
- [ ] 将 Windows AppContainer 接到 `exec_command` / `exec_script`：代码、异常退出恢复记录和启动时恢复路径已接入并完成 Windows/macOS 编译检查；命令临时文件使用 profile 专属 `AC\Temp`，工作区只读授权通过恢复记录撤销；仍需在目标 Windows 环境确认权限限制、网络隔离和 ACL 恢复；macOS 等其他平台暂时 fail closed。
- [ ] Shell/文件访问的其他平台 OS 沙箱，以及除插件工坊安装/回退表单外的插件/MCP/API 状态变更入口复用统一审批、插件生命周期与空间治理。

权限 profile、Agent loop 执行前策略判断和持久化审批主路径已接通。未知/MCP/插件工具默认询问，Shell 默认询问；工作区核心写入依 profile 决定。Windows 命令启动路径现已接入 AppContainer 后端，并持久化可在服务启动时恢复的临时只读 ACL 授权记录；命令写入仅落在 profile 专属临时目录。Windows 运行时隔离和恢复仍未验证。其他平台会拒绝命令执行。插件安装/回退、MCP 配置/启停/移除和项目设置移除已有精确目标的用户提交表单，但这些非 Agent 路径尚未统一复用持久化审批/审计服务。断点续跑已覆盖加密的模型 transcript、已完成工具结果、预算和审批决定；artifact/fragment/插件路由状态仍需人工核对，Provider 响应未提交时可能重发请求。系统级权限边界仍受 Windows 隔离验证和其余非 Agent 状态入口限制。

## 目标和边界

权限是 Agent Harness 的运行时能力，不局限于插件生成。对当前会话中的每一次工具调用，Harness 必须依据用户选择的会话权限策略、工具能力声明、调用参数和运行环境，在副作用发生前决定允许、暂停等待用户表单审批或拒绝。授权检查必须覆盖 Agent loop 以及可产生相同副作用的 HTTP/API 路径。

本计划不恢复已移除的评测或晋升机制，也不根据遥测自动修改稳定版本。可观测数据用于调查和辅助 Agent 提出工具改进；改动仍由用户决定是否应用。

## 阶段 0：工具和副作用盘点

**修改范围**：`backend/internal/coretools`、`backend/internal/plugins`、`backend/internal/mcp`、`backend/internal/pluginforge`、`backend/internal/agent`、`backend/internal/httpapi`。

1. 盘点 Agent 可调用的所有工具及非 Agent 的状态变更 API，逐项记录副作用、目标资源、是否外部访问、是否可重试、是否破坏性。
2. 将策略类别定义为 Harness 拥有的可信元数据；插件 manifest/MCP annotations 作为声明或提示，不能覆盖宿主的强制策略。
3. 未知工具默认按可能写入、破坏和访问外部处理；缺少执行隔离时禁止宣称 shell 被限制在工作目录。
4. 用盘点结果建立覆盖矩阵，确保任何新增工具必须声明风险和授权入口。

**完成条件**：工具清单、API 副作用清单和信任模型齐备；工具执行路径没有未盘点的副作用入口。

## 阶段 1：会话权限模型和持久化

**修改范围**：`backend/internal/domain/types.go`、`backend/internal/storage/sqlite.go`、`backend/internal/storage/*`、`backend/internal/agent/service.go`、`backend/internal/httpapi/server.go`、`frontend/app/api.ts`、`frontend/app/OApp.tsx`。

1. 为 Conversation 增加受校验的 `permissionProfile`，数据库迁移提供保守、兼容旧会话的默认值。
2. 增加读取/更新会话权限 profile 的后端 API；用户界面展示当前值，保存后读回并确认后再更新显示。
3. 每次 turn 启动时将 profile 和其具体限制复制到 turn 的不可变策略快照；切换权限只影响后续 turn。显式降低权限时，取消或重新检查尚未执行的请求。
4. 可选权限档位：`read_only`、`workspace_autonomous`、`fully_autonomous`。`fully_autonomous` 自动批准所有已由宿主分类的工具效果，但未分类操作仍拒绝执行；OS/AppContainer 沙箱和工具可用范围独立生效，权限档位不能关闭它们。旧会话的 `ask_on_sensitive` 保持兼容，但不作为新会话的默认档位。
5. “完全自动”只取消逐项用户审批，不等于无沙箱或可访问任意资源；文件、网络、进程、凭据边界仍由执行层强制落实。

**完成条件**：profile 跨应用重启保留；前端、API、DB、turn 快照使用相同枚举及默认值；不能通过伪造 profile 越权。

## 阶段 2：统一授权决策和工具能力描述

**修改范围**：新增 `backend/internal/permissions`；接入 `backend/internal/agent/loop.go`、`scope.go`、核心工具、插件运行时、MCP 客户端以及 API handler。

1. 定义主体、动作、资源、上下文组成的授权请求，以及 `allow`、`ask`、`deny` 决策和可追溯原因。
2. 为工具登记受宿主管理的风险元数据；MCP/插件提供的 read-only、destructive、open-world 等声明仅用于展示和风险加权。未知、无声明或来源不可信时使用保守默认值。
3. 在每次执行前检查工具、精确参数及目标资源；模型提示、工具描述和 UI 隐藏不能替代执行前授权。
4. 对 shell 等无法仅从工具名和参数可靠限定的操作使用 OS/容器沙箱。文件系统、网络、进程、凭据边界由执行层强制，不靠命令字符串扫描。
5. 所有同类状态变更 API 复用该策略检查，避免 HTTP 入口绕过 Agent 的审批规则。

**完成条件**：执行 handler 只在决策允许或有效审批被消费后调用；未知工具 fail closed；沙箱决策和审批策略互相独立。

## 阶段 3：持久化审批、暂停与恢复

**修改范围**：`backend/internal/domain`、`backend/internal/storage`、`backend/internal/agent/loop.go`、`backend/internal/agent/service.go`、`backend/internal/httpapi/server.go`、前端会话组件。

1. 新建持久化 approval request，绑定用户、会话、turn、tool call、参数摘要、目标资源、profile 快照、过期时间和 trace ID。
2. 审批状态使用明确状态迁移：pending → approved/denied/expired/cancelled；批准的单次操作再转为 executing → completed/failed/needs_reconciliation。
3. 工具副作用发生前使 turn 进入 `awaiting_approval`，写入足以恢复同一轮运行的 checkpoint（已完成消息、模型返回的工具调用、当前索引、metrics 和确切策略快照）。一次只阻塞并展示当前待处理调用。
4. UI 展示用户可理解的审批表单，包含工具来源、实际动作/参数、资源、影响、风险理由、权限 profile 和拒绝/仅此次/限定会话范围的选择。敏感参数在 UI 中按规则遮蔽。
5. 审批 API 校验调用者、会话归属、请求状态、时效和参数摘要；CAS/事务消费决定，重复提交不重复执行。
6. 用户批准后恢复同一 turn，并只执行已批准的精确调用；拒绝、过期、取消和服务重启均有明确状态和安全恢复行为。执行结果未确认时不自动重放外部副作用。

**完成条件**：批准之前副作用为零；批准后只执行绑定请求；重启可恢复 pending 审批；中断/不确定操作不会被报告为成功或被盲目重试。

## 阶段 4：权限表单与会话控制 UI

**修改范围**：`frontend/app/OApp.tsx`、`frontend/app/api.ts`、相关样式以及审批 API。

1. 在 composer 中显示当前会话权限等级，并提供可理解的预设说明；权限选择在会话之间独立。
2. 对设置更新由后端读回确认；运行中的 turn 仍使用创建时的 profile 快照，用户可以明确取消运行后采用新 profile。
3. 显示 Agent 当前等待用户决定的卡片，允许用户查看请求并提交表单；刷新或重开会话后仍能取得 pending 请求。
4. 允许用户按具体工具和会话范围保存有限的后续授权；不提供无范围、永久的“全部允许”。

**完成条件**：无 JS-only 权限状态；pending request 与后端状态一致；UI 不能伪造批准或绕过 API。

## 阶段 5：工具/插件授权整合

**修改范围**：Agent Creator 工具、插件 Forge 服务和 handler、MCP 插件中心、删除/卸载 API、grant 存储。

1. 删除插件安装路径中的自动 `Approve`；安装只消费已经存在且匹配不可变 release/权限摘要的用户授权。
2. Agent 可提案、生成、编辑和构建插件，但安装、提权、启用外部副作用、卸载和永久删除经统一审批管线。
3. 权限扩大重新请求同意；减少权限不应静默扩大其他 grant。
4. MCP server 工具按工具风险和当前会话 policy 进入同一授权管线；OAuth 认证授权与本地工具操作审批分开处理。
5. UI 原生 confirm 仅作交互提示，不作安全边界；后端 API 必须校验审批。

**完成条件**：Agent 不可自批；UI、Agent、MCP、HTTP 对同一操作产生一致的决策和审计记录。

## 阶段 6：插件版本生命周期和空间治理

**修改范围**：`backend/internal/pluginforge`、`backend/internal/pluginruntime`、对应 API/UI 与数据迁移。

1. 分离源码修订、不可变 Release 和安装实例；版本列表呈现 digest、权限差异、构建报告和 active/rollback 状态。
2. 升级先构建独立 immutable release，保留现行版本；检查就绪后原子切换。失败恢复旧实例。
3. 回滚只重新激活仍保留且授权有效的 release；版本不可变更改。
4. Bundle 按摘要去重并在复用前校验内容；插件构建临时二进制目录会清理。版本包实占统计已提供 API 和工坊摘要（只统计可验证的内容寻址 release bundle；缺失包单独计数）。配额、源码/临时文件占用统计、旧 release 保留策略和显式清理仍待实现，当前保留所有历史 release 以支持回退。清理策略需先确定保留版本数和回滚语义，再实现可恢复的 tombstone/分阶段流程。
5. 默认不按数量淘汰历史版本。用户明确标记错误版本后先持久化 `unusable_pending_cleanup` 和审计记录；下次启动校验内容寻址路径、安装引用和符号链接后删除 bundle，再将状态核验为 `unusable` 并记录完成审计。release 行和权限/测试元数据保留；活动安装引用阻止标记，待清理项可在下次启动重试。

**完成条件**：活动版本、固定版本和策略保留的回滚版本不能被 GC 删除；删错/中断清理可恢复；空间占用和删除结果可读回核验。

## 阶段 7：观测、审计和验证

1. 在同一 trace 中记录 permission check、approval requested/resolved、tool started/completed、plugin lifecycle 和结果；关联 conversation、turn、tool call、tool/provider/plugin release ID。
2. 指标覆盖按工具/release/项目聚合的调用、拒绝、审批、失败和执行耗时，以及插件 build/activate/deactivate/rollback、会话装载裁剪和 turn 压缩计数/字符变化；Agent 只读工具、HTTP API 和应用内观测面板可读最近 365 天聚合。重试、重启恢复和插件空间配额/回收观测仍待实现；普通日志不写原始凭据及未遮蔽敏感参数。
3. 变更必须验证用户可见的下游效果：错误模式不能调用 handler、审批边界、参数绑定、重复提交、拒绝、过期、并发、重启恢复、权限 profile 持久化、API 绕过、沙箱隔离、回滚以及 GC 引用保护。

## 建议实施顺序

先做阶段 0 的盘点，随后按阶段 1、2、3 落地权限主链路；阶段 4 紧随 API 状态完成。之后整合插件和 MCP（阶段 5），最后做版本存储治理及观测完善（阶段 6、7）。权限核心没有完成之前，不开放声称可安全“全部允许”的会话 profile。
