// @ts-check
//
// The New Session connection picker (#new-node, auth_modal.js renderNodePicker)
// labels every node with getNodeStatus (session_ident.js): 'local' follows the
// WebSocket state machine (connected, still authenticating, or no socket at
// all), a remote reports the status the server's node snapshot carries, and a
// remote with no status reads as offline rather than reachable.
//
// 跑法：cd test/e2e && npx playwright test node_picker_status.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');

test.use({ viewport: { width: 1280, height: 800 } });

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'the picker labels do not depend on the viewport; desktop-chrome only');
  }
});

function multiNodeSessions() {
  const sessions = defaultSessions();
  sessions.nodes = {
    local: { display_name: 'Local', status: 'ok' },
    mac: { display_name: 'Mac', status: 'unreachable' },
    pi: { display_name: 'Pi' },
  };
  return sessions;
}

/** @type {Awaited<ReturnType<typeof startMockServer>> | null} */
let mock = null;
// The page holds the WebSocket open, and server.close() waits for it.
test.afterEach(async ({ page }) => {
  await page.close();
  if (mock) mock.server.close();
  mock = null;
});

/**
 * @param {import('@playwright/test').Page} page
 * @param {string} wsState - the WS_STATES value to wait for before opening the picker
 * @param {{reload?: boolean}} [opts] - reload:false keeps the already-loaded page
 */
async function pickerLabels(page, wsState, opts = {}) {
  if (!mock) throw new Error('mock not started');
  if (opts.reload !== false) await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.waitForFunction((s) => wsm.state === s, wsState);
  await page.click('.hdr-btn[title="New Session"]');
  await page.waitForSelector('#new-node');
  return page.locator('#new-node option').allTextContents();
}

test('each connection option carries its node status', async ({ page }) => {
  mock = await startMockServer({ ws: true, sessions: multiNodeSessions() });
  const labels = await pickerLabels(page, 'connected');
  expect(labels).toEqual(['本地 · connected', 'Mac · unreachable', 'Pi · offline']);
});

test('local reads connecting while the socket waits for auth_ok', async ({ page }) => {
  mock = await startMockServer({ ws: true, wsHoldAuth: true, sessions: multiNodeSessions() });
  const labels = await pickerLabels(page, 'authenticating');
  expect(labels[0]).toBe('本地 · connecting');
});

// Without ws:true the mock drops every /ws upgrade and wsm keeps redialling.
// Waiting for backoff >= 4000 means the pending redial is at least 2s away, so
// the picker cannot render during a brief CONNECTING window.
test('local reads offline when there is no socket', async ({ page }) => {
  mock = await startMockServer({ sessions: multiNodeSessions() });
  await page.goto(mock.url + '/dashboard');
  await page.waitForFunction(() => wsm.state === WS_STATES.DISCONNECTED && wsm.backoff >= 4000);
  const labels = await pickerLabels(page, 'disconnected', { reload: false });
  expect(labels[0]).toBe('本地 · offline');
});
