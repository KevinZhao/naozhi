// @ts-check
//
// R219-SEC-4: the KaTeX and Mermaid assets that render_md.js injects at runtime
// must carry SRI integrity hashes and crossOrigin='anonymous', so a compromised
// CDN response is rejected by the browser instead of executed.
//
// This replaced internal/server/static_cdn_sri_test.go, which read render_md.js
// and counted `integrity` / `sha384-` / `'anonymous'` occurrences inside the
// loadKatex and loadMermaid function-body windows (#2547). Counting occurrences
// in source text cannot see:
//
//   - whether the attributes reach the elements the browser actually loads. The
//     count is satisfied by any `integrity` token in the window, including one
//     in a comment or on an element that is never appended.
//   - whether the assets carry *different* hashes. A hash copy-pasted from a
//     sibling asset keeps every count at its threshold, and the browser then
//     rejects the asset at load time — which is the outcome SRI exists to
//     prevent, arrived at by the SRI itself.
//
// Both are checked here off the DOM, with the CDN blocked so no network is
// needed: aborting the request does not stop the element from being created and
// appended with its attributes.
//
// 跑法：cd test/e2e && npx playwright test cdn_sri.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const KEY_A = 'dashboard:direct:2026-01-01-120000-1:myproject';
const MERMAID_FENCE = '```mermaid\ngraph TD;A-->B;\n```';
/** Hash shape: base64 of SHA-384 is 64 chars, so 60+ is a safe floor. */
const SHA384 = /^sha384-[A-Za-z0-9+/]{60,}={0,2}$/;

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'asset injection is viewport-independent; desktop-chrome only');
  }
});

/**
 * Opens the dashboard with the CDN blocked, triggers both lazy loaders, and
 * returns every CDN asset element the page injected.
 *
 * KaTeX: rendering inline math takes the katexReady=false branch, which calls
 * loadKatex() and appends the stylesheet link plus the script.
 *
 * Mermaid: loadMermaid() is only reached from runMermaid(), which needs both a
 * pending diagram and a post-render flush, so this goes through the production
 * path — a session bubble carrying a mermaid fence.
 *
 * @param {import('@playwright/test').Browser} browser
 * @returns {Promise<{cleanup: () => Promise<void>, assets: {tag: string, url: string, integrity: string, crossOrigin: string}[]}>}
 */
async function injectedAssets(browser) {
  const mock = await startMockServer({
    eventsByKey: {
      [KEY_A]: [{ type: 'user', detail: 'draw me a graph', time: Date.now(), uuid: 'sri-user-1' }],
    },
  });
  const ctx = await browser.newContext();
  // Block the CDN: the injected elements still land in <head> with their
  // attributes, and nothing here depends on the real assets arriving.
  await ctx.route(/cdn\.jsdelivr\.net/, route => route.abort());
  const page = await ctx.newPage();
  const cleanup = async () => {
    await ctx.close();
    mock.server.close();
  };

  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click(`.session-card[data-key="${KEY_A}"]`);
  await page.waitForSelector('#events-scroll .event');

  await page.evaluate(() => (/** @type {any} */ (window)).renderMd('$x+y$'));
  await page.waitForSelector('link[data-nz-katex]', { state: 'attached' });
  await page.waitForSelector('script[src*="katex.min.js"]', { state: 'attached' });

  await page.evaluate((fence) => (/** @type {any} */ (window)).appendEvents([{
    type: 'text', detail: fence, time: Date.now() + 1000, uuid: 'sri-mermaid-1',
  }]), MERMAID_FENCE);
  await page.waitForSelector('script[src*="mermaid.min.js"]', { state: 'attached' });

  const assets = await page.evaluate(() =>
    Array.from(document.querySelectorAll(
      'script[src*="cdn.jsdelivr.net"], link[href*="cdn.jsdelivr.net"]'
    )).map(el => ({
      tag: el.tagName.toLowerCase(),
      url: el.getAttribute('src') || el.getAttribute('href') || '',
      integrity: el.getAttribute('integrity') || '',
      crossOrigin: el.getAttribute('crossorigin') || '',
    })));
  return { cleanup, assets };
}

test.describe('CDN asset injection carries SRI', () => {
  test('每个注入的 CDN 资产都带 sha384 SRI 与 crossOrigin=anonymous', async ({ browser }) => {
    const { cleanup, assets } = await injectedAssets(browser);
    try {
      // With the CDN blocked, onerror clears the loading flag, so a later
      // render can inject a second element for the same URL. De-dupe by URL:
      // the invariant is per asset, not per element.
      const byURL = groupByURL(assets);
      const urls = [...byURL.keys()].sort();
      expect(urls.filter(u => u.includes('/katex@'))).toHaveLength(2); // css + js
      expect(urls.filter(u => u.includes('/mermaid@'))).toHaveLength(1);

      for (const [url, els] of byURL) {
        for (const el of els) {
          expect(el.integrity, `${url} must carry an SRI hash`).toMatch(SHA384);
          expect(el.crossOrigin, `${url} must be crossOrigin=anonymous`).toBe('anonymous');
        }
        // Every element for one URL must agree: a retry that drops the hash
        // would load the asset unverified.
        expect(new Set(els.map(e => e.integrity)).size,
          `${url} was injected with more than one integrity value`).toBe(1);
      }
    } finally {
      await cleanup();
    }
  });

  test('不同资产的 SRI 哈希互不相同', async ({ browser }) => {
    const { cleanup, assets } = await injectedAssets(browser);
    try {
      const byURL = groupByURL(assets);
      expect(byURL.size).toBeGreaterThanOrEqual(3);
      /** @type {Map<string, string>} */
      const byHash = new Map();
      for (const [url, els] of byURL) {
        const hash = els[0].integrity;
        const prev = byHash.get(hash);
        expect(prev, `${url} reuses the SRI hash of ${prev}`).toBeUndefined();
        byHash.set(hash, url);
      }
    } finally {
      await cleanup();
    }
  });
});

/**
 * @param {{tag: string, url: string, integrity: string, crossOrigin: string}[]} assets
 * @returns {Map<string, {tag: string, url: string, integrity: string, crossOrigin: string}[]>}
 */
function groupByURL(assets) {
  /** @type {Map<string, any[]>} */
  const m = new Map();
  for (const a of assets) {
    const list = m.get(a.url) || [];
    list.push(a);
    m.set(a.url, list);
  }
  return m;
}
