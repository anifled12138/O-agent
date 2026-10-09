import fs from 'node:fs/promises';
import path from 'node:path';
import http from 'node:http';
import { spawn } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import assert from 'node:assert/strict';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..');
const appDir = path.resolve(process.argv[2] || '');
assert.equal(process.platform, 'win32', 'Windows package smoke requires Windows');
const runtime = path.join(appDir, 'resources/app/runtime');
for (const name of ['o-host.exe', 'axiom-command-runner.exe', 'axiom-sandbox-setup.exe', 'ui/index.html', 'BUILD-INFO']) {
  assert.ok((await fs.stat(path.join(runtime, name))).size > 0, `missing runtime asset: ${name}`);
}
const scratch = await fs.mkdtemp(path.join(root, '.tmp', 'windows-package-smoke-'));
await fs.mkdir(path.join(scratch, 'workspace'));
const credential = randomBytes(32).toString('hex');
let heartbeats = 0, claims = 0, wakes = 0, protocolError;
let child, exited, mainError;
const control = http.createServer(async (req, res) => {
  try {
    assert.equal(req.headers.authorization, `Bearer ${credential}`);
    if (req.url === '/api/v1/nodes/heartbeat') {
      let body = '';
      for await (const chunk of req) body += chunk;
      const data = JSON.parse(body);
      assert.equal(data.platform, 'windows');
      assert.ok(data.capabilities.includes('agent-runtime'));
      assert.ok(data.resources.memoryTotalBytes > 0);
      heartbeats++;
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify(data));
    } else if (req.url === '/api/v1/nodes/tasks/claim') {
      claims++;
      res.writeHead(204); res.end();
    } else if (req.url === '/api/v1/nodes/connect') {
      wakes++;
      res.writeHead(503); res.end('unavailable wake channel in package smoke');
    } else throw new Error(`unexpected control route: ${req.url}`);
  } catch (error) { protocolError = error; res.writeHead(500); res.end('package smoke protocol failure'); }
});
await new Promise((resolve, reject) => { control.once('error', reject); control.listen(0, '127.0.0.1', resolve); });
const controlUrl = `http://127.0.0.1:${control.address().port}`;
const cleanEnv = Object.fromEntries(Object.entries(process.env).filter(([name]) => !/^(O_|AXIOM_)/i.test(name)));
async function start() {
  const listener = http.createServer();
  await new Promise((resolve, reject) => { listener.once('error', reject); listener.listen(0, '127.0.0.1', resolve); });
  const port = listener.address().port;
  await new Promise((resolve, reject) => listener.close(error => error ? reject(error) : resolve()));
  const url = `http://127.0.0.1:${port}`;
  const previous = { heartbeats, claims, wakes };
  child = spawn(path.join(runtime, 'o-host.exe'), [], {
    cwd: runtime, windowsHide: true, stdio: ['pipe', 'pipe', 'pipe'],
    env: { ...cleanEnv, O_ADDR: `127.0.0.1:${port}`, O_EXECUTION_ROLE: 'local',
      O_DATA_DIR: path.join(scratch, 'data'), O_WORKSPACE_ROOT: path.join(scratch, 'workspace'),
      O_AGENT_TEMP_DIR: path.join(scratch, 'temp'), O_DESKTOP_STDIN: '1',
      O_FRONTEND_ORIGIN: url, O_NODE_CONTROL_URL: controlUrl, O_NODE_CREDENTIAL: credential },
  });
  let logs = '';
  child.stdout.on('data', chunk => { logs = (logs + chunk).slice(-12000); });
  child.stderr.on('data', chunk => { logs = (logs + chunk).slice(-12000); });
  exited = new Promise((resolve, reject) => { child.once('error', reject); child.once('exit', (code, signal) => resolve({ code, signal })); });
  // Attach a rejection handler immediately so startup errors remain attributable.
  exited.catch(() => {});
  const deadline = Date.now() + 25000;
  while (Date.now() < deadline) {
    if (protocolError) throw protocolError;
    if (child.exitCode !== null || child.signalCode !== null) throw new Error(`packaged host exited during startup: ${logs}`);
    try {
      const health = await fetch(`${url}/api/v1/health`, { signal: AbortSignal.timeout(1500) });
      if (health.ok && (await health.json()).status === 'ok' && heartbeats > previous.heartbeats && claims > previous.claims && wakes > previous.wakes) return url;
    } catch (error) { if (protocolError) throw error; }
    await new Promise(resolve => setTimeout(resolve, 100));
  }
  throw new Error(`packaged local host or remote polling failed startup verification: ${logs}`);
}
async function stop() {
  if (!child) return;
  const process = child;
  process.stdin.end();
  let timer;
  try {
    const result = await Promise.race([exited, new Promise((_, reject) => { timer = setTimeout(() => reject(new Error('packaged host did not stop gracefully')), 10000); })]);
    assert.equal(result.code, 0, `packaged host stop: ${JSON.stringify(result)}`);
  } catch (error) { process.kill(); await exited; throw error; }
  finally { clearTimeout(timer); child = undefined; }
}
async function request(url, route, method = 'GET', body) {
  const response = await fetch(url + route, { method, headers: { Origin: url, 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(5000) });
  const data = await response.json();
  assert.ok(response.ok, `packaged API ${route} failed: ${JSON.stringify(data)}`);
  return data;
}
try {
  let url = await start();
  const provider = await request(url, '/api/v1/providers', 'POST', {
    name: 'Package smoke fixture', kind: 'openai-compatible', baseUrl: controlUrl,
    model: 'package-smoke-fixture', apiKey: 'synthetic-package-smoke-key',
  });
  assert.ok(provider.id, 'missing persisted provider identity');
  const conversation = await request(url, '/api/v1/conversations', 'POST', {
    title: 'Windows package persistence smoke', providerId: provider.id,
  });
  assert.ok(conversation.id, 'missing durable conversation identity');
  const firstRead = await request(url, `/api/v1/conversations/${conversation.id}`);
  assert.equal(firstRead.title, conversation.title);
  await stop();
  url = await start();
  const afterRestart = await request(url, `/api/v1/conversations/${conversation.id}`);
  assert.equal(afterRestart.id, conversation.id);
  assert.equal(afterRestart.title, conversation.title);
  await stop();
  if (protocolError) throw protocolError;
  process.stdout.write('Packaged Windows host verified: remote heartbeat/polling and conversation persistence across restart.\n');
} catch (error) {
  mainError = error;
  throw error;
} finally {
  const cleanup = await Promise.allSettled([
    stop(),
    (async () => {
      control.closeAllConnections();
      await new Promise((resolve, reject) => control.close(error => error ? reject(error) : resolve()));
    })(),
  ]);
  const failures = cleanup.filter(result => result.status === 'rejected').map(result => result.reason);
  if (failures.length) throw new AggregateError([...(mainError ? [mainError] : []), ...failures], 'Windows package smoke or cleanup failed');
}
