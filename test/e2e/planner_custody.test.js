// E2E: the Planner's custody blocks (planner RFC §5.4, D93) render as cards
// in its conversation: turning a session's custody on or off, and a
// go-ahead with what the session was told; the session by its card, by
// name, never the key; no 看不懂的动作块.
const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const PLANNER = 'planner:main';
const TASK = 'task:0a1b2c3d4e5f';
const now = Date.now();
const VIEWPORTS = [
  { name: 'desktop', viewport: { width: 1440, height: 900 } },
  { name: 'phone', viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true },
];
const block = (a) => '```naozhi-action\n' + JSON.stringify(a) + '\n```';

function sessions() {
  const s = defaultSessions();
  s.sessions.push({ key: TASK, state: 'ready', platform: 'task', agent: 'general', cli_name: 'claude', purpose: 'task', user_label: 'iso 修登录页',
    purpose_title: 'iso 修登录页', workspace: '/home/user/workspace/myproject', project: 'myproject', node: 'local', last_active: now - 10000 });
  return s;
}

test('custody blocks render as cards naming the session', async ({ browser }) => {
  for (const vp of VIEWPORTS) {
    const ctx = await browser.newContext({ viewport: vp.viewport, isMobile: !!vp.isMobile, hasTouch: !!vp.hasTouch });
    const page = await ctx.newPage();
    const mock = await startMockServer({
      ws: true, sessions: sessions(),
      planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
      plannerEvents: [
        { type: 'text', time: now - 5000, uuid: 'pc-t1', detail: '我盯着它，让它接着修。\n\n' +
          block({ type: 'custody', key: TASK, on: true }) + '\n\n' +
          block({ type: 'custody', key: TASK, text: 'iso 接着把登录页的测试补上' }) + '\n\n' +
          block({ type: 'custody', key: TASK, on: false }) },
      ],
    });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      await page.click('#abnav-planner');
      const reply = page.locator('#pl-stream .event.text').first();
      const cards = reply.locator('.pl-act-custody');
      await expect(cards.locator('.pl-act-h')).toHaveText(['打开托管', '托管 · 让它继续', '关掉托管']);
      await expect(cards.nth(1).locator('.pl-act-q')).toHaveText('iso 接着把登录页的测试补上');
      await expect(cards.locator('.pl-scard[data-key="' + TASK + '"] .pl-scard-name')).toHaveText(['iso 修登录页', 'iso 修登录页', 'iso 修登录页']);
      await expect(reply.locator('.pl-act-bad')).toHaveCount(0);
      await expect(reply).not.toContainText(TASK);
      await expect(reply).not.toContainText('custody');
      await reply.screenshot({ path: test.info().outputPath('custody-cards-' + vp.name + '.png') });
    } finally {
      await ctx.close();
      mock.server.close();
    }
  }
});
