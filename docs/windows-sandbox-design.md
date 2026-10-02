# O Agent Windows 原生命令沙箱设计

版本：2.0
日期：2026-09-30
状态：设计完成，待实现与运行验收
适用范围：O Agent 本机 Windows 命令和脚本执行

## 1. 目标与最终决策

让 Agent 使用本机 PowerShell、Git、Node、Python、Go 等开发工具，同时由 Windows 限制文件写入、联网及进程访问。设计包括安装、执行、并发、权限变更、Git、凭据、恢复、升级、卸载和验收。

采用一个 Windows 原生后端：**专用低权限账户 + 受限 token + 文件 ACL + WFP 网络规则 + Job Object + 私有桌面**。所有命令与脚本进入同一条执行路径。

主程序使用现有 Go 后端，增加两个可执行程序：初始化程序和命令 runner。日常执行无需管理员权限。保留现有工具参数与审批流程，不另建一套会话权限系统。

本方案是完整目标架构。分阶段实现只安排开发顺序，不削减最终能力，也不以“以后再补”为默认行为。实施完成并通过运行验收后才切换默认执行后端。

## 2. 为什么有两个账户

两个账户分别承担稳定的网络身份：

| 执行身份 | Windows 网络政策 | 使用条件 |
| --- | --- | --- |
| Offline | 持久阻止该身份发起的直接网络连接 | networkAccess=false |
| Online | 按宿主网络条件允许出站 | networkAccess=true 且权限层已放行 |

它们共用一套 runner、文件策略、输出处理和恢复机制。账户数量不随项目、会话或命令增长。真正的命令隔离单位是**登录会话**，不是账户数量。

使用两个身份的原因是：同一账户上的静态网络规则会影响该账户的全部进程。若为某条命令临时给唯一账户打开网络，同账户下已经运行的离线命令也可能得到网络能力。按 Git/Python 等 executable 路径禁网也不能表达“这一次调用离线，另一次调用联网”。

可以用单账户配合按会话的网络过滤或强制代理实现，但这要求维护动态特权网络政策、代理认证及非 HTTP 协议支持。本项目现有 networkAccess 是联网开关，不是域名白名单。两个稳定身份符合当前需求，减少运行时网络变更及恢复步骤。

**决定保留两账户；取消全局命令串行。** 每个真实 Windows 用户拥有自己的账户对，名称带宿主用户 SID 的稳定短标识，例如 AxiomOffline_ab12 / AxiomOnline_ab12。机器上多个真实用户不共享密码、账户 profile 或读取授权；同一用户的所有 O Agent 实例共享这对账户及其安装记录。

## 3. 产品边界

### 3.1 权限语义

| 能力 | 默认行为 | 放行后行为 |
| --- | --- | --- |
| 工作区读取 | 可读取当前工作区及批准的工具运行时 | 宿主可增加明确的外部只读根 |
| 工作区写入 | 禁止 | writeAccess=true 允许当前工作区普通项目文件写入 |
| 临时存储 | 每条命令有独立 TEMP/HOME | 不受工作区写开关影响 |
| 网络 | 阻止直接连接 | networkAccess=true 允许出站并使用已配置代理 |
| 敏感资源 | 宿主凭据、应用控制数据受保护 | 通过宿主受控工具提供特定能力 |
| 命令后代 | 与父命令相同权限和生命周期 | 不能自行请求扩权 |

审批政策决定某次操作是否可开始，OS 沙箱决定已经开始的进程能做什么。workspace_autonomous 和 fully_autonomous 不能关闭 OS 边界。模型不能选择执行账户、后端或授权 SID。

### 3.2 安全边界的准确表述

这是宿主内核上的开发工具进程沙箱，保护宿主个人身份、凭据、控制数据和非授权写入位置。Windows 向普通用户公开的系统及运行时文件仍可读。允许的设备兼容权限、注册表和系统临时资源必须在 token 基线中明确记录。

不把 WRITE_RESTRICTED 等同于完整读取隔离，也不声称原生进程沙箱可以抵御内核漏洞。不同命令之间保护临时数据和控制通道，但不是强对抗多租户计算平台。若产品要求全文件系统可见性白名单或运行高度不可信的多租户任务，应另采用 VM，不给当前后端附加虚假的承诺。

