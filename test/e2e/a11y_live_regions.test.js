// @ts-check
// Screen readers hear a turn end once, not the turn streaming. The event log
// and the running banner are not live regions (the banner's elapsed chip ticks
// every second, its tool line changes on every event); updateSendButton
// announces the open session's running → non-running edge through
// #sr-announce instead:
//  (a) #events-scroll is role=log with aria-live=off, the banner has no live
//      role and the elapsed chip is aria-hidden;
//  (b) a turn that streams, then gets its result and the ready push, is
//      announced exactly once, with its elapsed time;
//  (c) a turn that ends in a non-ready state, or that the user stopped, says
//      the turn ended;
//  (d) opening a ready session, switching away from a running one, or
//      creating a session while a running one is open, is silent;
//  (e) a send rolled back by its ack (/clear's reset) never became a turn and
//      is silent;
//  (f) toggling voice mode re-applies the state on screen, even for a new
//      session's first send that has no snapshot entry yet.
//
// Run: cd test/e2e && npx playwright test a11y_live_regions.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const READY = 'dashboard:direct:2026-01-01-120000-1:myproject';
const RUNNING = 'dashboard:direct:2026-01-01-120001-2:otherproject';
const TURN_END = /回复完成|回合已结束/;

let mock;
test.beforeEach(async ({ }, testInfo) => {
  testInfo.skip(testInfo.project.name !== 'desktop-chrome', 'desktop-chrome only');
  mock = await startMockServer({ ws: true, eventsByKey: { [RUNNING]: [] } });
});
test.afterEach(() => { mock?.server.close(); mock = undefined; });

// quiesce returns once every announcement already made is in the record:
// announce() writes after 50 ms and its timers fire in call order, so a marker
// announced now lands after all of them.
let marks = 0;
async function quiesce(page) {
  const mark = '§' + ++marks;
  await page.evaluate(async (m) => (await import('/static/utilities.js')).announce(m), mark);
  await expect.poll(() => page.evaluate((m) => /** @type {any} */ (window).__announced.includes(m), mark)).toBe(true);
}
const subs = (conn, key) => conn.messages.filter((m) => m.type === 'subscribe' && m.key === key);

// open loads the dashboard, records every #sr-announce write, and selects key.
async function open(browser, key) {
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  await page.goto(mock.url + '/dashboard');
  await page.waitForFunction(() => wsm.state === WS_STATES.CONNECTED);
  const conn = mock.wsConnections[mock.wsConnections.length - 1];
  await page.evaluate(() => {
    const w = /** @type {any} */ (window);
    w.__announced = [];
    const el = document.getElementById('sr-announce');
    new MutationObserver(() => { if (el.textContent) w.__announced.push(el.textContent); })
      .observe(el, { childList: true, characterData: true, subtree: true });
  });
  await page.click(`.session-card[data-key="${key}"]`);
  await expect.poll(() => subs(conn, key).length).toBe(1);
  conn.send({ type: 'subscribed', key });
  await page.waitForFunction((k) => sessionStream.subscribedKey === k, key);
  // Start from a clean record: the socket's own 已连接 lands just after connect.
  await quiesce(page);
  await page.evaluate(() => { /** @type {any} */ (window).__announced = []; });
  return { ctx, page, conn, errors };
}

const turnEnds = async (page) => (await page.evaluate(() => /** @type {any} */ (window).__announced))
  .filter((/** @type {string} */ t) => TURN_END.test(t));
// state pushes a process state and makes the REST snapshot agree, so a poll
// that lands mid-turn cannot reconcile the turn away.
const state = (conn, key, s) => {
  mock.setSessionStateWithoutVersionBump(key, s);
  conn.send({ type: 'session_state', key, node: 'local', state: s });
};
const event = (conn, key, ev) => conn.send({ type: 'event', key, event: { time: Date.now(), ...ev } });

test('the event log and running banner are not live regions', async ({ browser }) => {
  const { ctx, page, errors } = await open(browser, READY);
  try {
    const log = page.locator('#events-scroll');
    await expect(log).toHaveAttribute('role', 'log');
    await expect(log).toHaveAttribute('aria-live', 'off');
    expect(await log.getAttribute('aria-relevant')).toBeNull();
    const banner = page.locator('#running-banner');
    expect(await banner.getAttribute('aria-live')).toBeNull();
    expect(await banner.getAttribute('role')).toBeNull();
    await expect(page.locator('#rb-elapsed')).toHaveAttribute('aria-hidden', 'true');
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
  }
});

test('a streamed turn is announced once when it ends, with its elapsed time', async ({ browser }) => {
  const { ctx, page, conn, errors } = await open(browser, READY);
  try {
    state(conn, READY, 'running');
    await expect(page.locator('#btn-stop')).toBeVisible();
    event(conn, READY, { type: 'tool_use', tool: 'Bash', summary: 'ls' });
    event(conn, READY, { type: 'text', summary: 'working on it' });
    // The ticking chip and the streamed events say nothing.
    await expect(page.locator('#rb-elapsed')).toHaveText(/^0:0[1-9]$/, { timeout: 5000 });
    await quiesce(page);
    expect(await page.evaluate(() => /** @type {any} */ (window).__announced)).toEqual([expect.stringMatching(/^§\d+$/)]);

    event(conn, READY, { type: 'result', summary: 'done' });
    state(conn, READY, 'ready');
    // Every frame above is handled once the interrupt_ack it precedes toasts.
    conn.send({ type: 'interrupt_ack', status: 'not_running' });
    await expect(page.locator('#toast')).toContainText('会话未在运行');
    await quiesce(page);
    expect(await turnEnds(page)).toEqual([expect.stringMatching(/^回复完成，用时 0:0[1-9]$/)]);
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
  }
});

