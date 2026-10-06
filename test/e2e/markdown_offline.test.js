// @ts-check
//
// KaTeX loads lazily from /static/vendor/ and mermaid from the CDN. When a
// load fails (offline, a VPC with no egress, a server mid-restart), the
// formula and the diagram have to stay readable as source and say why, and
// the page must not inject a fresh failing script on every re-render:
// render_md.js retries once, CDN_RETRY_MS (60s) after the failure, and then
// waits for a page reload.
//
// 跑法：cd test/e2e && npx playwright test markdown_offline.test.js --project=desktop-chrome

const fs = require('fs');
const path = require('path');
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const KEY_A = 'dashboard:direct:2026-01-01-120000-1:myproject';
const DIAGRAM = 'graph TD;A-->B;';
const TEXT = 'Formula $x^2 + y_1$ and a diagram\n\n```mermaid\n' + DIAGRAM + '\n```';
const KATEX_DIST = path.join(__dirname, '..', '..', 'internal', 'server', 'static', 'vendor', 'katex-0.16.21');
const RETRY = '01:01';

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'lazy loading is viewport-independent; desktop-chrome only');
  }
});

/** @typedef {'katex' | 'katexCSS' | 'mermaid'} Asset */

/**
 * Opens session A with the KaTeX and mermaid loads routed through serve, which
 * answers each request (counted per asset) or aborts it; anything else under
 * /static/vendor/ or on the CDN (the KaTeX fonts) is aborted. The page clock
 * is installed so the retry delay can be skipped.
 *
 * @param {import('@playwright/test').Browser} browser
 * @param {(asset: Asset, n: number, route: import('@playwright/test').Route) => Promise<void>} serve
 */
async function openOffline(browser, serve) {
  const mock = await startMockServer({
    eventsByKey: {
      [KEY_A]: [
        { type: 'user', detail: 'draw it', time: Date.now() - 2000, uuid: 'off-user' },
        { type: 'text', detail: TEXT, time: Date.now() - 1000, uuid: 'off-text' },
      ],
    },
  });
  const ctx = await browser.newContext();
  const requests = { katex: 0, katexCSS: 0, mermaid: 0 };
  await ctx.route(/\/static\/vendor\/|cdn\.jsdelivr\.net/, route => route.abort());
  await ctx.route(/\/static\/vendor\/katex-[^/]+\/katex\.min\.(js|css)$|cdn\.jsdelivr\.net\/npm\/mermaid@[^/]+\/dist\/mermaid\.min\.js$/, route => {
    const url = route.request().url();
    /** @type {Asset} */
    const asset = url.includes('/mermaid@') ? 'mermaid' : url.endsWith('.css') ? 'katexCSS' : 'katex';
    return serve(asset, ++requests[asset], route);
  });
  const page = await ctx.newPage();
  await page.clock.install();
  await page.goto(mock.url + '/dashboard');
  await page.locator(`.session-card[data-key="${KEY_A}"]`).click();
  await page.waitForSelector('#events-scroll .event');
  const cleanup = async () => {
    await ctx.close();
    mock.server.close();
  };
  return { page, requests, cleanup };
}

/** @param {import('@playwright/test').Page} page */
async function assetCounts(page) {
  return page.evaluate(() => ({
    katex: document.querySelectorAll('script[src*="katex.min.js"]').length,
    katexCSS: document.querySelectorAll('link[href*="katex.min.css"]').length,
    mermaid: document.querySelectorAll('script[src*="mermaid.min.js"]').length,
  }));
}

/**
 * @param {string} type
 * @param {Buffer} body
 */
function fulfill(type, body) {
  return { status: 200, headers: { 'Content-Type': type, 'Access-Control-Allow-Origin': '*' }, body };
}

/**
 * Appends n more bubbles carrying math and a diagram, each one a re-render
 * that flushes the pending KaTeX and mermaid slots.
 *
 * @param {import('@playwright/test').Page} page
 * @param {number} n
 * @param {string} tag
 */
async function rerender(page, n, tag) {
  for (let i = 0; i < n; i++) {
    await page.evaluate(([detail, uuid]) => (/** @type {any} */ (window)).appendEvents([{
      type: 'text', detail, time: Date.now() + 1000, uuid,
    }]), [TEXT, `off-${tag}-${i}`]);
  }
}

