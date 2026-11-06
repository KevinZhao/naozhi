// E2E: the notify action block (planner RFC §15.6 提醒你): a reply's notify
// blocks render as 提醒你 / 记入早报 cards, and a Planner notice in 通知
// links to the page it is about. Desktop and phone width.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const PLANNER = 'planner:main';
const now = Date.now();
const iso = (ms) => new Date(ms).toISOString();
const fence = (body) => '```naozhi-action\n' + JSON.stringify(body) + '\n```';
const LINK = 'https://example.com/mail/1';

function plannerEvents() {
  return [
    { type: 'user', detail: '<naozhi-snapshot at="2026-11-05 10:00 CST">\n</naozhi-snapshot>\n\niso 今天有什么要我回的？', time: now - 60000, uuid: 'nt-u1' },
    {
      type: 'text', time: now - 55000, uuid: 'nt-t1',
      detail: '有一封邮件在等你回复，推给你了；周报记进明早的早报。\n\n' +
        fence({ type: 'notify', title: 'iso 有人等你回复邮件', why: '超过 4 小时没回', level: 'timely', link: LINK, id: 'm1' }) + '\n\n' +
        fence({ type: 'notify', title: 'iso 周报已发布', why: '不急', level: 'brief', link: 'javascript:alert(1)' }),
    },
  ];
}

function inbox() {
  return {
    open: [
      { id: 'in_n', kind: 'notice', level: 'timely', unit: PLANNER, title: 'iso 有人等你回复邮件', why: '超过 4 小时没回', link: LINK, created: iso(now - 50000), state: 'open', source: 'planner:ab12' },
    ],
    closed: [],
    counts: { open: 1, badge: 0 },
  };
}

for (const vp of [{ name: 'desktop', width: 1280, height: 800 }, { name: 'phone', width: 390, height: 844 }]) {
  test(`${vp.name}: notify blocks are cards and a Planner notice is not in 等你`, async ({ browser }) => {
    const ctx = await browser.newContext({ viewport: { width: vp.width, height: vp.height } });
    const page = await ctx.newPage();
    const mock = await startMockServer({
      ws: true, plannerEvents: plannerEvents(), inbox: inbox(),
      planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
    });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      await page.evaluate(() => document.getElementById('abnav-planner').click());
      await expect(page.locator('body')).toHaveClass(/nz-view-planner/);
      const cards = page.locator('#pl-stream .pl-act-notify');
      await expect(cards).toHaveCount(2);
      await expect(page.locator('#pl-stream .pl-act-bad')).toHaveCount(0);
      await expect(cards.nth(0).locator('.pl-act-h')).toHaveText('提醒你');
      // A card is what was done: no fixed note under it (D91).
      await expect(cards.nth(0)).not.toContainText('同一件事当天只推一次');
      const open = cards.nth(0).locator('a.pl-link');
      await expect(open).toHaveAttribute('href', LINK);
      await expect(open).toHaveAttribute('target', '_blank');
      await expect(cards.nth(1).locator('.pl-act-h')).toHaveText('记入早报');
      // Only an http(s) link becomes one.
      await expect(cards.nth(1).locator('a')).toHaveCount(0);
      await cards.nth(0).scrollIntoViewIfNeeded();
      await page.screenshot({ path: test.info().outputPath(`planner-notify-${vp.name}.png`) });

      // A notice is the morning brief's (D5): the left column lists only
      // what waits on you to act, the badge's count.
      if (vp.name === 'phone') await page.click('#pl-left-toggle');
      await expect(page.locator('#pl-wait')).toHaveText('没有等你的事');
      await expect(page.locator('#pl-wait .pl-card[data-id="in_n"]')).toHaveCount(0);
    } finally {
      await ctx.close();
      await new Promise((r) => mock.server.close(r));
    }
  });
}
