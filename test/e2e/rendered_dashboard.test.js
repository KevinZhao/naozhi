// @ts-check
// Boots the dashboard from the page the Go server renders (#3330), not the raw
// file the rest of the suite gets: entry modules become inline loaders, an
// import map pins every module to its ?v= URL, modulepreload links fetch them
// early, and the CSP carries a hash per inline script. tools/render-dashboard
// writes that page and policy (CI renders it once and passes the directory in
// NAOZHI_RENDERED_DASHBOARD_DIR); the mock serves them with its usual stubs.
// Both projects run it: import maps and modulepreload are where engines differ.
const { test, expect } = require('@playwright/test');
const { execFileSync } = require('child_process');
const fs = require('fs');
const os = require('os');
const path = require('path');
const { startMockServer } = require('./mock-server');

const REPO = path.join(__dirname, '..', '..');

// renderedDashboard returns the rendered page and CSP, or null when Go is not
// installed. A missing toolchain fails under CI: a required check must not
// pass by skipping.
function renderedDashboard() {
  const given = process.env.NAOZHI_RENDERED_DASHBOARD_DIR;
  const dir = given || fs.mkdtempSync(path.join(os.tmpdir(), 'nz-rendered-dashboard-'));
  try {
    if (!given) execFileSync('go', ['run', './tools/render-dashboard', '-out', dir], { cwd: REPO, stdio: 'pipe', timeout: 240000 });
    return {
      html: fs.readFileSync(path.join(dir, 'dashboard.html'), 'utf8'),
      csp: fs.readFileSync(path.join(dir, 'dashboard.csp'), 'utf8'),
    };
  } catch (e) {
    if (/** @type {NodeJS.ErrnoException} */ (e).code === 'ENOENT' && !given && !process.env.CI) return null;
    throw e;
  } finally {
    if (!given) fs.rmSync(dir, { recursive: true, force: true });
  }
}

// The entry modules the raw page names; the rendered page loads them through
// inline loaders that import the bare /static path.
const ENTRY_MODULES = [...fs.readFileSync(path.join(REPO, 'internal', 'server', 'static', 'dashboard.html'), 'utf8')
  .matchAll(/<script type="module" src="(\/static\/[\w.]+\.js)"><\/script>/g)].map((m) => m[1]);

const CONSOLE_ERROR_RE = /Content.Security.Policy|Refused to|import ?map|modulepreload|module specifier|does not resolve|resolve module|dynamically imported module|SyntaxError/i;

test.describe('dashboard rendered by the Go server', () => {
  /** @type {{html: string, csp: string} | null} */
  let rendered;
  let mock;

  test.beforeAll(async () => {
    test.setTimeout(300000);
    rendered = renderedDashboard();
    if (rendered) mock = await startMockServer({ dashboardPage: rendered, shim: false, ws: true });
  });
  test.afterAll(() => mock?.server.close());

  test('boots under its CSP with every /static asset versioned and fetched once', async ({ browser }) => {
    test.skip(!rendered, 'go is not installed; go run ./tools/render-dashboard is needed');
    expect(ENTRY_MODULES.length).toBeGreaterThan(0);

    const ctx = await browser.newContext();
    const page = await ctx.newPage();
    await page.addInitScript(() => {
      /** @type {any} */ (window).__cspv = [];
      document.addEventListener('securitypolicyviolation', (e) => {
        /** @type {any} */ (window).__cspv.push(`${e.violatedDirective} ${e.blockedURI}`);
      });
    });
    const errors = [];
    page.on('pageerror', (e) => errors.push(e.message));
    // Console errors are kept only when they name what this page can break:
    // the policy or module loading. Engines log other noise at error level
    // (unstubbed API 404s, viewport keys WebKit does not know); a /static
    // failure is caught on the responses below. require-sri-for is not a
    // directive browsers implement; they warn on it.
    page.on('console', (m) => {
      const text = m.text();
      if (m.type() === 'error' && CONSOLE_ERROR_RE.test(text) && !/require-sri-for/.test(text)) errors.push(text);
    });
    /** @type {URL[]} */
    const statics = [];
    page.on('request', (r) => {
      const u = new URL(r.url());
      if (u.pathname.startsWith('/static/')) statics.push(u);
    });
    const failed = [];
    page.on('requestfailed', (r) => { if (new URL(r.url()).pathname.startsWith('/static/')) failed.push(r.url()); });
    page.on('response', (r) => {
      if (new URL(r.url()).pathname.startsWith('/static/') && r.status() !== 200) failed.push(`${r.status()} ${r.url()}`);
    });

    try {
      await page.goto(mock.url + '/dashboard');
      // A blocked loader leaves the page without its modules: report the
      // violation rather than time out waiting for a card.
      await page.waitForFunction(() => document.querySelector('.session-card') || /** @type {any} */ (window).__cspv.length);
      expect(await page.evaluate(() => /** @type {any} */ (window).__cspv)).toEqual([]);
      await page.waitForSelector('.session-card');
      await page.waitForLoadState('networkidle');

      const named = await page.evaluate(() => {
        const maps = document.querySelectorAll('script[type="importmap"]');
        const imports = maps.length ? JSON.parse(maps[0].textContent || '{}').imports : {};
        const urls = [...Object.values(imports)];
        for (const el of document.querySelectorAll('[href^="/static/"], [src^="/static/"]')) {
          urls.push(el.getAttribute('href') || el.getAttribute('src'));
        }
        return { maps: maps.length, imports, urls, nz: typeof (/** @type {any} */ (window).nz), cspv: /** @type {any} */ (window).__cspv };
      });

      // The raw page has no import map: this guards against serving it.
      expect(named.maps).toBe(1);
      expect(named.nz).toBe('object');
      expect(named.cspv).toEqual([]);
      expect(errors).toEqual([]);
      expect(failed, '/static requests that did not answer 200').toEqual([]);

      // The one ?v= the page names for each /static path.
      /** @type {Map<string, Set<string>>} */
      const versionOf = new Map();
      for (const raw of named.urls) {
        const u = new URL(raw, mock.url);
        if (!versionOf.has(u.pathname)) versionOf.set(u.pathname, new Set());
        versionOf.get(u.pathname).add(u.searchParams.get('v') || '');
      }
      const ambiguous = [...versionOf].filter(([, vs]) => vs.size !== 1 || vs.has('')).map(([p, vs]) => `${p} ${[...vs]}`);
      expect(ambiguous, 'page names a /static path without a version or under two').toEqual([]);

      expect(statics.length).toBeGreaterThan(0);
      const unversioned = statics.filter((u) => {
        const vs = versionOf.get(u.pathname);
        return !vs || !vs.has(u.searchParams.get('v') || '');
      }).map((u) => u.pathname + u.search);
      expect(unversioned, '/static requests without the version the page names').toEqual([]);

      /** @type {Map<string, number>} */
      const count = new Map();
      for (const u of statics) count.set(u.pathname, (count.get(u.pathname) || 0) + 1);
      const twice = [...count].filter(([, n]) => n > 1).map(([p, n]) => `${p} x${n}`);
      expect(twice, '/static paths requested more than once').toEqual([]);

      const js = [...count.keys()].filter((p) => p.endsWith('.js'));
      expect(js.filter((p) => !(p in named.imports)), 'modules outside the import map').toEqual([]);
      expect(ENTRY_MODULES.filter((p) => !count.has(p)), 'entry modules never loaded').toEqual([]);
    } finally {
      await ctx.close();
    }
  });
});
