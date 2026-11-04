// E2E: the 会话 sidebar's "等你 N · 答应你的 N" bar (side_wait.js, planner
// RFC §15.3 ①) — the Planner page's counts, its cards unfolded in place and
// closed there, one thin line with nothing open — and the deep links of
// §15.3 ④ (notify.js): #planner&item=<id> and a notification without a
// session open the Planner on the item; #session=<key>&item=<id> opens the
// session and scrolls to the item's note, or the Planner when the session is
// not listed.
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

const promises = () => ({ open: [{ id: 'pr_1', text: 'iso 明早提醒你看压测结果', due: iso(now + 3600000), state: 'open', created: iso(now - 60000), source: 'planner' }], closed: [] });
const planner = { enabled: true, stopped: false, key: PLANNER, alive: true };

for (const [name, viewport] of [['desktop', { width: 1280, height: 800 }], ['mobile', { width: 390, height: 844 }]]) {
  test.describe(`sidebar 等你 bar (${name})`, () => {
    test('counts, unfolds in place, closes and approves there', async ({ browser }) => {
      const ctx = await browser.newContext({ viewport });
      const page = await ctx.newPage();
      const box = inbox();
      const mock = await startMockServer({ ws: true, inbox: box, promises: promises(), plannerEvents: [], planner });
      try {
        await page.goto(mock.url + '/dashboard');
        await waitForWs(page, 'CONNECTED');
        const bar = page.locator('#side-wait');
        // The Planner page's count (every open item) and its open promises.
        await expect(page.locator('#sw-wait-n')).toHaveText('3');
        await expect(page.locator('#sw-promise-n')).toHaveText('1');
        await expect(bar).not.toHaveClass(/sw-zero/);
        await expect(page.locator('#sw-body')).toBeHidden();
        // The sidebar's own groups are untouched below it.
        await expect(page.locator('#session-list .session-card').first()).toBeVisible();
        await page.screenshot({ path: test.info().outputPath(`side-wait-${name}.png`) });

        await page.click('#sw-toggle');
        await expect(page.locator('#sw-toggle')).toHaveAttribute('aria-expanded', 'true');
        const cards = page.locator('#sw-wait .pl-card');
        await expect(cards).toHaveCount(3);
        await expect(cards.nth(0).locator('.pl-kind')).toHaveText('决定');
        await expect(page.locator('#sw-promise .pm-card')).toHaveText(/iso 明早提醒你看压测结果/);
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
        await expect(page.locator('#sw-wait')).toContainText('没有等你的事');
        await page.locator('#sw-promise .pm-close').click();
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
      const mock = await startMockServer({ ws: true, inbox: inbox(), promises: promises(), plannerEvents: [], planner });
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
      } finally {
        await ctx.close();
        mock.server.close();
      }
    });
  });
}
