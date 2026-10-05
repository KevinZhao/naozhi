// @ts-check
// A sessions poll answers with the snapshot the server built when the request
// arrived. Over a live socket a session_state push can overtake that response,
// and the poll must not undo it (#3277, #3278):
//  (a) a 'ready' snapshot taken before a 'running' push keeps the turn running
//      and announces no turn end;
//  (b) a 'running' snapshot taken before the 'ready' push that ends the turn
//      does not bring the banner back;
//  (c) a 'ready' snapshot taken before the subscribed ack's 'running' keeps
//      the turn running;
//  (d) a snapshot taken before a 'dead' push keeps the push's death_reason;
//  (e) a poll sent after the push still applies its snapshot, which is how a
//      dropped terminal push heals;
//  (f) a snapshot whose state the push overrode does not count as seen, so
//      the next poll applies in full: that snapshot may be the newer one.
//  (g) a 'running' snapshot taken before a result event, which ends the turn
//      ahead of its 'ready' push, does not bring the turn back, so the turn
//      end is announced once and a stopped turn is not reported as a reply;
//      a poll sent after the result still applies its snapshot.
//
// Run: cd test/e2e && npx playwright test session_state_stale_poll.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const READY = 'dashboard:direct:2026-01-01-120000-1:myproject';
const OTHER = 'dashboard:direct:2026-01-01-120001-2:otherproject';
const TURN_END = /回复完成|回合已结束/;

/** @type {Awaited<ReturnType<typeof startMockServer>> | undefined} */
let mock;
test.beforeEach(async ({ }, testInfo) => {
  testInfo.skip(testInfo.project.name !== 'desktop-chrome', 'desktop-chrome only');
  mock = await startMockServer({ ws: true });
});
test.afterEach(() => { mock?.server.close(); mock = undefined; });

// open loads the dashboard over a live socket, selects READY, waits out the
// load-time polls and records every #sr-announce write.
async function open(browser) {
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const page = await ctx.newPage();
  /** @type {string[]} */
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  await page.goto(mock.url + '/dashboard');
  await page.waitForFunction(() => wsm.state === WS_STATES.CONNECTED);
  const conn = mock.wsConnections[mock.wsConnections.length - 1];
  await page.click(`.session-card[data-key="${READY}"]`);
  await expect(page.locator('#btn-send')).toBeVisible();
  await page.waitForLoadState('networkidle');
  await page.evaluate(() => {
    const w = /** @type {any} */ (window);
    w.__announced = [];
    const el = document.getElementById('sr-announce');
    new MutationObserver(() => { if (el.textContent) w.__announced.push(el.textContent); })
      .observe(el, { childList: true, characterData: true, subtree: true });
  });
  return { ctx, page, conn, errors };
}

// holdPoll starts a sessions poll whose response the server builds now but the
// page receives only on release(), which resolves once the page applied it.
async function holdPoll(page) {
  /** @type {() => void} */
  let release = () => {};
  const released = new Promise((r) => { release = r; });
  /** @type {() => void} */
  let built = () => {};
  const snapshotted = new Promise((r) => { built = r; });
  await page.route('**/api/sessions', async (route) => {
    const response = await route.fetch();
    built();
    await released;
    await route.fulfill({ response });
  }, { times: 1 });
  await page.evaluate(async () => {
    /** @type {any} */ (window).__poll = (await import('/static/session_list.js')).fetchSessions();
  });
  await snapshotted;
  return async () => {
    release();
    await page.evaluate(() => /** @type {any} */ (window).__poll);
  };
}

// push sends a process state and makes the REST snapshot agree from now on.
const push = (conn, s) => {
  mock.setSessionStateWithoutVersionBump(READY, s);
  conn.send({ type: 'session_state', key: READY, node: 'local', state: s });
};
// turnEnds lists the turn-end announcements, once every one already made is in
// the record: announce() writes after 50 ms in call order, so a marker
// announced now lands after all of them.
async function turnEnds(page) {
  await page.evaluate(async () => (await import('/static/utilities.js')).announce('§'));
  await expect.poll(() => page.evaluate(() => /** @type {any} */ (window).__announced.includes('§'))).toBe(true);
  return (await page.evaluate(() => /** @type {any} */ (window).__announced)).filter((/** @type {string} */ t) => TURN_END.test(t));
}
const dot = (page) => page.locator(`.session-card[data-key="${READY}"] .sc-dot`);
const result = (conn) => conn.send({ type: 'event', key: READY, event: { time: Date.now(), type: 'result', summary: 'done' } });

// endByResult ends a running turn with a result event while a poll holding a
// running snapshot is in flight, lands that poll, then sends the ready push.
async function endByResult(page, conn) {
  const release = await holdPoll(page);
  result(conn);
  await expect(page.locator('#btn-send')).toBeVisible();
  await expect(page.locator('#running-banner')).toHaveClass(/nz-hidden/);
  await release();
  await expect(page.locator('#btn-stop')).toBeHidden();
  await expect(page.locator('#running-banner')).toHaveClass(/nz-hidden/);
  push(conn, 'ready');
}

test('a ready snapshot requested before a running push keeps the turn running', async ({ browser }) => {
  const { ctx, page, conn, errors } = await open(browser);
  try {
    const release = await holdPoll(page);
    push(conn, 'running');
    await expect(page.locator('#btn-stop')).toBeVisible();
    await release();
    await expect(page.locator('#btn-stop')).toBeVisible();
    await expect(page.locator('#running-banner')).not.toHaveClass(/nz-hidden/);
    await expect(dot(page)).toHaveClass(/dot-running/);
    expect(await turnEnds(page)).toEqual([]);
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
  }
});

