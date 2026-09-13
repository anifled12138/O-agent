const { app, BrowserWindow, dialog, net, protocol, session } = require('electron');
const { spawn } = require('node:child_process');
const fs = require('node:fs');
const http = require('node:http');
const path = require('node:path');
const { pathToFileURL } = require('node:url');

protocol.registerSchemesAsPrivileged([
  { scheme: 'oapp', privileges: { standard: true, secure: true, supportFetchAPI: true, corsEnabled: true } },
]);

const development = !app.isPackaged;
const repositoryRoot = path.resolve(__dirname, '..');
let mainWindow;
let backendProcess;
let uiProcess;
let runtimeOrigin = '';
let allowingQuit = false;
let backendReady = false;

if (!app.requestSingleInstanceLock()) {
  app.quit();
} else {
  app.on('second-instance', () => {
    if (!mainWindow) return;
    if (mainWindow.isMinimized()) mainWindow.restore();
    mainWindow.show();
    mainWindow.focus();
  });
}

function pipeLogs(child, name) {
  const logDir = path.join(app.getPath('userData'), 'logs');
  fs.mkdirSync(logDir, { recursive: true });
  const stream = fs.createWriteStream(path.join(logDir, `${name}.log`), { flags: 'a' });
  child.stdout?.pipe(stream, { end: false });
  child.stderr?.pipe(stream, { end: false });
  child.once('close', () => stream.end());
}

function runProcess(command, args, options) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, { ...options, windowsHide: true });
    let output = '';
    child.stdout?.on('data', (chunk) => { output += chunk; });
    child.stderr?.on('data', (chunk) => { output += chunk; });
    child.once('error', reject);
    child.once('exit', (code) => code === 0 ? resolve() : reject(new Error(output.trim() || `${command} exited with ${code}`)));
  });
}

async function availablePort() {
  return new Promise((resolve, reject) => {
    const server = http.createServer();
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => {
      const address = server.address();
      const port = typeof address === 'object' && address ? address.port : 0;
      server.close((error) => error ? reject(error) : resolve(port));
    });
  });
}

async function waitFor(url, timeoutMs, child) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (child && (child.exitCode !== null || child.signalCode !== null)) throw new Error(`本地运行时过早退出（${child.exitCode ?? child.signalCode}）`);
    try {
      const response = await fetch(url);
      if (response.ok) return;
    } catch {}
    await new Promise((resolve) => setTimeout(resolve, 150));
  }
  throw new Error(`等待本地服务超时：${url}`);
}

async function startDevelopmentUI() {
  const vite = path.join(repositoryRoot, 'frontend', 'node_modules', 'vite', 'bin', 'vite.js');
  if (!fs.existsSync(vite)) throw new Error('缺少前端依赖，请先在 frontend 目录运行 npm install');
  uiProcess = spawn(process.execPath, [vite, '--config', 'vite.desktop.config.ts'], {
    cwd: path.join(repositoryRoot, 'frontend'),
    env: { ...process.env, ELECTRON_RUN_AS_NODE: '1' },
    stdio: ['ignore', 'pipe', 'pipe'],
    windowsHide: true,
  });
  pipeLogs(uiProcess, 'desktop-ui');
  await waitFor('http://127.0.0.1:5173', 20_000, uiProcess);
  return 'http://127.0.0.1:5173/';
}

