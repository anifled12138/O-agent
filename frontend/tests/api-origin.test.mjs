import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import test from 'node:test';

const clientURL = new URL('../app/api.ts', import.meta.url).href;
function runClient(window, configured) {
  const code = `
    globalThis.window = ${JSON.stringify(window)};
    delete process.env.NEXT_PUBLIC_API_URL;
    ${configured ? `process.env.NEXT_PUBLIC_API_URL = ${JSON.stringify(configured)};` : ''}
    let seen;
    globalThis.fetch = async (url, options) => { seen = {url, cache:options.cache}; return new Response('[]', {headers:{'Content-Type':'application/json'}}); };
    const api = await import(${JSON.stringify(clientURL)});
    await api.request(api.API_V2 + '/agent/turns/t/events');
    process.stdout.write(JSON.stringify({v1:api.API,v2:api.API_V2,assets:api.ASSET_ORIGIN,seen}));
  `;
  return JSON.parse(execFileSync(process.execPath, ['--experimental-strip-types', '--no-warnings', '--input-type=module', '-e', code], { encoding: 'utf8' }));
}

test('phone API and assets follow HTTPS gateway; v2 is not prefixed with v1', () => {
  const result = runClient({ location: { origin: 'https://o.example' } });
  assert.equal(result.v1, 'https://o.example/api/v1');
  assert.equal(result.v2, 'https://o.example/api/v2');
  assert.equal(result.assets, 'https://o.example');
  assert.deepEqual(result.seen, { url: 'https://o.example/api/v2/agent/turns/t/events', cache: 'no-store' });
});

test('Electron preload API keeps priority over web environment', () => {
  const result = runClient({ location: { origin: 'file://' }, oDesktop: { apiOrigin: 'http://127.0.0.1:12345' } }, 'https://other.example/api/v1');
  assert.equal(result.v1, 'http://127.0.0.1:12345/api/v1');
  assert.equal(result.assets, 'http://127.0.0.1:12345');
});

test('explicit API override resolves both absolute and same-origin URLs', () => {
  const browser = { location: { origin: 'https://o.example' } };
  assert.equal(runClient(browser, '/api/v1/').v2, 'https://o.example/api/v2');
  assert.equal(runClient(browser, 'https://gateway.example/api/v1').assets, 'https://gateway.example');
});
