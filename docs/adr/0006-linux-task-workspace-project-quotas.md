# ADR 0006：Linux 任务工作区硬磁盘配额

状态：Accepted；ext4/XFS 内核读写原语、root Unix socket helper、SQLite 唯一 ID 分配、新 workspace 创建接线及启动对账已实现并有持久化/失败路径测试；Ubuntu CI 已加入隔离 ext4/XFS loop mount 的内核拒写验收步骤，安全 scratch GC 已实现；CI 与目标 VPS 运行验收仍待取得执行结果

## 背景

Linux 沙箱已用 Bubblewrap 和 systemd/cgroup v2 限制每条命令的 CPU、内存、进程数和时长。Bubblewrap 把任务 worktree 以可写 bind mount 映入 `/workspace`，但 cgroup 不限制这类宿主文件系统写入；tmpfs 的内存上限也不限制工作区占用。仅轮询 `du` 或目录大小不能防止任务快速写满 VPS 磁盘。

云端有 Git 项目的执行任务现会使用独立 linked worktree/branch；无项目、非 Git 项目或关闭项目工作区的任务使用独立 scratch workspace。工作区路径与执行任务 ID 已绑定到 SQLite 持久记录，并在重启后读回；原项目 checkout 保留为基线和共享 Git object store。该隔离是目录与写入归属隔离，不是磁盘容量隔离：任务可以消耗所在文件系统的剩余空间，不能把它称为 per-task quota。

CloudWorker 将持久 `ExecutionTask` ID 传入 Agent turn 与 continuation；Agent service 已按任务准备独立 Git 或 scratch 工作区，并在任务完成前把工作区成果持久化为 artifact。`execution_task_workspaces` 持久记录 quota ID/limit/state，并使用唯一索引防止 ID 重用。Linux root helper 只接受 `SO_PEERCRED` 指定的 `oagent` UID、生成的 task workspace 相对路径和 apply/inspect/release 请求；cloud backend 启动时核验 helper socket 与挂载前置条件，准备 workspace 时分配并读回 quota。启动时会核查 ready workspace 的内核限额；不匹配会持久转为 degraded 且保留文件。preparing/degraded/released/releasing 状态的启动协调与带成果校验的 scratch workspace/quota GC 已实现并有持久化/失败路径测试；Ubuntu CI 和目标 VPS 的真实主机验收仍待执行。数据库中的 `applied` 只有在 backend 收到 helper 的内核读回后才写入；ready workspace 的持续一致性由启动检查再次确认。

## 决策

使用 Linux 文件系统 project quota 对每个云端任务 attempt 的独立可写 worktree 实施硬 block quota。支持并验收 ext4 和 XFS；云端数据所在文件系统必须启用 project quota（例如挂载选项 `prjquota`）。不支持或未启用 project quota 时，预检准确报告 `unavailable`，不得把使用量监控伪报为隔离配额。

配额只作用于该 attempt 的可写工作区。共享 Git object database、不可变内容寻址成果对象、用户已存在的源码仓库及可重建缓存不因该配额迁移、删除或限制；它们各自由存储容量及引用策略管理。配额不是 500MB 同步规则，不能拒绝本地任务、限制本地项目大小或限制上传批次数。

## 特权与执行顺序

