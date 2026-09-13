import { packager } from '@electron/packager';
import { createWindowsInstaller } from 'electron-winstaller';
import { spawn } from 'node:child_process';
import fs from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const desktopDir = path.resolve(scriptDir, '..');
const repositoryRoot = path.resolve(desktopDir, '..');
const frontendDir = path.join(repositoryRoot, 'frontend');
const backendDir = path.join(repositoryRoot, 'backend');
const buildDir = path.join(desktopDir, '.runtime', 'package');

function run(command, args, cwd, env = process.env) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, { cwd, env, stdio: 'inherit', windowsHide: true });
    child.once('error', reject);
    child.once('exit', (code) => code === 0 ? resolve() : reject(new Error(`${command} exited with ${code}`)));
  });
}

await fs.mkdir(buildDir, { recursive: true });
const npmCLI = process.env.npm_execpath;
if (!npmCLI) throw new Error('npm CLI path is unavailable');
await run(process.execPath, [npmCLI, 'run', 'build:desktop-ui'], frontendDir);

const go = process.env.O_GO_EXE || (process.platform === 'win32' ? 'D:\\DevTools\\go\\bin\\go.exe' : 'go');
const backendName = process.platform === 'win32' ? 'o-host.exe' : 'o-host';
const backendOutput = path.join(buildDir, backendName);
await run(go, ['build', '-trimpath', '-ldflags=-s -w', '-o', backendOutput, './cmd/axiom'], backendDir, { ...process.env, CGO_ENABLED: '0' });

const stamp = new Date().toISOString().replace(/[-:]/g, '').replace(/\.\d{3}Z$/, 'Z');
const outputRoot = path.join(repositoryRoot, 'release', `O-0.1.0-${stamp}`);
const appPaths = await packager({
  dir: desktopDir,
  name: 'O',
  executableName: 'O',
  appVersion: '0.1.0',
  platform: process.platform,
  arch: process.arch,
  out: outputRoot,
  asar: false,
  icon: path.join(desktopDir, 'assets', 'icon.ico'),
  prune: true,
  ignore: [/^[/\\](?:\.runtime|scripts|node_modules[/\\]\.cache)(?:[/\\]|$)/],
  win32metadata: { CompanyName: 'O', FileDescription: 'O local-first Agent', ProductName: 'O' },
});

for (const appPath of appPaths) {
  const runtimeDir = path.join(appPath, process.platform === 'darwin' ? 'O.app/Contents/Resources/app/runtime' : 'resources/app/runtime');
  await fs.mkdir(path.join(runtimeDir, 'ui'), { recursive: true });
  await fs.copyFile(backendOutput, path.join(runtimeDir, backendName));
  await fs.cp(path.join(frontendDir, 'dist-desktop'), path.join(runtimeDir, 'ui'), { recursive: true });
  process.stdout.write(`\nO desktop package: ${appPath}\n`);
}

if (process.platform === 'win32') {
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
  process.stdout.write(`O desktop installer: ${path.join(installerDir, 'O-Setup.exe')}\n`);
}
