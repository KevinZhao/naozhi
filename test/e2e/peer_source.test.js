// E2E: a cross-session message's sender line (event_render.js peerSourceHtml,
// planner RFC §6.2, D92). The Planner is known by its key, planner:main: its
// --name is per instance (planner-<hash>), and a bare "planner" that resolved
// to no session here is some other naozhi's, so it reads as a terminal
// session.
const path = require('path');
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');
const { openSessionCard } = require('./session_nav');

const A = 'dashboard:direct:2026-01-01-120000-1:myproject';
const now = Date.now();
const T = (min) => now - min * 60000;

const EVENTS = [
  { type: 'user', detail: '写一个 hello.md', time: T(30), uuid: 'u1' },
  { type: 'peer_message', peer: 'planner-ab12cd', peer_key: 'planner:main', detail: '进展怎么样？', time: T(20), uuid: 'p1' },
  { type: 'peer_message', peer: 'planner', detail: '另一个实例的消息', time: T(10), uuid: 'p2' },
  { type: 'text', detail: '已写好', time: T(9), uuid: 't1' },
];

for (const [name, viewport] of [['desktop', { width: 1440, height: 900 }], ['mobile', { width: 390, height: 844 }]]) {
  test(`the Planner's message reads 来自 Planner by its key (${name})`, async ({ page }) => {
    await page.setViewportSize(viewport);
    const mock = await startMockServer({ ws: true });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      await openSessionCard(page, page.locator(`.session-card[data-key="${A}"]`));
      const conn = mock.wsConnections[mock.wsConnections.length - 1];
      await expect.poll(() => conn.messages.filter((m) => m.type === 'subscribe' && m.key === A).length).toBe(1);
      conn.send({ type: 'subscribed', key: A, state: 'ready' });
      conn.send({ type: 'history', key: A, initial: true, events: EVENTS });

      const sources = page.locator('#events-scroll .peer-source');
      await expect(sources).toHaveCount(2);
      await expect(sources.nth(0)).toHaveText('来自 Planner');
      await expect(sources.nth(1)).toHaveText('来自终端会话 planner');
      if (process.env.NZ_SHOT_DIR) {
        if (name === 'mobile') await expect(page.locator('body')).toHaveClass(/mobile-chat-view/);
        await page.waitForTimeout(400); // the list's slide-out
        await page.screenshot({ path: path.join(process.env.NZ_SHOT_DIR, `peer-source-${name}.png`) });
      }
    } finally {
      mock.server.close();
    }
  });
}
