// @ts-check
// sessionFrames — the history / event / send_ack / session_state handlers —
// keep the session subscription's bookkeeping on sessionStream, driven through a real
// socket:
//  - a reconnect resumes after the last event a history or an event frame
//    delivered (the cursor both frames advance);
//  - one subscribe consumes one opening frame: a second frame flagged initial
//    appends instead of repainting;
//  - a send_ack for another session subscribes it from an initial page (the
//    cursor is reset, not carried over);
//  - a running push for the session already subscribed does not resubscribe;
//    a dead→running push resubscribes from an initial page, and so does a
//    running push after a suspended subscribe, a running push after a
//    subscription_timeout dropped the subscription, and the running push for a
//    dead session this tab sent to (the optimistic flip already wrote running);
//  - a backfill history frame carrying a user event locks the question card
//    already on screen;
//  - a turn ending on a session off screen raises its card's unread chip;
//  - an opening frame anchors the turn timer at its last turn's first event;
//    a background task reporting while the session is idle anchors no timer,
//    on either frame path, so the next send's timer starts at the send;
//  - a result on either frame path ends the turn this tab started (running
//    flips to ready, the cost lands on total_cost), and a time-less user replay
//    on either path is not painted twice;
//  - a send_error for the session on screen undoes the send this tab made
//    (toast, optimistic bubble, running flip); for a session sent to and then
//    left it only rolls the running flip back; a tab that sent nothing ignores
//    it.
//  - a session_state carrying a reason refetches the session list past the
//    version cache (a process state change does not bump the store version).
// A bookkeeping write that lands on sessionFrames instead of sessionStream leaves its
// fields stale without throwing, so each case asserts a frame the mock saw.
const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const desktop = { viewport: { width: 1280, height: 800 } };
const KEY_A = 'dashboard:direct:2026-01-01-120000-1:myproject';
const KEY_B = 'dashboard:direct:2026-01-01-120001-2:otherproject';

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') testInfo.skip(true, 'desktop-chrome only');
});

const subs = (conn, key) => conn.messages.filter((m) => m.type === 'subscribe' && m.key === key);
const ev = (text, time) => ({ type: 'text', detail: text, summary: text, time, uuid: 'u-' + text });

// open selects KEY_A over a connected socket and acks its subscribe; ack
// carries extra fields for the subscribed frame (e.g. a reason).
async function open(browser, mock, ack = {}) {
  const ctx = await browser.newContext({ ...desktop });
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  page.on('console', (m) => { if (m.text().includes('ws parse error')) errors.push(m.text()); });
  await page.goto(mock.url + '/dashboard');
  await waitForWs(page);
  await page.click(`.session-card[data-key="${KEY_A}"]`);
  const conn = mock.wsConnections[mock.wsConnections.length - 1];
  await expect.poll(() => subs(conn, KEY_A).length).toBe(1);
  conn.send({ type: 'subscribed', key: KEY_A, ...ack });
  await page.waitForFunction((key) => sessionStream.subscribedKey === key, KEY_A);
  // Settle the connect-time sessions refetch (debounced 300 ms): landing later,
  // its snapshot would overwrite the state a test's frames just wrote.
  await page.evaluate(() => debouncedFetchSessions());
  return { ctx, page, conn, errors };
}

// shown waits until the frame carrying uuid u-<text> has been painted.
const shown = (page, text) => page.waitForSelector(`#events-scroll .event[data-uuid="u-${text}"]`);

// sendText sends text on the selected session over the socket and waits for
// the send frame; the mock never acks it, so the optimistic state stays up.
async function sendText(page, conn, text) {
  await page.evaluate((t) => { setMsgValue(document.getElementById('msg-input'), t); sendMessage(); }, text);
  await expect.poll(() => conn.messages.filter((m) => m.type === 'send' && m.text === text).length).toBe(1);
}

const running = (page, key) => page.evaluate((k) => sessionsData[sid(k, 'local')].state, key);

