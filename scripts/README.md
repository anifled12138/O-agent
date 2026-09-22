# Agent Harness 打包工具指南 (Scripts)

本目录为 Agent Harness 的工程化便捷打包脚本集合，支持完整打包、极速构建、单独编译前端或后端。


---

## ⚠️ 核心开发准则：正常的插件与日常开发绝对不需要发版！

> **重要认知**：插件是挂载在宿主上的动态能力单位，**开发或修改插件绝不等于发布软件新版本**。
> 严禁在日常修改插件、微调功能时频繁运行打包命令（`npm run package` 或 `package.mjs`）。

### 1. 为什么正常插件不需要发版？
- **技能插件 (Skills)**：位于 `.skills/` 目录（如 `SKILL.md`）。系统在每次对话组织上下文时动态读取，改动**即刻生效，无需重启客户端，更不需要发版**。
- **MCP 服务型插件 (Model Context Protocol)**：作为外部进程（stdio / SSE）在「插件与能力」面板动态配置与启停，独立运行，**完全不需要客户端重新打包**。
- **核心功能插件与界面微调 (Core Plugins & Frontend/Backend)**：
  - 前端修改：执行 `npm run package:ui`（仅 ~200ms），在已打开的桌面客户端按 **`Ctrl + R`** 即可热刷新生效；
  - 后端修改：执行 `npm run package:backend` 编译单个 `o-host.exe` 覆盖本地目录，下次启动自动挂载；
  - **严禁随意整包打包**：全量打包不仅产生数百兆冗余文件，还会因 Windows 单例进程锁导致无法测试到最新代码。

### 2. 只有在以下严格限定的场景下才需要发版打包：
1. **对外正式发布版本**：需要向最终用户分发独立的桌面客户端安装包或便携绿色压缩包（`.zip`）时；
2. **升级底层桌面主进程**：修改了 Electron 主进程（`desktop/main.cjs`）、预加载脚本（`desktop/preload.cjs`）或窗口架构时；
3. **生成初次离线运行环境**：在一台没有开发环境的全新机器上，初次生成完整的绿色免安装 `O-win32-x64` 执行套件时；
4. **用户明确且主动要求**：用户给出了“生成发布用安装包”、“打一个便携压缩包”等明确指示时。

---

## 常用命令

### 1. 完整桌面端打包（推荐发布时使用）
编译前端 UI + 编译 Go 后端 + 封装 Electron 独立桌面程序 + 生成绿色便携 Zip：
```bash
# 方式 A: 根目录 npm
npm run package

# 方式 B: 直接运行脚本 (Windows CMD / 双击)
build.bat
# 或
.\scripts\build.bat

# 方式 C: PowerShell
.\scripts\build.ps1
```

### 2. 快速生成绿色版（推荐日常测试使用）
跳过耗时的 zip 压缩与安装包制作，**仅需 3~5 秒**直接输出可执行程序目录：
```bash
npm run package:fast
# 或
.\scripts\build.bat --fast
# 或
.\scripts\build.ps1 -Fast
```
产物位于 `release/O-0.1.0-xxxx/O-win32-x64/O.exe`，双击即可直接启动运行。

### 3. 仅构建前端 UI
修改了前端 React/CSS 代码后，快速重新生成桌面端静态资源（仅需 ~200ms）：
```bash
npm run package:ui
# 或
.\scripts\build.bat --ui
```

### 4. 仅编译后端二进制
修改了 Go 代码后，重新编译生成 `o-host.exe`：
```bash
npm run package:backend
# 或
.\scripts\build.bat --backend
```

### 5. 清理历史构建产物
释放 `release/` 占用的磁盘空间：
```bash
npm run package:clean
# 或
.\scripts\build.bat --clean
```

---

## 产物结构说明
打包成功后，产物统一输出在项目根目录的 `release/` 文件夹下：
```text
release/
└── O-0.1.0-20260920TxxxxxxZ/
    ├── O-win32-x64/                  # 绿色免安装直接运行目录
    │   ├── O.exe                     # 桌面主程序
    │   └── resources/app/runtime/
    │       ├── o-host.exe            # 后端 Go 引擎
    │       └── ui/                   # 前端静态包
    └── O-win32-x64-portable.zip      # 一键分发便携压缩包
```