offline 指阻断沙箱身份直接发起的 IP 网络连接。Windows DNS 等宿主系统服务的间接行为需单独验证、如实记录；不能把这一模式命名为“绝对零网络泄露”。

### 3.3 执行面

exec_command 与 exec_script 均进入此后端。Host 内的文件工具由宿主路径与权限检查约束。MCP、浏览器、插件服务和桌面自动化仅在其实际进程进入此后端时受此沙箱约束；UI 不用一个“已沙箱化”标识覆盖全部工具。

## 4. 组件与职责

| 组件 | 职责 | 权限 |
| --- | --- | --- |
| 现有 permission/coretools | 审批、参数校验、宿主 policy 构造、输出归档 | 真实用户 |
| internal/sandbox | 路径计划、授权日志、ACL 管理、runner 启动、取消、恢复、Probe | 真实用户 |
| axiom-sandbox-setup.exe | 创建/修复账户、安装 WFP、凭据与安装清单、升级卸载 | 仅安装维护时管理员 |
| axiom-command-runner.exe | 独立登录会话、token、私有桌面、Job、stdio、子进程 | 专用低权限身份 |

不引入常驻管理员服务、内核驱动、容器平台或第二套业务数据库。网络规则在 setup 时持久安装，命令执行时只选身份。凭据 broker 是 Host 内部模块，通过同一受保护 IPC 提供能力，不是新服务。

## 5. 身份、登录和控制通道

每条命令以所选账户新建独立 Windows logon session，取得独立 logon SID。同账户的两条命令不得复用登录会话。logon SID 用于当前命令的文件读取、临时目录、进程/线程与 IPC 对象访问；受限 SID 用于额外的写限制。

Host 以绝对路径调用 CreateProcessWithLogonW 启动 runner；runner 在自己的身份下生成受限 token，再调用 CreateProcessAsUserW 创建目标进程。禁止使用 LOGON_NETCREDENTIALS_ONLY，它不会切换本地身份。账户保留启动 API 必需的 Log On Locally 权限；不配置与该 API 冲突的 Deny logon locally。随机密码不显示给用户，runner 使用独立私有桌面。

受限 token 删除不必要特权，并限制其可使用的组与写权限。实现采用已审核的 token 基线，记录完整 SID/权限集合；不能只写“随机 SID”便假定 NUL、命名管道和开发工具都会工作。兼容性 SID可能放开宿主公共可写对象，必须与路径限制一起做负向验收；不能添加 Everyone/账户 SID 后仍宣称写范围天然仅有 workspace。

控制通道使用每条命令的随机 named pipe、版本化长度帧和显式安全描述符。Host 验证客户端 PID、进程开始时间与已启动 runner 身份；runner 只接受一次 Host 握手和一次执行计划。命令、stdin、环境和凭据不通过进程命令行传递。密码不会发送给 runner 的目标子进程。

runner 与其进程/token 不允许目标子进程或其他登录会话获取写入、注入、复制 token、修改 DACL 等权限。进程和对象默认 DACL 使用命令 logon SID，并用 OWNER RIGHTS ACE约束共享账户作为对象 owner 的隐含改 DACL 权限。Host 保留管理权限；控制句柄不继承到目标进程。没有完成这些控制前，不允许跨命令并发。

## 6. 安装、Probe 和持久化

### 6.1 安装流程

1. 取得按真实 Windows 用户标识的跨进程 setup 锁，检查已有账户和安装所有权。发现同名外部账户时不覆盖。
2. 生成账户随机密码，创建对应标准本地用户；不加入管理员组，不授予 debug/备份/恢复/提权权限。验证真实启动 API所需登录权。
3. 安装匹配 Offline 用户 SID的持久 WFP 规则，同时限制两个账户的入站与不必要本地访问。
4. 部署受保护 runner；只有 Host/System/管理员能写其文件、依赖和启动配置。保留标准系统 profile供 Windows兼容使用，命令 TEMP/HOME另行分配。
5. 密码以 DPAPI machine scope加密并保存在限制 Host 所有者、System、管理员访问的文件中。DPAPI不能代替 ACL；两沙箱账户无读取权限。此方式兼容 UAC 使用不同管理员身份的安装流程。
6. 回读账户 SID、组/登录权、凭据可解密性、runner版本、WFP条件与动作；运行目标身份的实际启动 Probe。
7. 原子写安装 manifest并回读；Host再次 Probe通过后才开放命令。

