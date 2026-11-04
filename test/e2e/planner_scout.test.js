// E2E: the scout (planner RFC §15.6): a turn a scout run woke the Planner
// with shows as a 来自巡检 line rather than your bubble, a pre-meeting
// brief's turn as a 来自会前简报 line, the refused
// dispatch of that turn as the gateway's line, and the scout as one 巡检 row
// in 全会话健康. Desktop and phone width.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const PLANNER = 'planner:main';
const now = Date.now();
const iso = (ms) => new Date(ms).toISOString();

const SCOUT_BLOCK = '<naozhi-scout findings="2" run="20261105T100000-ab12">\n巡检发现了需要看一眼的事。\n' +
  '{"source":"slack","kind":"reply_needed","id":"D1/1.1","title":"iso 问进度"}\n' +
  '{"source":"asana","kind":"deadline","id":"t1","title":"iso 今天到期"}\n</naozhi-scout>\n\n' +
  '<naozhi-snapshot at="2026-11-05 10:00 CST">\n等你（0）\n</naozhi-snapshot>';

const MEETING_BLOCK = '<naozhi-scout kind="meeting" start="10:00" run="20261105T094500-cd34">\n一场会议 15 分钟内开始。\n' +
  '{"meeting":{"id":"e1","title":"iso 预算评审"}}\n{"brief":{"why":"iso 定预算"}}\n</naozhi-scout>\n\n' +
  '<naozhi-snapshot at="2026-11-05 09:45 CST">\n等你（0）\n</naozhi-snapshot>';

function plannerEvents() {
  return [
    { type: 'user', detail: '<naozhi-snapshot at="2026-11-05 09:00 CST">\n</naozhi-snapshot>\n\niso 今天有什么安排？', time: now - 120000, uuid: 'sc-u1' },
    { type: 'text', detail: '上午没有会。', time: now - 110000, uuid: 'sc-t1' },
    { type: 'user', detail: SCOUT_BLOCK, summary: SCOUT_BLOCK.slice(0, 120), time: now - 60000, uuid: 'sc-u2' },
    { type: 'text', detail: '有人在 Slack 等你回复进度，还有一个 Asana 任务今天到期。', time: now - 55000, uuid: 'sc-t2' },
    { type: 'system', summary: '巡检触发的回合不能派活：「iso 回复进度」没有派出', time: now - 54000, uuid: 'sc-s1' },
    { type: 'user', detail: MEETING_BLOCK, summary: MEETING_BLOCK.slice(0, 120), time: now - 30000, uuid: 'sc-u3' },
    { type: 'text', detail: '会前简报：iso 预算评审 10:00 开始。', time: now - 25000, uuid: 'sc-t3' },
  ];
}

const HEALTH = {
  at: iso(now - 20000),
  unowned: 0,
  units: [
    { id: 'sys:scout', kind: 'sysession', source: 'scout', coverage: 'gateway', state: 'idle', since: iso(now - 600000), last_active: iso(now - 600000) },
  ],
};

for (const vp of [{ name: 'desktop', width: 1280, height: 800 }, { name: 'phone', width: 390, height: 844 }]) {
  test(`${vp.name}: a scout turn is marked 来自巡检 and the scout has its health row`, async ({ browser }) => {
    const ctx = await browser.newContext({ viewport: { width: vp.width, height: vp.height } });
    const page = await ctx.newPage();
    const mock = await startMockServer({
      ws: true, plannerEvents: plannerEvents(), systemHealth: HEALTH,
      planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
    });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      await page.evaluate(() => document.getElementById('abnav-planner').click());
      await expect(page.locator('body')).toHaveClass(/nz-view-planner/);
      const stream = page.locator('#pl-stream');
      await expect(stream).toContainText('来自巡检：2 条新发现');
      await expect(stream).toContainText('巡检触发的回合不能派活');
      await expect(stream).toContainText('来自会前简报：10:00 开始的会议');
      // Only what you typed is a bubble; the scout's data is not.
      await expect(stream.locator('.event.user')).toHaveCount(1);
      await expect(stream).not.toContainText('naozhi-scout');
      await expect(stream).not.toContainText('iso 问进度');
      await expect(stream).not.toContainText('iso 定预算');
      await page.screenshot({ path: test.info().outputPath(`planner-scout-${vp.name}.png`) });

      await page.evaluate(() => document.getElementById('abnav-system').click());
      const row = page.locator('.sys-health tr[data-unit="sys:scout"]');
      await expect(row.locator('.hl-name')).toHaveText('巡检');
      await expect(row.locator('.hl-tag')).toHaveText('系统');
    } finally {
      await ctx.close();
      await new Promise((r) => mock.server.close(r));
    }
  });
}
