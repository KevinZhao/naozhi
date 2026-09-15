// @ts-check
// #2429, two palette contracts that were pinned as substrings of auth_modal.js
// and are both directly observable.
//
// 1. Path highlight ranges must be computed against the path the row actually
//    shows. Rows render deps.shortPath(p.path), which collapses the home prefix
//    to "~", so ranges taken from the full path land on the wrong characters
//    when applied to the shorter string. The retired anchor checked this by
//    requiring `matchProjectPath(q, p.path)` to appear and
//    `fuzzyMatch(q, p.path)` not to — which says nothing about where the <mark>
//    ends up.
//
// 2. Hovering a row must move the KEYBOARD cursor, not just the highlight.
//    setActiveIdx paints the .active class; the bug was that hover called only
//    that and never wrote state.activeIdx, so Enter still picked whatever the
//    keyboard had selected. The anchor forbade the literal `setActiveIdx(idx))`
//    on mouseenter, which a refactor satisfies by renaming while keeping the
//    same one-sided update — and the .active class is set either way, so
//    asserting on it would not catch the bug. Enter is what tells them apart.
//
// 跑法：cd test/e2e && npx playwright test project_palette.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'palette behaviour is viewport-independent; desktop-chrome only');
  }
});

/**
 * @param {import('@playwright/test').Browser} browser
 */
async function openPalette(browser) {
  const mock = await startMockServer();
  const ctx = await browser.newContext({ ...desktop });
  const page = await ctx.newPage();
  /** @type {{url: string, body: string}[]} */
  const posts = [];
  page.on('request', r => {
    if (r.method() === 'POST') posts.push({ url: r.url(), body: r.postData() || '' });
  });
  const cleanup = async () => {
    await ctx.close();
    mock.server.close();
  };
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.evaluate(() => (/** @type {any} */ (window)).openProjectPalette());
  await page.waitForSelector('.cmd-palette-overlay .cmd-palette-item');
  return { page, posts, cleanup };
}

test.describe('#2429 项目命令面板', () => {
  test('路径高亮框住的是行上真正显示的字符', async ({ browser }) => {
    const { page, cleanup } = await openPalette(browser);
    try {
      // Rows show ~/workspace/<name>. "work" sits at index 2 there and at
      // index 11 in /home/user/workspace/<name>, so ranges taken from the full
      // path would mark "ace/m" instead.
      await page.fill('.cmd-palette input', 'work');
      const marks = page.locator('.cmd-palette-item .cp-path mark');
      await expect(marks.first()).toBeVisible();

      // Per row: the marked runs, joined, must be exactly what was typed. The
      // fuzzy matcher may split a query across runs within one row, so join
      // within a row — not across rows, of which there are several.
      const rows = page.locator('.cmd-palette-item:has(.cp-path mark)');
      const rowCount = await rows.count();
      expect(rowCount).toBeGreaterThan(0);
      for (let i = 0; i < rowCount; i++) {
        const runs = await rows.nth(i).locator('.cp-path mark').allInnerTexts();
        expect(runs.join('').toLowerCase(), `row ${i} marked the wrong characters`).toBe('work');
      }

      // And the row really is showing the shortened path, or the assertion
      // above would be checking the wrong string.
      const shown = await page.locator('.cmd-palette-item .cp-path').first().innerText();
      expect(shown).toContain('~/');
      expect(shown).not.toContain('/home/');
    } finally {
      await cleanup();
    }
  });

  test('鼠标悬停会移动键盘光标：悬停后回车打开的是被悬停的那一行', async ({ browser }) => {
    const { page, posts, cleanup } = await openPalette(browser);
    try {
      const items = page.locator('.cmd-palette-overlay .cmd-palette-item');
      const count = await items.count();
      expect(count, 'need at least two rows to tell hover from keyboard').toBeGreaterThan(1);

      // The Enter handler is bound to the palette input, so focus has to be
      // there or the keypress goes nowhere — without this the test passes for
      // the wrong reason (measured: zero requests after Enter).
      await page.click('.cmd-palette input');
      // The keyboard cursor starts on row 0.
      await expect(items.nth(0)).toHaveClass(/active/);

      // Pick a row that is not the cursor's and that carries a path, so the
      // outbound request can be attributed to it.
      const target = page.locator('.cmd-palette-item:has(.cp-path)').nth(1);
      const wantedPath = await target.locator('.cp-path').innerText();
      await target.hover();
      await expect(target).toHaveClass(/active/);
      await expect(items.nth(0)).not.toHaveClass(/active/);

      // Enter reads state.items[state.activeIdx], so a hover that only
      // repainted .active would open row 0 here.
      await page.keyboard.press('Enter');

      await expect.poll(() => posts.length, { timeout: 5000 }).toBeGreaterThan(0);
      const bodies = posts.map(p => p.body).join('\n');
      // The shown path is shortened; compare on the project's folder name,
      // which appears in both forms.
      const folder = wantedPath.split('/').filter(Boolean).pop() || '';
      expect(folder.length).toBeGreaterThan(0);
      expect(bodies, `Enter after hovering row 1 did not open ${folder}`).toContain(folder);
    } finally {
      await cleanup();
    }
  });
});
