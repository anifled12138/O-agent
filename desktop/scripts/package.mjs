import fs from 'node:fs/promises';
import { createReadStream } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { spawn, execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { packager } from '@electron/packager';
import { createWindowsInstaller } from 'electron-winstaller';

const desktopDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const repositoryRoot = path.resolve(desktopDir, '..');
const backendDir = path.join(repositoryRoot, 'backend');
const frontendDir = path.join(repositoryRoot, 'frontend');
const packageInfo = JSON.parse(await fs.readFile(path.join(desktopDir, 'package.json'), 'utf8'));
const rootPackage = JSON.parse(await fs.readFile(path.join(repositoryRoot, 'package.json'), 'utf8'));
const releaseVersion = packageInfo.version;
if (!releaseVersion || rootPackage.version !== releaseVersion) throw new Error('root and desktop versions must agree');
const sourceCommit = execFileSync('git', ['rev-parse', 'HEAD'], { cwd: repositoryRoot, encoding: 'utf8', windowsHide: true }).trim();
if (!/^[0-9a-f]{40}$/.test(sourceCommit)) throw new Error('source commit is missing');
const buildInfo = `source_commit=${sourceCommit}\nversion=${releaseVersion}\nplatform=${process.platform}\narch=${process.arch}\n`;

function run(command, args, cwd, extraEnv = {}) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, {
      cwd, stdio: 'inherit', windowsHide: true,
      shell: process.platform === 'win32' && /\.(?:cmd|bat)$/i.test(command),
      env: { ...process.env, ...extraEnv },
    });
    child.once('error', reject);
    child.once('exit', (code, signal) => code === 0 ? resolve() : reject(new Error(`${command} exited with ${code ?? signal}`)));
  });
}

async function verifyFile(filename, executable = false) {
  const info = await fs.stat(filename);
  if (!info.isFile() || info.size < 2) throw new Error(`required package file missing or empty: ${filename}`);
  if (executable && process.platform === 'win32') {
    const file = await fs.open(filename, 'r');
    try {
      const bytes = Buffer.alloc(2);
      const { bytesRead } = await file.read(bytes, 0, 2, 0);
      if (bytesRead !== 2 || bytes.toString('ascii') !== 'MZ') throw new Error(`invalid Windows executable: ${filename}`);
    } finally { await file.close(); }
  }
}

async function digest(filename) {
  const hash = createHash('sha256');
  for await (const chunk of createReadStream(filename)) hash.update(chunk);
  return hash.digest('hex');
}

const npm = process.platform === 'win32' ? 'npm.cmd' : 'npm';
const bundledGo = path.join(repositoryRoot, 'work', 'toolchains', 'go', 'bin', process.platform === 'win32' ? 'go.exe' : 'go');
const go = process.env.O_GO_EXE || (await fs.stat(bundledGo).then(() => true, () => false) ? bundledGo : 'go');
await run(npm, ['run', 'build:desktop-ui'], frontendDir);
const timestamp = new Date().toISOString().replace(/[-:]/g, '').replace(/\..*$/, 'Z');
const outputRoot = path.join(repositoryRoot, 'release', `O-${releaseVersion}-${timestamp}`);
const runtimeSource = path.join(desktopDir, '.runtime', 'package');
await fs.mkdir(runtimeSource, { recursive: true });
const binaryTargets = process.platform === 'win32'
  ? [['o-host.exe', './cmd/axiom'], ['axiom-command-runner.exe', './cmd/axiom-command-runner'], ['axiom-sandbox-setup.exe', './cmd/axiom-sandbox-setup']]
  : [['o-host', './cmd/axiom']];
for (const [name, target] of binaryTargets) {
  const output = path.join(runtimeSource, name);
  await run(go, ['build', '-trimpath', '-o', output, target], backendDir, {
    CGO_ENABLED: '0', GOCACHE: path.join(repositoryRoot, '.gocache'),
  });
  await verifyFile(output, true);
}

