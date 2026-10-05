// @ts-check
//
// A takeover accepted with 202 can still fail in the background, after the
// external CLI was terminated. A local takeover's 202 carries a takeover_id;
// the composer polls GET /api/discovered/takeover/status alongside the session
// list, sends only once that status says ready and the key is listed, and stops
// at the first "failed" of any class, naming the cause. A session another actor
// put on the key is never sent into. An expired outcome ("unknown") falls back
// to the key being listed. The poll is bounded by a 10s deadline however slow
// its reads are. Without a takeover_id (a remote node, an older build) it keeps
// the session-list poll and its timeout says the external CLI is gone.
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

/**
 * Lists `key` on `node` in every /api/sessions answer once `when()` is true.
 * @param {import('@playwright/test').Page} page
 * @param {string} key
 * @param {() => boolean} when
 * @param {string} [node]
 */
async function listKeyWhen(page, key, when, node = 'local') {
  await page.route((url) => url.pathname === '/api/sessions', async (route) => {
    const res = await route.fetch();
    const body = await res.json();
    if (when()) {
      body.sessions = [...(body.sessions || []), { key, state: 'ready', platform: 'dashboard', agent: 'general', cli_name: 'claude',
        workspace: '/home/user/workspace/bgproj', last_active: Date.now(), node, project: 'bgproj' }];
    }
    return route.fulfill({ response: res, json: body });
  });
}

/** @param {number} from */
function sentKeys(from) {
  return mock.sendCalls.slice(from).map((b) => JSON.parse(b).key);
}

// in_progress: another actor holds the key, so the session listed there is not
// this attempt's and the text must not go into it.
test('an in_progress failure stops the poll at once and does not send into the key\'s session', async ({ page }) => {
  const key = 'local:takeover:bgproj';
  const calls = await routeTakeover(page, { status: 'accepted', key, takeover_id: TAKEOVER_ID },
    { state: 'failed', class: 'in_progress' });
  await listKeyWhen(page, key, () => calls.takeover > 0);
  const sendsBefore = mock.sendCalls.length;
  await sendOnDiscovered(page);

  await expect(page.locator('.toast')).toContainText(
    '接管失败：该会话正被另一次接管占用，外部 CLI 已终止；请稍后从历史记录重新打开该会话（对话记录仍在）', { timeout: 2500 });
  expect(calls.statusIDs).toEqual([TAKEOVER_ID, TAKEOVER_ID]);
  await expect(page.locator('#msg-input')).toHaveAttribute('contenteditable', 'true');
  expect(sentKeys(sendsBefore)).not.toContain(key);
});

// The key is listed from the first round, but the attempt's own status only
// says ready on the fourth read: the send waits for it, and the poll then ends.
test('a listed key does not send while the attempt is still pending', async ({ page }) => {
  const key = 'local:takeover:bgproj';
  let statusReads = 0;
  await page.route((url) => url.pathname === '/api/discovered/takeover', (route) => route.fulfill({ status: 202,
    contentType: 'application/json', body: JSON.stringify({ status: 'accepted', key, takeover_id: TAKEOVER_ID }) }));
  await page.route((url) => url.pathname === '/api/discovered/takeover/status', (route) => {
    statusReads++;
    return route.fulfill({ status: 200, contentType: 'application/json',
      body: JSON.stringify({ state: statusReads < 4 ? 'pending' : 'ready' }) });
  });
  await listKeyWhen(page, key, () => true);
  const sendsBefore = mock.sendCalls.length;
  await sendOnDiscovered(page);

  await expect.poll(() => sentKeys(sendsBefore), { timeout: 8000 }).toContain(key);
  expect(statusReads).toBe(4);
  await expect(page.locator('.toast')).not.toContainText('接管');
});

// The outcome expired (or naozhi restarted): the key being listed is enough.
test('an unknown outcome falls back to the key being listed', async ({ page }) => {
  const key = 'local:takeover:bgproj';
  const calls = await routeTakeover(page, { status: 'accepted', key, takeover_id: TAKEOVER_ID }, { state: 'unknown' });
  await listKeyWhen(page, key, () => calls.status >= 2);
  const sendsBefore = mock.sendCalls.length;
  await sendOnDiscovered(page);

  await expect.poll(() => sentKeys(sendsBefore), { timeout: 8000 }).toContain(key);
  await expect(page.locator('.toast')).not.toContainText('接管');
});

// A status read may take its full 2s timeout; the poll still ends near 10s
// rather than after twenty such rounds (about 50s).
test('a stalled status route does not stretch the poll past its deadline', async ({ page }) => {
  test.setTimeout(40000);
  let statusReads = 0;
  await page.route((url) => url.pathname === '/api/discovered/takeover', (route) => route.fulfill({ status: 202,
    contentType: 'application/json', body: JSON.stringify({ status: 'accepted', key: 'local:takeover:bgproj', takeover_id: TAKEOVER_ID }) }));
  // Never answered: every read runs into its 2s client-side timeout.
  await page.route((url) => url.pathname === '/api/discovered/takeover/status', () => { statusReads++; });
  await sendOnDiscovered(page);

  await expect(page.locator('.toast')).toContainText(
    '接管超时：外部 CLI 已终止，但新会话未就绪。对话记录仍在，可稍后从历史记录重新打开', { timeout: 15000 });
  expect(statusReads).toBeGreaterThanOrEqual(2);
  expect(statusReads).toBeLessThanOrEqual(6);
});

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

// A remote node has no status route, so its key being listed is what it sends on.
test('a takeover on a remote node sends once its key is listed there', async ({ page }) => {
  const key = 'local:takeover:remoteproj';
  const calls = await routeTakeover(page, { status: 'accepted', key, node: 'remote1', takeover_id: TAKEOVER_ID },
    { state: 'failed', class: 'max_procs' });
  let reads = 0;
  await listKeyWhen(page, key, () => calls.takeover > 0 && ++reads >= 2, 'remote1');
  const sendsBefore = mock.sendCalls.length;
  await sendOnDiscovered(page, 780);

  await expect.poll(() => sentKeys(sendsBefore), { timeout: 8000 }).toContain(key);
  expect(calls.status).toBe(0);
});
