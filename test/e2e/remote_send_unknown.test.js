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

// The WS-down fallback (HTTP POST) has no ack: the primary reports a lost
// answer as send_error {status:"unknown", turn_id}. The bubble stays with the
// same retry, and the retry is the WS frame of that send — same send_id.
test('unknown send_error keeps the HTTP send and retries it with the same send_id', async ({ browser }) => {
  const mock = await startMockServer({ ws: true, sessions: remoteSessions() });
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const page = await ctx.newPage();
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page);
    await openSessionCard(page, page.locator(`.session-card[data-key="${REMOTE_KEY}"][data-node="mac"]`));
    // The frame fails to leave, so the send falls through to HTTP.
    await page.evaluate(() => { window.__wsSend = wsm.send; wsm.send = () => false; });
    const input = page.locator('#msg-input');
    await input.click();
    await input.pressSequentially('iso http hello');
    await page.keyboard.press('Enter');
    await expect.poll(() => mock.sendCalls.length).toBe(1);
    const body = JSON.parse(mock.sendCalls[0]);
    expect(body.send_id).toMatch(/^s[0-9a-f]{32}$/);
    await page.evaluate(() => { wsm.send = window.__wsSend; });

    const conn = mock.wsConnections[mock.wsConnections.length - 1];
    conn.send({ type: 'send_error', key: REMOTE_KEY, node: 'mac', status: 'unknown', turn_id: body.send_id, error: '远程节点未及时回应，消息状态未知' });
    const bubble = page.locator(`#events-scroll .optimistic-msg[data-send-id="${body.send_id}"]`);
    const chip = bubble.locator('.msg-unknown-chip');
    await expect(chip).toContainText('状态未知，可重试');
    await expect(page.locator('#toast.show')).toHaveCount(0);

    await chip.locator('.msg-retry-btn').click();
    await expect.poll(() => sendFrames(conn).length).toBe(1);
    expect(sendFrames(conn)[0]).toMatchObject({ key: REMOTE_KEY, node: 'mac', text: 'iso http hello', id: body.send_id, send_id: body.send_id });
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

// A new remote session: the access-profile picker hides (a remote node runs on
// its own credentials and the primary refuses the pick), and an unknown ack
// gives the spawn picks back, so the next message carries them again in case
// the first never reached the node.
for (const [name, viewport] of [['desktop', { width: 1280, height: 800 }], ['phone', { width: 390, height: 844 }]]) {
  test(`new remote session: no access profile, picks survive an unknown ack (${name})`, async ({ browser }) => {
    const mock = await startMockServer({ ws: true, sessions: remoteSessions() });
    const ctx = await browser.newContext({ viewport, hasTouch: name === 'phone', isMobile: name === 'phone' });
    const page = await ctx.newPage();
    try {
      await page.route('**/api/access-profiles', (route) => route.fulfill({ json: {
        profiles: [{ id: 'work', display_name: 'Work' }, { id: 'home', display_name: 'Home' }], default: 'work' } }));
      await page.route('**/api/cli/backends**', (route) => route.fulfill({ json: {
        backends: [{ id: 'claude', display_name: 'Claude' }, { id: 'kiro', display_name: 'Kiro' }], default: 'claude' } }));
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page);
      // Viewing a session on mac makes mac the new session's connection.
      await openSessionCard(page, page.locator(`.session-card[data-key="${REMOTE_KEY}"][data-node="mac"]`));
      if (name === 'phone') await page.locator('.main .btn-mobile-back').click();
      await page.locator('#btn-new-session').click();
      await expect(page.locator('#new-node')).toHaveValue('mac');
      const profile = page.locator('#new-access-profile');
      await expect(profile).toBeHidden();
      await page.selectOption('#new-node', 'local');
      await expect(profile).toBeVisible();
      await page.selectOption('#new-node', 'mac');
      await expect(profile).toBeHidden();
      await page.selectOption('#new-backend', 'kiro');
      await page.fill('#new-workspace', '/home/user/iso');
      await page.screenshot({ path: test.info().outputPath(`new-remote-${name}.png`), animations: 'disabled' });
      await page.click('[data-action="session-create"]');

      const input = page.locator('#msg-input');
      await input.click();
      await input.pressSequentially('iso first');
      await page.keyboard.press('Enter');
      const conn = mock.wsConnections[mock.wsConnections.length - 1];
      await expect.poll(() => sendFrames(conn).length).toBe(1);
      const first = sendFrames(conn)[0];
      expect(first).toMatchObject({ node: 'mac', backend: 'kiro', workspace: '/home/user/iso' });
      expect(first.access_profile).toBeUndefined();

      conn.send({ type: 'send_ack', id: first.id, key: first.key, node: 'mac', status: 'unknown', turn_id: first.send_id });
      await expect(page.locator(`#events-scroll .optimistic-msg[data-send-id="${first.id}"] .msg-unknown-chip`)).toBeVisible();
      await input.click();
      await input.pressSequentially('iso second');
      await page.keyboard.press('Enter');
      await expect.poll(() => sendFrames(conn).length).toBe(2);
      const second = sendFrames(conn)[1];
      expect(second.send_id).not.toBe(first.send_id);
      expect(second).toMatchObject({ key: first.key, node: 'mac', backend: 'kiro', workspace: '/home/user/iso' });
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
}