每一步先记录预期效果、再执行并读回。失败反向补偿本次创建的资源；修复已有安装时保留旧配置，不能失败后删除原本可用的账户/规则。多步骤失败和回滚失败均返回明确错误。

### 6.2 状态来源

| 数据 | 权威来源 |
| --- | --- |
| 用户选择的后端、运行时读目录、代理配置 | 现有后端持久化 settings |
| 安装版本、Owner SID、账户 SID、资源所有权 | 受保护 installation manifest |
| 账户、WFP、token启动能力 | Windows查询及实际 Probe |
| 活命令、ACL授权、恢复进度 | 每命令授权 journal及 OS进程状态 |

同一数据不在浏览器/数据库/manifest中各留一份互相竞争的权威副本。UI只显示后端读回的状态及诊断信息。

安装生命周期为 absent → installing → installed；健康结果为 healthy / unhealthy及明确原因。它们是不同事实：安装文件存在可以同时出现 WFP丢失。运行中命令单独有自身状态，不再以多个 enabled/running布尔推断。

### 6.3 启动和日常检查

应用启动先恢复遗留 journal，再 Probe。每次执行检查 runner版本、账户身份与关键网络状态；WFP状态变化、账户删除、凭据失效立即阻止依赖该能力的新命令。Probe记录时间和检查项，不能无限期使用缓存健康结果。

账户/WFP系统准备只在初始化、升级或修复时需要管理员权限。正常命令与清理能否操作目标 ACL由 Host预先检查；无权限时返回具体路径错误，不申请管理员权去运行模型命令。

## 7. 每命令执行计划

模型参数仍为 cmd、workdir、timeoutSeconds、writeAccess、networkAccess。Host在审批后编译不可扩权的 ExecutionPlan：

- commandId、protocolVersion、installationVersion；
- workspace的规范化路径和卷/文件身份；
- cwd、shell绝对路径、参数、stdin；
- ReadRoots、WriteRoots、ProtectedPaths，包含 Git元数据根；
- networkMode、过滤后环境、proxy设置；
- 本次 TEMP/HOME、journal位置；
- timeout、输出上限、进程数及取消信号。

日志存计划的摘要和恢复所需资源标识；不存明文密码、令牌或未脱敏环境。

路径解析依据目录句柄/卷身份，不仅检查字符串前缀。拒绝越界 reparse point、设备命名路径和不能保证语义的文件系统；NTFS本地盘为支持基线。FAT/exFAT、UNC/SMB等缺少相同 ACL语义时明确报 unsupported，不换成普通宿主执行。

## 8. 完整执行流程

~~~mermaid
sequenceDiagram
    participant A as Agent
    participant H as Host
    participant W as Windows ACL/WFP
    participant R as Runner
    participant C as Shell及后代
    A->>H: exec_command / exec_script
    H->>H: 审批、Probe、编译ExecutionPlan
    H->>H: 持久化prepared journal
    H->>R: 以所选账户启动独立登录会话
    R->>H: 握手、logon SID、进程身份
    H->>W: 应用并读回本次路径授权
    H->>R: 启动许可
    R->>W: 受限token、私有桌面、Job
    R->>C: 挂Job后恢复执行
    C-->>H: stdout/stderr
    R-->>H: exit / timeout / cancel
    H->>R: 终止并等待全部后代
    H->>W: 移除本次授权并读回
    H->>H: 清理TEMP、归档输出、提交终态
    H-->>A: 真实退出状态及cleanup结果
~~~

runner初始仅执行可信启动代码，握手后等待 Host确认授权完成。模型命令在 token、Job、桌面及路径检查全部完成后才恢复执行。失败时终止待命 runner并回滚。

Host在启动前创建持久 journal，阶段记录 prepared、granted、running、cleaning、finished。每阶段表示已经发生并读回的效果，PID和开始时间防止复用误判。

