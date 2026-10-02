// @ts-check
// sendComposerTurn's branches, pinned in a browser so the function can be
// split without a shape test (see #3025): the WS path, the HTTP path's
// rejections (4xx with and without files_consumed, 429, 401), the `reset`
// ack, and a network failure. What each branch owes the operator is the
// composer text (put back on failure, even when the box changed while the
// request was in flight; cleared on success),
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
// A chip's thumbnail must decode: a broken <img> fires a capture-phase error
// event, and the global handler's toast would race the one under test.
const PIXEL = 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=';

/** @type {Awaited<ReturnType<typeof startMockServer>>} */
let mock;
test.beforeAll(async () => { mock = await startMockServer(); });
test.afterAll(async () => { await new Promise((r) => mock.server.close(r)); });

/**
 * Opens KEY with text in the composer and, when withFile (or file, whose
 * fields override the ready chip's), one uploaded chip; each of more adds
 * another chip the same way.
 */
async function compose(page, text, { withFile = false, file = null, more = [], url = mock.url } = {}) {
  await page.goto(url + '/dashboard');
  await page.click(`.session-card[data-key="${KEY}"]`);
  await page.waitForSelector('#msg-input');
  const chips = [...(file || withFile ? [file || {}] : []), ...more];
  await page.evaluate(({ text, chips, pixel }) => {
    const t = window.nz.test;
    t.setMsgValue(document.getElementById('msg-input'), text);
    chips.forEach((chip, i) => {
      t.pendingFiles.push({
        id: 'file-' + (i + 1), kind: 'image', status: 'ready', normalizedSize: 16,
        file: new File([new Uint8Array(16)], 'photo.png', { type: 'image/png' }),
        blobUrl: pixel, ...chip,
      });
    });
    if (chips.length) t.renderFilePreviews();
  }, { text, chips, pixel: PIXEL });
}

const send = (page) => page.evaluate(() => window.nz.test.sendMessage());
const inputText = (page) => page.$eval('#msg-input', (el) => el.innerText.trim());
const fileCount = (page) => page.evaluate(() => window.nz.test.pendingFiles.length);
const toast = (page) => page.$eval('#toast', (el) => el.textContent);
const display = (page, id) => page.evaluate((i) => document.getElementById(i).style.display, id);

/**
 * Records the delay of every setInterval/setTimeout armed from here on, and
 * keeps each timeout's callback so fireTimeout can run it early.
 */
const spyTimers = (page) => page.evaluate(() => {
  const w = /** @type {any} */ (window);
  w.__armed = { interval: [], timeout: [] };
  w.__timeoutFns = [];
  for (const [name, list] of [['setInterval', 'interval'], ['setTimeout', 'timeout']]) {
    const orig = w[name];
    w[name] = (fn, ms, ...rest) => {
      w.__armed[list].push(ms);
      if (list === 'timeout') w.__timeoutFns.push([ms, fn]);
      return orig(fn, ms, ...rest);
    };
  }
});
const armedTimers = (page) => page.evaluate(() => /** @type {any} */ (window).__armed);
/** Runs the callback of the latest timeout armed with delay ms, now. */
const fireTimeout = (page, ms) => page.evaluate((d) => {
  const fns = /** @type {any} */ (window).__timeoutFns.filter(([m]) => m === d);
  fns[fns.length - 1][1]();
}, ms);

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

// composerSendBlocked's own two checks (#3025 S19-7): a failed upload and
// the 9 MB batch cap. Both must stop the send before it ever reaches the
// network — asserted here via the absence of a POST body, not just the
// toast — and leave the chip in place for the user to remove or retry.
test('a failed upload blocks the send and keeps the chip for retry', async ({ page }) => {
  await compose(page, 'has a bad upload', { file: { id: '', status: 'error', error: 'image decode failed' } });
  const bodies = await reply(page, 200, { status: 'accepted' });
  await send(page);
  expect(bodies).toHaveLength(0);
  expect(await inputText(page)).toBe('has a bad upload');
  expect(await fileCount(page)).toBe(1);
  expect(await toast(page)).toContain('图片上传失败（image decode failed）');
  await expectIdle(page);
});

test('a batch over the 9 MB cap blocks the send without touching the server', async ({ page }) => {
  await compose(page, 'too much', { file: { normalizedSize: 10 * 1024 * 1024 } });
  const bodies = await reply(page, 200, { status: 'accepted' });
  await send(page);
  expect(bodies).toHaveLength(0);
  expect(await inputText(page)).toBe('too much');
  expect(await fileCount(page)).toBe(1);
  expect(await toast(page)).toContain('图片总大小 10 MB 超过 9 MB 上限');
  await expectIdle(page);
});

