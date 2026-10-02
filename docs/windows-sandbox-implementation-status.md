# Windows 沙箱实施与验收记录

更新时间：2026-10-02
权威设计：[windows-sandbox-design.md](windows-sandbox-design.md)（本文不替代或修改设计）

## 当前状态

实现仍处于**未完成运行验收**状态。下表只记录本工作树中实际执行并观察到的结果；代码能编译或单元测试通过，不代表 Windows 安全边界已在最终 token 下成立。

## 当前环境已验证

| 检查 | 结果 | 证据范围 |
| --- | --- | --- |
| Windows amd64 构建 | 通过 | 后端全仓交叉构建通过；沙箱、runner、setup 定向构建通过 |
| Linux amd64 构建 | 通过 | 后端全仓交叉构建通过；该设计不表示 Linux 后端已按 Windows 方案验收 |
| Windows 沙箱/runner/setup `go vet` | 通过 | 定向包静态检查 |
| Linux 沙箱 `go vet` | 通过 | 定向包静态检查 |
| Windows `internal/sandbox` 测试二进制 | 通过 | 测试 exe 输出至工作区并在本机实际执行 |
| 默认后端设置持久化/回滚 | 通过 | Windows Service 测试关闭并重开 SQLite Store 验证执行配置读回；注入 SQLite 写失败后验证持久值与执行配置回滚 |
| 原生沙箱工具说明 | 通过 | `exec_command` / `exec_script` 描述与命令级 SSH agent broker 行为一致，回归测试通过 |
| 沙箱及 HTTPS Git 凭据测试 | 通过 | `internal/sandbox`、`internal/gitcredential` |
| SSH 代理协议测试 | 通过 | 身份枚举过滤、未选密钥签名拒绝、密钥管理请求拒绝及异常帧 |
| Windows SSH broker 命名管道往返 | 通过 | 使用当前登录 SID 和临时测试 Host agent，验证客户端身份/命令 pipe、选定身份枚举与签名转发，并验证活动/空闲客户端下的 lease 停止及重复清理 |
| Windows HTTPS Git helper | 通过 | 在 scratch 生成与安装 runner 哈希相同的命令专属 `.exe`，验证 Git helper 名称到本命令 pipe/ID 参数解析；不再经由 `.cmd`/shell 转发凭据请求 |
| Windows SSH 配置回归测试 | 通过 | Windows 测试二进制在本机运行；验证 SSH remote 解析/命令行选项注入拒绝、Host `known_hosts` 复制、只落盘公钥、Host 私钥未复制、全局 SSH commit/tag 签名设置透传、OpenPGP 配置区分 |
| Offline 本地 SSH 签名代理接入 | 通过 | Host 全局或仓库本地存在 SSH signing 配置时，即使没有 SSH remote 且 `networkAccess=false` 也会启动命令级 Host agent broker；agent/公钥不可用时普通 Git 命令仍可启动，签名设置保留并指向不存在的命令 pipe/公钥路径，让签名失败而不静默生成未签名提交；Windows 测试用临时 Git 仓库验证本地配置探测 |
| 账户名跨 Windows 用户适配 | 通过 | 定向测试验证同一 owner SID 稳定得到同一 Offline/Online 账户名，不同 SID 不共享名称，且名称不超过 Windows 本地账户长度限制；未创建真实账户 |
| Windows/Linux 定向构建与检查 | 通过 | `internal/sandbox`、`axiom-command-runner`、`axiom-sandbox-setup` |
| WFP 失败/回滚错误传播 | 通过 | Windows 测试覆盖 begin/apply/commit/abort 顺序与 apply、commit、abort 错误保留；WFP engine close 错误并入 setup/remove/probe 结果 |
| 清理失败后的命令隔离 | 通过 | 新命令在 per-user setup 锁内扫描持久 journal；有未撤销授权且路径重叠时 fail-closed；Windows 测试覆盖真实 journal 读回、活动命令并发、cleaning 阶段、撤销状态、SID/命令 ID 校验及路径边界 |
| journal 大小上限和原子保留 | 通过 | 写入端在替换前实施与恢复端相同的 1 MiB 限制；Windows 测试确认超限写失败后旧的可恢复记录逐字节保留 |
| journal 启动扫描/恢复校验一致 | 通过 | 两个入口使用同一验证函数和严格 JSON 解码；未知字段/生命周期阶段、尾随数据、缺失 logon SID、runner PID/start 不成对、无效 token baseline 或文件身份均 fail-closed；本机 Windows 全包沙箱测试通过 |
| ACL/文件身份句柄清理错误 | 通过 | ACL 写入并读回后、路径身份遍历中，文件句柄关闭失败均进入返回错误；Windows/Linux 全后端构建及定向 vet 通过 |
| 复用的工具缓存目录 ACL | 通过 | 缓存目录无论新建或已存在都会先验证 NTFS 路径、重设私有 DACL、回读精确 DACL 并确认文件身份未变；Windows 测试用宽松 Everyone ACL 验证已有目录被收紧 |
| 受保护目录与 runner 文件 DACL 回读 | 通过 | `setNativePathDACL` 对目录和文件均逐字核对 DACL read-back；Windows 测试覆盖目录收紧和 runner 文件的 System/Admin/owner/BU 精确授权 |
| 沙箱账户与凭据 IPC 清理错误 | 通过 | LSA policy close/rights memory free、NetAPI 查询缓冲区、进程/token/lease 句柄的释放错误合并到结果；Git HTTPS/SSH command pipe 用 `CancelIoEx` 取消阻塞 I/O，由 listener 唯一拥有者关闭句柄，Stop 收集取消/关闭错误、可重复调用并有界等待。Windows 测试覆盖 LSA cleanup status、HTTPS broker 已连接但不发请求时的停止、SSH broker lease cleanup，以及 Git helper/SSH agent 共用的 WaitNamedPipeW 预期/异常错误区分；Host SSH agent 身份探测也会传播连接关闭错误 |
| 桌面 helper 和 host 构建产物 | 本机 backend package 构建、PE 与延迟读回通过 | 根目录 `npm run package` 与 `npm run build:desktop` 使用的 `desktop/scripts/package.mjs` 均构建并将 host、`axiom-command-runner.exe`、`axiom-sandbox-setup.exe` 放入 `resources/app/runtime` 同目录；修复第二入口漏包、Go 绝对路径依赖、Windows 子进程参数转义及缺少产物读回。最近一次 `node scripts/package.mjs --backend` 成功，三个 `.runtime/package` 文件均回读为 PE；host/runner/setup 大小分别为 37,692,928 / 5,035,008 / 5,496,320 字节，SHA256 分别为 `051492c0785340008190c96b9e7492123721e99b895b40ac60d08db0c86d6854` / `c3c9f8e9c5ae3e1d9e50676471887995a0e9129f43687aa09b1f8641fd34fd30` / `394b444de1f6bd877d94c9b1890b91455698071e00a6494df0b894dfc84e0485`。加 `-s -w` 的 host 在此环境会被清理，去除该 strip 参数后 host 持续存在；完整 Electron 安装包未运行 |
| Windows CI 覆盖 | 已配置，未由 GitHub Actions 执行 | `.github/workflows/ci.yml` 新增 Windows 2022 job，执行 Windows 全仓构建、runner/setup 输出检查、沙箱相关 vet 与 `internal/sandbox` / `internal/gitcredential` 测试；本机 YAML 解析通过；需要 Actions 实际运行结果作为远端验证 |
| 本机 Windows 定向验收复跑 | 通过 | 本轮重新运行 `go build ./...`、沙箱/runner/setup `go vet`、`go test ./internal/sandbox ./internal/gitcredential`；测试和构建使用仓库内 Go cache/temp 目录；两个打包脚本 Node 语法检查及本轮 diff whitespace 检查通过 |