正常结束、超时、取消、Host断连均先停止整个 Job中的进程，再撤销路径授权和临时资源。Job由Host保留生命周期控制句柄；runner/child不持有可令Job在Host崩溃后继续存活的额外句柄。断连也由runner独立终止Job。验证所有进程退出后才清理授权。

命令执行与清理是两个结果。命令失败但清理成功，返回原始非零退出码；命令成功但清理失败，返回 sandbox_cleanup_failed与已发生的文件效果，保留journal并阻止受影响资源继续使用。不能吞掉清理错误，也不能将已经发生的项目写入描述为自动回滚。

## 9. 文件与临时资源

### 9.1 授权

普通命令按当前logon SID授予读取；按本次token写能力授予WriteRoots。避免给长期账户 SID添加会随并发累积的workspace写权限。临时权限不写到全局配置。

只读命令也需要 TEMP/HOME写入、进程对象及必要设备访问。因此“只读”是工作区只读，不是完全禁止任何OS写操作。私有HKCU/profile的使用需属于沙箱自己的系统状态；不得使用宿主用户profile作为兼容性补丁。

禁止获得工作区外项目数据写权限、WRITE_DAC/WRITE_OWNER、改变宿主完整性标签或修改执行政策。设备兼容权限须单列，NUL允许丢弃数据，不等于给磁盘额外写能力。

已有显式DACL、禁止继承目录和新创建文件必须一起检查。授权管理记录具体自有ACE，不整份覆盖旧DACL。移除授权也必须处理已经继承到后代的ACE，不仅修改根目录便认定恢复完成。工具链造成新增文件时，其owner/default DACL不能令后续命令取得改权限的捷径。

### 9.2 受保护路径

保护沙箱安装、Host控制数据、私钥/凭据目录及项目中会被Host加载执行的配置。仓库.git/config与hooks默认不允许普通命令修改。父目录delete-child、重命名、硬链接及junction都属于保护验证范围；单纯给叶文件加deny不够。

允许对这些路径进行合法变更时，使用Host现有审批工具，校验具体目标与影响。普通构建输出、源代码及Git索引/对象/refs遵守writeAccess。保护列表由Host统一生成，配置文件和工具说明共用此定义。

### 9.3 临时资源与缓存

每命令独立scratch目录包含temp、home、git配置和必要的socket/pipe标识；根目录由Host拥有，DACL只允许Host和当前logon SID。共享账户身份不意味着其他命令能读取该目录。

缓存属于按真实用户隔离的沙箱cache根；以工具及版本划分，使用工具原生锁或最小路径锁。缓存不存凭据。可读缓存不会向命令开放宿主profile；可写缓存是明确WriteRoot，计入政策和磁盘配额。失效缓存可以重建，恢复journal不能放在cache中。

## 10. 并发

不同工作区可以并行。每条命令有独立logon SID、token、Job、desktop、scratch和journal，两个网络账号只提供稳定身份。

锁只用于确实共享的资源：

- setup/升级/卸载取得每用户安装维护锁；维护期间拒绝新命令，等待或取消已有命令。
- ACL修改按规范化目录及身份短时间加锁，添加/移除完成后释放。相同自有持久条目使用journal记录租约；不持锁跑完整命令。
- 同一workspace普通只读命令可并行；会发生项目写入的命令由workspace写租约串行，避免多个Git/index或构建写任务冲突。Git自身lock仍保留。
- 跨实例使用Windows命名mutex及durable lease；按统一路径排序拿多根锁，防止linked worktree死锁。
- cache写入依据工具自己的锁处理，不用整台机器单命令锁。

命令A不能读取B的scratch、打开B的进程/token或借B的write SID写项目。Offline/Online命令可以同时执行，选账号不修改另一身份的网络规则。

公共系统读取不是跨任务秘密隔离。对“两个共享账户下的同用户命令完全隔离全部注册表状态”不做承诺；独立logon/object DACL和Host保护资源才是明确约束。

## 11. 网络

Offline身份持久阻断直接IPv4/IPv6连接、TCP/UDP出站及不必要入站。**Offline不例外放行用户HTTP代理**，否则无网络开关失去意义。运行器使用named pipe，IPC无需网络连接。

