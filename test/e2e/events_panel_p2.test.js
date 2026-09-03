// @ts-check
// Regressions for the #2430 P2 items on the session events panel:
//
//  1. Markdown export must page the whole history. The old path issued one bare
//     `/api/sessions/events?key=` request, which the server answers from the
//     in-memory ring only (≤500 entries), then toasted "已导出 N 条" as if the
//     export were complete. The mock mirrors that ring cut-off for the bare
//     request and serves the rest through `before=&limit=` like handlers.go.
//  2. fetchEvents(full) must not be swallowed by the tail-poll in-flight gate.
//     With WS down (the mock rejects /ws), a slow `after=` poll on session A
//     used to make the click on session B return early; the next tick then ran
//     with lastEventTime=0 and no `limit`, appending the entire ring with no
//     "load earlier" button. The mock delays tail polls to force the race.
//  3. AskUserQuestion cards must lock when a `user` event arrives incrementally
//     (poll appendEvents / WS onEvent) and when the poll full path renders an
//     ask→user history, not only after a reload replays history via onHistory.
const { test, expect } = require('@playwright/test');
const fs = require('fs');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };
const KEY_A = 'dashboard:direct:2026-01-01-120000-1:myproject';
const KEY_B = 'dashboard:direct:2026-01-01-120001-2:otherproject';

// conversation(n) builds n alternating user/text events with strictly
// increasing timestamps (1 s apart) so both `after` and `before` cursors have
// unambiguous boundaries.
function conversation(n, tag) {
  const base = Date.now() - (n + 10) * 1000;
  const out = [];
  for (let i = 0; i < n; i++) {
    const user = i % 2 === 0;
    out.push({
      type: user ? 'user' : 'text',
      detail: (user ? 'Q' : 'A') + '-' + tag + '-' + i,
      time: base + i * 1000,
      uuid: 'ev-' + tag + '-' + i,
    });
  }
  return out;
}

async function openSession(page, mock, key) {
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click(`.session-card[data-key="${key}"]`);
  await page.waitForSelector('#events-scroll .event');
}