// armFallbackEventPoll: with the socket down nothing pushes the turn's
// events, so an accepted HTTP send polls them every 500 ms and drops back to
// the 1 s cadence after 15 s.
test('with the socket down, an accepted send speeds up event polling for 15 s', async ({ page }) => {
  await reply(page, 200, { status: 'accepted' });
  await compose(page, 'poll me');
  await spyTimers(page);
  await send(page);
  const armed = await armedTimers(page);
  expect(armed.interval).toContain(500);
  expect(armed.timeout).toContain(15000);
  // The WS-down fallback keeps its legacy no-bubble behaviour.
  await expect(page.locator('#events-scroll .optimistic-msg')).toHaveCount(0);

  // After the 15 s window the poll drops back to 1 s, not to 500 ms again.
  const before = armed.interval.length;
  await fireTimeout(page, 15000);
  const rearmed = (await armedTimers(page)).interval.slice(before);
  expect(rearmed).toContain(1000);
  expect(rearmed).not.toContain(500);
});

// finishHttpSend records the sent text before reading the ack: it is what
// an interrupt puts back into an empty composer.
test('an accepted HTTP send leaves its text for the interrupt re-fill', async ({ page }) => {
  await reply(page, 200, { status: 'accepted' });
  await page.route('**/api/sessions/interrupt', (route) => route.fulfill({ status: 200, contentType: 'application/json', body: '{}' }));
  await compose(page, 'refill me');
  await send(page);
  expect(await inputText(page)).toBe('');
  await page.evaluate(() => window.nz.test.interruptSession());
  expect(await inputText(page)).toBe('refill me');
});

// The 9 MB cap is on base64-inflated image bytes only: PDFs and file_ref
// attachments travel by reference, so their size must not block the send.
test('PDF and file_ref attachments do not count toward the 9 MB image cap', async ({ page }) => {
  const big = 10 * 1024 * 1024;
  await compose(page, 'mixed batch', {
    withFile: true,
    more: [{ kind: 'pdf', normalizedSize: big }, { serverKind: 'file_ref', normalizedSize: big }],
  });
  const bodies = await reply(page, 200, { status: 'accepted' });
  await send(page);
  expect(bodies.map((b) => b.file_ids)).toEqual([['file-1', 'file-2', 'file-3']]);
  expect(await inputText(page)).toBe('');
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

  // trySendViaWS declines file-bearing sends even on a live socket: the WS
  // upload owner is frozen at upgrade time and can diverge from the cookie
  // the upload used, so attachments always ride the HTTP POST.
  test('a send carrying files takes HTTP even while the socket is up', async ({ page }) => {
    await compose(page, 'with a photo', { withFile: true, url: wsMock.url });
    await page.waitForFunction(() => window.nz.test.wsm.state === window.nz.test.WS_STATES.CONNECTED);
    const bodies = await reply(page, 200, { status: 'accepted' });
    await spyTimers(page);
    await send(page);

    const conn = wsMock.wsConnections[wsMock.wsConnections.length - 1];
    expect(conn.messages.filter((m) => m.type === 'send')).toHaveLength(0);
    expect(bodies).toEqual([expect.objectContaining({ key: KEY, text: 'with a photo', file_ids: ['file-1'] })]);
    expect(await inputText(page)).toBe('');
    expect(await fileCount(page)).toBe(0);
    // With the socket live the HTTP path mirrors the WS path's optimistic bubble.
    await expect(page.locator('#events-scroll .optimistic-msg')).toHaveCount(1);
    // The live socket pushes the turn's events: no fallback poll.
    expect((await armedTimers(page)).interval).not.toContain(500);
  });

  // A send_error frame reaches every tab watching the key; only a tab with
  // an HTTP send still in flight (httpSendPending) or a turn's text on record
  // (lastSent) acts on it. An accepted send keeps both; a rejected send and
  // a reset ack must drop them, or a later frame toasts about a turn that
  // never ran.
  test('a late send_error is honoured after an accepted HTTP send, ignored after a rejected or reset one', async ({ page }) => {
    /** Pushes a send_error for KEY, then a barrier frame; returns every toast shown meanwhile. */
    const lateSendError = async () => {
      await page.evaluate(() => {
        const w = /** @type {any} */ (window);
        const el = document.getElementById('toast');
        w.__toasts = [];
        new MutationObserver(() => w.__toasts.push(el.textContent)).observe(el, { childList: true, characterData: true, subtree: true });
      });
      const conn = wsMock.wsConnections[wsMock.wsConnections.length - 1];
      conn.send({ type: 'send_error', key: KEY, error: 'late failure' });
      conn.send({ type: 'interrupt_ack', status: 'not_running' });
      await page.waitForFunction(() => /** @type {any} */ (window).__toasts.includes('会话未在运行，无需中断'));
      return page.evaluate(() => /** @type {any} */ (window).__toasts);
    };
    const opened = (text) => compose(page, text, { withFile: true, url: wsMock.url })
      .then(() => page.waitForFunction(() => window.nz.test.wsm.state === window.nz.test.WS_STATES.CONNECTED));

    await opened('accepted');
    await reply(page, 200, { status: 'accepted' });
    await send(page);
    expect((await lateSendError()).join('|')).toContain('late failure');
    await expectIdle(page);

    for (const [text, status, body] of [['rejected', 400, { error: 'workspace rejected' }], ['/clear', 200, { status: 'reset' }]]) {
      await page.unroute(SEND);
      await page.goto('about:blank');
      await opened(text);
      await reply(page, status, body);
      await send(page);
      expect((await lateSendError()).join('|')).not.toContain('late failure');
    }
  });
});