async function developmentBackend() {
  const runtimeDir = path.join(__dirname, '.runtime');
  const executable = path.join(runtimeDir, 'o-host.exe');
  fs.mkdirSync(runtimeDir, { recursive: true });
  const configuredGo = process.env.O_GO_EXE;
  const localGo = 'D:\\DevTools\\go\\bin\\go.exe';
  const go = configuredGo || (fs.existsSync(localGo) ? localGo : 'go');
  await runProcess(go, ['build', '-trimpath', '-o', executable, './cmd/axiom'], {
    cwd: path.join(repositoryRoot, 'backend'),
    env: process.env,
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  return executable;
}

async function startBackend(frontendOrigin) {
  const port = await availablePort();
  runtimeOrigin = `http://127.0.0.1:${port}`;
  const executable = development
    ? await developmentBackend()
    : path.join(app.getAppPath(), 'runtime', process.platform === 'win32' ? 'o-host.exe' : 'o-host');
  if (!fs.existsSync(executable)) throw new Error(`缺少本地 Go Host：${executable}`);
  const dataDir = process.env.O_DATA_DIR || (development ? path.join(repositoryRoot, 'data') : path.join(app.getPath('userData'), 'data'));
  const workspaceRoot = process.env.O_WORKSPACE_ROOT || (development ? repositoryRoot : app.getPath('documents'));
  backendProcess = spawn(executable, [], {
    cwd: development ? path.join(repositoryRoot, 'backend') : path.dirname(executable),
    env: {
      ...process.env,
      O_ADDR: `127.0.0.1:${port}`,
      O_DATA_DIR: dataDir,
      O_WORKSPACE_ROOT: workspaceRoot,
      O_FRONTEND_ORIGIN: frontendOrigin,
      O_DESKTOP_STDIN: '1',
    },
    stdio: ['pipe', 'pipe', 'pipe'],
    windowsHide: true,
  });
  pipeLogs(backendProcess, 'o-host');
  backendProcess.once('exit', (code, signal) => {
    if (!backendReady || allowingQuit) return;
    backendReady = false;
    dialog.showErrorBox('O 本地运行时已停止', `Go Host 意外退出（${code ?? signal ?? 'unknown'}）。`);
    allowingQuit = true;
    void stopChildren().finally(() => app.quit());
  });
  await waitFor(`${runtimeOrigin}/api/v1/health`, 20_000, backendProcess);
  backendReady = true;
}

function installLocalProtocol() {
  const rendererRoot = path.join(app.getAppPath(), 'runtime', 'ui');
  protocol.handle('oapp', (request) => {
    const url = new URL(request.url);
    if (url.host !== 'app') return new Response('Not found', { status: 404 });
    const requested = decodeURIComponent(url.pathname === '/' ? '/index.html' : url.pathname);
    const relative = path.normalize(requested).replace(/^([/\\])+/, '');
    const target = path.resolve(rendererRoot, relative);
    const contained = target === rendererRoot || target.startsWith(rendererRoot + path.sep);
    try {
      if (!contained || !fs.statSync(target).isFile()) return new Response('Not found', { status: 404 });
      return net.fetch(pathToFileURL(target).toString());
    } catch {
      return new Response('Not found', { status: 404 });
    }
  });
}

async function createWindow(url) {
  mainWindow = new BrowserWindow({
    width: 1440,
    height: 920,
    minWidth: 900,
    minHeight: 640,
    show: false,
    backgroundColor: '#ffffff',
    title: 'O',
    icon: path.join(__dirname, 'assets', 'icon.ico'),
    autoHideMenuBar: true,
    webPreferences: {
      preload: path.join(__dirname, 'preload.cjs'),
      additionalArguments: [`--o-api-origin=${runtimeOrigin}`],
      nodeIntegration: false,
      contextIsolation: true,
      sandbox: true,
      webSecurity: true,
    },
  });
  mainWindow.webContents.setWindowOpenHandler(() => ({ action: 'deny' }));
  mainWindow.webContents.on('will-navigate', (event, destination) => {
    if (destination !== url) event.preventDefault();
  });
  mainWindow.once('ready-to-show', () => mainWindow.show());
  await mainWindow.loadURL(url);
}

async function stopChildren() {
  if (backendProcess && backendProcess.exitCode === null && backendProcess.signalCode === null) {
    backendProcess.stdin?.end();
    await Promise.race([
      new Promise((resolve) => backendProcess.once('exit', resolve)),
      new Promise((resolve) => setTimeout(resolve, 5_000)),
    ]);
    if (backendProcess.exitCode === null && backendProcess.signalCode === null) backendProcess.kill();
  }
  if (uiProcess && uiProcess.exitCode === null && uiProcess.signalCode === null) uiProcess.kill();
}

app.whenReady().then(async () => {
  try {
    app.setAppUserModelId('local.o.agent');
    session.defaultSession.setPermissionRequestHandler((_webContents, _permission, callback) => callback(false));
    session.defaultSession.setPermissionCheckHandler(() => false);
    const windowURL = development ? await startDevelopmentUI() : 'oapp://app/index.html';
    if (!development) installLocalProtocol();
    await startBackend(development ? new URL(windowURL).origin : 'oapp://app');
    await createWindow(windowURL);
  } catch (error) {
    dialog.showErrorBox('O 启动失败', error instanceof Error ? error.message : String(error));
    allowingQuit = true;
    await stopChildren();
    app.quit();
  }
});

app.on('before-quit', (event) => {
  if (allowingQuit) return;
  event.preventDefault();
  allowingQuit = true;
  void stopChildren().finally(() => app.quit());
});

app.on('window-all-closed', () => app.quit());