test.describe('Events panel #2430 P2', () => {
  test('export pages past the in-memory ring and reports the true count', async ({ browser }) => {
    const mock = await startMockServer({
      eventsByKey: { [KEY_A]: conversation(700, 'a') },
      eventsRingSize: 500,
    });
    try {
      const ctx = await browser.newContext({ ...desktop, acceptDownloads: true });
      const page = await ctx.newPage();
      await openSession(page, mock, KEY_A);

      const dl = page.waitForEvent('download');
      await page.click('.btn-download');
      const download = await dl;
      const md = fs.readFileSync(await download.path(), 'utf8');

      // 700 alternating events → 350 user headings; a ring-only export has 250.
      expect((md.match(/^## 用户/gm) || []).length).toBe(350);
      expect(md).toContain('Q-a-0');
      await expect(page.locator('#toast')).toContainText('已导出 700 条事件');
      await expect(page.locator('#toast')).not.toContainText('已截断');

      // The pager walked the `before=` cursor (two pages: 500 ring + 200 older).
      const pages = mock.eventsCalls.filter(q => /before=/.test(q));
      expect(pages.length).toBeGreaterThanOrEqual(1);
      await ctx.close();
    } finally {
      mock.server.close();
    }
  });

  test('WS-down session switch during a slow tail poll still renders the paged first page', async ({ browser }) => {
    const mock = await startMockServer({
      eventsByKey: { [KEY_B]: conversation(150, 'b') },
      eventsTailDelayMs: 2500,
    });
    try {
      const ctx = await browser.newContext({ ...desktop });
      const page = await ctx.newPage();
      await openSession(page, mock, KEY_A);

      // Poll ticks every 1 s; the first tail poll (after=…) fires at ~1 s and
      // is held 2.5 s by the mock. Switch to B while it is in flight.
      await page.waitForTimeout(1500);
      await page.click(`.session-card[data-key="${KEY_B}"]`);

      // The paged full fetch (limit=100 + X-Events-Has-More=1) must run and
      // mount "load earlier"; the swallowed-full path never mounts it because
      // the next tick's bare `after=0` request appends the whole ring instead.
      await expect(page.locator('#earlier-events-btn')).toBeVisible({ timeout: 6000 });
      const rendered = await page.locator('#events-scroll .event').count();
      expect(rendered).toBeLessThanOrEqual(100);
      expect(rendered).toBeGreaterThan(0);
      const fullForB = mock.eventsCalls.filter(q => q.includes(encodeURIComponent(KEY_B)) && /limit=100/.test(q));
      expect(fullForB.length).toBeGreaterThanOrEqual(1);
      // And the stale tail from A must not have grafted into B's pane.
      expect(await page.locator('#events-scroll .event.user', { hasText: 'hello world' }).count()).toBe(0);
      await ctx.close();
    } finally {
      mock.server.close();
    }
  });

  test('AskUserQuestion card locks when a user event arrives via poll append / WS push / poll full render', async ({ browser }) => {
    const mock = await startMockServer();
    try {
      const ctx = await browser.newContext({ ...desktop });
      const page = await ctx.newPage();
      await openSession(page, mock, KEY_A);

      const result = await page.evaluate(() => {
        const w = /** @type {any} */ (window);
        // eslint-disable-next-line no-eval
        const sk = eval('typeof selectedKey !== "undefined" ? selectedKey : null');
        const sn = eval('typeof selectedNode !== "undefined" ? selectedNode : "local"');
        const ws = eval('typeof wsm !== "undefined" ? wsm : null');
        const answered = eval('typeof _askAnswered !== "undefined" ? _askAnswered : null');
        if (!sk || !ws || !answered) return { err: 'globals missing' };
        const el = document.getElementById('events-scroll');
        const times = [...el.querySelectorAll('.event[data-time]')].map(n => Number(n.getAttribute('data-time')));
        let T = Math.max(...times, Date.now()) + 60000;
        const ask = (tuid) => ({
          type: 'ask_question', time: (T += 1000), uuid: 'ask-' + tuid,
          ask_question: { tool_use_id: tuid, items: [
            { header: 'Color', question: 'Pick one', options: [{ label: 'Red' }, { label: 'Blue' }] },
          ] },
        });
        const user = (tag) => ({ type: 'user', detail: 'answered elsewhere ' + tag, time: (T += 1000), uuid: 'usr-' + tag });
        const card = (tuid) => el.querySelector('.event.ask_question[data-tool-use-id="' + tuid + '"]');
        const state = (tuid) => {
          const c = card(tuid);
          if (!c) return { missing: true };
          const btns = [...c.querySelectorAll('button')];
          return {
            buttons: btns.length,
            disabled: btns.every(b => b.disabled),
            status: !!c.querySelector('.ask-status'),
            answered: answered.has(tuid),
          };
        };

        // (a) poll path: ask card lands, operator picks an option (submit
        //     unlocks), then a user event from another surface arrives.
        w.appendEvents([ask('tu-poll')]);
        const beforePoll = state('tu-poll');
        card('tu-poll').querySelector('.ask-opt').click();
        const submitEnabled = !card('tu-poll').querySelector('.ask-submit').disabled;
        w.appendEvents([user('poll')]);
        const afterPoll = state('tu-poll');

        // (b) WS push path.
        ws.onEvent({ key: sk, node: sn, event: ask('tu-ws') });
        const beforeWs = state('tu-ws');
        ws.onEvent({ key: sk, node: sn, event: user('ws') });
        const afterWs = state('tu-ws');

        // (c) poll full render of an ask→user history must render locked.
        w.renderEvents([ask('tu-full'), user('full')], false);
        const full = state('tu-full');

        return { beforePoll, submitEnabled, afterPoll, beforeWs, afterWs, full };
      });

      expect(result.err).toBeUndefined();
      // Fresh cards are actionable (options enabled; submit gated on selection).
      expect(result.beforePoll.buttons).toBe(3);
      expect(result.beforePoll.disabled).toBe(false);
      expect(result.beforePoll.answered).toBe(false);
      expect(result.submitEnabled).toBe(true);
      // A later user event locks them on every path.
      for (const s of [result.afterPoll, result.afterWs, result.full]) {
        expect(s.missing).toBeUndefined();
        expect(s.disabled).toBe(true);
        expect(s.status).toBe(true);
        expect(s.answered).toBe(true);
      }
      expect(result.beforeWs.disabled).toBe(false);
      await ctx.close();
    } finally {
      mock.server.close();
    }
  });
});
