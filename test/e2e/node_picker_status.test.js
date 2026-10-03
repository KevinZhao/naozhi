// @ts-check
//
// The New Session connection picker (#new-node, auth_modal.js renderNodePicker)
// labels every node with getNodeStatus (session_ident.js): 'local' follows the
// WebSocket state machine, a remote reports the status the server's node
// snapshot carries, and a remote with no status reads as offline rather than
// reachable.
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

/** @type {Awaited<ReturnType<typeof startMockServer>>} */
let mock;
test.beforeAll(async () => {
  const sessions = defaultSessions();
  sessions.nodes = {
    local: { display_name: 'Local', status: 'ok' },
    mac: { display_name: 'Mac', status: 'unreachable' },
    pi: { display_name: 'Pi' },
  };
  mock = await startMockServer({ ws: true, sessions });
});
test.afterAll(async () => { await new Promise((r) => mock.server.close(r)); });

test('each connection option carries its node status', async ({ page }) => {
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.waitForFunction(() => wsm.state === WS_STATES.CONNECTED);
  await page.click('.hdr-btn[title="New Session"]');
  await page.waitForSelector('#new-node');
  const labels = await page.locator('#new-node option').allTextContents();
  expect(labels).toEqual(['本地 · connected', 'Mac · unreachable', 'Pi · offline']);
});