Online身份允许现有networkAccess语义的出站连接，并使用Host解析的HTTP(S)/SOCKS代理。该模式不提供域名白名单，公开说明其范围。代理失效按工具错误返回；若用户政策是强制代理，必须增加对应OS出口约束后才能提供此模式，不能仅设置HTTP_PROXY。

持久WFP规则匹配稳定用户SID，使用自有provider/sublayer/filter GUID，覆盖相关ALE层。定义loopback、监听及IPv6动作并做行为验证，不声称所有本机通信都被出站filter自动隔离。Offline如需本地测试服务器，使用显式Host分类的局域例外能力；默认政策保持阻断，不能偷偷启用。

Windows DNS/系统服务代请求可能在不同身份发起。Probe及验收记录系统resolver、直接DNS和代理行为。如果产品要求阻断这类间接出口，需验证额外系统控制或使用VM；这不以“未来补个规则”冒充已解决。

代理密码通过受保护环境/IPC提供，仅授权命令可见，输出及journal脱敏。Online命令本身持有网络能力，因此看到其自身使用的临时凭据是预期授权；凭据broker限制目标、时长和作用域。

## 12. Git及开发工具

### 12.1 NUL和运行时

新进程不使用AppContainer身份，不再依赖ALL APPLICATION PACKAGES设备授权脚本。**这并不保证任意受限SID配置都能打开NUL**：在最终token下测试读写NUL及Git内部重定向。必要的设备兼容SID与普通文件写边界一并审核；不修改全局NUL ACL作为默认安装步骤。

Host维护可信运行时根：Git完整安装、PowerShell、Node/Python/Go对应运行时及用户选择的工具目录。支持用户目录安装，只授予具体运行时根；不整体授权用户profile，也不企图从命令字符串猜出全部依赖。

shell启动路径使用可信绝对路径。命令可以执行workspace内的程序，但继承相同沙箱，不会因PATH替换获得Host身份。对含秘密的环境变量不全量继承；提供过滤后的系统、语言和工具环境，TEMP/HOME重定向，缓存根显式加入policy。

### 12.2 仓库与worktree

Host用Git机器可解析输出/文件身份识别worktree、gitdir、common-dir及submodule元数据，形成RepositoryScope。所有写入授权先验证其属于用户选定项目。

普通仓库的索引/对象/refs位于工作区内。linked worktree需要访问主仓库共享元数据：读取自动加入已确认仓库scope，写入只有writeAccess放行时增加必要根；不开放主仓库整个工作目录。若无法确认归属，返回需要补充授权的确切资源，不从gitdir文本直接信任任意外部路径。

same common-dir共享写租约；Git自身锁继续处理原子提交。保护真实config/hooks路径，包括linked worktree和submodule；授权不得修改其他会话的配置。

### 12.3 身份、配置与凭据

HOME使用私有目录，Git全局配置由Host生成：精确safe.directory、用户选择的作者身份、允许的代理和凭据helper。系统Git配置只有经筛选的受信任内容生效；禁止读取宿主全局配置时自动导入任意helper或命令。只读命令使用GIT_OPTIONAL_LOCKS=0；optional locks不是写权限控制。

HTTPS使用Host内credential broker按scheme/host/repository匹配现有凭据，禁止store落盘。broker请求与命令ID、IPC身份、网络授权绑定，过期即撤销。clone/fetch/pull/push的write/network组合由真实效果决定，不能用命令名称代替OS权限。

SSH和签名提交通过受控凭据能力提供：用户选定的SSH/signing identity交由Host代理操作，避免把整棵.ssh或私钥文件交给命令。任意用户SSH配置、agent pipe、credential manager程序不能自动透传。具体能力不可用时返回明确错误，普通本地Git仍可工作；不要求关闭沙箱。

Git hooks和配置命令执行可能是项目流程一部分，其后代仍留在token/Job内；沙箱命令不得通过改hooks/config植入日后Host执行代码。LFS、submodule、包管理器下载、证书验证和用户代理进入相同完整兼容性验收。

### 12.4 退出码

