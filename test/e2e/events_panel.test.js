// @ts-check
// Regressions for the session events panel:
//
//  1. renameSession must not wipe the conversation. It used to call
//     renderMainShell, which rebuilds #events-scroll empty while nothing on the
//     rename path refetches history (WS cursor untouched, poll `after=` past
//     every event) — the panel stayed blank until the session was re-selected.
//  2. Same-millisecond thinking → text must not lose the text bubble. The WS
//     incremental history path gated on `e.time <= lastRenderedEventTime`, and
//     eventHtml(thinking) renders nothing yet still advanced the cursor, so a
//     text block sharing the thinking block's millisecond (one CLI frame's
//     content blocks; ACP's trailing thinking/text/result) was dropped. The
//     gate is now strict `<` and same-ms events dedup by uuid instead.
//
// The mock server rejects /ws to force HTTP fallback, so (2) drives the
// exported `wsm.onHistory` / `appendEvents` directly in-page — the exact
// functions a live socket / poll tick invokes per frame (same technique as
// ws_dom_trim.test.js).
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };
const SESSION_KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';

async function openSession(browser, mock) {
  const ctx = await browser.newContext({ ...desktop });
  const page = await ctx.newPage();
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click(`.session-card[data-key="${SESSION_KEY}"]`);
  // The fixture has a user + text bubble; wait for the poll to paint them.
  await page.waitForSelector('#events-scroll .event.text');
  return { ctx, page };
}

