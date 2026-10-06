// release-gate-static.mjs — the release gate's check of how the shipped binary's
// dashboard loads its /static assets (#3330). The page the server renders
// carries one import map, an inline loader per entry module and a ?v= on every
// /static URL; the gate's first authenticated view must show that page loading
// with no CSP violation, each /static path requested once under the one URL the
// page names for it, and every response a 200 with the cache policy that
// version earns. The e2e suite's rendered-dashboard spec applies the same rules
// to the mock, which serves /static itself and so cannot see Cache-Control.

// IMMUTABLE_CACHE is what staticCacheControl (internal/server) sends for a
// request naming the asset's current hash.
export const IMMUTABLE_CACHE = 'private, max-age=31536000, immutable';

const isStatic = (raw) => new URL(raw).pathname.startsWith('/static/');

// A /static/vendor/ path names its release, so the server caches it as
// immutable with no ?v=. render_md.js loads the vendored scripts as classic
// scripts when a message needs them, and their stylesheets fetch the fonts:
// none of it is in the import map, and the fonts are named by no element.
const isVendor = (pathname) => pathname.startsWith('/static/vendor/');

// recordStatic collects page's /static requests, failures and responses until
// stop() is called.
export function recordStatic(page) {
  const rec = { requests: [], failed: [], responses: [] };
  const onRequest = (r) => { if (isStatic(r.url())) rec.requests.push(r.url()); };
  const onFailed = (r) => { if (isStatic(r.url())) rec.failed.push(r.url()); };
  const onResponse = (r) => {
    if (isStatic(r.url())) rec.responses.push({ url: r.url(), status: r.status(), cacheControl: r.headers()['cache-control'] || '' });
  };
  page.on('request', onRequest);
  page.on('requestfailed', onFailed);
  page.on('response', onResponse);
  rec.stop = () => {
    page.off('request', onRequest);
    page.off('requestfailed', onFailed);
    page.off('response', onResponse);
  };
  return rec;
}

// readRenderedPage runs in the browser (page.evaluate): what the page names —
// import maps, every /static URL in it, the entry modules its inline loaders
// import — and the CSP violations the gate's init script recorded.
export function readRenderedPage() {
  const maps = document.querySelectorAll('script[type="importmap"]');
  let imports = {};
  try {
    imports = (maps.length && JSON.parse(maps[0].textContent || '{}').imports) || {};
  } catch (_) { /* reported as a missing map entry for every module */ }
  const urls = Object.values(imports);
  for (const el of document.querySelectorAll('[href^="/static/"], [src^="/static/"]')) {
    urls.push(el.getAttribute('href') || el.getAttribute('src'));
  }
  const entries = [];
  for (const s of document.querySelectorAll('script[type="module"]:not([src])')) {
    const m = /^import "(\/static\/[^"]+)";$/.exec((s.textContent || '').trim());
    if (m) entries.push(m[1]);
  }
  return { maps: maps.length, imports, urls, entries, violations: window.__cspv || [] };
}

// staticLoadProblems returns one line per broken rule, empty when the page
// (from readRenderedPage) and the loads recorded for it are as rendered.
export function staticLoadProblems(named, rec) {
  const problems = [];
  if (named.maps !== 1) problems.push(`page has ${named.maps} import maps, want 1`);
  for (const v of named.violations) problems.push(`CSP violation: ${v}`);
  if (!named.entries.length) problems.push('page has no inline module loader');

  const versionOf = new Map();
  for (const raw of named.urls) {
    const u = new URL(raw, 'http://page.invalid');
    if (isVendor(u.pathname)) continue;
    if (!versionOf.has(u.pathname)) versionOf.set(u.pathname, new Set());
    versionOf.get(u.pathname).add(u.searchParams.get('v') || '');
  }
  for (const [p, vs] of versionOf) {
    if (vs.size !== 1 || vs.has('')) problems.push(`page names ${p} as ${[...vs].map((v) => (v ? 'v=' + v : 'unversioned')).join(' and ')}`);
  }

  if (!rec.requests.length) problems.push('no /static request seen');
  const count = new Map();
  for (const raw of rec.requests) {
    const u = new URL(raw);
    if (!isVendor(u.pathname) && !versionOf.get(u.pathname)?.has(u.searchParams.get('v') || '')) {
      problems.push(`requested ${u.pathname}${u.search}, not the URL the page names`);
    }
    count.set(u.pathname, (count.get(u.pathname) || 0) + 1);
  }
  for (const [p, n] of count) {
    if (n > 1) problems.push(`${p} requested ${n} times`);
    if (p.endsWith('.js') && !isVendor(p) && !Object.hasOwn(named.imports, p)) problems.push(`module ${p} is not in the import map`);
  }
  for (const p of named.entries) {
    if (!count.has(p)) problems.push(`entry module ${p} never loaded`);
  }

  for (const u of rec.failed) problems.push(`request failed: ${new URL(u).pathname}`);
  for (const r of rec.responses) {
    const u = new URL(r.url);
    if (r.status !== 200) problems.push(`${r.status} for ${u.pathname}${u.search}`);
    else if (r.cacheControl !== IMMUTABLE_CACHE) problems.push(`${u.pathname}${u.search} sent Cache-Control "${r.cacheControl}", want "${IMMUTABLE_CACHE}"`);
  }
  return problems;
}
