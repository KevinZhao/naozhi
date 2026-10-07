// @ts-check
// A queued send's failure arrives late: the merged turn that carried it ran
// after the send was acked. Since #3004 D the server reports that failure
// BEFORE the turn's settled session_state (dashboard origins are finished
// ahead of the post-turn broadcast), and the dashboard JS is unchanged. These
// pin what the client does with frames in that order:
//  (a) WS: the queued send's error ack, then ready — a toast, only that
//      send's bubble goes, the owner's stays, the running banner settles;
//  (b) a send made after ready keeps its optimistic running flip and its
//      lastSent, because no error for an earlier send can follow ready;
//  (c) HTTP: the key's send_error, then ready — the tab that sent shows it,
//      a tab watching the same key ignores it.
// The contrast cases replay the order the server used before D (ready first)
// and show why the order is load-bearing: the late frame then rolls back the
// newer send (WS), or is dropped by the originator's gates (HTTP).
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const desktop = { viewport: { width: 1280, height: 800 } };
const KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';
const SEND = '**/api/sessions/send';
// An attachment chip's thumbnail must decode, or the global error handler's
// toast races the one under test.
const PIXEL = 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=';

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') testInfo.skip(true, 'desktop-chrome only');
});

let mock;
test.beforeAll(async () => { mock = await startMockServer({ ws: true }); });
test.afterAll(() => mock.server.close());
// Each test starts from the listing a fresh mock has; state() moves it.
test.beforeEach(() => mock.setSessionStateWithoutVersionBump(KEY, 'ready'));

const subs = (conn) => conn.messages.filter((m) => m.type === 'subscribe' && m.key === KEY);
const sends = (conn) => conn.messages.filter((m) => m.type === 'send');

// open selects KEY in a fresh tab over its own socket and acks the subscribe.
async function open(browser) {
  const ctx = await browser.newContext({ ...desktop });
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  await page.goto(mock.url + '/dashboard');
  await waitForWs(page);
  const conn = mock.wsConnections[mock.wsConnections.length - 1];
  await page.click(`.session-card[data-key="${KEY}"]`);
  await expect.poll(() => subs(conn).length).toBe(1);
  conn.send({ type: 'subscribed', key: KEY });
  await page.waitForFunction((key) => sessionStream.subscribedKey === key, KEY);
  return { ctx, page, conn, errors };
}

// sendWS types text and sends it over the socket; returns the frame's id.
async function sendWS(page, conn, text) {
  const before = sends(conn).length;
  await page.evaluate((t) => { setMsgValue(document.getElementById('msg-input'), t); sendMessage(); }, text);
  await expect.poll(() => sends(conn).length).toBe(before + 1);
  return sends(conn)[before].id;
}

// state pushes a process state and makes the mock's sessions listing agree,
// so a sessions poll sent after the push, whenever and by whatever (the
// socket's connect, a discovered-set change), reports it rather than rolling
// it back.
const state = (conn, s) => {
  mock.setSessionStateWithoutVersionBump(KEY, s);
  conn.send({ type: 'session_state', key: KEY, node: 'local', state: s });
};
const errorAck = (conn, id) => conn.send({ type: 'send_ack', id, status: 'error', key: KEY, error: 'boom' });
const stateIs = (page, s) => page.waitForFunction(([k, want]) => sessionsData[sid(k, 'local')].state === want, [KEY, s]);
const composerText = (page) => page.$eval('#msg-input', (el) => el.innerText.trim());

// refilled returns what interrupting puts back in the empty composer:
// interruptSession re-fills it from lastSent, the send this tab made last.
async function refilled(page) {
  await page.evaluate(() => interruptSession());
  return composerText(page);
}

const bubble = (page, id) => page.locator(`#events-scroll .optimistic-msg[data-send-id="${id}"]`);

// recordToasts starts collecting every text #toast shows, afresh.
const recordToasts = (page) => page.evaluate(() => {
  const w = /** @type {any} */ (window);
  w.__toasts = [];
  if (w.__toastObserver) return;
  const el = document.getElementById('toast');
  w.__toastObserver = new MutationObserver(() => w.__toasts.push(el.textContent));
  w.__toastObserver.observe(el, { childList: true, characterData: true, subtree: true });
});
const toastsSeen = (page) => page.evaluate(() => /** @type {any} */ (window).__toasts);

// barrier: every frame sent before it has been handled once the page answers
// the interrupt_ack toast it triggers.
async function barrier(page, conn) {
  conn.send({ type: 'interrupt_ack', status: 'not_running' });
  await expect(page.locator('#toast')).toContainText('会话未在运行');
}

// runningWithQueued leaves the tab with an accepted owner send and a queued
// one behind it, the session running. Returns both send ids.
async function runningWithQueued(page, conn) {
  const owner = await sendWS(page, conn, 'owner msg');
  conn.send({ type: 'send_ack', id: owner, status: 'accepted', key: KEY });
  state(conn, 'running');
  await stateIs(page, 'running');
  const queued = await sendWS(page, conn, 'queued msg');
  conn.send({ type: 'send_ack', id: queued, status: 'queued', key: KEY });
  await expect(bubble(page, queued).locator('.msg-queued-chip')).toHaveCount(1);
  return { owner, queued };
}

