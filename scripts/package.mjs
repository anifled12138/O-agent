#!/usr/bin/env node
/**
 * Agent Harness 一键打包工具
 * 支持全量打包、快速构建、单独编译前端或后端。
 */
import fs from 'node:fs/promises';
import { existsSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawn, execSync } from 'node:child_process';
import { createRequire } from 'node:module';

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);
const rootDir = path.resolve(__dirname, '..');
const desktopDir = path.join(rootDir, 'desktop');
const frontendDir = path.join(rootDir, 'frontend');
const backendDir = path.join(rootDir, 'backend');

// 从 desktop 目录解析依赖 (如 @electron/packager)
const desktopRequire = createRequire(path.join(desktopDir, 'package.json'));

// 颜色输出辅助函数
const colors = {
  reset: '\x1b[0m',
  bold: '\x1b[1m',
  dim: '\x1b[2m',
  green: '\x1b[32m',
  blue: '\x1b[34m',
  cyan: '\x1b[36m',
  yellow: '\x1b[33m',
  red: '\x1b[31m',
};

function logStep(step, total, title) {
  console.log(`\n${colors.bold}${colors.cyan}[${step}/${total}]${colors.reset} ${colors.bold}${title}${colors.reset}`);
}

function logSuccess(msg) {
  console.log(`${colors.green}✔ ${msg}${colors.reset}`);
}

function logInfo(msg) {
  console.log(`${colors.dim}  ℹ ${msg}${colors.reset}`);
}

function logError(msg) {
  console.error(`${colors.red}✖ ${msg}${colors.reset}`);
}

function printUsage() {
  console.log(`
${colors.bold}Agent Harness 一键打包工具${colors.reset}
${colors.yellow}⚠️  重要提示: 正常的插件开发 (Skills/MCP/Core) 纯热加载生效，绝对不需要发版打包！只有对外正式发布或交付完整客户端时才使用本工具。${colors.reset}

${colors.bold}用法:${colors.reset}
  node scripts/package.mjs [选项]
  .\\scripts\\build.bat [选项]
  .\\scripts\\build.ps1 [选项]

${colors.bold}选项:${colors.reset}
  ${colors.cyan}(默认无参数)${colors.reset}       完整打包: 前端 + 后端 + Electron 封装 + 便携绿色包 (.zip)
  ${colors.cyan}--fast, -f${colors.reset}         快速打包: 生成 O-win32-x64 可直接运行目录，跳过较慢的压缩与安装包
  ${colors.cyan}--ui, -u${colors.reset}           仅构建前端桌面 UI (输出到 frontend/dist-desktop, 约 200ms)
  ${colors.cyan}--backend, -b${colors.reset}      仅编译 Go 后端二进制服务 (输出到 desktop/.runtime/package/o-host.exe)
  ${colors.cyan}--skip-installer${colors.reset}   跳过 Squirrel 安装程序打包，仅生成便携应用与 Zip
  ${colors.cyan}--clean${colors.reset}            清理历史 release/ 构建物
  ${colors.cyan}--help, -h${colors.reset}         显示帮助信息

${colors.bold}快捷 npm 命令:${colors.reset}
  npm run package            # 完整发布打包
  npm run package:fast       # 快速生成绿色版
  npm run package:ui         # 仅构建前端
  npm run package:backend    # 仅编译后端
`);
}

function findGo() {
  const customToolchain = path.join(rootDir, 'work', 'toolchains', 'go', 'bin', process.platform === 'win32' ? 'go.exe' : 'go');
  if (existsSync(customToolchain)) return customToolchain;
  try {
    const sysGo = execSync(process.platform === 'win32' ? 'where go' : 'which go', { encoding: 'utf8' }).trim().split('\n')[0].trim();
    if (sysGo && existsSync(sysGo)) return sysGo;
  } catch {}
  return process.platform === 'win32' ? 'go.exe' : 'go';
}

function run(command, args, cwd, extraEnv = {}) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, {
      cwd,
      stdio: 'inherit',
      shell: process.platform === 'win32',
      env: { ...process.env, ...extraEnv },
    });
    child.on('exit', (code) => {
      if (code === 0) resolve();
      else reject(new Error(`${command} ${args.join(' ')} 退出，错误码: ${code}`));
    });
  });
}