本轮补充 runner 进程清理：命令等待失败、超时或 Host 控制管道断开时，终止并有界等待 Job Object 中的进程树；终止/等待失败进入命令结果。受限 token Probe 子进程和账户级 runner Probe 超时后同样终止并有界等待。提权 setup、Probe 子进程及受限 token 构造过程中的句柄关闭失败现在也会进入返回状态。改动已通过 Windows 全仓构建、Linux 全仓交叉构建、两端定向 vet，以及本机执行的 Windows 沙箱测试二进制；这些检查不等价于管理员级最终 token 实机验收。

## 未通过或不能据此判定

| 检查 | 观察结果 | 影响 |
| --- | --- | --- |
| 全仓 `go test ./...` | 本机用逐包编译到包目录并直接运行测试二进制的方式执行 32 个 Windows 测试包：31 个通过；`coretools` 唯一失败包中的 `TestCoreToolsExecution` 调用旧 AppContainer 后端创建 profile 时返回 HRESULT `0x80070002`。这绕过了 Go 默认临时目录测试 exe 被 Windows 拒绝启动的问题，但没有绕过 AppContainer 的真实系统失败 | 全仓测试状态为失败。不能据此判定 Windows 原生后端；`coretools` 默认集成测试在本环境仍会进入旧 AppContainer profile 创建路径并失败 |
| race 检测 | 未运行：当前 Go 工具链要求启用 cgo，环境未启用 | 并发 race 检测未验证 |
| Linux 测试执行 | 本轮在 Windows 主机设置 `GOOS=linux` 后运行 `go test`，无法启动生成的 ELF 测试程序（`%1 is not a valid Win32 application`） | Linux 构建与 vet 通过；Linux 测试必须在 Linux 主机或兼容执行环境运行 |
| 最终受限 token 下的 runner Probe | 从本轮构建的 runner 实际执行 `--probe`；当前 Codex 执行账号的 token `IsRestricted=true` 且有 6 个 restricting SID。隔离测试确认 `CreateRestrictedToken` 的 `WRITE_RESTRICTED` (0x8)、含该标志的 0x9/0xc/0xd 在此 token 上返回 `ERROR_INVALID_PARAMETER`，不含该标志的组合可创建 | 当前外层受限 token 不能代替按设计创建的专用账户 token，因此 NUL 和受限子进程 Probe 对目标账户仍不可判定；执行路径按失败关闭。需在管理员 Windows 主机完成真实账户 Probe |

