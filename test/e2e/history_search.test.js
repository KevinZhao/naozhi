// The history popover's reply search (A9): typing asks the gateway for the
// sessions whose recent replies hold the query (GET /api/sessions/history?q=),
// a hit shows the sentence, and a click opens the session scrolled to that
// reply. Esc closes the popover even while its search box has the focus.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const KEY = 'dashboard:direct:2026-01-01-120001-2:otherproject';
const SHOTS = process.env.NZ_SHOTS_DIR || '';

// A long conversation whose matching reply sits far above the newest one.
function longEvents(now) {
  const out = [];
  for (let i = 0; i < 30; i++) {
    const t = now - (30 - i) * 60000;
    out.push({ type: 'user', detail: 'question ' + i, time: t, uuid: 'u-' + i });
    const text = i === 3 ? '结论：g6e 适合中小模型推理，p5 适合大规模训练。' : ('answer ' + i + '\n\n' + 'filler line\n\n'.repeat(4));
    out.push({ type: 'text', detail: text, time: t + 1000, uuid: 'r-' + i });
  }
  return out;
}

for (const [name, viewport] of [['desktop', { width: 1440, height: 900 }], ['mobile', { width: 390, height: 844 }]]) {
  test.describe(`history reply search (${name})`, () => {
    let mock;
    const now = Date.now();

    test.beforeAll(async () => {
      mock = await startMockServer({
        keyEvents: { [KEY]: longEvents(now) },
        replyHits: [
          { key: KEY, time: now - 27 * 60000 + 1000, uuid: 'r-3', snippet: '结论：g6e 适合中小模型推理，p5 适合大规模训练。' },
          { session_id: 'hist-001', time: now - 86400000, uuid: 'h-1', snippet: 'g6e.12xlarge 的 SPS 分数最高。' },
        ],
      });
    });
    test.afterAll(() => mock.server.close());

    test('a reply hit shows its sentence and opens the session at it', async ({ browser }) => {
      const ctx = await browser.newContext({ viewport });
      const page = await ctx.newPage();
      await page.goto(mock.url + '/dashboard');
      await page.waitForSelector('.session-card');
      await page.click('#btn-history');
      await page.locator('#hp-search').fill('G6E');

      const live = page.locator('.history-popover-item[data-action="history-jump"]');
      await expect(live).toHaveCount(1);
      await expect(live.locator('.hp-prompt')).toHaveText('review this code');
      await expect(live.locator('.hp-snippet mark')).toHaveText('g6e');
      // The history session matched by its reply only, not its prompt.
      const hist = page.locator('.history-popover-item[data-sid="hist-001"]');
      await expect(hist.locator('.hp-snippet')).toHaveText('g6e.12xlarge 的 SPS 分数最高。');
      expect(mock.historySearchCalls).toContain('g6e');
      if (SHOTS) await page.screenshot({ path: `${SHOTS}/history_search_${name}.png` });

      await live.click();
      await expect(page.locator('.history-popover, .history-sheet')).toHaveCount(0);
      const target = page.locator('#events-scroll .event[data-uuid="r-3"]');
      await expect(target).toHaveClass(/nav-highlight/);
      await expect(target).toBeInViewport();
      if (SHOTS) await page.screenshot({ path: `${SHOTS}/history_search_${name}_jump.png` });
      await ctx.close();
    });

    test('Esc closes the popover from its search box', async ({ browser }) => {
      const ctx = await browser.newContext({ viewport });
      const page = await ctx.newPage();
      await page.goto(mock.url + '/dashboard');
      await page.waitForSelector('.session-card');
      await page.click('#btn-history');
      await page.locator('#hp-search').focus();
      await page.keyboard.press('Escape');
      await expect(page.locator('.history-popover, .history-sheet')).toHaveCount(0);
      await expect(page.locator('.history-backdrop')).toHaveCount(0);
      await ctx.close();
    });
  });
}
