// @ts-check
//
// KaTeX and mermaid load lazily from the CDN. When that load fails (offline,
// or a VPC with no egress), the formula and the diagram have to stay readable
// as source and say why, and the page must not inject a fresh failing script
// on every re-render: render_md.js retries once, CDN_RETRY_MS (60s) after the
// failure, and then waits for a page reload.
//
// 跑法：cd test/e2e && npx playwright test markdown_offline.test.js --project=desktop-chrome

const fs = require('fs');
const path = require('path');
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const KEY_A = 'dashboard:direct:2026-01-01-120000-1:myproject';
const DIAGRAM = 'graph TD;A-->B;';
const TEXT = 'Formula $x^2 + y_1$ and a diagram\n\n```mermaid\n' + DIAGRAM + '\n```';
const KATEX_DIST = path.join(__dirname, 'node_modules', 'katex', 'dist');
const RETRY = '01:01';

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'CDN loading is viewport-independent; desktop-chrome only');
  }
});

/**
 * Opens session A with the CDN routed through serve, which answers each
 * request (counted per asset) or aborts it. The page clock is installed so
 * the retry delay can be skipped.
 *
 * @param {import('@playwright/test').Browser} browser
 * @param {(asset: 'katex' | 'mermaid', n: number, route: import('@playwright/test').Route) => Promise<void>} serve
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
  const requests = { katex: 0, mermaid: 0 };
  await ctx.route(/cdn\.jsdelivr\.net\/npm\/(katex|mermaid)@[^/]+\/dist\/[^/]+\.min\.js$/, route => {
    const asset = /** @type {'katex' | 'mermaid'} */ (route.request().url().includes('/katex@') ? 'katex' : 'mermaid');
    return serve(asset, ++requests[asset], route);
  });
  await ctx.route(/cdn\.jsdelivr\.net\/npm\/katex@.*\.css$/, route => route.abort());
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
async function scriptCounts(page) {
  return page.evaluate(() => ({
    katex: document.querySelectorAll('script[src*="katex.min.js"]').length,
    mermaid: document.querySelectorAll('script[src*="mermaid.min.js"]').length,
  }));
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
    expect(await scriptCounts(page)).toEqual({ katex: 1, mermaid: 1 });
    expect(requests).toEqual({ katex: 1, mermaid: 1 });

    // One retry falls due after the delay; it fails too, and that is the last.
    await page.clock.fastForward(RETRY);
    await expect.poll(() => scriptCounts(page)).toEqual({ katex: 2, mermaid: 2 });
    await expect.poll(() => ({ ...requests })).toEqual({ katex: 2, mermaid: 2 });
    await page.clock.fastForward(RETRY);
    await rerender(page, 5, 'b');
    await page.clock.fastForward(RETRY);
    await expect(page.locator('#events-scroll pre.mermaid-pending.md-render-unavailable')).toHaveCount(11);
    expect(await scriptCounts(page)).toEqual({ katex: 2, mermaid: 2 });
    expect(requests).toEqual({ katex: 2, mermaid: 2 });
  } finally {
    await cleanup();
  }
});

test('a failed KaTeX load heals on the retry', async ({ browser }) => {
  const katexJS = fs.readFileSync(path.join(KATEX_DIST, 'katex.min.js'));
  const { page, requests, cleanup } = await openOffline(browser, (asset, n, route) => (asset === 'katex' && n === 2
    ? route.fulfill({ status: 200, headers: { 'Content-Type': 'application/javascript', 'Access-Control-Allow-Origin': '*' }, body: katexJS })
    : route.abort()));
  try {
    const ktx = page.locator('#events-scroll .katex-pending').first();
    await expect(ktx).toHaveClass(/md-render-unavailable/);
    await page.clock.fastForward(RETRY);
    await expect(ktx.locator('.katex')).toBeVisible();
    await expect(ktx).not.toHaveClass(/md-render-unavailable/);
    await expect(ktx).toHaveAttribute('title', '');
    expect(requests.katex).toBe(2);
    // Mermaid's retry fails, and the diagram keeps showing its source.
    await expect(page.locator('#events-scroll pre.mermaid-pending.md-render-unavailable')).toHaveText(DIAGRAM);
  } finally {
    await cleanup();
  }
});
