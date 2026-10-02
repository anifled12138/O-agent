# O 原生 Android / iPhone App 备选方案与 Apple 分发参考

日期：2026-10-01。用户最新选择为 Web / PWA，当前实施路线见 [Web 手机入口](web-mobile-development.md)。本文保留原生开发与个人安装参考，不是当前交付要求；未生成安装包或提交发布。

## 1. 未来如需原生 App 的备选范围

Android 和 iPhone 同时支持。手机共用同一个 O 后端，向本地电脑与持久 Linux 云电脑发起任务；同一会话可切换执行电脑，共享产物、事件和审批。手机断线或 App 退出不终止服务器/本机上的工作。

推荐 React Native + Expo + TypeScript。复用现有 React 技术经验和提取后的 API SDK；手机界面使用原生组件，现有 Electron、DOM、CSS 和浏览器特有 API 不直接照搬。iPad 后续适配；macOS 桌面与 iOS 的构建、签名和交付分别管理。

## 2. 从 Windows 开发 iOS

| 环节 | 当前 Windows 环境 | 推荐方式 |
| --- | --- | --- |
| TypeScript、React Native 界面和业务逻辑 | 可以 | 本地开发、代码检查、真机开发客户端 |
| Android 开发和构建 | 可以配置原生工具链；云构建也可用 | 初期 EAS，后续按需要配置 Android Studio |
| iOS 编译、签名 | 需要 macOS + Xcode 环境 | 从 Windows 发起 EAS 云构建 |
| iOS 模拟器、原生调试 | 需要 Mac | 先用真实 iPhone；后续自有或远程 Mac |
| TestFlight 提交 | 可从当前环境调用云提交工具 | App Store Connect + 签名构建 |
| O Agent 后端运行 | 当前 Ubuntu 云服务器承担 | 与 iOS 编译环境分开 |

