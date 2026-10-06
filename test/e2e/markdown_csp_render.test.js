// @ts-check
//
// KaTeX and mermaid under the dashboard CSP. style-src has no 'unsafe-inline',
// so the style attributes and <style> elements both libraries emit as markup
// are refused by the browser; render_md.js re-applies them through CSSOM. The
// other markdown tests block both loads, so without this file nothing renders
// either library under the real policy.
//
// KaTeX comes from the mock's /static/vendor/, the files the binary embeds.
// Mermaid is 75 MB on npm, so its one bundle is fetched from the CDN through
// the route.
//
// 跑法：cd test/e2e && npx playwright test markdown_csp_render.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

// Two of the mock's default ready sessions.
const KEY_A = 'dashboard:direct:2026-01-01-120000-1:myproject';
const KEY_B = 'dashboard:direct:2026-01-01-120002-3:myproject';

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'CSP enforcement is viewport-independent; desktop-chrome only');
  }
});

/** @param {string} text */
function eventsWith(text) {
  return [
    { type: 'user', detail: 'show me', time: Date.now() - 2000, uuid: 'u-' + text.length },
    { type: 'text', detail: text, time: Date.now() - 1000, uuid: 't-' + text.length },
  ];
}

/** @param {import('@playwright/test').BrowserContext} ctx */
async function routeMermaid(ctx) {
  await ctx.route(/cdn\.jsdelivr\.net\/npm\/mermaid@/, async route => {
    let lastErr;
    for (let i = 0; i < 3; i++) {
      try {
        const resp = await route.fetch();
        await route.fulfill({ response: resp });
        return;
      } catch (e) { lastErr = e; }
    }
    throw lastErr;
  });
}

test('KaTeX and mermaid render with their styles under the CSP', async ({ browser }) => {
  const mock = await startMockServer({
    eventsByKey: {
      // A: KaTeX not loaded yet, so the formula takes the pending path.
      [KEY_A]: eventsWith('Pending path: $x^2 + y_1$, a note [[user_csp_note]] and a diagram\n\n```mermaid\ngraph TD;A-->B;\n```'),
      // B: opened after KaTeX loaded, so renderToString emits markup.
      [KEY_B]: eventsWith('Sync path: $\\frac{a}{b} + z^3$'),
    },
    memories: {
      user_csp_note: { found: true, slug: 'user_csp_note', body: 'Note with $m^2$ and\n\n```mermaid\ngraph LR;M-->N;\n```' },
    },
  });
  const ctx = await browser.newContext();
  await routeMermaid(ctx);
  const page = await ctx.newPage();
  try {
    await page.goto(mock.url + '/dashboard');
    await page.locator(`.session-card[data-key="${KEY_A}"]`).click();
    await expect(page.locator('#events-scroll .katex').first()).toBeVisible({ timeout: 15000 });
    await expect(page.locator('#events-scroll .mermaid svg')).toBeVisible({ timeout: 30000 });

    // Mermaid: the diagram's theme sheet applies. Unstyled SVG shapes fill black.
    await expect.poll(() => page.evaluate(() => {
      const shape = document.querySelector('#events-scroll .mermaid svg .node rect, #events-scroll .mermaid svg .node polygon');
      return shape ? getComputedStyle(shape).fill : 'missing';
    }), { timeout: 10000 }).not.toMatch(/^(missing|rgb\(0, 0, 0\))$/);

    // The memory popover renders markdown into its own container, so it has to
    // flush the pending diagram and style the formula like the chat does.
    await page.locator('#events-scroll .md-memlink[data-slug="user_csp_note"]').click();
    await expect(page.locator('.mem-pop-body .katex')).toBeVisible({ timeout: 10000 });
    await expect(page.locator('.mem-pop-body .mermaid svg')).toBeVisible({ timeout: 15000 });
    expect(await page.evaluate(() => {
      const strut = /** @type {HTMLElement|null} */ (document.querySelector('.mem-pop-body .katex .strut'));
      return strut ? strut.style.height : 'missing';
    })).not.toMatch(/^(missing|)$/);
    await page.keyboard.press('Escape');

    await page.locator(`.session-card[data-key="${KEY_B}"]`).click();
    await expect(page.locator('#events-scroll .katex').first()).toBeVisible({ timeout: 15000 });

    // KaTeX: every strut carries its height through CSSOM. A refused style
    // attribute leaves el.style empty and the strut at zero height, which is
    // what collapses fractions and superscripts.
    const struts = await page.evaluate(() => [...document.querySelectorAll('#events-scroll .katex .strut')].map(el => ({
      attr: el.getAttribute('style') || '',
      applied: /** @type {HTMLElement} */ (el).style.height,
    })));
    expect(struts.length).toBeGreaterThan(0);
    for (const s of struts) {
      expect(s.attr).toMatch(/height/);
      expect(s.applied, `strut style "${s.attr}" not applied`).not.toBe('');
    }
    // The stylesheet's fonts come from /static/vendor/ under font-src 'self'.
    await expect.poll(() => page.evaluate(() => [...document.fonts]
      .some(f => f.family.replace(/"/g, '') === 'KaTeX_Main' && f.status === 'loaded'))).toBe(true);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