const appPaths = await packager({
  dir: desktopDir, out: outputRoot, tmpdir: path.join(repositoryRoot, '.tmp', 'packager'),
  download: { cache: path.join(repositoryRoot, '.cache', 'electron') },
  name: 'O', executableName: 'O', platform: process.platform, arch: process.arch,
  overwrite: false, asar: false, icon: path.join(desktopDir, 'assets', 'icon.ico'),
  appVersion: releaseVersion, buildVersion: releaseVersion, prune: true,
  ignore: [/^[\/\\](?:\.runtime|scripts|node_modules[\/\\]\.cache)(?:[\/\\]|$)/],
  win32metadata: { CompanyName: 'O', FileDescription: 'O local-first Agent', ProductName: 'O' },
});
for (const appPath of appPaths) {
  const runtimeDir = path.join(appPath, process.platform === 'darwin' ? 'O.app/Contents/Resources/app/runtime' : 'resources/app/runtime');
  await fs.mkdir(runtimeDir, { recursive: true });
  for (const [name] of binaryTargets) {
    const destination = path.join(runtimeDir, name);
    await fs.copyFile(path.join(runtimeSource, name), destination);
    await verifyFile(destination, true);
    if (await digest(destination) !== await digest(path.join(runtimeSource, name))) throw new Error(`packaged binary differs: ${name}`);
  }
  await fs.cp(path.join(frontendDir, 'dist-desktop'), path.join(runtimeDir, 'ui'), { recursive: true });
  await verifyFile(path.join(runtimeDir, 'ui', 'index.html'));
  await fs.writeFile(path.join(runtimeDir, 'BUILD-INFO'), buildInfo);
}
await fs.writeFile(path.join(outputRoot, 'BUILD-INFO'), buildInfo);

if (process.platform === 'win32') {
  const installerDir = path.join(outputRoot, 'installer');
  await createWindowsInstaller({
    appDirectory: appPaths[0], outputDirectory: installerDir,
    authors: 'O', description: 'O local-first Agent desktop client', exe: 'O.exe',
    noMsi: true, setupExe: 'O-Setup.exe', setupIcon: path.join(desktopDir, 'assets', 'icon.ico'), title: 'O',
  });
  const installer = path.join(installerDir, 'O-Setup.exe');
  await verifyFile(installer, true);
  const zipBase = path.join(outputRoot, `O-win32-${process.arch}-portable`);
  const portable = `${zipBase}.zip`;
  const python = process.env.O_PYTHON_EXE || 'python';
  await run(python, ['-c', 'import sys,shutil;shutil.make_archive(sys.argv[1],"zip",sys.argv[2])', zipBase, appPaths[0]], repositoryRoot);
  await verifyFile(portable);
  await run(python, ['-c', 'import sys,zipfile;z=zipfile.ZipFile(sys.argv[1]);assert z.testzip() is None;required=["O.exe","resources/app/runtime/o-host.exe","resources/app/runtime/axiom-command-runner.exe","resources/app/runtime/axiom-sandbox-setup.exe","resources/app/runtime/ui/index.html","resources/app/runtime/BUILD-INFO"];assert all(p in z.namelist() for p in required)', portable], repositoryRoot);
  for (const filename of [installer, portable]) {
    const checksum = `${await digest(filename)}  ${path.basename(filename)}\n`;
    await fs.writeFile(`${filename}.sha256`, checksum);
    if (await fs.readFile(`${filename}.sha256`, 'utf8') !== checksum) throw new Error('checksum read-back failed');
  }
  process.stdout.write(`Windows installer: ${installer}\nWindows portable: ${portable}\n`);
}
if (process.env.GITHUB_OUTPUT) await fs.appendFile(process.env.GITHUB_OUTPUT, `release_dir=${outputRoot}\napp_dir=${appPaths[0]}\n`);
process.stdout.write(`Desktop package files verified: ${outputRoot}\n`);
