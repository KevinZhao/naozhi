// E2E: the 会话 sidebar's "等你 N" bar (side_wait.js, planner RFC §15.3 ①,
// §15.7 S1) — the questions, decisions and errors, the rail badge's count,
// their cards unfolded in place without an inner scroll and closed or
// answered there, no 任务, and not rendered with none — and the deep links of
// §15.3 ④ (notify.js): #planner&item=<id> and a notification without a
// session open the Planner on the item, the brief's #planner the page too;
// #session=<key>&item=<id> opens the session and scrolls to the item's note,
// or the Planner when the session is not listed.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const SESSION = 'dashboard:direct:2026-01-01-120000-1:myproject';
const PLANNER = 'planner:main';
const now = Date.now();
const iso = (ms) => new Date(ms).toISOString();

function inbox() {
  return {
    open: [
      { id: 'in_err', kind: 'error', level: 'timely', unit: 'cron:nightly', title: 'iso nightly 失败', why: '定时任务运行失败', created: iso(now - 3 * 3600000), state: 'open', source: 'health:cron:nightly' },
      { id: 'in_q', kind: 'question', level: 'timely', unit: SESSION, title: 'iso 会话在等你回复', why: '有提问等你回答超过 10 分钟', created: iso(now - 12 * 60000), state: 'open', source: 'waiting:' + SESSION, auto_close: true },
      { id: 'in_d', kind: 'decision', level: 'timely', title: 'iso 周报用哪个模板？', why: 'Planner 问你', created: iso(now - 5 * 60000), state: 'open', source: 'planner:d1', decision: { handler: 'ask', actions: ['模板 A', '模板 B'] } },
    ],
    closed: [],
    counts: { open: 3, badge: 3 },
  };
}

const TASK = 'task:ab12cd34ef56';
const tasks = () => ({ tasks: [
  { id: 'pr_1', kind: 'promise', title: 'iso 明早提醒你看压测结果', due: iso(now + 3600000), state: 'remind', at: iso(now - 60000), created: iso(now - 60000) },
  { id: TASK, kind: 'session', title: 'iso 写 hello', state: 'doing', at: iso(now - 600000), created: iso(now - 600000) },
] });
const planner = { enabled: true, stopped: false, key: PLANNER, alive: true };

