/* Cache only a generic offline notice. Conversations, API, SSE, files, and
   authenticated HTML always use the network and are never stored here. */
const OFFLINE_CACHE = 'o-offline-v2';
const OFFLINE_PAGE = '/offline.html';

self.addEventListener('install', (event) => {
  event.waitUntil((async () => {
    // Reject login redirects and SPA fallbacks rather than persisting their HTML.
    const response = await fetch(OFFLINE_PAGE, { cache: 'no-store', credentials: 'omit', redirect: 'error' });
    if (!response.ok || response.redirected ||
        !response.headers.get('content-type')?.includes('text/html') ||
        !(await response.clone().text()).includes('<meta name="o-offline-page" content="1">')) {
      throw new Error('The generic offline page is unavailable');
    }
    const cache = await caches.open(OFFLINE_CACHE);
    await cache.put(OFFLINE_PAGE, response);
    await self.skipWaiting();
  })());
});

self.addEventListener('activate', (event) => {
  event.waitUntil(caches.keys().then((keys) => Promise.all(
    keys.filter((key) => key.startsWith('o-offline-') && key !== OFFLINE_CACHE).map((key) => caches.delete(key)),
  )).then(() => self.clients.claim()));
});

self.addEventListener('fetch', (event) => {
  const url = new URL(event.request.url);
  if (event.request.method !== 'GET' || event.request.mode !== 'navigate' ||
      url.origin !== self.location.origin || url.pathname.startsWith('/api/') ||
      url.pathname === '/api') return;
  event.respondWith(fetch(event.request).catch(async () => {
    const cache = await caches.open(OFFLINE_CACHE);
    return await cache.match(OFFLINE_PAGE) || new Response('网络不可用，请恢复网络后重新打开 O。', {
      status: 503, headers: { 'Content-Type': 'text/plain; charset=utf-8' },
    });
  }));
});