PowerShell原生命令失败可能不改变Shell最终退出码。定义统一wrapper：捕获终止异常、显式exit和最后一个原生命令的退出状态，非零真实传回。复合脚本可自行处理失败，Host不能凭扫描stderr判定成功。

工具只报告命令真实结果；git --version不能代表commit已保存，提交验收必须宿主重新读Git对象、refs和文件效果。

## 13. 资源限制与输出

继承现有统一默认：command/script输入各最多1 MiB；默认执行30秒、最长180秒；Job最多128进程；stdout/stderr每流捕获1 MiB。常量在单一位置定义，schema、描述、UI和测试引用同一来源。

Queue wait、启动失败、执行timeout、取消分别记录。执行timeout从目标进程恢复时开始；排队本身受调用context约束。达到capture上限后仍持续读取并丢弃额外字节，防止管道阻塞。

Host存档bounded capture并标明captureComplete，显示截断独立标记。超时/取消保留已捕获输出与已发生文件效果。归档失败不能报告成功的captureSourceId。

内存/CPU及scratch配额由Host设置并在result中说明是否启用；不能把未配置的资源限制写进契约。无需为此增加复杂的资源调度器。

## 14. 恢复与错误

journal位于Host私有状态目录，不在workspace或scratch。原子写、flush、回读之后才执行权限变更。记录command ID、owner PID/开始时间、runner/Job标识、volume/file identity、logon/restricting SID及自有ACE；不信任模型写出的恢复记录。

崩溃后先确认原Host、runner、Job均不再执行。单看PID不存在不够；若仍有待命进程或权限存活，先终止或标记隔离，不提前撤销目录权限造成清理竞态。

恢复按已记录的资源反向执行：结束进程 → 精确移除自有ACL租约及继承条目 → 清理scratch/credential lease → 回读 → 移除journal。保留第三方期间的合法DACL变更。目标被移动/替换时使用file identity确认；不能按旧路径修改一个新对象。

任何无法证明恢复完成的错误保留journal，状态为unhealthy。只阻断受影响能力/资源；若涉及身份、runner或网络基线则阻断全部命令。对外返回结构化error code、原始OS错误、恢复步骤和可修复路径。

命令生命周期为 prepared → granted → running → cleaning → finished；终态携带exit/timeout/cancel/launchFailure及cleanup结果。finished仅表示生命周期收束，不等于命令成功。

## 15. 升级、修复与卸载

升级先阻止新启动、等待已有命令退出并恢复授权，准备新runner与versioned manifest。协议版本不匹配拒绝启动，不解析未知字段后继续执行。Probe新版本通过才提交切换；失败恢复旧runner和policy。

只轮换需要更新的账户密码，更新凭据与账户密码采用可补偿顺序，避免先改密码再丢失新值。密码/规则变化期间不启动新logon。机器重启后持久WFP与账户仍需实际Probe；不以manifest存在代替。

修复只恢复自有资源，不覆盖企业管理的外部规则。企业策略阻止登录/WFP时给出具体unsupported/unhealthy原因，交由管理员选择修复；不在普通用户模式静默弱化禁网。

卸载先结束执行、恢复全部journal、撤销自有规则和grant，再删除自有账户及受保护凭据。未恢复资源存在时返回卸载未完成及保留原因。workspace文件、用户Git历史和普通缓存清理需按明确范围处理，不按整盘或整个profile删除。

## 16. 项目接入与配置

沿用internal/sandbox.Policy，内部编译ExecutionPlan；account、SID、journal、Git范围均由Host生成，不增加模型可控字段。Run继续区分runErr和cleanupErr，返回值补充后端/命令诊断。

bootstrap执行Recover、Probe；coretools根据实际后端提供工具描述。配置只提供一个默认后端选择、批准的外部读根/运行时与现有代理设置。网络/写入由现有逐命令字段表达，不再添加“configured/enabled/running”重复开关。

UI展示安装情况、当前health、最近错误和修复入口；不向普通用户展示SID/ACL实现选项。运行输出可显示读写/网络范围，便于判断授权影响。日志对账户秘密、API key、代理密码脱敏。

