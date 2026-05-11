// RNEW-UX-005: minimal service worker kept intentionally small.
// Exists purely for PWA installability (browsers require a fetch listener)
// and as a future escape hatch — bump SW_VERSION and drop stale nz-sw- caches
// if we ever add real caching, so existing users are never pinned on old UI.
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

// No-op fetch handler preserves PWA installability (browsers mark the SW
// as "controlling" only when a fetch listener is present).
self.addEventListener('fetch', () => {});