// barrier: frames the page sent while handling earlier frames are on the
// socket ahead of a ping sent after them.
async function barrier(page, conn) {
  const n = conn.messages.filter((m) => m.type === 'ping').length;
  await page.evaluate(() => wsm.send({ type: 'ping' }));
  await expect.poll(() => conn.messages.filter((m) => m.type === 'ping').length).toBe(n + 1);
}

test.describe('sessionFrames keep the bookkeeping on sessionStream', () => {
  let mock;
  test.beforeAll(async () => { mock = await startMockServer({ ws: true }); });
  test.afterAll(() => mock.server.close());

  test('a reconnect resumes after the last event a history or an event frame delivered', async ({ browser }) => {
    const { ctx, page, conn: first, errors } = await open(browser, mock);
    let conn = first;
    const T = Date.now() + 60000; // newer than anything the REST page painted
    const reconnect = async () => {
      const before = mock.wsConnections.length;
      conn.close();
      await expect.poll(() => mock.wsConnections.length, { timeout: 5000 }).toBe(before + 1);
      conn = mock.wsConnections[before];
      await expect.poll(() => subs(conn, KEY_A).length, { message: 'reconnect must re-subscribe' }).toBe(1);
      conn.send({ type: 'subscribed', key: KEY_A });
      return subs(conn, KEY_A)[0];
    };

    conn.send({ type: 'history', key: KEY_A, events: [ev('h1', T)] });
    await shown(page, 'h1');
    expect((await reconnect()).after, 'a history frame advances the resume cursor').toBe(T);

    conn.send({ type: 'event', key: KEY_A, event: ev('e1', T + 1) });
    await shown(page, 'e1');
    expect((await reconnect()).after, 'an event frame advances the resume cursor').toBe(T + 1);
    expect(errors).toEqual([]);
    await ctx.close();
  });

  test('one subscribe consumes one opening frame: a second initial frame appends', async ({ browser }) => {
    const { ctx, page, conn, errors } = await open(browser, mock);
    const T = Date.now() + 60000;
    conn.send({ type: 'history', key: KEY_A, initial: true, events: [ev('i1', T)] });
    await shown(page, 'i1');
    conn.send({ type: 'history', key: KEY_A, initial: true, events: [ev('i2', T + 1)] });
    await shown(page, 'i2');
    await expect(page.locator('#events-scroll .event[data-uuid="u-i1"]'),
      'the second frame was appended, not painted over the first').toHaveCount(1);
    expect(errors).toEqual([]);
    await ctx.close();
  });

  test('a send_ack for another session subscribes it from an initial page', async ({ browser }) => {
    const { ctx, page, conn, errors } = await open(browser, mock);
    conn.send({ type: 'event', key: KEY_A, event: ev('a1', Date.now() + 60000) });
    await shown(page, 'a1');
    conn.send({ type: 'send_ack', id: 'r1', status: 'accepted', key: KEY_B });
    await expect.poll(() => subs(conn, KEY_B).length).toBe(1);
    const sub = subs(conn, KEY_B)[0];
    expect(sub.after, 'the cursor of the session left behind is not carried over').toBeUndefined();
    expect(sub.limit).toBeGreaterThan(0);
    expect(errors).toEqual([]);
    await ctx.close();
  });

  test('a running push for the subscribed session does not resubscribe it', async ({ browser }) => {
    const { ctx, page, conn, errors } = await open(browser, mock);
    conn.send({ type: 'session_state', key: KEY_A, node: 'local', state: 'running' });
    await page.waitForFunction((key) => sessionsData[sid(key, 'local')].state === 'running', KEY_A);
    await barrier(page, conn);
    expect(subs(conn, KEY_A).length, 'an already-live subscription is left alone').toBe(1);
    expect(errors).toEqual([]);
    await ctx.close();
  });

  // reSub asserts that the running push sent a second subscribe for KEY_A,
  // asking for an initial page rather than resuming after the cursor.
  async function reSub(conn) {
    await expect.poll(() => subs(conn, KEY_A).length, { message: 'running must resubscribe' }).toBe(2);
    const sub = subs(conn, KEY_A)[1];
    expect(sub.after, 'the resubscribe asks for an initial page').toBeUndefined();
    expect(sub.limit).toBeGreaterThan(0);
  }

  test('a dead→running push resubscribes from an initial page', async ({ browser }) => {
    const { ctx, page, conn, errors } = await open(browser, mock);
    conn.send({ type: 'event', key: KEY_A, event: ev('d1', Date.now() + 60000) });
    await shown(page, 'd1');
    // Back to back, so no sessions poll lands between the two pushes.
    conn.send({ type: 'session_state', key: KEY_A, node: 'local', state: 'dead' });
    conn.send({ type: 'session_state', key: KEY_A, node: 'local', state: 'running' });
    await reSub(conn);
    expect(errors).toEqual([]);
    await ctx.close();
  });

  test('a running push after a suspended subscribe resubscribes', async ({ browser }) => {
    const { ctx, page, conn, errors } = await open(browser, mock, { reason: 'suspended' });
    await page.waitForFunction(() => sessionStream._subscriptionSuspended === true);
    conn.send({ type: 'session_state', key: KEY_A, node: 'local', state: 'running' });
    await reSub(conn);
    expect(errors).toEqual([]);
    await ctx.close();
  });

  test('a subscription_timeout drops the subscription, so the next running push resubscribes', async ({ browser }) => {
    const { ctx, page, conn, errors } = await open(browser, mock);
    // Back to back, so no sessions poll lands between the two pushes.
    conn.send({ type: 'session_state', key: KEY_A, node: 'local', state: 'ready', reason: 'subscription_timeout' });
    conn.send({ type: 'session_state', key: KEY_A, node: 'local', state: 'running' });
    await reSub(conn);
    expect(errors).toEqual([]);
    await ctx.close();
  });

  test('a backfill history frame carrying a user event locks the question card already on screen', async ({ browser }) => {
    const { ctx, page, conn, errors } = await open(browser, mock);
    const T = Date.now() + 60000;
    conn.send({ type: 'event', key: KEY_A, event: {
      type: 'ask_question', time: T, uuid: 'ask-1',
      ask_question: { tool_use_id: 'tu-1', items: [{ header: 'Color', question: 'Pick one', options: [{ label: 'Red' }, { label: 'Blue' }] }] },
    } });
    const card = page.locator('#events-scroll .event.ask_question[data-tool-use-id="tu-1"]');
    await expect(card.locator('.ask-opt').first()).toBeEnabled();
    // Only the user event: the frame holds no ask→user pair for the answered-set
    // hydration to find, so the lock has to come from the user event landing.
    conn.send({ type: 'history', key: KEY_A, events: [{ type: 'user', detail: 'answered elsewhere', time: T + 1000, uuid: 'usr-1' }] });
    await page.waitForSelector('#events-scroll .event[data-uuid="usr-1"]');
    await expect(card.locator('.ask-opt').first()).toBeDisabled();
    await expect(card.locator('.ask-status')).toHaveCount(1);
    expect(errors).toEqual([]);
    await ctx.close();
  });

  test('a turn ending on a session off screen raises its card\'s unread chip at once', async ({ browser }) => {
    const { ctx, page, conn, errors } = await open(browser, mock);
    // KEY_B is running in the mock; no sessions poll follows a reason-less push,
    // so the chip can only come from the push's own card patch.
    conn.send({ type: 'session_state', key: KEY_B, node: 'local', state: 'ready' });
    // Read the chip the moment the push lands: a later sidebar repaint paints
    // it from perSession.unread whatever the push's own patch did.
    const seen = await (await page.waitForFunction((k) => {
      if (sessionsData[sid(k, 'local')].state !== 'ready') return false;
      const chip = document.querySelector(`.session-card[data-key="${k}"] .sc-unread`);
      return { text: chip ? chip.textContent : null };
    }, KEY_B)).jsonValue();
    expect(seen.text, 'the push patched the chip onto the card').toBe('1');
    conn.send({ type: 'session_state', key: KEY_A, node: 'local', state: 'running' });
    conn.send({ type: 'session_state', key: KEY_A, node: 'local', state: 'ready' });
    await expect.poll(() => running(page, KEY_A)).toBe('ready');
    // The chip paint skips the active card whatever the count says, so leave
    // the card and have a push that ends no turn (ready→dead) repaint its chip
    // from the count: a turn read live must not have bumped it.
    await page.click(`.session-card[data-key="${KEY_B}"]`);
    await page.waitForFunction((key) => selectedKey === key, KEY_B);
    conn.send({ type: 'session_state', key: KEY_A, node: 'local', state: 'dead' });
    await expect.poll(() => running(page, KEY_A)).toBe('dead');
    await expect(page.locator(`.session-card[data-key="${KEY_A}"] .sc-unread`), 'the card on screen is read live').toHaveCount(0);
    expect(errors).toEqual([]);
    await ctx.close();
  });

  test('an opening frame anchors the turn timer at the first event of its last turn', async ({ browser }) => {
    const { ctx, page, conn, errors } = await open(browser, mock);
    const T0 = Date.now() - 125000;
    conn.send({ type: 'history', key: KEY_A, initial: true, events: [
      { type: 'user', detail: 'go', time: T0, uuid: 'u-go' },
      { type: 'tool_use', tool: 'Bash', summary: 'ls', time: T0 + 1000, uuid: 'u-tool' },
    ] });
    await page.waitForSelector('#events-scroll .event[data-uuid="u-go"]');
    expect(await page.evaluate(() => turnState.turnStartTime), 'anchored at the turn, not at render time').toBe(T0 + 1000);
    expect(errors).toEqual([]);
    await ctx.close();
  });

  test('a background task reporting while the session is idle does not anchor the next turn\'s timer', async ({ browser }) => {
    const { ctx, page, conn, errors } = await open(browser, mock);
    const T = Date.now() + 60000;
    const seen = (t) => page.waitForFunction((at) => sessionStream.lastEventTimeWs === at, t);
    conn.send({ type: 'event', key: KEY_A, event: { type: 'task_progress', task_id: 'a1', summary: 'step', time: T, uuid: 'tp-1' } });
    await seen(T);
    conn.send({ type: 'event', key: KEY_A, event: { type: 'task_done', task_id: 'a1', status: 'completed', time: T + 1, uuid: 'td-1' } });
    await seen(T + 1);
    expect(await page.evaluate(() => turnState.turnStartTime), 'idle task events start no turn').toBe(0);
    const before = await page.evaluate(() => Date.now());
    await sendText(page, conn, 'after-idle');
    expect(await page.evaluate(() => turnState.turnStartTime), 'the timer starts at the send').toBeGreaterThanOrEqual(before);

    // A running turn's task events are the turn's: the first one anchors.
    conn.send({ type: 'session_state', key: KEY_B, node: 'local', state: 'running' });
    await page.click(`.session-card[data-key="${KEY_B}"]`);
    await page.waitForFunction((key) => selectedKey === key && turnState.turnStartTime === 0, KEY_B);
    conn.send({ type: 'event', key: KEY_B, event: { type: 'task_progress', task_id: 'b1', summary: 'step', time: T + 2, uuid: 'tp-2' } });
    await page.waitForFunction(() => turnState.turnStartTime > 0);
    expect(errors).toEqual([]);
    await ctx.close();
  });

  test('an opening frame whose last turn is followed by idle task reports anchors no timer', async ({ browser }) => {
    const { ctx, page, conn, errors } = await open(browser, mock);
    const T0 = Date.now() - 125000;
    conn.send({ type: 'history', key: KEY_A, initial: true, events: [
      { type: 'user', detail: 'go', time: T0, uuid: 'u-go' },
      { type: 'text', detail: 'done', summary: 'done', time: T0 + 1000, uuid: 'u-done' },
      { type: 'result', time: T0 + 2000, uuid: 'u-res' },
      { type: 'task_progress', task_id: 'a1', summary: 'step', time: T0 + 3000, uuid: 'u-tp' },
      { type: 'task_done', task_id: 'a1', status: 'completed', time: T0 + 4000, uuid: 'u-td' },
    ] });
    await page.waitForSelector('#events-scroll .event[data-uuid="u-done"]');
    expect(await page.evaluate(() => turnState.turnStartTime), 'no turn runs after the result').toBe(0);
    expect(errors).toEqual([]);
    await ctx.close();
  });

  // A result event ends the turn on either frame path: the optimistic running
  // flips to ready (no session_state needed) and its cost bumps total_cost.
  for (const path of ['event', 'history']) {
    test(`a result in ${path === 'event' ? 'an event' : 'a backfill history'} frame ends the turn this tab started`, async ({ browser }) => {
      const { ctx, page, conn, errors } = await open(browser, mock);
      await sendText(page, conn, 'turn-' + path);
      await expect(page.locator('#btn-stop')).toBeVisible();
      const result = { type: 'result', summary: 'done', time: Date.now() + 60000, uuid: 'res-' + path, cost: 7.5 };
      conn.send(path === 'event' ? { type: 'event', key: KEY_A, event: result } : { type: 'history', key: KEY_A, events: [result] });
      await expect.poll(() => running(page, KEY_A), { message: 'the result flips running to ready' }).toBe('ready');
      await expect(page.locator('#btn-send')).toBeVisible();
      expect(await page.evaluate((k) => sessionsData[sid(k, 'local')].total_cost, KEY_A)).toBe(7.5);
      expect(errors).toEqual([]);
      await ctx.close();
    });
  }

  // A user bubble without a time skips the cursor guards, so only its uuid
  // keeps a replay of it from painting twice, on either frame path.
  for (const path of ['event', 'history']) {
    test(`a user replay in ${path === 'event' ? 'an event' : 'a backfill history'} frame is not painted twice`, async ({ browser }) => {
      const { ctx, page, conn, errors } = await open(browser, mock);
      const user = { type: 'user', detail: 'same message', uuid: 'usr-replay-' + path };
      conn.send({ type: 'event', key: KEY_A, event: user });
      await page.waitForSelector(`#events-scroll .event[data-uuid="${user.uuid}"]`);
      conn.send(path === 'event' ? { type: 'event', key: KEY_A, event: user } : { type: 'history', key: KEY_A, events: [user] });
      conn.send({ type: 'event', key: KEY_A, event: ev('after-' + path, Date.now() + 60000) });
      await shown(page, 'after-' + path); // the replay ahead of it has been handled
      await expect(page.locator(`#events-scroll .event[data-uuid="${user.uuid}"]`)).toHaveCount(1);
      expect(errors).toEqual([]);
      await ctx.close();
    });
  }

  test('a send_error for the session on screen undoes the send it failed', async ({ browser }) => {
    const { ctx, page, conn, errors } = await open(browser, mock);
    await sendText(page, conn, 'm1');
    const bubble = page.locator('#events-scroll .event.user.optimistic-msg');
    await expect(bubble).toHaveCount(1);
    await expect(page.locator('#btn-stop')).toBeVisible();
    expect(await running(page, KEY_A)).toBe('running');

    conn.send({ type: 'send_error', key: KEY_A, error: 'boom' });
    await expect(page.locator('#toast')).toHaveClass(/\berror\b/);
    await expect(page.locator('#toast')).toContainText('发送消息失败');
    await expect(page.locator('#toast')).toContainText('boom');
    await expect(bubble, 'the failed send\'s bubble is removed').toHaveCount(0);
    await expect(page.locator('#btn-send')).toBeVisible();
    expect(await running(page, KEY_A), 'the running flip is rolled back').toBe('ready');
    expect(errors).toEqual([]);
    await ctx.close();
  });

  test('a send_error for a session sent to and left only rolls its running flip back', async ({ browser }) => {
    const { ctx, page, conn, errors } = await open(browser, mock);
    await sendText(page, conn, 'm2');
    await page.click(`.session-card[data-key="${KEY_B}"]`);
    await page.waitForFunction((key) => selectedKey === key, KEY_B);
    expect(await running(page, KEY_A)).toBe('running');

    conn.send({ type: 'send_error', key: KEY_A, error: 'boom' });
    // Well inside the 20s safety timer, so only the send_error can roll it back.
    await expect.poll(() => running(page, KEY_A), { message: 'the running flip is rolled back' }).toBe('ready');
    await expect(page.locator('#toast'), 'no toast for a session off screen').not.toContainText('发送消息失败');
    expect(errors).toEqual([]);
    await ctx.close();
  });

  test('a send_error for a send this tab never made is ignored', async ({ browser }) => {
    const { ctx, page, conn, errors } = await open(browser, mock);
    conn.send({ type: 'send_error', key: KEY_A, error: 'boom' });
    conn.send({ type: 'event', key: KEY_A, event: ev('n1', Date.now() + 60000) });
    await shown(page, 'n1'); // the send_error ahead of it has been handled
    await expect(page.locator('#toast')).not.toContainText('发送消息失败');
    expect(errors).toEqual([]);
    await ctx.close();
  });
});

