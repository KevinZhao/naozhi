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
const { waitForWs } = require('./shim_wait');

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
    await waitForWs(page);

    // The handshake the server saw must start with the auth frame.
    expect(mock.wsConnections.length).toBeGreaterThan(0);
    expect(mock.wsConnections[0].messages[0].type).toBe('auth');

    await ctx.close();
  });

  test('server-pushed session_state dispatches through onMessage into sessionsData', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page);
    await page.waitForFunction(
      (key) => !!window.nz.test.sessionsData[window.nz.test.sid(key, 'local')],
      SESSION_KEY
    );
    // CONNECTED schedules a debounced /api/sessions refresh. One sent after
    // the push applies the mock's static 'ready' snapshot over it, which is
    // correct (a later snapshot is how a dropped push heals, case (e) in
    // session_state_stale_poll), so push only once that refresh has started.
    await page.evaluate(async () => {
      /** @type {any} */ (window).__timers = (await import('/static/state.js')).timers;
    });
    await page.waitForFunction(() => /** @type {any} */ (window).__timers.fetchDebounce === null);

    const conn = mock.wsConnections[mock.wsConnections.length - 1];
    conn.send({ type: 'session_state', key: SESSION_KEY, node: 'local', state: 'running' });
    await page.waitForFunction(
      (key) => window.nz.test.sessionsData[window.nz.test.sid(key, 'local')].state === 'running',
      SESSION_KEY
    );
    // Guards the fixture: the snapshot never says 'running', so a REST poll
    // cannot have produced the flip.
    const served = await page.evaluate(async (key) => {
      const r = await fetch('/api/sessions');
      return (await r.json()).sessions.find((s) => s.key === key).state;
    }, SESSION_KEY);
    expect(served).toBe('ready');

    await ctx.close();
  });
});
