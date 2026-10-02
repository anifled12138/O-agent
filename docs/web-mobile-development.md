# O Web / PWA 手机入口

> Web 入口专题。当前多设备、仓库项目、500MB 传输分批策略、归档与云接续的统一设计见 [O 云端优先完整方案](o-cloud-first-architecture.md)。本文描述的本地 Web 基础不代表云端方案已实施。

日期：2026-10-01。用户已选择 Web 页面作为 Android 和 iPhone 的首版入口，替代原生 App 优先路线。当前代码提供本地 Web / PWA 基础；公网登录、设备通道、Linux 执行、共享成果及本地/云端交接仍需实施和验收，不能因页面可打开就标为上线。

## 使用方式

Android、iPhone 和电脑使用同一个 HTTPS 网站。iPhone 在 Safari 的分享菜单中添加到主屏幕；Android 在 Chrome 菜单中安装或添加到主屏幕。Manifest 使用 `standalone`，安装后从图标打开工作台。不需要办理 Apple 开发者会员或上传 App Store。[WebKit 主屏幕 Web App 说明](https://webkit.org/blog/13878/web-push-for-web-apps-on-ios-and-ipados/)。

手机只作为控制入口，Agent 在指定的本地电脑或云电脑上执行。浏览器关闭不应成为任务生命周期的依据；常驻 Host、持久任务和恢复机制要在后端验收。当前 Electron 退出会关闭其拥有的 Host，远程模式必须另行部署常驻进程。

## 本次代码范围

- 复用 O 的真实会话、模型配置、任务事件、审批、取消和现有工具界面，不建立另一个模拟任务系统。
- 手机侧栏改为抽屉，包含关闭按钮、Escape、焦点限制和背景不可交互；收起侧栏不留可聚焦的隐藏按钮。
- 小屏输入框字号、按钮触控区域、换行与安全区域适配；触控设备使用发送按钮，Enter 换行，桌面保留 Enter 发送，输入法组词不触发发送。
- Web API 默认使用网页同域的 `/api/v1` 与 `/api/v2`；桌面仍使用 preload 提供的私有 API 地址。默认同域使 Cookie 认证和 EventSource 使用同一入口。
- 安装清单、普通/可裁剪图标、Apple 图标和安装说明；只使用浏览器实际提供的安装能力，不根据“同意安装”显示成功。
- Service Worker 仅持久缓存通用离线提示页，正常导航走网络；API、事件流、非导航文件和所有写请求不由它处理，不缓存私密会话 HTML。它不提供离线任务队列。缓存失败会拒绝安装并显示帮助；缺失离线页时返回明确 503。

Web Push 尚未实现。后续可增加授权后订阅、服务端绑定/撤销、有限状态通知和重新认证回读。iOS/iPadOS 16.4+ 的主屏幕 Web App 可使用 Web Push，需用户交互授权，不要求 Apple Developer 会员；不能依赖手机后台持续维持 SSE。[WebKit Web Push](https://webkit.org/blog/13878/web-push-for-web-apps-on-ios-and-ipados/)。

## 本地开发

从 `frontend/` 运行 `npm run dev`，默认预览 `http://127.0.0.1:3000/`。Go Host 仍监听环回 `127.0.0.1:9171`；开发服务器代理 `/api` 到该端口，保留浏览器 Origin，Host 的 `O_FRONTEND_ORIGIN` 应与预览地址一致。

`O_WEB_API_PROXY` 可覆盖开发代理目标。`NEXT_PUBLIC_API_URL` 是兼容现有环境的构建期覆盖项，接受完整 URL 或以 `/` 开头的路径；生产推荐不设置，统一走同域网关。网页、SSE 和文件都通过这一网关。此开发代理仅用于 `vinext dev`，不是生产部署服务。

执行 `npm run typecheck`、`npm run lint`、`npm run test:web`、`npm run build`。共用屏幕影响 Electron，因此也构建 `npm run build:desktop-ui`。自动检查不能代替 Android/iPhone 的真实安装、软键盘、浏览器切后台和恢复测试。

## 公网部署的实际链路

```text
手机 HTTPS → 身份网关 → Web 页面
                    → /api/* → 授权路由 → 本地 Bridge / 云端 Host
```

当前 API 是单用户本地接口，没有公网登录与设备权限。服务器转发配置必须先接好身份校验、会话注销、CSRF/Origin 检查、受控下载，再对外提供；不能仅把 9171 转发到公网。运行控制服务、数据和管理 SSH 密钥与 Agent 的 Linux 工作区隔离。

生产网关需保留或按受信代理策略校验 Origin；Go Host 配置正式 HTTPS Origin。`/api/v2/.../events` 关闭缓冲、支持长连接与事件游标，不用普通页面的短超时。API、带身份的 HTML 和成果下载设置 `Cache-Control: no-store`，禁止 CDN 缓存；`sw.js` 禁止长期缓存。静态文件根据版本缓存。身份过期时事件连接停止重试并回到登录；不要通过改写 Origin 绕过身份校验。

生产 UI 可通过现有 vinext 服务部署到环回地址，前置 HTTPS 网关代理。当前工程同时有 Cloudflare 构建插件，Ubuntu 部署必须实测生产启动及 `/api`、静态路径，不能将本地开发成功等同生产启动成功。原生客户端构建不在本阶段交付范围。

离线提示页、`sw.js`、manifest 和图标作为不含用户信息的静态资源公开提供；其余页面和 API 均按身份网关策略控制。Service Worker 安装时不携带凭据，拒绝重定向，并验证离线页标记，网关错误返回登录页或工作台 HTML 时不得缓存。

## 后续验收

1. Android / iPhone 不同网络登录同一 HTTPS 入口，添加到主屏幕后真实启动；安装不是下载 IPA。
2. 手机向在线本地电脑提交文件任务；回读会话并确认真实文件、哈希和位置。停止任务后确认进程终止。
3. 手机关闭页面、切网络、执行电脑重启后，从持久日志恢复状态，不双发或误报完成。
4. 云电脑上线后，用同一会话选择电脑；停止旧执行归属、同步输入和成果版本后再启动目标。接口尚未实现前，不展示可操作的假切换。
5. 无网络不提交、不批准；恢复后读取最新服务端状态。退出账号后旧会话、事件和下载链接不可用；离线缓存不含用户数据。
6. HTTPS、网关权限、SSE、主屏幕安装和生产服务都实测通过后，才称为可从手机远程使用。
