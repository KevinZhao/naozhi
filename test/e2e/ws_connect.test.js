// @ts-check
// WebSocket connect-path regression. The rest of the suite runs against a
// mock server without an upgrade listener, so the dashboard silently falls
// back to polling and the wsm connect → auth → onMessage dispatch chain is
// never exercised. These tests opt into the minimal WS mock (ws:true) and
// pin that chain: the top-level wsm.connect() startup call must reach
// CONNECTED, and a server-pushed frame must flow through onMessage into
// dashboard state.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };
const SESSION_KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';

test.describe('WebSocket connect path', () => {
  let mock;

  test.beforeAll(async () => { mock = await startMockServer({ ws: true }); });
  test.afterAll(() => mock.server.close());

  test('startup wsm.connect() authenticates and reaches connected state', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForFunction(() => wsm.state === WS_STATES.CONNECTED);

    // The handshake the server saw must start with the auth frame.
    expect(mock.wsConnections.length).toBeGreaterThan(0);
    expect(mock.wsConnections[0].messages[0].type).toBe('auth');

    await ctx.close();
  });

  test('server-pushed session_state dispatches through onMessage into sessionsData', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForFunction(() => wsm.state === WS_STATES.CONNECTED);
    await page.waitForFunction(
      (key) => !!sessionsData[sid(key, 'local')],
      SESSION_KEY
    );

    const conn = mock.wsConnections[mock.wsConnections.length - 1];
    conn.send({ type: 'session_state', key: SESSION_KEY, node: 'local', state: 'running' });
    await page.waitForFunction(
      (key) => sessionsData[sid(key, 'local')].state === 'running',
      SESSION_KEY
    );

    await ctx.close();
  });
});