test('a turn that ends in a non-ready state says the turn ended', async ({ browser }) => {
  const { ctx, page, conn, errors } = await open(browser, READY);
  try {
    state(conn, READY, 'running');
    await expect(page.locator('#btn-stop')).toBeVisible();
    state(conn, READY, 'dead');
    await expect.poll(() => turnEnds(page)).toEqual(['回合已结束']);
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
  }
});

test('a turn stopped with the stop button says the turn ended, and the next one is a reply', async ({ browser }) => {
  const { ctx, page, conn, errors } = await open(browser, READY);
  try {
    state(conn, READY, 'running');
    await page.click('#btn-stop');
    await expect.poll(() => conn.messages.filter((m) => m.type === 'interrupt').length).toBe(1);
    event(conn, READY, { type: 'result', summary: 'interrupted' });
    state(conn, READY, 'ready');
    await expect.poll(() => turnEnds(page)).toEqual(['回合已结束']);

    state(conn, READY, 'running');
    await expect(page.locator('#btn-stop')).toBeVisible();
    state(conn, READY, 'ready');
    await expect.poll(() => turnEnds(page)).toEqual(['回合已结束', '回复完成']);
    await quiesce(page);
    expect(await turnEnds(page)).toEqual(['回合已结束', '回复完成']);
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
  }
});

test('opening a ready session or switching away from a running one is silent', async ({ browser }) => {
  const { ctx, page, conn, errors } = await open(browser, RUNNING);
  try {
    await expect(page.locator('#btn-stop')).toBeVisible();
    await page.click(`.session-card[data-key="${READY}"]`);
    await expect.poll(() => subs(conn, READY).length).toBe(1);
    await expect(page.locator('#btn-send')).toBeVisible();
    await quiesce(page);
    expect(await turnEnds(page)).toEqual([]);
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
  }
});

// Creating a session moves the selection without clearing the last applied
// state, which still names the running session.
test('creating a session while a running one is open is silent', async ({ browser }) => {
  const { ctx, page, errors } = await open(browser, RUNNING);
  try {
    await expect(page.locator('#btn-stop')).toBeVisible();
    await page.evaluate(async () => {
      const { doCreateInProject } = await import('/static/auth_modal.js');
      doCreateInProject('/home/user/myproject', 'myproject', 'local', '', 'general');
    });
    const selected = () => page.evaluate(async () => (await import('/static/state.js')).selection.key);
    await expect.poll(selected).toMatch(/^dashboard:/);
    expect(await selected()).not.toBe(RUNNING);
    await expect(page.locator('#btn-send')).toBeVisible();
    await quiesce(page);
    expect(await turnEnds(page)).toEqual([]);
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
  }
});

test('a send its ack rolls back is not announced as a turn end', async ({ browser }) => {
  const { ctx, page, conn, errors } = await open(browser, READY);
  try {
    await page.evaluate(() => { setMsgValue(document.getElementById('msg-input'), '/clear'); sendMessage(); });
    await expect.poll(() => conn.messages.filter((m) => m.type === 'send').length).toBe(1);
    await expect(page.locator('#btn-stop')).toBeVisible();
    const { id } = conn.messages.find((m) => m.type === 'send');
    conn.send({ type: 'send_ack', id, status: 'reset', key: READY });
    await expect(page.locator('#btn-send')).toBeVisible();
    await quiesce(page);
    expect(await turnEnds(page)).toEqual([]);
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
  }
});

// A new session has no snapshot entry until the server lists it, so its first
// send is running only in the last applied state.
test('toggling voice mode during a new session\'s first send keeps the turn running and silent', async ({ browser }) => {
  const { ctx, page, conn, errors } = await open(browser, READY);
  try {
    await page.evaluate(async () => {
      const { doCreateInProject } = await import('/static/auth_modal.js');
      doCreateInProject('/home/user/myproject', 'myproject', 'local', '', 'general');
    });
    const selected = () => page.evaluate(async () => (await import('/static/state.js')).selection.key);
    await expect.poll(selected).toMatch(/^dashboard:/);
    expect(await selected()).not.toBe(READY);
    await page.evaluate(() => { setMsgValue(document.getElementById('msg-input'), 'hello'); sendMessage(); });
    await expect.poll(() => conn.messages.filter((m) => m.type === 'send').length).toBe(1);
    await expect(page.locator('#btn-stop')).toBeVisible();
    await page.click('#btn-mic');
    await expect(page.locator('#input-area')).toHaveClass(/voice-mode/);
    await quiesce(page);
    expect(await turnEnds(page)).toEqual([]);
    await expect(page.locator('#btn-stop')).toBeVisible();
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
  }
});
