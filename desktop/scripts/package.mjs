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
const packageInfo = JSON.parse(await fs.readFile(path.join(desktopDir, 'package.json'), 'utf8'));
const releaseVersion = packageInfo.version;
if (!releaseVersion) throw new Error('desktop package version is missing');

function run(command, args, cwd, extraEnv = {}) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, {
      cwd,
      stdio: 'inherit',
      shell: process.platform === 'win32' && /\.(?:cmd|bat)$/i.test(command),
      env: { ...process.env, ...extraEnv },
    });
    child.on('exit', (code) => {
      if (code === 0) resolve();
      else reject(new Error(`${command} ${args.join(' ')} exited with code ${code}`));
    });
  });
}

async function verifyBuiltBinary(outputPath, settleMs = 0) {
  if (settleMs > 0) await new Promise((resolve) => setTimeout(resolve, settleMs));
  const info = await fs.stat(outputPath).catch((err) => {
    throw new Error(`Go build returned successfully but its output is missing: ${outputPath}: ${err.message}`);
  });
  if (!info.isFile() || info.size < 2) {
    throw new Error(`Go build output is not a non-empty file: ${outputPath}`);
  }
  if (process.platform === 'win32') {
    const file = await fs.open(outputPath, 'r');
    try {
      const signature = Buffer.alloc(2);
      const { bytesRead } = await file.read(signature, 0, signature.length, 0);
      if (bytesRead !== 2 || signature[0] !== 0x4d || signature[1] !== 0x5a) {
        throw new Error(`Go build output is not a Windows executable: ${outputPath}`);
      }
    } finally {
      await file.close();
    }
  }
}

const npm = process.platform === 'win32' ? 'npm.cmd' : 'npm';
const bundledGo = path.join(repositoryRoot, 'work', 'toolchains', 'go', 'bin', process.platform === 'win32' ? 'go.exe' : 'go');
const go = (await fs.stat(bundledGo).then(() => true, () => false)) ? bundledGo : (process.platform === 'win32' ? 'go.exe' : 'go');

await run(npm, ['run', 'build:desktop-ui'], frontendDir);

const timestamp = new Date().toISOString().replace(/[-:]/g, '').replace(/\..*$/, 'Z');
const outputRoot = path.join(repositoryRoot, 'release', `O-${releaseVersion}-${timestamp}`);
const backendOutput = path.join(desktopDir, '.runtime', 'package', process.platform === 'win32' ? 'o-host.exe' : 'o-host');
const backendName = process.platform === 'win32' ? 'o-host.exe' : 'o-host';

await fs.mkdir(path.dirname(backendOutput), { recursive: true });
await run(go, ['build', '-trimpath', '-o', backendOutput, './cmd/axiom'], backendDir, {
  ...process.env,
  CGO_ENABLED: '0',
  GOCACHE: path.join(repositoryRoot, '.gocache'),
  GOPATH: path.join(repositoryRoot, '.gopath'),
});
await verifyBuiltBinary(backendOutput, process.platform === 'win32' ? 1500 : 0);

const helperBinaries = process.platform === 'win32'
  ? ['axiom-command-runner.exe', 'axiom-sandbox-setup.exe']
  : [];
for (const binary of helperBinaries) {
  const packagePath = binary === 'axiom-command-runner.exe' ? './cmd/axiom-command-runner' : './cmd/axiom-sandbox-setup';
  await run(go, ['build', '-trimpath', '-o', path.join(path.dirname(backendOutput), binary), packagePath], backendDir, {
    ...process.env,
    CGO_ENABLED: '0',
    GOCACHE: path.join(repositoryRoot, '.gocache'),
    GOPATH: path.join(repositoryRoot, '.gopath'),
  });
  await verifyBuiltBinary(path.join(path.dirname(backendOutput), binary));
}

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
  appVersion: releaseVersion,
  buildVersion: releaseVersion,
  prune: true,
  ignore: [/^[\/\\](?:\.runtime|scripts|node_modules[\/\\]\.cache)(?:[\/\\]|$)/],
  win32metadata: { CompanyName: 'O', FileDescription: 'O local-first Agent', ProductName: 'O' },
});

for (const appPath of appPaths) {
  const runtimeDir = path.join(appPath, process.platform === 'darwin' ? 'O.app/Contents/Resources/app/runtime' : 'resources/app/runtime');
  await fs.mkdir(path.join(runtimeDir, 'ui'), { recursive: true });
  await new Promise((r) => setTimeout(r, 1000));
  const packagedBackend = path.join(runtimeDir, backendName);
  await fs.copyFile(backendOutput, packagedBackend);
  await verifyBuiltBinary(packagedBackend, process.platform === 'win32' ? 1500 : 0);
  for (const helper of helperBinaries) {
    const packagedHelper = path.join(runtimeDir, helper);
    await fs.copyFile(path.join(path.dirname(backendOutput), helper), packagedHelper);
    await verifyBuiltBinary(packagedHelper);
  }
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
    const zipInfo = await fs.stat(portableZip);
    if (zipInfo.size === 0) throw new Error('portable zip is empty');
    process.stdout.write(`O desktop portable zip: ${portableZip}\n`);
  } catch (zErr) {
    throw new Error(`Required portable zip generation failed: ${zErr.message}`);
  }
}