1. 控制数据库为每个任务 attempt 持久分配唯一 project ID、规范化工作区相对路径、额度字节数和分配状态；O task quota ID 使用 `0x80000000` 至 `0xffffffff` 的高位命名空间，降低与发行版/运维常用低位 ID 冲突的风险，唯一索引防止 O 任务间共用 quota ID。主机管理员仍需将该范围保留给 O。
2. 云 Agent 在空的 task worktree 目录上调用一个 root 所有者的窄权限 quota helper。helper 不接受任意挂载点或绝对路径，只接受 `oagent` UID、固定配置的 workspace 根目录下的生成式相对目录名、quota ID 和不高于 root 配置上限的额度；路径通过 `O_NOFOLLOW` 逐段打开。task/attempt 到 quota ID 的归属由 Agent 从持久数据库记录中读取，helper socket 不提供通用 `sudo` 或任意命令接口。
3. helper 在 Git checkout/worktree 创建前，为目录设置 project ID 和目录继承标志、设置硬 block limit，并回读文件系统、目录 ID、继承标志及硬额度。任何一步失败都不创建或运行该 worktree。
4. quota 准备成功后才创建 Git worktree 和持久 Agent task。多步过程失败时回滚新建的 Git ref/worktree 与 quota reservation；回滚失败要留下可恢复的明确 degraded 记录，不能吞掉错误。
5. 到达硬额度的写入按文件系统行为返回 `EDQUOT`/`ENOSPC`。任务必须保留现场，不能报告成功，也不能自动删除其文件来腾空间。用户可在核实任务状态后转到本地节点继续或由管理员调整额度；资源错误状态和 Ubuntu 真机续接路径需实测验收。所有任务结果仍需经过原有 artifact/hash/commit 确认流程。
6. 仅当 task 已终态、没有活动租约或 checkpoint/恢复操作引用该 workspace、Git worktree 已读回移除，才可释放 project quota；释放状态需从 quota subsystem 和数据库双向核对。
7. 服务启动时对账 SQLite 分配记录、文件系统 project ID/limits 和任务 worktree。未知或不一致 allocation 保留证据并报告 degraded，不自动重置额度或删除目录。
8. 只回收超过 `O_CLOUD_TASK_WORKSPACE_RETENTION`（默认 720 小时）的已完成 cloud scratch workspace；设为 `0` 可停止启动新的自动回收，但此前进入 `releasing` 或被标记为 GC 失败的操作仍会恢复完成。回收前必须核验终态任务、无租约/取消/检查点、无 Project workdir 引用，以及已绑定并从对象存储重新验 hash 的 `workspace_output`。Git worktree 永不由此 GC 自动删除。数据库先进入 `releasing`，再移除精确任务派生目录、读回目录不存在、释放并读回 quota，最后写入 `released`。任何步骤失败都留下可恢复状态，重启后继续；不删除持久 artifact。

root helper 通过固定命令接口和 root-owned 配置授权给 `oagent`，不提供通用 `sudo`、任意 `setquota` 参数、命令字符串或任意文件操作。Quota backend 不在 Agent 命令沙箱内运行。安装器不能擅自重挂文件系统；管理员须按目标 VPS 的实际文件系统检查并安排启用 project quota。

## 故障语义

- 目标文件系统不支持 project quota：本机节点仍正常工作；云节点声明 quota 不可用，不接收需要该保证的云执行。Web 给出可执行节点和主机准备要求，不把它显示为可用的硬配额。
- quota 配置或读回失败：回滚 worktree 创建；若无法完整回滚，持久记录清晰的 degraded 状态和路径。
- 工作区达到硬额度：任务保存已有改动，标记磁盘资源不足；用户可在本地节点继续、归档可释放内容或调整管理员额度后续接。
- 服务重启或 quota 工具暂不可用：保留分配及磁盘数据；不假定 quota 自动生效，也不宣称 sandbox health 完整。

## 验收

- ext4 和 XFS 实机创建 32 MiB 硬额度，写入超过额度后由内核拒绝分配；同一文件系统中其他任务和共享成果仍能正常写入。
- 分配完成后重启控制服务，读取 task attempt、目录 project ID、继承标志与 `repquota` 硬额度，确认配额继续生效。
- 并发分配、ID 唯一性、同路径重复请求、目标已有文件、符号链接逃逸、未知文件系统、quota 工具错误和额度读回错误都有测试。
- Git worktree 创建失败、数据库提交失败、配额设置失败分别验证跨组件回滚/显式 degraded 状态。
- quota 释放测试确认正在运行、可继续检查点、pending artifact outbox/未完成成果和 Project 引用的工作区不会被清理；GC 删除前必须先从持久对象验证成果哈希。
- 仅当云主机磁盘写入得到内核硬限额且状态读回后，才在节点健康和产品说明中标记“任务工作区配额已启用”。

## 参考

- [Linux `quotactl(2)`：PRJQUOTA 与内核硬配额接口](https://manpages.ubuntu.com/manpages/jammy/man2/quotactl.2.html)
- [Ubuntu `setquota(8)`：项目配额参数](https://manpages.ubuntu.com/manpages/focal/man8/setquota.8.html)
- [Linux 内核 ext4 superblock 特性：quota 与 project quota](https://www.kernel.org/doc/html/latest/filesystems/ext4/super.html)
- [Linux 内核 XFS 文档：`pquota` / `prjquota` 挂载选项](https://docs.kernel.org/admin-guide/xfs.html)
- [mke2fs(8)：创建带 project quota 特性的 ext4 文件系统](https://man7.org/linux/man-pages/man8/mke2fs.8.html)
- [Linux `FS_IOC_SETFLAGS(2const)`：目录 project quota inheritance](https://www.man7.org/linux/man-pages/man2/FS_IOC_GETFLAGS.2const.html)
- [XFS project quota 配置示例](https://manpages.ubuntu.com/manpages/jammy/man8/xfs_quota.8.html)