迁移时先保留旧AppContainer显式可选，新增后端通过完整验收后切换默认。任何后端失败均返回错误，不自动运行无沙箱命令。切换读取语义须更新既有tool contract/UI，并明确告知其系统公开资源边界。

非Windows平台继续当前不可用状态；以后实现对应平台backend共用policy与输出契约，不借Windows设计宣称跨平台已完成。

## 17. 实施与验收

开发分为安装身份、runner与控制通道、ACL与恢复、网络/Git/凭据、端到端接入五个实现单元，全部属于正式交付。每个单元先完成真实OS效果验证，再接入下一单元；不将半完成安装显示为healthy。

| 类别 | 必须验证的用户可见效果 |
| --- | --- |
| 安装 | 重复setup、升级失败补偿、账户登录、WFP readback；服务和Windows重启后仍能启动真实命令 |
| NUL/Git | 最终token读写NUL；Shell调用Git；status/diff/log、add/commit/checkout；宿主读回objects/refs/index |
| 仓库 | linked worktree、common-dir、submodule、LFS、config/hooks保护、Git原生锁 |
| 工具 | 真实Node/Python/Go构建及缓存、机器安装和用户目录安装、代理/证书、非零退出码 |
| 文件边界 | 只读时create/write/delete/rename失败；获准写入有持久效果；workspace外用户数据/控制数据写入失败 |
| 路径攻击 | junction/reparse/hardlink、父目录删除绕过、文件owner/DACL、受保护路径不存在后新建、卷和长路径 |
| 网络 | offline直接IPv4/IPv6 TCP/UDP、loopback、DNS/系统resolver；online可用；删代理变量不改变OS边界 |
| 并发 | 两工作区并行、只读/可写/online/offline混合、跨实例、scratch/进程/token互访失败、共享Git范围锁 |
| 进程 | 子孙继承token/Job、无breakaway、私有桌面、控制句柄不泄露；取消/超时/Host崩溃全部终止 |
| 恢复 | 在grant、运行、退出、清理阶段中断；重启移除全部自有权限；错误保留journal且不报告成功 |
| 凭据 | 未授权仓库不能调用broker；私钥不落scratch；Token不在argv/log；命令结束lease失效 |
| 状态 | 安装与实际健康不同步时显示真实错误；API/UI/工具描述与执行范围一致；输出capture归档读回 |

对绝对写范围、NUL兼容SID和DNS边界的承诺必须由上述实验与代码审查支撑，不能因为采用Codex相似架构就自动视为成立。未通过相应验收，交付状态为未完成/不可用；不降低安全承诺来通过验收。

## 18. 参考和本文权威范围

采用Codex Windows原生身份组合路线；参考其logon SID、OWNER RIGHTS和token基线做共享账户隔离。Claude Code产品沙箱与其开源Runtime的Windows alpha支持范围不同；Pi默认无内置沙箱。三者实现比较见调研文档，本文件是O Agent正式目标设计。

- [Microsoft CreateRestrictedToken](https://learn.microsoft.com/en-us/windows/win32/api/securitybaseapi/nf-securitybaseapi-createrestrictedtoken)：token、额外写检查及私有桌面要求。
- [Microsoft CreateProcessWithLogonW](https://learn.microsoft.com/en-us/windows/win32/api/winbase/nf-winbase-createprocesswithlogonw)：登录权限、profile及本地身份语义。
- [Microsoft WFP过滤条件](https://learn.microsoft.com/en-us/windows/win32/fwp/filtering-condition-identifiers-)：支持层与用户身份匹配。
- [Codex Windows官方说明](https://learn.chatgpt.com/docs/windows/windows-sandbox)：推荐原生低权限身份、文件与网络边界。
- [Codex token源码](https://github.com/openai/codex/blob/main/codex-rs/windows-sandbox-rs/src/token.rs)：每logon对象DACL及OWNER RIGHTS实现参考。实现时固定审核版本，保留许可证说明。
- [选型调研](windows-agent-sandbox-research.md)：来源与候选方案分析。

本文取代1.0版的全局串行、账号机器共享和将worktree/凭据支持留待后续的建议。设计文档已形成；系统账户、网络规则、runner和新后端尚未实施与验收。