test('a running snapshot requested before the ready push does not bring the banner back', async ({ browser }) => {
  const { ctx, page, conn, errors } = await open(browser);
  try {
    push(conn, 'running');
    await expect(page.locator('#btn-stop')).toBeVisible();
    const release = await holdPoll(page);
    push(conn, 'ready');
    await expect(page.locator('#btn-send')).toBeVisible();
    await release();
    await expect(page.locator('#btn-send')).toBeVisible();
    await expect(page.locator('#running-banner')).toHaveClass(/nz-hidden/);
    await expect(dot(page)).toHaveClass(/dot-ready/);
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
  }
});

test('a ready snapshot requested before the subscribed ack keeps its running state', async ({ browser }) => {
  const { ctx, page, conn, errors } = await open(browser);
  try {
    // The ack does not force a repaint, so the held snapshot needs a list
    // change elsewhere to get past the version short-circuit.
    mock.setSessionWorkspace(OTHER, '/tmp/elsewhere');
    const release = await holdPoll(page);
    mock.setSessionStateWithoutVersionBump(READY, 'running');
    conn.send({ type: 'subscribed', key: READY, node: 'local', state: 'running' });
    await expect(page.locator('#btn-stop')).toBeVisible();
    await release();
    await expect(page.locator('#btn-stop')).toBeVisible();
    await expect(page.locator('#running-banner')).not.toHaveClass(/nz-hidden/);
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
  }
});

test('a snapshot requested before a dead push keeps the push\'s death_reason', async ({ browser }) => {
  const { ctx, page, conn, errors } = await open(browser);
  try {
    const release = await holdPoll(page);
    // Park the poll the push's reason schedules, so the held one lands last.
    await page.route('**/api/sessions', () => {});
    mock.setSessionStateWithoutVersionBump(READY, 'dead');
    conn.send({ type: 'session_state', key: READY, node: 'local', state: 'dead', reason: 'idle_timeout' });
    await expect(dot(page)).toHaveClass(/dot-ready/);
    await release();
    const got = await page.evaluate(async (k) => {
      const { sessionList } = await import('/static/state.js');
      const sd = sessionList.sessionsData[(await import('/static/session_ident.js')).sid(k, 'local')];
      return { state: sd.state, reason: sd.death_reason };
    }, READY);
    expect(got).toEqual({ state: 'dead', reason: 'idle_timeout' });
    await expect(page.locator(`.session-card[data-key="${READY}"] .sc-exit`)).toHaveClass(/sc-exit-reclaimed/);
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
  }
});

test('a poll requested after the push still applies its snapshot', async ({ browser }) => {
  const { ctx, page, conn, errors } = await open(browser);
  try {
    // The REST snapshot stays ready: the terminal push is lost.
    conn.send({ type: 'session_state', key: READY, node: 'local', state: 'running' });
    await expect(page.locator('#btn-stop')).toBeVisible();
    const release = await holdPoll(page);
    await release();
    await expect(page.locator('#btn-send')).toBeVisible();
    await expect(page.locator('#running-banner')).toHaveClass(/nz-hidden/);
    await expect(dot(page)).toHaveClass(/dot-ready/);
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
  }
});

test('the poll after an overridden snapshot applies in full', async ({ browser }) => {
  const { ctx, page, conn, errors } = await open(browser);
  try {
    // The turn ends between the running push and the held snapshot, and its
    // ready push is lost: the REST snapshot stays ready throughout.
    const release = await holdPoll(page);
    conn.send({ type: 'session_state', key: READY, node: 'local', state: 'running' });
    await expect(page.locator('#btn-stop')).toBeVisible();
    await release();
    await expect(page.locator('#btn-stop')).toBeVisible();
    await page.evaluate(async () => (await import('/static/session_list.js')).fetchSessions());
    await expect(page.locator('#btn-send')).toBeVisible();
    await expect(page.locator('#running-banner')).toHaveClass(/nz-hidden/);
    await expect(dot(page)).toHaveClass(/dot-ready/);
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
  }
});

test('a running snapshot requested before a result event does not bring the turn back', async ({ browser }) => {
  const { ctx, page, conn, errors } = await open(browser);
  try {
    push(conn, 'running');
    await expect(page.locator('#btn-stop')).toBeVisible();
    await endByResult(page, conn);
    await expect(dot(page)).toHaveClass(/dot-ready/);
    expect(await turnEnds(page)).toEqual([expect.stringMatching(/^回复完成/)]);
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
  }
});

test('a stopped turn ended by a result event is announced once, as ended', async ({ browser }) => {
  const { ctx, page, conn, errors } = await open(browser);
  try {
    push(conn, 'running');
    await page.click('#btn-stop');
    await expect.poll(() => conn.messages.filter((m) => m.type === 'interrupt').length).toBe(1);
    await endByResult(page, conn);
    expect(await turnEnds(page)).toEqual(['回合已结束']);
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
  }
});

test('a poll requested after a result event still applies its snapshot', async ({ browser }) => {
  const { ctx, page, conn, errors } = await open(browser);
  try {
    push(conn, 'running');
    await expect(page.locator('#btn-stop')).toBeVisible();
    result(conn);
    await expect(page.locator('#btn-send')).toBeVisible();
    // The REST snapshot stays running (a new turn whose push was lost), and a
    // list change elsewhere gets the poll past the version short-circuit.
    mock.setSessionWorkspace(OTHER, '/tmp/elsewhere');
    await page.evaluate(async () => (await import('/static/session_list.js')).fetchSessions());
    await expect(page.locator('#btn-stop')).toBeVisible();
    await expect(dot(page)).toHaveClass(/dot-running/);
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
  }
});