EAS 的 iOS 构建实际启动 macOS VM 并使用 Xcode 工具链；Windows 或 Ubuntu 不会因此获得本地 Xcode 编译能力。[Expo iOS 构建流程](https://docs.expo.dev/build-reference/ios-builds/)、[Apple Xcode 系统要求](https://developer.apple.com/xcode/system-requirements)。

初期不用为了 React Native 原型立即购置 Mac。后续涉及复杂原生插件、签名问题或扩展开发时，Mac 会明显改善调试体验。云构建服务的当期限额和费用需在开始构建时核对，不预设无限免费额度。

## 3. 账号与签名

- 用户持有 Apple Developer Program 的个人或组织账号，启用双重认证；与 Expo/EAS 账号分别管理。
- 确定唯一 iOS Bundle ID 和 Android application ID；开发/生产 App 可用不同标识，正式标识不要随意更换。
- 创建 App Store Connect 应用记录，配置签名证书、Provisioning Profile 与推送能力。
- EAS 可选择托管签名材料，也可由用户保管并接入自有 macOS CI。实施时明确选择，签名私钥不写仓库或业务数据库。
- 组织注册按 Apple 要求完成实体核验；本方案不会替用户注册、购买或代表其接受协议。

Apple 开发者会员为 99 USD/年，按地区显示当地币种价格。完整测试/分发路线需符合会员与签名要求。[Apple 注册要求](https://developer.apple.com/programs/enroll/)。原型使用 Expo Go 可验证兼容功能，但不能替代签名 App 的推送、后台和原生验收。[Expo 构建准备](https://docs.expo.dev/build/setup/)。

## 4. 工程结构与后端接口

建议新增 `mobile/` 和 `packages/o-client/`；后者包含 API 类型、任务/会话/文件契约、认证接入、游标及错误映射。共享 SDK 使用平台无关 transport 接口，Web/Electron 和 React Native 分别实现事件连接与凭据存储。

手机只连接云端 HTTPS 域名。云端将请求按授权目标路由到本地主动通道或云电脑；手机不持有服务器管理员密码，不直接连本机数据库，不依赖 LAN 可达。

所需后端能力：登录及刷新/撤销、手机设备注册、电脑能力列表、幂等提交、共享会话日志、增量事件、审批 pending-only CAS、取消状态回读、同会话切换、成果元数据和授权下载、推送 token 注册/撤销。

原生登录使用系统浏览器及 PKCE。刷新令牌采用安全存储，读写失败返回错误并恢复登录，不能写入日志；服务端撤销后必须拒绝旧令牌，不以 App 本地“已退出”代替撤销。[SecureStore](https://docs.expo.dev/versions/latest/sdk/securestore/)。

## 5. 第一版功能与状态

| 功能 | 用户体验 | 权威状态和验证 |
| --- | --- | --- |
| 发任务 | 输入文字、附文件、选择电脑 | 幂等提交，服务端持久化后才显示已提交 |
| 看进度 | 共享会话、自然语言进度、工具记录 | 事件游标补读，覆盖所有终态 |
| 切换电脑 | 原会话选择本地/云端 | 停止确认、版本同步、目标读取后才显示可继续 |
| 审批 | 精确动作及参数，一次允许/拒绝 | 认证用户、固定版本、pending-only 状态回读 |
| 取消 | 立即显示停止中 | 目标进程确认停止后显示已取消 |
| 成果 | 预览、下载、系统分享 | 版本、内容哈希、权限、下载完成确认 |
| 浏览器查看 | 当前画面、页面信息、受控接管 | 页面归属锁；用户与 Agent 不同时操作 |
| 通知 | 完成、失败、需要审批时提示 | 通知只提示；打开后向后端回读 |

手机离线时输入为草稿，不能假装已排队。向离线本地电脑发任务，需要明确等待及过期策略。云端输入未准备好时不能立即改派。敏感审批通知可仅显示“需要你的处理”，不将工具参数或会话内容放入通知载荷。

## 6. 推送与后台

iOS 使用 APNs，Android 根据设备和发行地区使用兼容推送渠道，初期可选 Expo Push 对接 APNs/FCM。无可用推送通道时保留 App 内补读，不能承诺关闭 App 后立即通知。Android 不同设备与地区的 FCM 可达性需实际验证；如需国内厂商渠道，作为单独原生适配工作。

通知来自持久化事件的 notification outbox，可重试且去重；服务商接受通知不等于手机已展示。失效 token 清理、用户关闭通知、App 被系统停止、网络切换和迟到事件均需要处理。App 不靠后台永远运行来推动任务。[Expo 推送说明](https://docs.expo.dev/push-notifications/overview/)。

证书及推送 token 有撤销/轮换流程；退出账号、手机丢失或删除设备时取消绑定。通知和崩溃 SDK 的第三方数据路径进入隐私声明。

## 7. 开发与构建顺序

1. 固定最小兼容 API，建立手机工程及共用 SDK；登录后验证授权电脑列表。
2. 完成发任务、共享会话、事件流及文件查看，连接真实开发后端。
3. 为 Android/iOS 配置 development、preview、production 构建配置及唯一标识。
4. 在签名 development build 上接入推送、安全存储、文件分享、深链接。
5. 构建 Android 内测 APK 和 iOS TestFlight 包；打包范围排除 vault、数据目录、服务器凭据和私人工作文件。
6. 使用真实 Android 和 iPhone 完成端到端验收与崩溃收集，修复后冻结候选版本。
7. 如需公开分发，分别准备商店材料和审核；提交后跟踪实际可安装状态。

工程准备完成后的命令示意，当前未执行；需先准备项目、账号和签名：

```text
npx eas-cli@latest build:configure
npx eas-cli@latest build --platform all
npx eas-cli@latest submit --platform ios
```

内测 APK 类型及 iOS profile 在构建配置中分别声明；`--platform all` 不意味着自动得到两台手机均可安装的包。App Store Connect 收到构建后还要处理、选定分发并验证安装。[EAS Build](https://docs.expo.dev/build/setup/)、[TestFlight](https://developer.apple.com/testflight/)。

## 8. Apple 测试和上架

先使用 TestFlight 验证 iPhone 版本；内部测试与外部测试的访问角色及审核要求不同，正式发布再提交 App Store Review。[Apple TestFlight](https://developer.apple.com/testflight/)。

发布材料覆盖图标、截图、描述、支持/隐私地址、可用的审查账号和在线后端。以原生任务控制、成果分享、审批与通知体现 App 用途，不只封装网站。提供账号创建时需支持 App 内账号删除；按实际 SDK、AI 数据传输和权限填写声明与同意流程。完整桌面/云软件投屏按 4.2.7 等条款另行评估，不能预先保证审核结果。[Apple 审核规则](https://developer.apple.com/app-store/review/guidelines/)。

## 9. 验收边界

同时在 Android 和 iPhone 验证：用手机向本地发任务并读回真实文件变化；向云电脑发任务并关闭 App，再重新打开读取完成成果；原会话跨电脑切换；审批拒绝/重复/过期；取消确认；文件传输中断；手机网络超时下幂等去重；断线游标恢复；推送缺失后的状态回读；令牌撤销和重启。

配置、配对和凭据存储需有重启持久化测试；切换/登录迁移/多步成果提交要有失败与回滚测试。模拟接口只能用于开发，不算上述真实效果的验收。

方案当前完成，安装包、签名、真机验证、账号办理和发布尚未执行。

## 10. 个人安装：不需要先公开上架

以下为 2026-10-01 查阅官方资料后的安装参考。当前已按用户决定改为 Web / PWA 优先，不办理原生签名或会员。

| 方式 | 适用情况 | 费用和维护 |
| --- | --- | --- |
| 免费 Personal Team / Xcode | 自己真机开发测试 | 免费签名；profile 7 天到期，需要重新构建安装；Xcode 需 Mac |
| AltStore Classic + AltServer | Windows 上安装自己的已有 IPA | 免费账号可用，需定期刷新；不能替代 macOS 编译与原生能力配置 |
| Ad Hoc + EAS 内部分发 | 自己及已登记的少量测试手机 | 付费 Apple Developer 账号；登记 UDID；维护签名有效期，无需公开上架 |
| TestFlight | 持续内测、较方便地邀请测试者 | 付费开发者账号；每个 build 最多 90 天；外部测试按规则审核 |
| 主屏幕 Web App / PWA | 先低成本使用远程任务入口 | 无 Apple 开发者会员及上架要求；Web 能力与原生能力分别评估 |

Apple 免费 Personal Team 目前有 7 天 profile 有效期和每台设备最多 3 个测试 App 等限制，不能视为一次安装后永久可用。[Apple 免费真机测试规则](https://developer.apple.com/help/account/basics/about-your-developer-account)。AltStore Classic 支持 Windows 安装，并可尝试后台刷新，但免费签名仍受 7 天有效期约束，不能承诺永远自动续签。[AltStore Windows 安装](https://faq.altstore.io/altstore-classic/how-to-install-altstore-windows)、[刷新规则](https://faq.altstore.io/altstore-classic/your-altstore)。

原生 O 的个人内测优先考虑 Ad Hoc：登记用户的 iPhone → 创建包含该 UDID 的 profile → 云端签名构建 → 从内部安装链接安装 → 真机验证。EAS 设置 `distribution: internal`，新手机要进入签名 profile 后才可安装，不以二维码登记或云构建成功代替验证。安装页面可要求授权账号登录，签名私钥只交给受控构建系统。[Apple Ad Hoc](https://developer.apple.com/help/account/provisioning-profiles/create-an-ad-hoc-provisioning-profile/)、[Expo 内部分发](https://docs.expo.dev/build/internal-distribution/)。

TestFlight 适合迭代期间的测试，但单个构建 90 天后失效，需要上传新构建；不当作永久私人安装渠道。[Apple TestFlight 有效期](https://developer.apple.com/help/app-store-connect/test-a-beta-version/testflight-overview/)。

若暂不购买会员，可先提供额外 PWA 入口：Safari 打开 O 的 HTTPS 网站，选择“添加到主屏幕/作为 Web App 打开”；采用原生 App 共用后端实现任务、会话、文件和审批。iOS/iPadOS 16.4 起支持主屏幕 Web App 推送，需用户交互授权，不要求 Apple Developer 会员；前台连接和后台推送分别实现并真机验证。原生分享扩展及后台能力仍按原生路线开发，不将 PWA 宣称为完整原生 App。[Apple 主屏幕 Web App](https://support.apple.com/guide/iphone/open-as-web-app-iphea86e5236/ios)、[WebKit Web Push](https://webkit.org/blog/13878/web-push-for-web-apps-on-ios-and-ipados/)。

当前选择：Android 和 iPhone 共用 Web / PWA；本文的 React Native、Ad Hoc 和 TestFlight 作为将来需要原生能力时的参考。当前未办理会员、原生构建或安装。
