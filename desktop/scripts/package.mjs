import fs from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawn, execSync } from 'node:child_process';
import { packager } from '@electron/packager';
import { createWindowsInstaller } from 'electron-winstaller';

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);
const desktopDir = path.resolve(__dirname, '..');
const repositoryRoot = path.resolve(desktopDir, '..');
const backendDir = path.join(repositoryRoot, 'backend');
const frontendDir = path.join(repositoryRoot, 'frontend');

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
      else reject(new Error(`${command} ${args.join(' ')} exited with code ${code}`));
    });
  });
}

const npm = process.platform === 'win32' ? 'npm.cmd' : 'npm';
const go = process.platform === 'win32' ? 'D:\\agent-harness\\work\\toolchains\\go\\bin\\go.exe' : 'go';

await run(npm, ['run', 'build:desktop-ui'], frontendDir);

const timestamp = new Date().toISOString().replace(/[-:]/g, '').replace(/\..*$/, 'Z');
const outputRoot = path.join(repositoryRoot, 'release', `O-0.1.0-${timestamp}`);
const backendOutput = path.join(desktopDir, '.runtime', 'package', process.platform === 'win32' ? 'o-host.exe' : 'o-host');
const backendName = process.platform === 'win32' ? 'o-host.exe' : 'o-host';

await fs.mkdir(path.dirname(backendOutput), { recursive: true });
await run(go, ['build', '-trimpath', '"-ldflags=-s -w"', '-o', backendOutput, './cmd/axiom'], backendDir, {
  ...process.env,
  CGO_ENABLED: '0',
  GOCACHE: path.join(repositoryRoot, '.gocache'),
  GOPATH: path.join(repositoryRoot, '.gopath'),
});

const appPaths = await packager({
  dir: desktopDir,
  out: outputRoot,
  tmpdir: path.join(repositoryRoot, '.tmp', 'packager'),
  download: {
    cache: path.join(repositoryRoot, '.cache', 'electron'),
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
  ignore: [/^[\/\\](?:\.runtime|scripts|node_modules[\/\\]\.cache)(?:[\/\\]|$)/],
  win32metadata: { CompanyName: 'O', FileDescription: 'O local-first Agent', ProductName: 'O' },
});

for (const appPath of appPaths) {
  const runtimeDir = path.join(appPath, process.platform === 'darwin' ? 'O.app/Contents/Resources/app/runtime' : 'resources/app/runtime');
  await fs.mkdir(path.join(runtimeDir, 'ui'), { recursive: true });
  await new Promise((r) => setTimeout(r, 1000));
  await fs.copyFile(backendOutput, path.join(runtimeDir, backendName));
  await new Promise((r) => setTimeout(r, 1000));
  await fs.cp(path.join(frontendDir, 'dist-desktop'), path.join(runtimeDir, 'ui'), { recursive: true });
  process.stdout.write(`\nO desktop package: ${appPath}\n`);
}

if (process.platform === 'win32') {
  const installerDir = path.join(outputRoot, 'installer');
  try {
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
  } catch (err) {
    process.stderr.write(`Installer generation note: ${err.message}\n`);
  }

  const portableZip = path.join(outputRoot, 'O-win32-x64-portable.zip');
  try {
    process.stdout.write('Generating portable distribution zip...\n');
    const baseZip = path.join(outputRoot, 'O-win32-x64-portable');
    execSync(`python -c "import sys, shutil; shutil.make_archive(sys.argv[1], 'zip', sys.argv[2])" "${baseZip}" "${appPaths[0]}"`);
    process.stdout.write(`O desktop portable zip: ${portableZip}\n`);
  } catch (zErr) {
    process.stderr.write(`Zip compression note: ${zErr.message}\n`);
  }
}