## 必须在管理员 Windows 环境完成的实机验收

当前进程 `WindowsPrincipal.IsInRole(Administrator)=false`，token 仅有 `CodexSandboxUsers` 和普通 `Users` 成员，完整性级别为 Medium；没有执行管理员级安装/修复权限。本机 `ssh-agent` 服务状态已读回为 `Stopped` 且启动类型为 `Disabled`，因此无法进行真实 agent 密钥调用验收。以下项目仍未验收，不得把沙箱标成健康或把默认后端切换到该实现：

- 执行重复 setup、失败补偿、卸载和升级回滚；确认 Offline/Online 两个 SID 派生账户、登录权、非管理员组成员关系、DPAPI 凭据 ACL、WFP 规则 read-back 均与 manifest 一致。
- 以实际 `CreateProcessWithLogonW` 登录会话运行 Probe，检查最终 token、私有桌面、Job 与系统重启后的持久状态。
- 在最终受限 token 下读写 `NUL`；执行 Git status/diff/log/add/commit/checkout，并由 Host 回读 index、objects、refs 和文件效果。
- 验证只读/可写文件边界、workspace 外用户文件与 Host 控制目录拒绝写入，以及 reparse point、hardlink、父目录删除绕过和长路径行为。
- 验证 Offline 的 IPv4/IPv6 TCP/UDP、loopback、DNS/系统 resolver 阻断；验证 Online 出站、代理和移除代理变量后的 OS 边界。
- 验证跨工作区、跨实例、Offline/Online 并发与共享 Git 写租约；验证取消、超时、Host 崩溃、各授权阶段中断后的 Job 终止、ACL 恢复和 journal 保留。
- 在不同 Windows 安装布局上验收系统级和用户级 Git、PowerShell、Node、Python、Go；覆盖 submodule、LFS、Git hooks、证书和 SSH agent/签名。SSH agent 验收需要在当前用户 OpenSSH Agent 中加载测试密钥，确认仅选定公钥可见且签名有效。

这些项目需要真实 Windows OS 效果读回；交叉构建、模拟测试或 manifest 存在都不能替代。完成前整体验收状态保持**未完成**。
