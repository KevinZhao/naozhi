// @ts-check
// The model sometimes writes tool-call syntax verbatim into an assistant *text*
// block instead of emitting a structured tool_use. dashboard.js's
// stripLeakedToolCalls detects that and folds the malformed payload behind a
// collapsed <details> so the bubble shows the prose, not a wall of XML.
//
// A false positive is worse than the bug: folding prose that merely quotes
// `<invoke name="…">` in backticks would shred legitimate technical discussion.
// So the boundary matters in both directions, and both sides of it — the Go
// runtime detector and the JS renderer — must agree.
//
// internal/leakguard/testdata/samples.json is the single table. Go reads it in
// internal/leakguard/samples_test.go and asserts leakguard.Detect's verdict;
// this file reads the same rows, renders each as a text event, and asserts the
// bubble folded iff the row says leak. Neither side can drift from the other
// without one of the two failing.
//
// This replaced internal/server/static_leaked_toolcall_test.go, which held the
// table in Go only and pinned the JS half with five substrings plus a check
// that `leakguard.Anchor` appeared verbatim inside dashboard.js (#2547). Textual
// identity of a regex literal is neither necessary nor sufficient: an identical
// literal that is never applied passes, and an exactly-equivalent rewrite
// (character class, escaping, a different quantifier) fails.
//
// 跑法：cd test/e2e && npx playwright test leaked_toolcall_fold.test.js --project=desktop-chrome

const fs = require('node:fs');
const path = require('node:path');
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };
const SESSION_KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';

/** @type {{name: string, text: string, leak: boolean}[]} */
const SAMPLES = JSON.parse(fs.readFileSync(
  path.join(__dirname, '..', '..', 'internal', 'leakguard', 'testdata', 'samples.json'), 'utf8'));

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'detection is viewport-independent; desktop-chrome only');
  }
});

/**
 * @param {import('@playwright/test').Browser} browser
 * @returns {Promise<{page: import('@playwright/test').Page, cleanup: () => Promise<void>}>}
 */
async function openSession(browser) {
  const mock = await startMockServer();
  const ctx = await browser.newContext({ ...desktop });
  const page = await ctx.newPage();
  const cleanup = async () => {
    await ctx.close();
    mock.server.close();
  };
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click(`.session-card[data-key="${SESSION_KEY}"]`);
  await page.waitForSelector('#events-scroll');
  return { page, cleanup };
}

test.describe('leaked tool-call fold', () => {
  test('每个样本的折叠判定与 leakguard 一致', async ({ browser }) => {
    const { page, cleanup } = await openSession(browser);
    try {
    // The table must exercise both verdicts, or "agrees with Go" would be
    // satisfiable by a renderer that folds everything (or nothing).
    expect(SAMPLES.filter(s => s.leak).length).toBeGreaterThan(0);
    expect(SAMPLES.filter(s => !s.leak).length).toBeGreaterThan(0);

    /** @type {{name: string, want: boolean, got: boolean}[]} */
    const disagreements = [];
    for (const [i, s] of SAMPLES.entries()) {
      const uuid = 'leak-' + i;
      const folded = await page.evaluate(async ({ detail, uuid }) => {
        const w = /** @type {any} */ (window);
        w.appendEvents([{ type: 'text', detail, time: Date.now() + 1000 + Number(uuid.split('-')[1]), uuid }]);
        const el = document.querySelector(`#events-scroll .event[data-uuid="${uuid}"]`)
          || Array.from(document.querySelectorAll('#events-scroll .event')).pop();
        if (!el) return null;
        return !!el.querySelector('.leaked-toolcall-summary, .leaked-toolcall-body');
      }, { detail: s.text, uuid });

      // An empty body renders no bubble at all, which is not a fold.
      const got = folded === null ? false : folded;
      if (got !== s.leak) disagreements.push({ name: s.name, want: s.leak, got });
    }
    expect(disagreements, 'dashboard.js and leakguard.Detect disagree on these samples')
      .toEqual([]);
    } finally {
      await cleanup();
    }
  });

  test('折叠出来的是收起的 details，prose 在外、XML 在内', async ({ browser }) => {
    const { page, cleanup } = await openSession(browser);
    try {
    const leak = SAMPLES.find(s => s.leak && s.text.includes('先读取它的完整范围'));
    expect(leak, 'the prose+leak sample must exist in samples.json').toBeTruthy();

    await page.evaluate((detail) => {
      (/** @type {any} */ (window)).appendEvents([{
        type: 'text', detail, time: Date.now() + 99000, uuid: 'leak-shape',
      }]);
    }, leak.text);

    const bubble = page.locator('#events-scroll .event[data-uuid="leak-shape"]');
    await expect(bubble).toHaveCount(1);

    // The prose survives outside the fold — that is the point of folding rather
    // than dropping.
    await expect(bubble).toContainText('先读取它的完整范围');

    const details = bubble.locator('details');
    await expect(details).toHaveCount(1);
    // Collapsed: the XML must not be on screen until the user asks for it.
    expect(await details.evaluate(el => /** @type {HTMLDetailsElement} */ (el).open)).toBe(false);
    await expect(bubble.locator('.leaked-toolcall-summary')).toHaveCount(1);

    const body = bubble.locator('.leaked-toolcall-body');
    await expect(body).toHaveCount(1);
    // The payload is inside the fold, and it is text — not a live element.
    await expect(body).toContainText('invoke name="Read"');
    await expect(body.locator('invoke')).toHaveCount(0);

    // The fold is styled: an unstyled <details> would be a wall of XML with a
    // triangle, which is the regression the CSS classes existed to prevent.
    const styled = await bubble.locator('.leaked-toolcall-summary').evaluate(el => {
      const cs = getComputedStyle(el);
      return { cursor: cs.cursor, display: cs.display };
    });
    expect(styled.display).not.toBe('inline');
    } finally {
      await cleanup();
    }
  });
});
