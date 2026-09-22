# AGENTS.md — Agent Harness 打包与构建工具使用指南

> **适用对象**：后续接手此仓库的 AI Agent（Codex、Claude Code、Cursor、Windsurf 等）及人类开发者。  

> 🛑 **CRITICAL RULE FOR AGENTS（所有接手 Agent 必读）**：
> **正常的插件与功能开发绝对不需要发版打包！**  
> 严禁在完成插件修改或日常 Bug 修复后盲目调用 `node scripts/package.mjs` 或 `npm run package:fast`！  
> - Skills 插件修改：只需改动 `.skills/` 对应文件，热加载即时生效；  
> - MCP 插件修改：更新配置或服务代码即可，完全独立于桌面包；  
> - 前端 UI 修改：仅需 `npm run package:ui`，通知用户在桌面窗口按 `Ctrl + R` 热刷新；  
> - 后端修改：仅需 `npm run package:backend` 编译单个二进制；  
> **只有在用户明确要求“制作发布包”或交付全新独立安装包时，才允许执行全量发版打包！**

> **核心目标**：指导 Agent 在完成代码修改后，以最高效、最正确的方式调用打包工具，避免盲目全量打包或遗漏产物同步。

---

## 1. 打包工具箱速查表

在项目根目录或任意终端中，优先推荐使用 Node.js 直接执行或调用根目录的批处理脚本：

| 目标场景 | 推荐命令 | 耗时 | 产物位置与作用 |
| :--- | :--- | :--- | :--- |
| **仅修改了前端代码** (UI/CSS/React) | `npm run package:ui`<br>或 `node scripts/package.mjs --ui` | ~200ms | `frontend/dist-desktop`<br>更新桌面端静态资源 |
| **仅修改了后端代码** (Go/API/Axiom) | `npm run package:backend`<br>或 `node scripts/package.mjs --backend` | ~300ms | `desktop/.runtime/package/o-host.exe`<br>更新独立后端执行体 |
| **日常开发/测试快速出包** (推荐) | `npm run package:fast`<br>或 `build.bat --fast` | ~2-3s | `release/O-0.1.0-xxxx/O-win32-x64/O.exe`<br>生成绿色免安装目录，可直接双击启动验证 |
| **全量发布交付打包** (包含 Zip) | `npm run package`<br>或 `build.bat` | ~15-25s | `release/O-0.1.0-xxxx/`<br>包含完整程序目录与 `O-win32-x64-portable.zip` |
| **磁盘空间清理** | `npm run package:clean`<br>或 `build.bat --clean` | <1s | 清理历史 `release/O-*` 过期产物 |

---

## 2. Agent 决策树（什么时候该调哪个命令？）

后续 Agent 在修改代码后，**请严格按照下列决策树选择命令**，不要盲目全量打包：

```text
你修改了代码:
  ├─ 仅修改了 frontend/ 下的文件 (如 OApp.tsx, globals.css, 组件等)
  │    └─► 执行: node scripts/package.mjs --ui (或 npm run package:ui)
  │         [原因]: 极速增量构建，避免重新编译 Go 与打包 Electron，约 200ms 完成。
  │
  ├─ 仅修改了 backend/ 下的文件 (如 main.go, agent/, provider/ 等)
  │    └─► 执行: node scripts/package.mjs --backend (或 npm run package:backend)
  │         [原因]: 仅调用 Go 编译器输出 o-host.exe，约 300ms 完成。
  │
  ├─ 用户要求“打包看看”、“重新打包”、“生成安装包/桌面版”或修改了多个端需要整体验证
  │    └─► 执行: node scripts/package.mjs --fast (或 npm run package:fast)
  │         [原因]: 3秒内完成前端+后端+Electron打包，输出 O-win32-x64，且跳过耗时且易因权限/流关闭失败的 Squirrel 安装包。
  │
  └─ 用户明确要求“生成发布用的 zip 压缩包”或“正式发版”
       └─► 执行: node scripts/package.mjs (或 npm run package)
            [原因]: 全量流程，生成便携 distribution zip。
```

---

## 3. 架构与依赖链路说明

打包脚本内部的处理流水线如下：

```text
1. [前端构建]
   Vite (vite.desktop.config.ts) ──► frontend/dist-desktop (纯静态 HTML/CSS/JS)

2. [后端编译]
   Go Compiler (CGO_ENABLED=0) ──► desktop/.runtime/package/o-host.exe

3. [Electron 封装]
   @electron/packager (读取 desktop/) ──► release/O-0.1.0-<TIMESTAMP>/O-win32-x64
   ├── 将 o-host.exe 复制到 resources/app/runtime/o-host.exe
   └── 将 dist-desktop 复制到 resources/app/runtime/ui/

4. [便携压缩 (可选)]
   Python shutil.make_archive ──► release/O-0.1.0-<TIMESTAMP>/O-win32-x64-portable.zip
```

### 环境依赖自检与回退机制
* **Go 编译器**：脚本会优先寻找本地预装好的工具链 `work/toolchains/go/bin/go.exe`，若无则自动 fallback 到系统 PATH 中的 `go`。
* **Node.js**：使用当前执行环境的 `node`，兼容系统 PATH 与常见默认安装路径（如 `D:\node.js\node.exe`）。
* **Electron Packager**：脚本直接使用 `createRequire` 复用 `desktop/package.json` 下已安装的 `@electron/packager`，**无需在根目录重复安装重量级依赖**。

---

## 4. 关键注意事项与避坑指南 (Pitfalls for Agents)

1. **优先使用 Node.js 直接调用**：
   在 Windows 环境下，PowerShell 的 ExecutionPolicy 限制可能阻止 `.ps1` 脚本运行。在 Agent 执行自动化指令时，优先使用：
   ```bash
   node scripts/package.mjs [参数]
   ```
   或调用 `build.bat [参数]`，稳定且不受权限限制。

2. **不要直接修改 `desktop/.runtime/package/`**：
   该目录是自动生成的临时产物。修改后端代码请在 `backend/` 下进行，然后运行 `--backend` 重新生成。

3. **桌面端与 Web 端的区别**：
   * 桌面端打包使用的是 `vite build --config vite.desktop.config.ts`，输出至 `frontend/dist-desktop`；
   * Web 独立运行模式（Next/Vinext）使用的是 `frontend/dist`。
   * 打包桌面端时切勿运行成 `npm run build:web`，务必使用 `npm run package:ui` 或打包脚本的 `--ui`。

4. **历史构建物管理**：
   每次打包都会生成一个新的带时间戳的子文件夹（如 `release/O-0.1.0-20260920T151941Z`）。如果频繁测试打包导致磁盘占用过大，可执行 `node scripts/package.mjs --clean` 进行清理。