for (const [name, viewport] of [['desktop', { width: 1280, height: 800 }], ['mobile', { width: 390, height: 844 }]]) {
  test.describe(`sidebar 通知 bar (${name})`, () => {
    test('a long bar leaves the session list its rows', async ({ browser }) => {
      const ctx = await browser.newContext({ viewport });
      const page = await ctx.newPage();
      const open = Array.from({ length: 12 }, (_, i) => ({ id: 'in_' + i, kind: 'question', level: 'timely', unit: SESSION, title: 'iso 提问 ' + i, why: '有提问等你回答', created: iso(now - i * 60000), state: 'open', source: 'waiting:' + i }));
      const mock = await startMockServer({ ws: true, inbox: { open, closed: [], counts: { open: 12, badge: 12 } }, tasks: { tasks: [] }, plannerEvents: [], planner });
      try {
        await page.goto(mock.url + '/dashboard');
        await waitForWs(page, 'CONNECTED');
        await page.click('#sw-toggle');
        await expect(page.locator('#sw-wait .pl-card')).toHaveCount(12);
        expect((await page.locator('#session-list').boundingBox()).height).toBeGreaterThan(viewport.height * 0.3);
        await expect(page.locator('#session-list .session-card').first()).toBeInViewport();
        // The bar scrolls as a whole to its last card.
        await page.locator('#sw-wait .pl-card[data-id="in_11"]').scrollIntoViewIfNeeded();
        await expect(page.locator('#sw-wait .pl-card[data-id="in_11"]')).toBeInViewport();
        await page.screenshot({ path: test.info().outputPath(`side-wait-long-${name}.png`) });
      } finally {
        await ctx.close();
        mock.server.close();
      }
    });

    test('counts what waits on you, unfolds in place, closes and answers there', async ({ browser }) => {
      const ctx = await browser.newContext({ viewport });
      const page = await ctx.newPage();
      const box = inbox();
      // A notice is the Planner page's, not the bar's.
      box.open.push({ id: 'in_n', kind: 'notice', level: 'timely', unit: PLANNER, title: 'iso 日程冲突', why: '两个会议重叠', created: iso(now - 60000), state: 'open', source: 'planner:n1' });
      box.counts = { open: 4, badge: 3 };
      const mock = await startMockServer({ ws: true, inbox: box, tasks: tasks(), plannerEvents: [], planner });
      try {
        await page.goto(mock.url + '/dashboard');
        await waitForWs(page, 'CONNECTED');
        const bar = page.locator('#side-wait');
        // One count: the questions, decisions and errors, as the rail badge.
        await expect(page.locator('#sw-toggle')).toHaveText('等你 3');
        await expect(page.locator('#abnav-planner-count')).toHaveText('3');
        await expect(bar).not.toContainText('任务');
        await expect(page.locator('#sw-wait')).toBeHidden();
        // The sidebar's own groups are untouched below it.
        await expect(page.locator('#session-list .session-card').first()).toBeVisible();
        await page.screenshot({ path: test.info().outputPath(`side-wait-${name}.png`) });

        await page.click('#sw-toggle');
        await expect(page.locator('#sw-toggle')).toHaveAttribute('aria-expanded', 'true');
        const cards = page.locator('#sw-wait .pl-card');
        await expect(cards).toHaveCount(3);
        await expect(cards.locator('.pl-kind')).toHaveText(['待你决定', '提问', '错误']);
        await expect(page.locator('#side-wait .tk-card')).toHaveCount(0);
        // Unfolded in place, not scrolled inside.
        expect(await page.locator('#sw-wait').evaluate((el) => getComputedStyle(el).overflowY)).toBe('visible');
        await expect(page.locator('body')).not.toHaveClass(/nz-view-planner/);
        await page.mouse.move(viewport.width - 1, viewport.height - 1);
        await page.waitForTimeout(250); // the chevron's turn
        await page.screenshot({ path: test.info().outputPath(`side-wait-open-${name}.png`) });

        await page.click('#sw-wait .pl-card[data-id="in_err"] .pl-close');
        await expect(cards).toHaveCount(2);
        expect(mock.inboxCalls[0]).toEqual({ path: '/api/inbox/close', method: 'POST', body: { id: 'in_err' } });
        await expect(page.locator('#sw-n')).toHaveText('2');

        await page.click('#sw-wait .pl-card[data-id="in_d"] [data-act="模板 A"]');
        await expect(cards).toHaveCount(1);
        expect(mock.inboxCalls[1]).toMatchObject({ path: '/api/inbox/decide', body: { id: 'in_d', action: '模板 A' } });
        await expect(page.locator('#sw-n')).toHaveText('1');
        await expect(page.locator('#abnav-planner-count')).toHaveText('1');

        // Another tab answered the last one: with only a notice left the bar
        // is not rendered.
        box.open = box.open.filter((it) => it.kind === 'notice');
        box.counts = { open: 1, badge: 0 };
        mock.wsConnections[mock.wsConnections.length - 1].send({ type: 'inbox_count', open: 1, badge: 0 });
        await expect(bar).toBeHidden();
        await expect(page.locator('#abnav-planner-count')).toBeHidden();
        await page.screenshot({ path: test.info().outputPath(`side-wait-zero-${name}.png`) });
        // Something new comes in: the bar is back, folded. The list says so
        // too (the idle Home's summary refetches it on a changed count).
        box.open.push({ id: 'in_q2', kind: 'question', level: 'timely', unit: SESSION, title: 'iso 又一个提问', created: iso(Date.now()), state: 'open', source: 'waiting:' + SESSION });
        box.counts = { open: 2, badge: 1 };
        mock.wsConnections[mock.wsConnections.length - 1].send({ type: 'inbox_count', open: 2, badge: 1 });
        await expect(page.locator('#sw-toggle')).toHaveText('等你 1');
        await expect(page.locator('#sw-toggle')).toHaveAttribute('aria-expanded', 'false');
      } finally {
        await ctx.close();
        mock.server.close();
      }
    });

    test('deep links and a notification click go to the item', async ({ browser }) => {
      const ctx = await browser.newContext({ viewport });
      const page = await ctx.newPage();
      const mock = await startMockServer({ ws: true, inbox: inbox(), tasks: tasks(), plannerEvents: [], planner });
      try {
        // #planner&item=: the Planner page, the item's card brought into view.
        await page.goto(mock.url + '/dashboard#planner&item=in_d');
        await expect(page.locator('body')).toHaveClass(/nz-view-planner/);
        await expect(page.locator('#pl-wait .pl-card[data-id="in_d"]')).toHaveClass(/inbox-lit/);
        await expect(page.locator('#pl-wait .pl-card[data-id="in_d"]')).toBeInViewport();
        expect(await page.evaluate(() => location.hash)).toBe('');
        await page.screenshot({ path: test.info().outputPath(`deep-link-planner-${name}.png`) });

        // #session=&item=: the session, scrolled to the item's note. The key's
        // ':' left as is, as notify.linkFor writes it.
        await page.evaluate((key) => { location.hash = 'session=' + key + '&item=in_q'; }, SESSION);
        await page.waitForFunction((key) => window.nz.test.selectedKey === key, SESSION);
        await page.evaluate(() => {
          const note = document.createElement('div');
          note.dataset.inboxItem = 'in_q';
          note.textContent = 'iso 旁注';
          document.getElementById('main').appendChild(note);
        });
        await expect(page.locator('#main [data-inbox-item="in_q"]')).toHaveClass(/inbox-lit/);

        // A session that is not listed: the Planner on the item.
        await page.evaluate(() => { location.hash = 'session=cron%3Anightly&item=in_err'; });
        await expect(page.locator('body')).toHaveClass(/nz-view-planner/);
        await expect(page.locator('#pl-wait .pl-card[data-id="in_err"]')).toHaveClass(/inbox-lit/);

        // A notification about no session, clicked from the chat view.
        await page.click('#abnav-chat');
        await expect(page.locator('body')).not.toHaveClass(/nz-view-planner/);
        await page.evaluate(async () => {
          window.Notification = class { constructor() { window.__note = this; } close() {} };
          window.Notification.permission = 'granted';
          Object.defineProperty(document, 'hidden', { configurable: true, get: () => true });
          const { onNotify } = await import('/static/notify.js');
          onNotify({ id: 'in_d', kind: 'decision', title: 'iso 等你决定', body: '' });
          window.__note.onclick();
        });
        await expect(page.locator('body')).toHaveClass(/nz-view-planner/);
        await expect(page.locator('#pl-wait .pl-card[data-id="in_d"]')).toHaveClass(/inbox-lit/);

        // The morning brief's #planner, from the chat view: the page too,
        // never the drawer (D91).
        await page.click('#abnav-chat');
        await page.evaluate(() => { location.hash = 'planner'; });
        await expect(page.locator('body')).toHaveClass(/nz-view-planner/);
        await expect(page.locator('#planner-drawer')).toBeHidden();
        await expect(page.locator('#pl-stream')).toBeVisible();
      } finally {
        await ctx.close();
        mock.server.close();
      }
    });
  });
}