async function cleanRelease() {
  const releaseDir = path.join(rootDir, 'release');
  if (!existsSync(releaseDir)) {
    logInfo('release 目录不存在，无需清理。');
    return;
  }
  const items = await fs.readdir(releaseDir);
  logInfo(`正在清理 ${items.length} 个历史打包构建...`);
  for (const item of items) {
    const itemPath = path.join(releaseDir, item);
    await fs.rm(itemPath, { recursive: true, force: true }).catch(() => {});
  }
  logSuccess('清理完成！');
}

async function syncUIToReleases() {
  const releaseDir = path.join(rootDir, 'release');
  if (!existsSync(releaseDir)) return;
  try {
    const items = await fs.readdir(releaseDir);
    for (const item of items) {
      const candidateWin = path.join(releaseDir, item, 'O-win32-x64', 'resources', 'app', 'runtime', 'ui');
      const candidateMac = path.join(releaseDir, item, 'O.app', 'Contents', 'Resources', 'app', 'runtime', 'ui');
      for (const cand of [candidateWin, candidateMac]) {
        if (existsSync(path.dirname(cand))) {
          await fs.cp(path.join(frontendDir, 'dist-desktop'), cand, { recursive: true });
          logInfo(`已同步前端 UI 到本地应用目录: ${cand}`);
        }
      }
    }
  } catch {}
}

async function buildFrontendUI() {
  const npmCmd = process.platform === 'win32' ? 'npm.cmd' : 'npm';
  await run(npmCmd, ['run', 'build:desktop-ui'], frontendDir);
  await syncUIToReleases();
  logSuccess('前端桌面 UI 构建完成 -> frontend/dist-desktop');
}

async function buildBackendBinary() {
  const goBin = findGo();
  const backendOutput = path.join(desktopDir, '.runtime', 'package', process.platform === 'win32' ? 'o-host.exe' : 'o-host');
  await fs.mkdir(path.dirname(backendOutput), { recursive: true });

  await run(goBin, ['build', '-trimpath', '"-ldflags=-s -w"', '-o', backendOutput, './cmd/axiom'], backendDir, {
    ...process.env,
    CGO_ENABLED: '0',
    GOCACHE: path.join(rootDir, '.gocache'),
    GOPATH: path.join(rootDir, '.gopath'),
  });

  logSuccess(`后端服务编译完成 -> ${backendOutput}`);
  return backendOutput;
}

