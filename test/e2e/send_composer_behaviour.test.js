// @ts-check
// sendComposerTurn's branches, pinned in a browser before S19-7 (#3025) splits
// it: the WS path, the HTTP path's rejections (4xx with and without
// files_consumed, 429, 401), the `reset` ack, and a network failure. What each
// branch owes the operator is the composer text (put back on failure, even
// when the box changed while the request was in flight; cleared on success),
// the attachment chips (dropped only when the server consumed them),
// the optimistic running flip (kept only for a turn that runs) and the
// pending-session blob in localStorage (consumed only by a send that went out).
//
// The mock rejects /ws by default, so sends go over HTTP; page.route answers
// /api/sessions/send per test. The WS test opts into the mock's socket.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.use({ viewport: { width: 1280, height: 800 } });

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'send logic does not depend on the viewport; desktop-chrome only');
  }
});

const KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';
const PROJ = '/home/user/workspace/myproject';
const SEND = '**/api/sessions/send';

/** @type {Awaited<ReturnType<typeof startMockServer>>} */
let mock;
test.beforeAll(async () => { mock = await startMockServer(); });
test.afterAll(async () => { await new Promise((r) => mock.server.close(r)); });

/** Opens KEY with text in the composer and, when withFile, one uploaded chip. */
async function compose(page, text, { withFile = false } = {}) {
  await page.goto(mock.url + '/dashboard');
  await page.click(`.session-card[data-key="${KEY}"]`);
  await page.waitForSelector('#msg-input');
  await page.evaluate(({ text, withFile }) => {
    const t = window.nz.test;
    t.setMsgValue(document.getElementById('msg-input'), text);
    if (withFile) {
      t.pendingFiles.push({
        id: 'file-1', kind: 'image', status: 'ready', normalizedSize: 16,
        file: new File([new Uint8Array(16)], 'photo.png', { type: 'image/png' }),
        blobUrl: 'data:image/png;base64,iVBORw0KGgo=',
      });
      t.renderFilePreviews();
    }
  }, { text, withFile });
}

const send = (page) => page.evaluate(() => window.nz.test.sendMessage());
const inputText = (page) => page.$eval('#msg-input', (el) => el.innerText.trim());
const fileCount = (page) => page.evaluate(() => window.nz.test.pendingFiles.length);
const toast = (page) => page.$eval('#toast', (el) => el.textContent);
const display = (page, id) => page.evaluate((i) => document.getElementById(i).style.display, id);

/** The optimistic flip was undone: send is back, stop is gone. */
async function expectIdle(page) {
  expect(await display(page, 'btn-send')).toBe('flex');
  expect(await display(page, 'btn-stop')).toBe('none');
}

function reply(page, status, body) {
  const bodies = [];
  return page.route(SEND, async (route) => {
    bodies.push(JSON.parse(route.request().postData() || '{}'));
    await route.fulfill({ status, contentType: 'application/json', body: typeof body === 'string' ? body : JSON.stringify(body) })
      .catch(() => {});
  }).then(() => bodies);
}

// holdSend answers the send only once release() is called, so a test can act
// while the request is in flight. The composer stays editable then, and a
// rejected send must put the text it sent back whatever the box holds by the
// time the answer arrives.
async function holdSend(page, answer) {
  let release, arrived;
  const go = new Promise((r) => { release = r; });
  const seen = new Promise((r) => { arrived = r; });
  await page.route(SEND, async (route) => { arrived(); await go; await answer(route); });
  return { seen, release };
}

/** Sends, empties the composer while the request is in flight, then lets the answer through. */
async function sendClearingMidFlight(page, held) {
  const sent = send(page);
  await held.seen;
  await page.evaluate(() => { window.nz.test.setMsgValue(document.getElementById('msg-input'), ''); });
  held.release();
  await sent;
}

test('HTTP 4xx with files_consumed restores the text and drops the dead chips', async ({ page }) => {
  const bodies = [];
  const held = await holdSend(page, async (route) => {
    bodies.push(JSON.parse(route.request().postData() || '{}'));
    await route.fulfill({ status: 400, contentType: 'application/json', body: JSON.stringify({ error: 'image decode failed', files_consumed: true }) });
  });
  await compose(page, 'keep me', { withFile: true });
  await sendClearingMidFlight(page, held);
  expect(bodies.map((b) => b.file_ids)).toEqual([['file-1']]);
  expect(await inputText(page)).toBe('keep me');
  expect(await fileCount(page)).toBe(0);
  expect(await toast(page)).toContain('image decode failed');
  await expectIdle(page);
});

test('HTTP 4xx without files_consumed keeps the text and the chips', async ({ page }) => {
  await reply(page, 400, { error: 'workspace rejected' });
  await compose(page, 'keep me too', { withFile: true });
  await send(page);
  expect(await inputText(page)).toBe('keep me too');
  expect(await fileCount(page)).toBe(1);
  expect(await toast(page)).toContain('发送消息失败');
  expect(await toast(page)).toContain('workspace rejected');
  await expectIdle(page);
});

test('HTTP 429 shows the limiter the server named, or a generic retry hint', async ({ page }) => {
  await reply(page, 429, { error: 'send rate limit exceeded' });
  await compose(page, 'slow down');
  await send(page);
  expect(await inputText(page)).toBe('slow down');
  expect(await toast(page)).toBe('send rate limit exceeded');
  await expectIdle(page);

  await page.unroute(SEND);
  await reply(page, 429, '');
  await send(page);
  expect(await inputText(page)).toBe('slow down');
  expect(await toast(page)).toBe('请求过于频繁，请稍后重试');
});

