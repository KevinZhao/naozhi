// @ts-check
// Regressions for the #2430 P3 items on the session events panel and the
// #2432 WS protocol reconciliation:
//
//  4. "Load earlier" must not leave two stacked time dividers at the
//     pagination seam. The first page renders its leading divider for
//     prevTime=0; prepending older bubbles that are seconds away used to keep
//     that divider under the freshly prepended page.
//  5. With the socket gone after a WS send, the poll (appendEvents) delivers
//     the real `user` event. It must replace the optimistic bubble (and dedup
//     a uuid replay) like onEvent/onHistory do — the message rendered twice.
//  6. A busy/error send_ack must roll back the bubble of THAT send (the send
//     frame `id` is echoed on the ack), not the first .optimistic-msg on
//     screen.
//  B. `unsubscribed` is a documented no-op; the PurgeNodeSubscriptions frame
//     error{node, "node disconnected"} deselects the selected session when it
//     lived on the dead node, keeps pending sessions and the draft intact.
const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };
const KEY_A = 'dashboard:direct:2026-01-01-120000-1:myproject';
const KEY_N = 'dashboard:direct:2026-01-01-120009-9:remoteproj';

// conversation(n) builds n alternating user/text events 1 s apart — all
// within one divider gap (5 min) for n ≤ 250.
function conversation(n, tag) {
  const base = Date.now() - (n + 10) * 1000;
  const out = [];
  for (let i = 0; i < n; i++) {
    out.push({
      type: i % 2 === 0 ? 'user' : 'text',
      detail: (i % 2 === 0 ? 'Q' : 'A') + '-' + tag + '-' + i,
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

test.describe('Events panel #2430 P3 / #2432 protocol', () => {
  test('load earlier does not stack a second divider at the pagination seam', async ({ browser }) => {
    // 150 events 1 s apart → first page = last 100, "load earlier" prepends
    // the other 50. Everything is inside one 5-min gap, so exactly one
    // divider (before the oldest bubble) is legitimate.
    const mock = await startMockServer({ eventsByKey: { [KEY_A]: conversation(150, 'a') } });
    try {
      const ctx = await browser.newContext({ ...desktop });
      const page = await ctx.newPage();
      await openSession(page, mock, KEY_A);
      await expect(page.locator('#earlier-events-btn')).toBeVisible();
      expect(await page.locator('#events-scroll .event-time-divider').count()).toBe(1);

      await page.click('#earlier-events-btn');
      await expect(page.locator('#events-scroll .event', { hasText: 'Q-a-0' })).toHaveCount(1);
      expect(await page.locator('#events-scroll .event').count()).toBe(150);
      // Old code: 2 (the prepended page's leading divider + the stale one).
      expect(await page.locator('#events-scroll .event-time-divider').count()).toBe(1);
      // The surviving divider precedes every bubble, and the prepended page
      // is chronological (the old loop inserted before a moving firstChild
      // and rendered it newest-first).
      const head = await page.evaluate(() => {
        const el = document.getElementById('events-scroll');
        return [...el.children]
          .filter(c => c.classList.contains('event-time-divider') || c.classList.contains('event'))
          .slice(0, 4)
          .map(c => c.classList.contains('event-time-divider') ? 'divider' : c.textContent.replace(/^>_/, '').trim().slice(0, 6));
      });
      expect(head).toEqual(['divider', 'Q-a-0', 'A-a-1', 'Q-a-2']);
      const seam = await page.evaluate(() => {
        const bubbles = [...document.querySelectorAll('#events-scroll .event')].map(c => c.textContent);
        const i = bubbles.findIndex(t => t.includes('A-a-49'));
        return [bubbles[i - 1], bubbles[i + 1]].map(t => (t || '').replace(/^>_/, '').trim().slice(0, 6));
      });
      expect(seam).toEqual(['Q-a-48', 'Q-a-50']);
      await ctx.close();
    } finally {
      mock.server.close();
    }
  });

  test('poll path replaces the optimistic bubble with the real user event and dedups its replay', async ({ browser }) => {
    const mock = await startMockServer();
    try {
      const ctx = await browser.newContext({ ...desktop });
      const page = await ctx.newPage();
      await openSession(page, mock, KEY_A);

      const result = await page.evaluate(() => {
        const w = /** @type {any} */ (window);
        const el = document.getElementById('events-scroll');
        const times = [...el.querySelectorAll('.event[data-time]')].map(n => Number(n.getAttribute('data-time')));
        const T = Math.max(...times, Date.now()) + 1000;
        const count = (txt) => [...el.querySelectorAll('.event.user')].filter(n => n.textContent.includes(txt)).length;

        // WS was up at send time: optimistic bubble rendered with a send id …
        w.renderOptimisticUserMsg('sent over ws then dropped', 'r1');
        const optBefore = el.querySelectorAll('.optimistic-msg').length;
        // … the socket dropped, an unrelated assistant chunk arrives via poll
        // (must NOT eat the bubble) …
        w.appendEvents([{ type: 'text', detail: 'partial answer', time: T, uuid: 'txt-1' }]);
        const optAfterText = el.querySelectorAll('.optimistic-msg').length;
        // … then the poll echoes the real user event.
        const real = { type: 'user', detail: 'sent over ws then dropped', time: T + 1, uuid: 'usr-real-1' };
        w.appendEvents([real]);
        const optAfterUser = el.querySelectorAll('.optimistic-msg').length;
        const userBubbles = count('sent over ws then dropped');
        // A uuid replay of the same user event (same ms re-admitted or a
        // later page overlap) must not paint a second copy.
        w.appendEvents([real]);
        const afterReplay = count('sent over ws then dropped');
        // A genuinely new user event with a fresh uuid must still render.
        w.appendEvents([{ type: 'user', detail: 'teammate says hi', time: T + 2, uuid: 'usr-real-2' }]);
        const teammate = count('teammate says hi');
        return { optBefore, optAfterText, optAfterUser, userBubbles, afterReplay, teammate };
      });

      expect(result.optBefore).toBe(1);
      expect(result.optAfterText).toBe(1);
      expect(result.optAfterUser).toBe(0);
      expect(result.userBubbles).toBe(1);
      expect(result.afterReplay).toBe(1);
      expect(result.teammate).toBe(1);
      await ctx.close();
    } finally {
      mock.server.close();
    }
  });

  test('busy/error send_ack rolls back only the bubble of that send id', async ({ browser }) => {
    const mock = await startMockServer();
    try {
      const ctx = await browser.newContext({ ...desktop });
      const page = await ctx.newPage();
      await openSession(page, mock, KEY_A);

      const result = await page.evaluate(() => {
        const w = /** @type {any} */ (window);
        // eslint-disable-next-line no-eval
        const ws = eval('typeof wsm !== "undefined" ? wsm : null');
        if (!ws) return { err: 'wsm missing' };
        const el = document.getElementById('events-scroll');
        const ids = () => [...el.querySelectorAll('.optimistic-msg')].map(n => n.getAttribute('data-send-id'));
        w.renderOptimisticUserMsg('first send', 'r1');
        w.renderOptimisticUserMsg('second send', 'r2');
        w.renderOptimisticUserMsg('third send', 'r3');
        const before = ids();
        ws.onSendAck({ type: 'send_ack', id: 'r2', status: 'busy', key: 'x' });
        const afterBusy = ids();
        ws.onSendAck({ type: 'send_ack', id: 'r3', status: 'error', error: 'boom', key: 'x' });
        const afterError = ids();
        // Legacy id-less ack keeps the first-bubble fallback.
        ws.onSendAck({ type: 'send_ack', status: 'error', error: 'boom', key: 'x' });
        const afterLegacy = ids();
        return { before, afterBusy, afterError, afterLegacy };
      });

      expect(result.err).toBeUndefined();
      expect(result.before).toEqual(['r1', 'r2', 'r3']);
      // Old code removed the FIRST bubble (r1) on the r2 ack.
      expect(result.afterBusy).toEqual(['r1', 'r3']);
      expect(result.afterError).toEqual(['r1']);
      expect(result.afterLegacy).toEqual([]);
      await ctx.close();
    } finally {
      mock.server.close();
    }
  });

  test('unsubscribed is a silent no-op; node disconnect deselects the stale session but not a pending one', async ({ browser }) => {
    const sessions = defaultSessions();
    sessions.nodes.n1 = { display_name: 'Node 1', status: 'ok' };
    sessions.sessions.push({
      key: KEY_N,
      state: 'ready',
      platform: 'dashboard',
      agent: 'general',
      cli_name: 'claude',
      cli_version: '1.0.30',
      workspace: '/remote/remoteproj',
      last_active: Date.now() - 1000,
      last_prompt: 'remote hello',
      node: 'n1',
      project: 'remoteproj',
    });
    const mock = await startMockServer({ sessions });
    try {
      const ctx = await browser.newContext({ ...desktop });
      const page = await ctx.newPage();
      // Uncaught exceptions only — the mock rejects /ws and a few optional
      // endpoints with 404, which the console reports as resource errors.
      const errors = [];
      page.on('pageerror', e => errors.push(String(e)));
      await openSession(page, mock, KEY_N);

      const result = await page.evaluate(() => {
        // eslint-disable-next-line no-eval
        const ws = eval('typeof wsm !== "undefined" ? wsm : null');
        const g = (expr) => eval(expr);
        if (!ws) return { err: 'wsm missing' };
        // B1: unsubscribed frame — no throw, no state change.
        const subBefore = { key: ws.subscribedKey, node: ws.subscribedNode };
        ws.onMessage({ type: 'unsubscribed', key: 'whatever' });
        const subAfter = { key: ws.subscribedKey, node: ws.subscribedNode };

        // Draft the operator was typing must survive the deselect.
        const inp = document.getElementById('msg-input');
        inp.innerText = 'half-typed draft';

        // A pending (never-sent) session on the same node must be left alone.
        g('sessionWorkspaces')['dashboard:direct:pending-on-n1:x'] = '/remote/x';
        g('sessionNodes')['dashboard:direct:pending-on-n1:x'] = 'n1';

        // Disconnect of an unrelated node: nothing happens to the selection.
        ws.onMessage({ type: 'error', node: 'n2', error: 'node disconnected' });
        const afterOther = { key: g('selectedKey'), node: g('selectedNode') };

        // Disconnect of the node hosting the selected session.
        ws.onMessage({ type: 'error', node: 'n1', error: 'node disconnected' });
        const afterN1 = {
          key: g('selectedKey'),
          node: g('selectedNode'),
          emptyState: !!document.querySelector('#main .empty-state.empty-quick'),
          draft: g('sessionDrafts')['dashboard:direct:2026-01-01-120009-9:remoteproj'],
          pendingKept: g('sessionWorkspaces')['dashboard:direct:pending-on-n1:x'] === '/remote/x',
          toast: (document.getElementById('toast') || {}).textContent || '',
        };
        return { subBefore, subAfter, afterOther, afterN1 };
      });

      expect(result.err).toBeUndefined();
      expect(result.subAfter).toEqual(result.subBefore);
      expect(result.afterOther.key).toBe(KEY_N);
      expect(result.afterOther.node).toBe('n1');
      expect(result.afterN1.key).toBeNull();
      expect(result.afterN1.node).toBe('local');
      expect(result.afterN1.emptyState).toBe(true);
      expect(result.afterN1.draft).toBe('half-typed draft');
      expect(result.afterN1.pendingKept).toBe(true);
      expect(result.afterN1.toast).toContain('已断开');
      expect(errors).toEqual([]);
      await ctx.close();
    } finally {
      mock.server.close();
    }
  });

  test('node disconnect leaves a selected pending session on that node selected', async ({ browser }) => {
    const sessions = defaultSessions();
    sessions.nodes.n1 = { display_name: 'Node 1', status: 'ok' };
    const mock = await startMockServer({ sessions });
    try {
      const ctx = await browser.newContext({ ...desktop });
      const page = await ctx.newPage();
      await openSession(page, mock, KEY_A);
      const result = await page.evaluate(() => {
        // eslint-disable-next-line no-eval
        const g = (expr) => eval(expr);
        const ws = g('wsm');
        // Simulate the operator sitting on a pending (never-sent) session
        // targeted at n1: it is only a draft target, so the purge must not
        // deselect or clear it.
        g('sessionWorkspaces')[g('selectedKey')] = '/remote/x';
        g('sessionNodes')[g('selectedKey')] = 'n1';
        g('selectedNode = "n1"');
        const before = g('selectedKey');
        ws.onMessage({ type: 'error', node: 'n1', error: 'node disconnected' });
        return { before, after: g('selectedKey'), pendingKept: g('sessionWorkspaces')[before] === '/remote/x' };
      });
      expect(result.after).toBe(result.before);
      expect(result.pendingKept).toBe(true);
      await ctx.close();
    } finally {
      mock.server.close();
    }
  });
});