test.describe('a queued send failing in a later merged turn', () => {
  test('(a) WS: the error ack before ready drops only that send\'s bubble and the banner settles', async ({ browser }) => {
    const { ctx, page, conn, errors } = await open(browser);
    const { owner, queued } = await runningWithQueued(page, conn);

    errorAck(conn, queued); // the merged turn failed: its origin is told first
    state(conn, 'ready'); // then the post-turn broadcast
    await expect(page.locator('#toast')).toHaveClass(/\berror\b/);
    await expect(page.locator('#toast')).toContainText('boom');
    await expect(bubble(page, queued), 'the failed queued send\'s bubble is removed').toHaveCount(0);
    await expect(bubble(page, owner), 'the owner send\'s bubble stays').toHaveCount(1);
    await stateIs(page, 'ready');
    await expect(page.locator('#btn-send'), 'the running banner is not stuck').toBeVisible();
    await expect(page.locator('#btn-stop')).toBeHidden();
    expect(errors).toEqual([]);
    await ctx.close();
  });

  test('(b) a send after ready keeps its running flip and lastSent', async ({ browser }) => {
    const { ctx, page, conn, errors } = await open(browser);
    const { queued } = await runningWithQueued(page, conn);
    errorAck(conn, queued);
    state(conn, 'ready');
    await stateIs(page, 'ready');

    await sendWS(page, conn, 'next msg');
    await barrier(page, conn);
    await stateIs(page, 'running'); // the new send's optimistic flip
    await expect(page.locator('#btn-stop')).toBeVisible();
    expect(await refilled(page), 'lastSent still holds the new send').toBe('next msg');
    expect(errors).toEqual([]);
    await ctx.close();
  });

  test('(b) contrast: an error ack for the earlier send after the new one rolls the new one back', async ({ browser }) => {
    const { ctx, page, conn, errors } = await open(browser);
    const { queued } = await runningWithQueued(page, conn);
    state(conn, 'ready'); // the pre-D order: ready first
    await stateIs(page, 'ready');
    await sendWS(page, conn, 'next msg');
    await stateIs(page, 'running');
    errorAck(conn, queued);
    await stateIs(page, 'ready');
    await expect(page.locator('#btn-send'), 'the new send\'s running flip is undone').toBeVisible();
    state(conn, 'running');
    await stateIs(page, 'running');
    expect(await refilled(page), 'the new send\'s lastSent is gone').toBe('');
    expect(errors).toEqual([]);
    await ctx.close();
  });

  test('(c) HTTP: the send_error before ready reaches the tab that sent, not the one watching', async ({ browser }) => {
    const sender = await open(browser);
    const watcher = await open(browser);
    await sender.page.route(SEND, (route) => route.fulfill({ status: 202, contentType: 'application/json',
      body: JSON.stringify({ status: 'queued', key: KEY }) }));
    // A send carrying files always takes HTTP; it queues behind the running turn.
    const httpQueued = async () => {
      for (const t of [sender, watcher]) {
        state(t.conn, 'running');
        await stateIs(t.page, 'running');
        await recordToasts(t.page);
      }
      const answered = sender.page.waitForResponse(SEND);
      await sender.page.evaluate((pixel) => {
        const tt = window.nz.test;
        tt.setMsgValue(document.getElementById('msg-input'), 'with a photo');
        tt.pendingFiles.push({ id: 'file-1', kind: 'image', status: 'ready', normalizedSize: 16,
          file: new File([new Uint8Array(16)], 'photo.png', { type: 'image/png' }), blobUrl: pixel });
        tt.renderFilePreviews();
        tt.sendMessage();
      }, PIXEL);
      await answered;
      await expect.poll(() => composerText(sender.page), { message: 'the accepted send cleared the composer' }).toBe('');
    };
    // deliver pushes frames to both tabs and returns each tab's toasts.
    const deliver = async (frames) => {
      const out = [];
      for (const t of [sender, watcher]) {
        frames(t.conn);
        await stateIs(t.page, 'ready');
        await barrier(t.page, t.conn);
        out.push((await toastsSeen(t.page)).join('|'));
      }
      return out;
    };

    await httpQueued();
    let [sent, watched] = await deliver((c) => { c.send({ type: 'send_error', key: KEY, error: 'boom' }); state(c, 'ready'); });
    expect(sent, 'the sending tab shows the failure').toContain('boom');
    expect(watched, 'a tab that sent nothing ignores it').not.toContain('boom');

    // Contrast, the pre-D order: ready first clears the sender's gates, and
    // the send_error after it is dropped there too.
    await httpQueued();
    [sent, watched] = await deliver((c) => { state(c, 'ready'); c.send({ type: 'send_error', key: KEY, error: 'boom' }); });
    expect(sent, 'after ready the sending tab drops the failure').not.toContain('boom');
    expect(watched).not.toContain('boom');
    expect(sender.errors).toEqual([]);
    expect(watcher.errors).toEqual([]);
    await sender.ctx.close();
    await watcher.ctx.close();
  });
});