async function main() {
  const args = process.argv.slice(2);

  if (args.includes('--help') || args.includes('-h')) {
    printUsage();
    return;
  }

  if (args.includes('--clean')) {
    await cleanRelease();
    return;
  }

  const isUIOnly = args.includes('--ui') || args.includes('-u');
  const isBackendOnly = args.includes('--backend') || args.includes('-b');
  const isFast = args.includes('--fast') || args.includes('-f');
  const skipInstaller = isFast || args.includes('--skip-installer');

  const startTime = Date.now();

  if (isUIOnly) {
    logStep(1, 1, '构建前端桌面 UI');
    await buildFrontendUI();
    console.log(`\n${colors.green}${colors.bold}✔ 前端构建耗时: ${((Date.now() - startTime) / 1000).toFixed(2)}s${colors.reset}`);
    return;
  }

  if (isBackendOnly) {
    logStep(1, 1, '编译 Go 后端服务');
    await buildBackendBinary();
    console.log(`\n${colors.green}${colors.bold}✔ 后端编译耗时: ${((Date.now() - startTime) / 1000).toFixed(2)}s${colors.reset}`);
    return;
  }

  // 全量打包流程
  const totalSteps = isFast ? 3 : 4;

  // 1. 前端 UI
  logStep(1, totalSteps, '构建前端桌面端资源 (Vite + React 19)');
  await buildFrontendUI();

  // 2. 后端二进制
  logStep(2, totalSteps, '编译 Go 后端服务 (Axiom Agent Runtime)');
  const backendOutput = await buildBackendBinary();
  const backendName = path.basename(backendOutput);

  // 3. Electron 打包
  logStep(3, totalSteps, '使用 Electron Packager 封装独立桌面程序');
  const timestamp = new Date().toISOString().replace(/[-:]/g, '').replace(/\..*$/, 'Z');
  const outputRoot = path.join(rootDir, 'release', `O-0.1.0-${timestamp}`);
  await fs.mkdir(outputRoot, { recursive: true });

  const { packager } = desktopRequire('@electron/packager');
  const appPaths = await packager({
    dir: desktopDir,
    out: outputRoot,
    tmpdir: path.join(rootDir, '.tmp', 'packager'),
    download: {
      cache: path.join(rootDir, '.cache', 'electron'),
      unsafelyDisableChecksums: true,
    },
    name: 'O',
    executableName: 'O',
    platform: process.platform,
    arch: process.arch,
    overwrite: true,
    asar: false,
    icon: path.join(desktopDir, 'assets', 'icon.ico'),
    appVersion: '0.1.0',
    buildVersion: '0.1.0',
    prune: true,
    ignore: [/^[/\\](?:.runtime|scripts|node_modules[/\\].cache)(?:[/\\]|$)/],
    win32metadata: { CompanyName: 'O', FileDescription: 'O local-first Agent', ProductName: 'O' },
  });

  for (const appPath of appPaths) {
    const runtimeDir = path.join(
      appPath,
      process.platform === 'darwin' ? 'O.app/Contents/Resources/app/runtime' : 'resources/app/runtime'
    );
    await fs.mkdir(path.join(runtimeDir, 'ui'), { recursive: true });
    await fs.copyFile(backendOutput, path.join(runtimeDir, backendName));
    await fs.cp(path.join(frontendDir, 'dist-desktop'), path.join(runtimeDir, 'ui'), { recursive: true });
    logSuccess(`桌面端程序包生成完成: ${appPath}`);
  }

  // 4. 压缩 / 安装包 (非 fast 模式)
  let portableZip = null;
  if (!isFast && process.platform === 'win32') {
    logStep(4, totalSteps, '生成便携 Zip 压缩包');
    portableZip = path.join(outputRoot, 'O-win32-x64-portable.zip');
    try {
      const baseZip = path.join(outputRoot, 'O-win32-x64-portable');
      execSync(`python -c "import sys, shutil; shutil.make_archive(sys.argv[1], 'zip', sys.argv[2])" "${baseZip}" "${appPaths[0]}"`);
      logSuccess(`便携压缩包生成成功: ${portableZip}`);
    } catch (zErr) {
      logInfo(`Zip 压缩提示 (可选): ${zErr.message}`);
    }

    if (!skipInstaller) {
      try {
        const { createWindowsInstaller } = desktopRequire('electron-winstaller');
        const installerDir = path.join(outputRoot, 'installer');
        await createWindowsInstaller({
          appDirectory: appPaths[0],
          outputDirectory: installerDir,
          authors: 'O',
          description: 'O local-first Agent desktop client',
          exe: 'O.exe',
          noMsi: true,
          setupExe: 'O-Setup.exe',
          setupIcon: path.join(desktopDir, 'assets', 'icon.ico'),
          title: 'O',
        });
        logSuccess(`Windows 安装包生成成功: ${path.join(installerDir, 'O-Setup.exe')}`);
      } catch (err) {
        logInfo(`安装包跳过 (未安装完整 Squirrel 环境，绿色便携版完全可用)`);
      }
    }
  }

  const duration = ((Date.now() - startTime) / 1000).toFixed(1);
  console.log(`\n${colors.bold}${colors.green}══════════════════════════════════════════════════════════════${colors.reset}`);
  console.log(`${colors.bold}${colors.green}🎉 打包成功完成！总耗时: ${duration}s${colors.reset}`);
  console.log(`${colors.bold}📁 绿色运行目录:${colors.reset} ${appPaths[0]}`);
  console.log(`${colors.bold}🚀 直接运行程序:${colors.reset} ${path.join(appPaths[0], 'O.exe')}`);
  if (portableZip && existsSync(portableZip)) {
    console.log(`${colors.bold}📦 便携分发压缩包:${colors.reset} ${portableZip}`);
  }
  console.log(`${colors.bold}${colors.green}══════════════════════════════════════════════════════════════${colors.reset}\n`);
}

main().catch((err) => {
  logError(`打包失败: ${err.message}`);
  process.exit(1);
});
