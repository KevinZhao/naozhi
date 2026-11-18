// minimal service worker kept intentionally small.
// Exists for PWA installability and as a future escape hatch — bump SW_VERSION
// and drop stale nz-sw- caches if we ever add real caching, so existing users
// are never pinned on old UI.
//
// Deliberately no fetch listener: installability no longer needs one, and
// WebKit (iOS Safari / home-screen PWA) routes every subresource of a page
// controlled by a fetch-listening SW past its HTTP cache, re-downloading all
// immutable /static/ assets on each cold launch. Chromium skips an empty
// listener; WebKit does not.
const SW_VERSION = 'nz-sw-v1';

self.addEventListener('install', (event) => {
  event.waitUntil(self.skipWaiting());
});

self.addEventListener('activate', (event) => {
  event.waitUntil((async () => {
    try {
      const names = await caches.keys();
      await Promise.all(
        names
          .filter((n) => n.startsWith('nz-sw-') && n !== SW_VERSION)
          .map((n) => caches.delete(n))
      );
    } catch (_) { /* caches unavailable — nothing to clean */ }
    await self.clients.claim();
  })());
});
