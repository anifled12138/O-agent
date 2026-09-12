# ADR 0005：单用户本地工作区

状态：Accepted

## 决策

O 本地产品不提供注册、登录、登出和 Cookie session。Web UI 或未来桌面客户端
连接 loopback/local socket 后直接进入工作台。

Go Host 在启动时解析一个持久的本地工作区所有者 ID。升级已有安装时复用最早创建的
账户 ID，从而保留 Provider、会话、Plugin、Generation 和评测数据；全新安装使用
`local-workspace`。数据库里的 `user_id` 暂时保留为工作区作用域键，避免进行高风险的
全库外键重写。

## 边界

取消登录不等于取消安全边界：

- HTTP Host 默认只监听 `127.0.0.1`；
- CORS 仍只允许配置的前端 Origin；
- Secret 继续只存在于加密 Vault；
- Plugin Grant、Broker、进程隔离和审计不变；
- 正式桌面客户端改用 OS 用户 ACL 保护 local socket。

如果未来需要多用户远程访问，应作为独立部署模式引入新的身份 Provider，不能重新把
登录页面硬编码进本地模式。
