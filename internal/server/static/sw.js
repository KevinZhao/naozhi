// Minimal service worker — required for PWA installability.
// No caching strategy; just satisfies the browser's SW requirement
// so that "Add to Home Screen" works and permissions persist.
self.addEventListener('fetch', () => {});
