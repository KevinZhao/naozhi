// A remote send whose answer was lost (docs/rfc/planner.md §15.5 ③): the
// primary acks it status "unknown", the bubble stays with "状态未知，可重试",
// and the retry sends the same frame — same id, same send_id — so the node
// answers it from its dedup table instead of running a second turn.
//
// 跑法：cd test/e2e && npx playwright test remote_send_unknown.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { waitForWs } = require('./shim_wait');
const { openSessionCard } = require('./session_nav');

const REMOTE_KEY = 'dashboard:direct:2026-01-01-130000-9:remoteproj';

function remoteSessions() {
  const s = defaultSessions();
  s.sessions.push({
    key: REMOTE_KEY, state: 'ready', platform: 'dashboard', agent: 'general',
    cli_name: 'claude', workspace: '/home/user/workspace/remoteproj',
    last_active: Date.now() - 10000, last_prompt: 'hi', node: 'mac', project: 'remoteproj',
  });
  s.nodes = { local: { display_name: 'Local', status: 'ok' }, mac: { display_name: 'Mac', status: 'ok' } };
  return s;
}

const sendFrames = (conn) => conn.messages.filter((m) => m.type === 'send');

for (const [name, viewport] of [['desktop', { width: 1280, height: 800 }], ['phone', { width: 390, height: 844 }]]) {
  test(`unknown send_ack keeps the bubble and retries with the same send_id (${name})`, async ({ browser }) => {
    const mock = await startMockServer({ ws: true, sessions: remoteSessions() });
    const ctx = await browser.newContext({ viewport, hasTouch: name === 'phone', isMobile: name === 'phone' });
    const page = await ctx.newPage();
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page);
      await openSessionCard(page, page.locator(`.session-card[data-key="${REMOTE_KEY}"][data-node="mac"]`));
      const input = page.locator('#msg-input');
      await input.click();
      await input.pressSequentially('iso remote hello');
      await page.keyboard.press('Enter');

      const conn = mock.wsConnections[mock.wsConnections.length - 1];
      await expect.poll(() => sendFrames(conn).length).toBe(1);
      const first = sendFrames(conn)[0];
      expect(first.node).toBe('mac');
      expect(first.send_id).toMatch(/^s[0-9a-f]{32}$/);
      expect(first.id).toBe(first.send_id);

      conn.send({ type: 'send_ack', id: first.id, key: REMOTE_KEY, node: 'mac', status: 'unknown', turn_id: first.send_id });
      const bubble = page.locator(`#events-scroll .optimistic-msg[data-send-id="${first.id}"]`);
      const chip = bubble.locator('.msg-unknown-chip');
      await expect(chip).toContainText('状态未知，可重试');
      await expect(bubble).toContainText('iso remote hello');
      await page.screenshot({ path: test.info().outputPath(`unknown-${name}.png`), animations: 'disabled' });

      await chip.locator('.msg-retry-btn').click();
      await expect.poll(() => sendFrames(conn).length).toBe(2);
      expect(sendFrames(conn)[1]).toEqual(first);
      await expect(chip).toHaveText('重试中…');

      conn.send({ type: 'send_ack', id: first.id, key: REMOTE_KEY, node: 'mac', status: 'accepted' });
      await expect(chip).toHaveCount(0);
      await expect(bubble).toBeVisible();
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
}
