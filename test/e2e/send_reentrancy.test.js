// @ts-check
// E2E: sendMessage reentrancy across the auto-orient wait (#2405).
//
// Regression guard for "dashboard 首条用户消息被重复发送 4 次". sendMessage()
// awaits awaitPendingOrients() (up to ORIENT_MAX_WAIT_MS) when an attached
// image is still being auto-oriented. Before the fix the only reentrancy gate
// was `if (sending) return;` at the top, but `sending = true` was set AFTER
// that await — so every Enter pressed during the wait spawned another
// sendMessage that captured the same text. Once orient settled, waiter #1
// POSTed text+file_ids and cleared the composer; waiters #2..N resumed with an
// empty pendingFiles and each fired one more text-only send. The mock server
// rejects /ws, so every send here is an HTTP POST and mock.sendCalls is the
// exact count of messages the backend would have received.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

let mock;

// Long enough to let the test fire several sends "mid-wait", short enough to
// keep the suite fast. Must stay well under ORIENT_MAX_WAIT_MS (8000) so the
// orient actually settles instead of hitting the client-side abort.
const ORIENT_DELAY_MS = 1500;
const PROJ = '/home/user/workspace/myproject';

test.beforeAll(async () => {
  mock = await startMockServer();
});

test.afterAll(async () => {
  await new Promise(r => mock.server.close(r));
});

test.beforeEach(async ({ page }) => {
  mock.resetCalls();
  // The mock server has no /api/sessions/orient route; answer it here with a
  // deliberate delay so `entry.orienting` stays true for ORIENT_DELAY_MS.
  await page.route('**/api/sessions/orient', async (route) => {
    await new Promise(r => setTimeout(r, ORIENT_DELAY_MS));
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ rotated: false }),
    });
  });
  await page.goto(`${mock.url}/dashboard`);
  await page.waitForSelector('.session-card', { timeout: 5000 });
  await page.evaluate(() => localStorage.removeItem('nz:pending_sessions'));
});

// armComposer creates a fresh (not yet server-listed) session, seeds the
// composer text and one already-uploaded image whose auto-orient call is in
// flight — exactly the state a user is in when they hit Enter right after
// dropping a photo.
async function armComposer(page, text) {
  return page.evaluate(({ proj, text }) => {
    doCreateInProject(proj, 'myproject', 'local', undefined, 'general', { mode: 'new' });
    const input = document.getElementById('msg-input');
    setMsgValue(input, text);
    const entry = {
      id: 'file-1', kind: 'image', status: 'ready',
      file: new File([new Uint8Array(16)], 'photo.png', { type: 'image/png' }),
      blobUrl: 'data:image/png;base64,iVBORw0KGgo=', normalizedSize: 16,
    };
    pendingFiles.push(entry);
    renderFilePreviews();
    maybeAutoOrient(entry); // NOT awaited — flips entry.orienting until the delayed route answers
    return { key: selectedKey, orienting: !!entry.orienting };
  }, { proj: PROJ, text });
}

test('Enter mashed during the orient wait produces exactly one send', async ({ page }) => {
  const armed = await armComposer(page, 'hello with image');
  expect(armed.orienting).toBe(true);

  // Three sends ~60ms apart while orient is still pending. The first call
  // must close the reentrancy gate synchronously (before its first await) and
  // give immediate button feedback; the other two must be no-ops.
  const immediate = await page.evaluate(async () => {
    const btn = document.getElementById('btn-send');
    sendMessage();
    const gateClosed = sending === true;
    const btnFeedback = !!btn && btn.classList.contains('sending');
    await new Promise(r => setTimeout(r, 60));
    sendMessage();
    await new Promise(r => setTimeout(r, 60));
    sendMessage();
    return { gateClosed, btnFeedback };
  });
  expect(immediate.gateClosed, 'sending must be true before the first await').toBe(true);
  expect(immediate.btnFeedback, '#btn-send must carry .sending before the first await').toBe(true);

  await expect.poll(() => mock.sendCalls.length, { timeout: 8000 }).toBeGreaterThan(0);
  // Old code: the two stragglers land within ~250ms of the first POST as
  // text-only ghosts. Give them ample time to show up before counting.
  await page.waitForTimeout(1500);

  const payloads = mock.sendCalls.map(b => JSON.parse(b));
  expect(payloads.length, 'send POSTs: ' + JSON.stringify(payloads)).toBe(1);
  expect(payloads[0].text).toBe('hello with image');
  expect(payloads[0].file_ids).toEqual(['file-1']);

  // Gate released afterwards so the next real message can go out.
  await expect.poll(() => page.evaluate(() => sending)).toBe(false);
});

test('real Enter keypresses during the orient wait are collapsed to one send', async ({ page }) => {
  await armComposer(page, 'keyboard mash');
  const input = page.locator('#msg-input');
  await input.focus();
  await page.keyboard.press('Enter');
  await page.waitForTimeout(60);
  await page.keyboard.press('Enter');
  await page.waitForTimeout(60);
  await page.keyboard.press('Enter');

  await expect.poll(() => mock.sendCalls.length, { timeout: 8000 }).toBeGreaterThan(0);
  await page.waitForTimeout(1500);
  const payloads = mock.sendCalls.map(b => JSON.parse(b));
  expect(payloads.length, 'send POSTs: ' + JSON.stringify(payloads)).toBe(1);
  expect(payloads[0].file_ids).toEqual(['file-1']);
});

test('first send on a session the server has not listed yet flips the composer to running', async ({ page }) => {
  await armComposer(page, 'first message');
  await page.evaluate(() => { sendMessage(); });
  await expect.poll(() => mock.sendCalls.length, { timeout: 8000 }).toBe(1);

  const ui = await page.evaluate(() => ({
    listed: !!sessionsData[sid(selectedKey, selectedNode)],
    stop: (document.getElementById('btn-stop') || {}).style?.display,
    send: (document.getElementById('btn-send') || {}).style?.display,
    justSent: turnState.justSent,
  }));
  // Premise: the mock's /api/sessions never returns the freshly created key,
  // so markSessionOptimisticRunning has no sessionsData entry to flip.
  expect(ui.listed).toBe(false);
  // The send→stop swap and the "已发送，正在处理…" banner must still happen —
  // this missing feedback is what invited the Enter mashing in #2405.
  expect(ui.stop).toBe('flex');
  expect(ui.send).toBe('none');
  expect(ui.justSent).toBe(true);
});

test('an upload that starts mid-wait blocks the send instead of being silently dropped', async ({ page }) => {
  await armComposer(page, 'text plus late upload');
  await page.evaluate(() => {
    sendMessage();
    // A second drop lands while orient is pending: still uploading, no id.
    pendingFiles.push({
      kind: 'image', status: 'uploading',
      file: new File([new Uint8Array(8)], 'late.png', { type: 'image/png' }),
      blobUrl: 'data:image/png;base64,iVBORw0KGgo=',
    });
    renderFilePreviews();
  });
  await page.waitForTimeout(ORIENT_DELAY_MS + 1000);

  const state = await page.evaluate(() => ({
    files: pendingFiles.length,
    text: getMsgValue(document.getElementById('msg-input')),
    sending,
  }));
  // Old code: fileIDs.filter(Boolean) dropped the id-less upload, the send went
  // out with only file-1, and clearPendingFiles() then deleted the late upload.
  expect(mock.sendCalls.length, 'send must be blocked by the post-wait uploading gate').toBe(0);
  expect(state.files).toBe(2);
  expect(state.text).toBe('text plus late upload');
  expect(state.sending).toBe(false);
});
