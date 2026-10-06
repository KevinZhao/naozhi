// @ts-check
//
// Memory [[wiki-link]] popover (mem_popover.js, docs/rfc/memory-link-rendering.md):
// hover preview and leave grace, click/Enter pinning until Escape or an
// outside mousedown, the 404 negative cache that marks a chip broken and stops
// refetching it, and the copy handler that turns chips back into [[slug]].
// markdown_csp_render.test.js only checks the popover's markdown under the CSP.
//
// 跑法：cd test/e2e && npx playwright test mem_popover.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';
const KNOWN = 'user_known_note';
const GONE = 'project_gone_note';

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'hover-driven; desktop-chrome only');
  }
});

test.describe('memory wiki-link popover', () => {
  let mock;
  test.beforeAll(async () => {
    mock = await startMockServer({
      eventsByKey: {
        [KEY]: [
          { type: 'user', detail: 'notes?', time: Date.now() - 2000, uuid: 'u-mem' },
          { type: 'text', detail: `See [[${KNOWN}]] and [[${GONE}]] here.`, time: Date.now() - 1000, uuid: 't-mem' },
        ],
      },
      memories: {
        [KNOWN]: { found: true, slug: KNOWN, type: 'user', description: 'A known note', body: 'Body text' },
      },
    });
  });
  test.afterAll(() => { mock.server.close(); });

  /** @param {import('@playwright/test').Page} page */
  async function open(page) {
    /** @type {string[]} */
    const fetched = [];
    page.on('request', (r) => {
      const m = new URL(r.url()).pathname.match(/^\/api\/memory\/(.+)$/);
      if (m) fetched.push(decodeURIComponent(m[1]));
    });
    // The fake clock flows in real time; runFor() fires the hover and leave
    // timers on demand for the "nothing happens after the delay" assertions.
    await page.clock.install();
    await page.goto(mock.url + '/dashboard');
    await page.locator(`.session-card[data-key="${KEY}"]`).click();
    await expect(page.locator(`#events-scroll .md-memlink[data-slug="${GONE}"]`)).toBeVisible();
    return fetched;
  }
  const away = (page) => page.locator('#events-scroll .event', { hasText: 'notes?' }).first();
  const chip = (page, slug) => page.locator(`#events-scroll .md-memlink[data-slug="${slug}"]`);

  test('hover previews and leave hides; a click pins until an outside mousedown', async ({ page }) => {
    await open(page);
    const pop = page.locator('#mem-popover');
    await chip(page, KNOWN).hover();
    await expect(pop).toHaveClass(/\bshow\b/);
    await expect(pop.locator('.mem-pop-desc')).toHaveText('A known note');
    await expect(pop).not.toHaveClass(/\bpinned\b/);
    await away(page).hover();
    await expect(pop).not.toHaveClass(/\bshow\b/);

    await chip(page, KNOWN).click();
    await expect(pop).toHaveClass(/\bpinned\b/);
    await away(page).hover();
    await page.clock.runFor(500); // past the 200ms leave grace
    await expect(pop).toHaveClass(/\bshow\b/);
    await away(page).click();
    await expect(pop).toHaveAttribute('aria-hidden', 'true');
    await expect(pop).not.toHaveClass(/\bshow\b/);
  });

  test('a 404 slug is marked broken and never fetched again', async ({ page }) => {
    const fetched = await open(page);
    const pop = page.locator('#mem-popover');
    await chip(page, GONE).click();
    await expect(pop.locator('#mem-pop-content')).toHaveText('未找到该记忆');
    await expect(chip(page, GONE)).toHaveClass(/\bmd-memlink-broken\b/);
    await page.keyboard.press('Escape');
    await expect(pop).not.toHaveClass(/\bshow\b/);

    await away(page).hover(); // the click left the pointer on the chip
    await chip(page, GONE).hover();
    await page.clock.runFor(700); // past the 300ms hover debounce
    await expect(pop).not.toHaveClass(/\bshow\b/);
    expect(fetched.filter((s) => s === GONE)).toEqual([GONE]);
  });

  test('Enter on a focused chip opens a pinned preview and Escape closes it', async ({ page }) => {
    await open(page);
    const pop = page.locator('#mem-popover');
    await chip(page, KNOWN).focus();
    await page.keyboard.press('Enter');
    await expect(pop).toHaveClass(/\bpinned\b/);
    await expect(pop.locator('.mem-pop-body')).toContainText('Body text');
    await page.keyboard.press('Escape');
    await expect(pop).toHaveAttribute('aria-hidden', 'true');
  });

  test('copying a selection that crosses chips yields [[slug]] text', async ({ page }) => {
    await open(page);
    const out = await page.evaluate(() => {
      const bubble = /** @type {Element} */ (document.querySelector('#events-scroll .md-memlink')).parentElement;
      const range = document.createRange();
      range.selectNodeContents(/** @type {Element} */ (bubble));
      const sel = /** @type {Selection} */ (document.getSelection());
      sel.removeAllRanges();
      sel.addRange(range);
      const dt = new DataTransfer();
      const ev = new ClipboardEvent('copy', { clipboardData: dt, bubbles: true, cancelable: true });
      document.dispatchEvent(ev);
      return { text: dt.getData('text/plain'), prevented: ev.defaultPrevented };
    });
    expect(out.prevented).toBe(true);
    expect(out.text).toContain(`See [[${KNOWN}]] and [[${GONE}]] here.`);
  });
});
