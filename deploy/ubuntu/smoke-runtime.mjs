import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { once } from 'node:events';
import { cp, mkdir, mkdtemp, rm } from 'node:fs/promises';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';

const input = process.argv[2];
if (!input) throw new Error('Usage: node smoke-runtime.mjs <extracted-release-directory> [--web-only]');
const root = await mkdtemp(path.join(os.tmpdir(), 'o-runtime-smoke-'));
const runtime = path.join(root, 'runtime');
const children = new Set();
async function port() {
  const socket = net.createServer();
  socket.listen(0, '127.0.0.1');
  await once(socket, 'listening');
  const value = socket.address().port;
  await new Promise((resolve, reject) => socket.close(error => error ? reject(error) : resolve()));
  return value;
}
function launch(executable, args, cwd, env) {
  const child = spawn(executable, args, { cwd, env: { ...process.env, NODE_PATH: '', ...env }, stdio: ['ignore', 'pipe', 'pipe'], windowsHide: true });
  child.logs = '';
  child.failure = null;
  child.finished = new Promise(resolve => {
    child.once('error', error => { child.failure = error; resolve(); });
    child.once('exit', (code, signal) => { child.failure = new Error(`process exited: ${code ?? signal}`); resolve(); });
  });
  for (const stream of [child.stdout, child.stderr]) stream.on('data', data => { child.logs = (child.logs + data).slice(-65536); });
  children.add(child);
  return child;
}
async function stop(child) {
  if (!child) return;
  if (!child.failure) child.kill('SIGTERM');
  let timer;
  try {
    await Promise.race([child.finished, new Promise((_, reject) => { timer = setTimeout(() => reject(new Error('runtime did not stop')), 10000); })]);
  } catch (error) {
    child.kill('SIGKILL');
    await child.finished;
    throw error;
  } finally { clearTimeout(timer); }
  children.delete(child);
}
async function ready(child, url) {
  const deadline = Date.now() + 30000;
  while (Date.now() < deadline) {
    if (child.failure) throw new Error(`${child.failure.message}\n${child.logs}`);
    try {
      const response = await fetch(url, { signal: AbortSignal.timeout(2000), redirect: 'manual' });
      if (response.status === 200) return response;
    } catch { /* Retry only while the process is still starting. */ }
    await new Promise(resolve => setTimeout(resolve, 100));
  }
  throw new Error(`runtime never served ${url}\n${child.logs}`);
}
try {
  // No checkout node_modules, source tree, or host credentials can satisfy a
  // missing packaged dependency; exercise the extracted archive in isolation.
  await cp(path.resolve(input), runtime, { recursive: true });
  const frontend = path.join(runtime, 'frontend', 'standalone');
  const webPort = await port();
  const web = launch(process.execPath, ['server.js'], frontend, { HOST: '127.0.0.1', PORT: String(webPort) });
  const page = await ready(web, `http://127.0.0.1:${webPort}/`);
  assert.match(page.headers.get('content-type') ?? '', /text\/html/);
  assert.match(await page.text(), /<title>O<\/title>/);
  const icon = await fetch(`http://127.0.0.1:${webPort}/favicon.svg`);
  assert.equal(icon.status, 200);
  assert.match(await icon.text(), /<svg/);
  await stop(web);
  console.log('Extracted Web runtime served the O page and public asset without external dependencies.');

  if (!process.argv.includes('--web-only')) {
    const data = path.join(root, 'data');
    const workspaces = path.join(root, 'workspaces');
    await mkdir(workspaces);
    const apiPort = await port();
    const origin = 'https://runtime-smoke.example.test';
    const bootstrap = randomBytes(32).toString('hex');
    const env = {
      O_EXECUTION_ROLE: 'cloud', O_ADDR: `127.0.0.1:${apiPort}`, O_FRONTEND_ORIGIN: origin,
      O_AUTH_BOOTSTRAP_TOKEN: bootstrap, O_DATA_DIR: data, O_WORKSPACE_ROOT: workspaces,
      O_AGENT_TEMP_DIR: path.join(root, 'scratch'), O_AUTH_OWNER_EMAIL: '', O_RESEND_API_KEY: '', O_AUTH_MAIL_FROM: '',
      O_TURNSTILE_SITE_KEY: '', O_TURNSTILE_SECRET_KEY: '', O_NODE_CONTROL_URL: '', O_NODE_CREDENTIAL: '',
      O_CLOUD_TASK_WORKSPACE_QUOTA_ENABLED: 'false', AXIOM_CLOUD_TASK_WORKSPACE_QUOTA_ENABLED: '',
      // Old installer values cannot require an unavailable helper or project mount.
      O_CLOUD_TASK_WORKSPACE_QUOTA_BYTES: '8589934592', O_QUOTA_HELPER_SOCKET: '/nonexistent/o-runtime-smoke-quota.sock',
      O_CLOUD_TASK_WORKSPACE_RETENTION: '0',
    };
    const executable = path.join(runtime, 'backend', process.platform === 'win32' ? 'axiom.exe' : 'axiom');
    const base = `http://127.0.0.1:${apiPort}`;
    let host = launch(executable, [], root, env);
    assert.equal((await (await ready(host, `${base}/api/v1/health`)).json()).status, 'ok');
    const credentials = { email: 'runtime-smoke@example.test', displayName: 'runtime smoke', password: randomBytes(24).toString('hex') };
    const setup = await fetch(`${base}/api/v1/auth/setup`, { method: 'POST', headers: { 'Content-Type': 'application/json', Origin: origin, 'X-O-Bootstrap-Token': bootstrap }, body: JSON.stringify(credentials) });
    assert.equal(setup.status, 200, await setup.text());
    await stop(host);
    host = launch(executable, [], root, env);
    await ready(host, `${base}/api/v1/health`);
    const auth = await (await fetch(`${base}/api/v1/auth/status`)).json();
    assert.equal(auth.setupRequired, false);
    const login = await fetch(`${base}/api/v1/auth/login`, { method: 'POST', headers: { 'Content-Type': 'application/json', Origin: origin }, body: JSON.stringify({ email: credentials.email, password: credentials.password }) });
    assert.equal(login.status, 200, await login.text());
    assert.match(login.headers.get('set-cookie') ?? '', /o_session=/);
    await stop(host);
    console.log('Cloud backend started without quotas; account survived restart and accepted login.');
    const rejected = launch(executable, [], root, { ...env, O_CLOUD_TASK_WORKSPACE_QUOTA_ENABLED: 'true' });
    let failureTimer;
    try {
      await Promise.race([rejected.finished, new Promise((_, reject) => { failureTimer = setTimeout(() => reject(new Error('enabled quota did not reject the unavailable helper')), 10000); })]);
    } finally { clearTimeout(failureTimer); }
    assert.match(rejected.logs, process.platform === 'linux' ? /verify cloud workspace project quota helper and mount/ : /kernel project quota helper is supported only on Linux/);
    await stop(rejected);
    host = launch(executable, [], root, env);
    await ready(host, `${base}/api/v1/health`);
    assert.equal((await (await fetch(`${base}/api/v1/auth/status`)).json()).setupRequired, false);
    await stop(host);
    console.log('Explicit quota opt-in failed closed without its helper; returning to default mode preserved the account.');
  }
} finally {
  const results = await Promise.allSettled([...children].map(stop));
  const failed = results.find(result => result.status === 'rejected');
  if (failed) throw failed.reason;
  // Only the directory allocated by mkdtemp for this invocation is removed.
  if (!root.startsWith(path.join(os.tmpdir(), 'o-runtime-smoke-'))) throw new Error('unsafe smoke cleanup path');
  await rm(root, { recursive: true, force: true });
}