test('offline: the diagram and formula show their source, and re-renders inject no new script', async ({ browser }) => {
  const { page, requests, cleanup } = await openOffline(browser, (_asset, _n, route) => route.abort());
  try {
    const pre = page.locator('#events-scroll pre.mermaid-pending').first();
    await expect(pre).toHaveClass(/md-render-unavailable/);
    await expect(pre).toHaveText(DIAGRAM);
    // The hint comes from CSS generated content, not a style attribute (CSP).
    expect(await pre.evaluate(el => getComputedStyle(el, '::before').content)).toContain('离线');
    const ktx = page.locator('#events-scroll .katex-pending').first();
    await expect(ktx).toHaveClass(/md-render-unavailable/);
    await expect(ktx).toHaveText('x^2 + y_1');
    await expect(ktx).toHaveAttribute('title', /离线/);

    await rerender(page, 5, 'a');
    await expect(page.locator('#events-scroll pre.mermaid-pending.md-render-unavailable')).toHaveCount(6);
    await expect(page.locator('#events-scroll .katex-pending.md-render-unavailable')).toHaveCount(6);
    await expect(page.locator('#events-scroll pre.mermaid-pending').last()).toHaveText(DIAGRAM);
    expect(await assetCounts(page)).toEqual({ katex: 1, katexCSS: 1, mermaid: 1 });
    expect(requests).toEqual({ katex: 1, katexCSS: 1, mermaid: 1 });

    // One retry falls due after the delay; it fails too, and that is the last.
    await page.clock.fastForward(RETRY);
    await expect.poll(() => assetCounts(page)).toEqual({ katex: 2, katexCSS: 2, mermaid: 2 });
    await expect.poll(() => ({ ...requests })).toEqual({ katex: 2, katexCSS: 2, mermaid: 2 });
    await page.clock.fastForward(RETRY);
    await rerender(page, 5, 'b');
    await page.clock.fastForward(RETRY);
    await expect(page.locator('#events-scroll pre.mermaid-pending.md-render-unavailable')).toHaveCount(11);
    expect(await assetCounts(page)).toEqual({ katex: 2, katexCSS: 2, mermaid: 2 });
    expect(requests).toEqual({ katex: 2, katexCSS: 2, mermaid: 2 });
  } finally {
    await cleanup();
  }
});

// Both KaTeX files fail at first and come back for the retry. The stylesheet
// has to come back with the script: KaTeX markup without it shows the MathML
// copy next to the HTML one.
test('a failed KaTeX load heals on the retry, stylesheet included', async ({ browser }) => {
  const katexJS = fs.readFileSync(path.join(KATEX_DIST, 'katex.min.js'));
  const katexCSS = fs.readFileSync(path.join(KATEX_DIST, 'katex.min.css'));
  const { page, requests, cleanup } = await openOffline(browser, (asset, n, route) => {
    if (n !== 2 || asset === 'mermaid') return route.abort();
    return route.fulfill(asset === 'katex' ? fulfill('application/javascript', katexJS) : fulfill('text/css', katexCSS));
  });
  try {
    const ktx = page.locator('#events-scroll .katex-pending').first();
    await expect(ktx).toHaveClass(/md-render-unavailable/);
    await page.clock.fastForward(RETRY);
    await expect(ktx.locator('.katex')).toBeVisible();
    await expect(ktx).not.toHaveClass(/md-render-unavailable/);
    await expect(ktx).toHaveAttribute('title', '');
    await expect.poll(() => ({ ...requests })).toEqual({ katex: 2, katexCSS: 2, mermaid: 2 });
    // The stylesheet is live: the MathML copy is clipped out of view.
    expect(await ktx.locator('.katex-mathml').evaluate(el => getComputedStyle(el).position)).toBe('absolute');
    // Mermaid's retry fails, and the diagram keeps showing its source.
    await expect(page.locator('#events-scroll pre.mermaid-pending.md-render-unavailable')).toHaveText(DIAGRAM);
  } finally {
    await cleanup();
  }
});

// The stylesheet loaded the first time and only the script failed: the retry
// re-requests the script alone.
test('a KaTeX retry re-requests only the asset that failed', async ({ browser }) => {
  const katexJS = fs.readFileSync(path.join(KATEX_DIST, 'katex.min.js'));
  const katexCSS = fs.readFileSync(path.join(KATEX_DIST, 'katex.min.css'));
  const { page, requests, cleanup } = await openOffline(browser, (asset, n, route) => {
    if (asset === 'katexCSS') return route.fulfill(fulfill('text/css', katexCSS));
    if (asset === 'katex' && n === 2) return route.fulfill(fulfill('application/javascript', katexJS));
    return route.abort();
  });
  try {
    const ktx = page.locator('#events-scroll .katex-pending').first();
    await expect(ktx).toHaveClass(/md-render-unavailable/);
    await page.clock.fastForward(RETRY);
    await expect(ktx.locator('.katex')).toBeVisible();
    await expect.poll(() => assetCounts(page)).toEqual({ katex: 2, katexCSS: 1, mermaid: 2 });
    expect(requests.katexCSS).toBe(1);
  } finally {
    await cleanup();
  }
});
