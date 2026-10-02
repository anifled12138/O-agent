import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';

const source = await readFile(new URL('../public/sw.js', import.meta.url), 'utf8');

function worker({ networkFails = false, cacheFails = false, wrongOfflinePage = false } = {}) {
  const events = {};
  const stored = new Map();
  const calls = [];
  const cache = {
    async put(path, response) {
      if (cacheFails) throw new Error('quota exceeded');
      stored.set(path, response);
    },
    async match(path) { return stored.get(path)?.clone(); },
  };
  const self = {
    location: { origin: 'https://o.example' },
    addEventListener(name, handler) { events[name] = handler; },
    async skipWaiting() { calls.push('skipWaiting'); },
    clients: { async claim() { calls.push('claim'); } },
  };
  vm.runInNewContext(source, {
    self, URL, Response,
    caches: {
      async open() { return cache; },
      async keys() { return ['o-offline-v1', 'o-offline-v2', 'other-app']; },
      async delete(key) { calls.push(`delete:${key}`); },
    },
    async fetch(request, options) {
      if (request === '/offline.html') {
        assert.deepEqual(JSON.parse(JSON.stringify(options)), { cache: 'no-store', credentials: 'omit', redirect: 'error' });
        return new Response(wrongOfflinePage ? 'private authenticated HTML' : '<meta name="o-offline-page" content="1">generic offline notice', { headers: { 'Content-Type': 'text/html' } });
      }
      calls.push(`fetch:${request.url}`);
      if (networkFails) throw new Error('offline');
      return new Response('private online conversation');
    },
  });
  return { events, stored, calls };
}

async function lifecycle(handler) {
  let work;
  handler({ waitUntil(value) { work = value; } });
  await work;
}

async function navigate(state, path, options = {}) {
  let response;
  state.events.fetch({
    request: { url: new URL(path, 'https://o.example').href, method: 'GET', mode: 'navigate', ...options },
    respondWith(value) { response = value; },
  });
  return response;
}

test('activation preserves unrelated caches; only generic offline HTML is durable', async () => {
  const state = worker();
  await lifecycle(state.events.install);
  await lifecycle(state.events.activate);
  assert.deepEqual([...state.stored.keys()], ['/offline.html']);
  assert.deepEqual(state.calls, ['skipWaiting', 'delete:o-offline-v1', 'claim']);
});

test('online private HTML is never cached; navigation falls back after network failure', async () => {
  const online = worker();
  await lifecycle(online.events.install);
  assert.equal(await (await navigate(online, '/')).text(), 'private online conversation');
  assert.deepEqual([...online.stored.keys()], ['/offline.html']);
  const offline = worker({ networkFails: true });
  await lifecycle(offline.events.install);
  assert.equal(await (await navigate(offline, '/')).text(), '<meta name="o-offline-page" content="1">generic offline notice');
  assert.deepEqual([...offline.stored.keys()], ['/offline.html']);
});

test('API, SSE, files, mutations and cross-origin requests bypass the worker', async () => {
  const state = worker({ networkFails: true });
  for (const [path, options] of [
    ['/api', {}], ['/api/v1/conversations', {}], ['/api/v2/agent/turns/t/events', {}],
    ['/api/v1/assets/private.txt', {}], ['/', { method: 'POST' }],
    ['/icons/o-192.png', { mode: 'cors' }], ['https://other.example/', {}],
  ]) assert.equal(await navigate(state, path, options), undefined, path);
  assert.equal(state.calls.length, 0);
  assert.equal(state.stored.size, 0);
});

test('failed cache installation is rejected; missing fallback returns explicit 503', async () => {
  const state = worker({ networkFails: true, cacheFails: true });
  await assert.rejects(lifecycle(state.events.install), /quota exceeded/);
  assert.equal(state.calls.includes('skipWaiting'), false);
  assert.equal(state.stored.size, 0);
  const response = await navigate(state, '/');
  assert.equal(response.status, 503);
});

test('a gateway returning authenticated HTML in place of offline.html is never cached', async () => {
  const state = worker({ wrongOfflinePage: true });
  await assert.rejects(lifecycle(state.events.install), /generic offline page/);
  assert.equal(state.stored.size, 0);
  assert.equal(state.calls.includes('skipWaiting'), false);
});