test.describe('Session events panel regressions', () => {
  let mock;
  test.beforeAll(async () => { mock = await startMockServer(); });
  test.afterAll(() => mock.server.close());

  test('renameSession keeps the rendered conversation (header-only repaint)', async ({ browser }) => {
    const { ctx, page } = await openSession(browser, mock);

    const before = await page.evaluate(() => {
      const el = document.getElementById('events-scroll');
      // Tag the live scroller so we can prove the same node survives.
      el.setAttribute('data-e2e-marker', 'keep');
      return el.querySelectorAll(':scope > .event').length;
    });
    expect(before).toBeGreaterThanOrEqual(2);

    // Kick off the rename flow (don't await — it blocks on promptDialog) and
    // drive the themed dialog like an operator would.
    await page.evaluate(() => { /** @type {any} */ (window).renameSession(); });
    await page.waitForSelector('.prompt-dialog .prompt-input');
    await page.fill('.prompt-dialog .prompt-input', '重命名后的标题');
    await page.click('.prompt-dialog .prompt-ok');

    // Header reflects the new title...
    await expect(page.locator('#main .main-header h2')).toContainText('重命名后的标题');
    expect(mock.labelCalls.length).toBe(1);

    // ...and the conversation is still on screen — same node, same bubbles.
    const after = await page.evaluate(() => {
      const el = document.getElementById('events-scroll');
      return {
        marker: el ? el.getAttribute('data-e2e-marker') : null,
        bubbles: el ? el.querySelectorAll(':scope > .event').length : -1,
      };
    });
    expect(after.marker).toBe('keep');
    expect(after.bubbles).toBe(before);

    // Survive a poll tick too (the poll must not double-paint or blank it).
    await page.waitForTimeout(1500);
    const later = await page.evaluate(() => document.querySelectorAll('#events-scroll > .event').length);
    expect(later).toBe(before);

    await ctx.close();
  });

  test('wsm.onHistory: same-ms thinking + text in one frame renders the text', async ({ browser }) => {
    const { ctx, page } = await openSession(browser, mock);

    const result = await page.evaluate(() => {
      // selectedKey/selectedNode/wsm are top-level lexical globals in
      // dashboard.js; reach them via a direct eval in the page scope.
      // eslint-disable-next-line no-eval
      const sk = eval('typeof selectedKey !== "undefined" ? selectedKey : null');
      const sn = eval('typeof selectedNode !== "undefined" ? selectedNode : "local"');
      const w = eval('typeof wsm !== "undefined" ? wsm : null');
      if (!w || !sk) return { err: 'globals missing' };
      const el = document.getElementById('events-scroll');
      const times = [...el.querySelectorAll('.event[data-time]')].map(n => Number(n.getAttribute('data-time')));
      const T = Math.max(...times, Date.now()) + 60000; // strictly newer than everything on screen
      const frame = () => ({
        key: sk, node: sn,
        events: [
          { type: 'thinking', detail: 'let me think', time: T, uuid: 'TH-same-ms' },
          { type: 'text', detail: 'SAME_MS_TEXT_MARKER', time: T, uuid: 'TX-same-ms' },
        ],
      });
      w.onHistory(frame());
      const afterFirst = el.querySelectorAll('.event.text[data-uuid="TX-same-ms"]').length;
      // Backend re-admits the watermark millisecond on the next backfill
      // (#2402): the replay must be absorbed by uuid, not painted twice.
      w.onHistory(frame());
      const afterReplay = el.querySelectorAll('.event.text[data-uuid="TX-same-ms"]').length;
      return { afterFirst, afterReplay, thinking: el.querySelectorAll('.event.thinking').length };
    });

    expect(result.err).toBeUndefined();
    expect(result.afterFirst).toBe(1);
    expect(result.afterReplay).toBe(1);
    expect(result.thinking).toBe(0);

    await ctx.close();
  });

  test('wsm.onHistory: thinking(T) in one frame, text(T) in the next still renders', async ({ browser }) => {
    const { ctx, page } = await openSession(browser, mock);

    const result = await page.evaluate(() => {
      // eslint-disable-next-line no-eval
      const sk = eval('typeof selectedKey !== "undefined" ? selectedKey : null');
      const sn = eval('typeof selectedNode !== "undefined" ? selectedNode : "local"');
      const w = eval('typeof wsm !== "undefined" ? wsm : null');
      if (!w || !sk) return { err: 'globals missing' };
      const el = document.getElementById('events-scroll');
      const times = [...el.querySelectorAll('.event[data-time]')].map(n => Number(n.getAttribute('data-time')));
      const T = Math.max(...times, Date.now()) + 120000;
      w.onHistory({ key: sk, node: sn, events: [{ type: 'thinking', detail: 'hmm', time: T, uuid: 'TH-split' }] });
      w.onHistory({ key: sk, node: sn, events: [{ type: 'text', detail: 'SPLIT_FRAME_TEXT', time: T, uuid: 'TX-split' }] });
      return { text: el.querySelectorAll('.event.text[data-uuid="TX-split"]').length };
    });

    expect(result.err).toBeUndefined();
    expect(result.text).toBe(1);

    await ctx.close();
  });

  test('appendEvents (poll path): two same-ms text blocks both render, replay dedups by uuid', async ({ browser }) => {
    const { ctx, page } = await openSession(browser, mock);

    const result = await page.evaluate(() => {
      const el = document.getElementById('events-scroll');
      const times = [...el.querySelectorAll('.event[data-time]')].map(n => Number(n.getAttribute('data-time')));
      const T = Math.max(...times, Date.now()) + 180000;
      const batch = [
        { type: 'text', detail: 'BLOCK_A', time: T, uuid: 'TX-a' },
        { type: 'text', detail: 'BLOCK_B', time: T, uuid: 'TX-b' },
      ];
      /** @type {any} */ (window).appendEvents(batch);
      const first = el.querySelectorAll('.event.text[data-uuid="TX-a"], .event.text[data-uuid="TX-b"]').length;
      /** @type {any} */ (window).appendEvents(batch);
      const replay = el.querySelectorAll('.event.text[data-uuid="TX-a"], .event.text[data-uuid="TX-b"]').length;
      return { first, replay };
    });

    expect(result.first).toBe(2);
    expect(result.replay).toBe(2);

    await ctx.close();
  });
});