test('HTTP 401 restores the text and asks for the token', async ({ page }) => {
  await reply(page, 401, { error: 'unauthorized' });
  await compose(page, 'who am i');
  await send(page);
  expect(await inputText(page)).toBe('who am i');
  await expect(page.locator('.modal-overlay [aria-label="Dashboard API token"]')).toBeVisible();
  await expectIdle(page);
});

test('a network failure restores the text and rolls the running flip back', async ({ page }) => {
  const held = await holdSend(page, (route) => route.abort('failed'));
  await compose(page, 'offline');
  await sendClearingMidFlight(page, held);
  expect(await inputText(page)).toBe('offline');
  expect(await toast(page)).toContain('发送消息失败：网络错误');
  await expectIdle(page);
});

test('an accepted send clears the composer and keeps the running flip; a reset ack rolls it back', async ({ page }) => {
  await reply(page, 200, { status: 'accepted' });
  await compose(page, 'go', { withFile: true });
  await send(page);
  expect(await inputText(page)).toBe('');
  expect(await fileCount(page)).toBe(0);
  expect(await display(page, 'btn-stop')).toBe('flex');
  expect(await display(page, 'btn-send')).toBe('none');

  await page.unroute(SEND);
  await page.goto('about:blank');
  await reply(page, 200, { status: 'reset' });
  await compose(page, '/clear');
  await send(page);
  expect(await inputText(page)).toBe('');
  await expectIdle(page);
});

test('the HTTP path keeps a pending session\'s durable entry until a send is accepted', async ({ page }) => {
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  const blob = () => page.evaluate(() => JSON.parse(localStorage.getItem('nz:pending_sessions') || '{}'));
  const key = await page.evaluate((proj) => {
    window.nz.test.doCreateInProject(proj, 'myproject', 'local', undefined, 'general', { mode: 'new' });
    return window.nz.test.selectedKey;
  }, PROJ);
  await page.waitForSelector('#msg-input');
  expect((await blob())[key]).toEqual({ ws: PROJ });

  // Rejected: the durable entry stays, so a reload still restores it.
  let bodies = await reply(page, 500, { error: 'spawn failed' });
  await page.evaluate(() => { window.nz.test.setMsgValue(document.getElementById('msg-input'), 'first try'); });
  await send(page);
  expect(bodies[0].workspace).toBe(PROJ);
  expect((await blob())[key]).toEqual({ ws: PROJ });

  await page.unroute(SEND);
  await page.reload();
  await page.waitForSelector(`.session-card[data-key="${key}"]`);
  await page.click(`.session-card[data-key="${key}"]`);
  await page.waitForSelector('#msg-input');
  bodies = await reply(page, 200, { status: 'accepted' });
  await page.evaluate(() => { window.nz.test.setMsgValue(document.getElementById('msg-input'), 'second try'); });
  await send(page);
  expect(bodies[0].workspace).toBe(PROJ);
  expect(await blob()).not.toHaveProperty(key);
  expect(await page.evaluate((k) => window.nz.test.sessionWorkspaces[k], key)).toBeUndefined();
});

test.describe('over the WebSocket', () => {
  /** @type {Awaited<ReturnType<typeof startMockServer>>} */
  let wsMock;
  test.beforeAll(async () => { wsMock = await startMockServer({ ws: true }); });
  test.afterAll(async () => { await new Promise((r) => wsMock.server.close(r)); });

  test('a text send goes out as a frame, clears the composer and consumes the pending workspace', async ({ page }) => {
    await page.goto(wsMock.url + '/dashboard');
    await page.waitForFunction(() => window.nz.test.wsm.state === window.nz.test.WS_STATES.CONNECTED);
    await page.waitForSelector('.session-card');
    const blob = () => page.evaluate(() => JSON.parse(localStorage.getItem('nz:pending_sessions') || '{}'));
    const key = await page.evaluate((proj) => {
      window.nz.test.doCreateInProject(proj, 'myproject', 'local', undefined, 'general', { mode: 'new' });
      return window.nz.test.selectedKey;
    }, PROJ);
    await page.waitForSelector('#msg-input');
    expect((await blob())[key]).toEqual({ ws: PROJ });
    const httpSends = wsMock.sendCalls.length;

    await page.evaluate(() => { window.nz.test.setMsgValue(document.getElementById('msg-input'), 'over ws'); });
    await send(page);

    const conn = wsMock.wsConnections[wsMock.wsConnections.length - 1];
    const frames = conn.messages.filter((m) => m.type === 'send');
    expect(frames).toHaveLength(1);
    expect(frames[0]).toMatchObject({ key, text: 'over ws', workspace: PROJ });
    expect(wsMock.sendCalls.length).toBe(httpSends);
    expect(await inputText(page)).toBe('');
    await expect(page.locator('#events-scroll .optimistic-msg')).toHaveCount(1);
    expect(await blob()).not.toHaveProperty(key);
    expect(await page.evaluate((k) => window.nz.test.sessionWorkspaces[k], key)).toBeUndefined();
    expect(await display(page, 'btn-stop')).toBe('flex');
  });
});