test.describe('a send from this tab to a dead session', () => {
  let mock;
  test.beforeAll(async () => {
    const data = defaultSessions();
    data.sessions.find((s) => s.key === KEY_A).state = 'dead';
    mock = await startMockServer({ ws: true, sessions: data });
  });
  test.afterAll(() => mock.server.close());

  // The send flips the state to running before the round trip, so the running
  // push that follows finds 'running' already there; the resubscribe must judge
  // on the state the flip replaced.
  test('resubscribes on the running push the revival sends', async ({ browser }) => {
    const { ctx, page, conn, errors } = await open(browser, mock);
    expect(await running(page, KEY_A)).toBe('dead');
    await sendText(page, conn, 'wake');
    expect(await running(page, KEY_A), 'the send flipped the state optimistically').toBe('running');
    conn.send({ type: 'session_state', key: KEY_A, node: 'local', state: 'running' });
    await expect.poll(() => subs(conn, KEY_A).length, { message: 'running must resubscribe' }).toBe(2);
    expect(subs(conn, KEY_A)[1].after, 'the resubscribe asks for an initial page').toBeUndefined();
    expect(errors).toEqual([]);
    await ctx.close();
  });
});

test.describe('a session_state carrying a reason', () => {
  let mock;
  let data;
  test.beforeAll(async () => {
    data = defaultSessions();
    mock = await startMockServer({ ws: true, sessions: data });
  });
  test.afterAll(() => mock.server.close());

  // The list changes without a version bump, as a process state transition
  // leaves storeGen alone; only the refetch the reason triggers, with the
  // version cache cleared, can paint it.
  test('refetches the session list and repaints it past the version cache', async ({ browser }) => {
    const { ctx, page, conn, errors } = await open(browser, mock);
    const card = page.locator(`.session-card[data-key="${KEY_B}"]`);
    await expect(card).toContainText('review this code');
    data.sessions.find((s) => s.key === KEY_B).summary = 'refetched after the push';
    conn.send({ type: 'session_state', key: KEY_B, node: 'local', state: 'dead', reason: 'no_output_timeout' });
    await expect(card, 'the reason refetches and repaints the sidebar').toContainText('refetched after the push');
    expect(errors).toEqual([]);
    await ctx.close();
  });
});
