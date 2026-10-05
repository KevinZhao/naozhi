// @ts-check
//
// A takeover accepted with 202 can still fail in the background, after the
// external CLI was terminated. A local takeover's 202 carries a takeover_id;
// the composer polls GET /api/discovered/takeover/status alongside the session
// list and stops at the first "failed", naming the cause. Without a
// takeover_id (a remote node, an older build) it keeps the session-list poll
// and its timeout says the external CLI is gone.
//
// 跑法：cd test/e2e && npx playwright test takeover_failure.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, '仅 desktop-chrome project 跑');
  }
});

/** @type {Awaited<ReturnType<typeof startMockServer>>} */
let mock;
test.beforeAll(async () => {
  mock = await startMockServer({
    discovered: [
      { pid: 779, session_id: 'disc-bgfail', cwd: '/home/user/workspace/bgproj', proc_start_time: 1,
        node: 'local', cli_name: 'claude-code', type_label: 'Claude Terminal', state: 'ready', started_at: Date.now() - 70000 },
      { pid: 780, session_id: 'disc-remote-bg', cwd: '/srv/remoteproj', proc_start_time: 1,
        node: 'remote1', cli_name: 'claude-code', type_label: 'Claude Terminal', state: 'ready', started_at: Date.now() - 70000 },
    ],
  });
});
test.afterAll(() => mock.server.close());

const TAKEOVER_ID = '0123456789abcdef0123456789abcdef';

/**
 * Answers the takeover with 202 (body) and the status route with `pending`
 * on its first call and `outcome` afterwards; returns the call counters.
 * @param {import('@playwright/test').Page} page
 * @param {object} body
 * @param {object} outcome
 */
async function routeTakeover(page, body, outcome) {
  const calls = { takeover: 0, status: 0, statusIDs: /** @type {string[]} */ ([]) };
  await page.route((url) => url.pathname === '/api/discovered/takeover', (route) => {
    calls.takeover++;
    return route.fulfill({ status: 202, contentType: 'application/json', body: JSON.stringify(body) });
  });
  await page.route((url) => url.pathname === '/api/discovered/takeover/status', (route) => {
    calls.status++;
    calls.statusIDs.push(new URL(route.request().url()).searchParams.get('id') || '');
    const st = calls.status === 1 ? { state: 'pending' } : outcome;
    return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(st) });
  });
  return calls;
}

/**
 * @param {import('@playwright/test').Page} page
 * @param {number} [pid]
 */
async function sendOnDiscovered(page, pid = 779) {
  await page.goto(mock.url + '/dashboard');
  await page.locator(`.session-card[data-key^="_discovered:${pid}"]`).click();
  await expect(page.locator('#msg-input')).toHaveAttribute('data-placeholder', 'send a message to take over...');
  await page.fill('#msg-input', 'take it over');
  await page.click('#btn-send');
}

for (const c of [
  { cls: 'max_procs', want: '接管失败：进程数已满，外部 CLI 已终止；请关闭一个空闲会话后从历史记录重新打开该会话（对话记录仍在）' },
  { cls: 'shim_stuck', want: '接管失败：旧会话进程尚未释放，外部 CLI 已终止；请稍后从历史记录重新打开该会话（对话记录仍在）' },
]) {
  test(`a background takeover failure (${c.cls}) stops the poll at once and names the cause`, async ({ page }) => {
    const calls = await routeTakeover(page, { status: 'accepted', key: 'local:takeover:bgproj', takeover_id: TAKEOVER_ID },
      { state: 'failed', class: c.cls });
    await sendOnDiscovered(page);

    // Two 500ms poll rounds: pending, then failed. The toast only comes once
    // the poll loop has ended, well before its 10s timeout.
    await expect(page.locator('.toast')).toContainText(c.want, { timeout: 2500 });
    expect(calls.takeover).toBe(1);
    expect(calls.statusIDs).toEqual([TAKEOVER_ID, TAKEOVER_ID]);
    const input = page.locator('#msg-input');
    await expect(input).toHaveAttribute('contenteditable', 'true');
    await expect(input).toHaveAttribute('data-placeholder', 'send a message...');
  });
}

test('without a takeover_id the poll runs to its timeout, which says the external CLI is gone', async ({ page }) => {
  test.setTimeout(30000);
  const calls = await routeTakeover(page, { status: 'accepted', key: 'local:takeover:bgproj' }, { state: 'failed', class: 'max_procs' });
  await sendOnDiscovered(page);

  await expect(page.locator('.toast')).toContainText(
    '接管超时：外部 CLI 已终止，但新会话未就绪。对话记录仍在，可稍后从历史记录重新打开', { timeout: 15000 });
  expect(calls.takeover).toBe(1);
  expect(calls.status).toBe(0);
  await expect(page.locator('#msg-input')).toHaveAttribute('contenteditable', 'true');
});

// The status route only knows this node's takeovers; a remote one is not asked.
test('a takeover on a remote node never queries the local status route', async ({ page }) => {
  const calls = await routeTakeover(page, { status: 'accepted', key: 'local:takeover:remoteproj', node: 'remote1', takeover_id: TAKEOVER_ID },
    { state: 'failed', class: 'max_procs' });
  await sendOnDiscovered(page, 780);
  await expect.poll(() => calls.takeover).toBe(1);
  // Each poll round reads the session list and, for a local takeover, the
  // status alongside it; three session reads cover at least two rounds.
  const sessionsAtTakeover = mock.sessionsGetCalls;
  await expect.poll(() => mock.sessionsGetCalls).toBeGreaterThanOrEqual(sessionsAtTakeover + 3);
  expect(calls.status).toBe(0);
});
