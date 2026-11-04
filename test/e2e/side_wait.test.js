// E2E: the 会话 sidebar's "通知 N · 任务 N" bar (side_wait.js, planner RFC
// §15.3 ①, §15.4) — the Planner page's counts, its cards unfolded in place
// and closed there (a task marked done there, a dispatched session opening
// in the chat view), one thin line with nothing open — and the deep links of
// §15.3 ④ (notify.js): #planner&item=<id> and a notification without a
// session open the Planner on the item, the brief's #planner the drawer;
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
      { id: 'in_d', kind: 'decision', level: 'timely', title: 'iso 派出一个会话整理周报？', why: 'Planner 提议', created: iso(now - 5 * 60000), state: 'open', source: 'planner:d1', decision: { handler: 'dispatch', actions: ['approve', 'reject'] } },
    ],
    closed: [],
    counts: { open: 3, badge: 2 },
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
    test('counts, unfolds in place, closes and approves there', async ({ browser }) => {
      const ctx = await browser.newContext({ viewport });
      const page = await ctx.newPage();
      const box = inbox();
      const mock = await startMockServer({ ws: true, inbox: box, tasks: tasks(), plannerEvents: [], planner });
      try {
        await page.goto(mock.url + '/dashboard');
        await waitForWs(page, 'CONNECTED');
        const bar = page.locator('#side-wait');
        // The Planner page's count (every open item) and its tasks not done.
        await expect(page.locator('#sw-toggle')).toHaveText(/通知 3 · 任务 2/);
        await expect(page.locator('#sw-wait-n')).toHaveText('3');
        await expect(page.locator('#sw-task-n')).toHaveText('2');
        await expect(bar).not.toHaveClass(/sw-zero/);
        await expect(page.locator('#sw-body')).toBeHidden();
        // The sidebar's own groups are untouched below it.
        await expect(page.locator('#session-list .session-card').first()).toBeVisible();
        await page.screenshot({ path: test.info().outputPath(`side-wait-${name}.png`) });

        await page.click('#sw-toggle');
        await expect(page.locator('#sw-toggle')).toHaveAttribute('aria-expanded', 'true');
        const cards = page.locator('#sw-wait .pl-card');
        await expect(cards).toHaveCount(3);
        await expect(cards.nth(0).locator('.pl-kind')).toHaveText('待你决定');
        await expect(page.locator('#sw-task .tk-card')).toHaveText([/iso 写 hello/, /iso 明早提醒你看压测结果/]);
        await expect(page.locator('body')).not.toHaveClass(/nz-view-planner/);
        await page.mouse.move(viewport.width - 1, viewport.height - 1);
        await page.waitForTimeout(250); // the chevron's turn
        await page.screenshot({ path: test.info().outputPath(`side-wait-open-${name}.png`) });

        await page.click('#sw-wait .pl-card[data-id="in_err"] .pl-close');
        await expect(cards).toHaveCount(2);
        expect(mock.inboxCalls[0]).toEqual({ path: '/api/inbox/close', method: 'POST', body: { id: 'in_err' } });

        await page.click('#sw-wait .pl-card[data-id="in_d"] [data-act="approve"]');
        await expect(cards).toHaveCount(1);
        expect(mock.inboxCalls[1]).toMatchObject({ path: '/api/inbox/decide', body: { id: 'in_d', action: 'approve' } });
        await expect(page.locator('#sw-wait-n')).toHaveText('1');
        await expect(page.locator('#abnav-planner-count')).toHaveText('1');

        // Another tab answered the last one: the count frame refetches the list.
        box.open = [];
        box.counts = { open: 0, badge: 0 };
        mock.wsConnections[mock.wsConnections.length - 1].send({ type: 'inbox_count', open: 0, badge: 0 });
        await expect(page.locator('#sw-wait')).toContainText('没有通知');
        await page.locator('#sw-task .tk-card[data-id="pr_1"] .tk-menu').selectOption('done');
        await expect.poll(() => mock.tasksCalls).toEqual([{ id: 'pr_1', state: 'done' }]);
        await expect(page.locator('#sw-task-n')).toHaveText('1');
        // A dispatched session opens in the chat view, not on the Planner page.
        const open = page.locator('#sw-task .tk-card[data-id="' + TASK + '"] .tk-open');
        await expect(open).toHaveAttribute('data-action', 'session-select');
        await page.locator('#sw-task .tk-card[data-id="' + TASK + '"] .tk-menu').selectOption('done');
        await expect(bar).toHaveClass(/sw-zero/);
        await page.click('#sw-toggle');
        await expect(page.locator('#sw-body')).toBeHidden();
        await page.mouse.move(viewport.width - 1, viewport.height - 1);
        await page.waitForTimeout(250);
        await page.screenshot({ path: test.info().outputPath(`side-wait-zero-${name}.png`) });
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

        // The morning brief's #planner, from the chat view: the drawer.
        await page.click('#abnav-chat');
        await page.evaluate(() => { location.hash = 'planner'; });
        await expect(page.locator('#planner-drawer')).toBeVisible();
        await expect(page.locator('body')).not.toHaveClass(/nz-view-planner/);
      } finally {
        await ctx.close();
        mock.server.close();
      }
    });
  });
}
