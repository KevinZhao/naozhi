// @ts-check
// Session-list behaviour that fetchSessions / renderSidebar / msg_nav own,
// pinned in a browser rather than by the functions' source shape, so they
// can be split and moved (see #3025):
//
//  1. The fallback reconcile compares REST with the state the main area last
//     applied, and does not re-apply one it already applied (updateSendButton
//     is not idempotent).
//  2. A pending session the backend now lists is dropped from the durable
//     localStorage blob, so a reload cannot resurrect it as a ghost card.
//  3. Cmd+↓ / Cmd+↑ / Cmd+N walk the selected session's group in the sidebar's
//     order (created_at, oldest first), which msg_nav reads from
//     sessionList.allSessionsCache.
//  4. A poll that applies a new snapshot repaints the open session's CLI
//     label in the header: dashboard.js registers updateHeaderCLI with
//     session_list's onSessionsApplied, and nothing else repaints it then.
//  5. The hooks get the real socket state. With the banner already up, a
//     REST 'running' is applied only when the socket is down; over a live
//     socket it is a lagging snapshot and must not re-run the running
//     transition (stop button, agent re-seed).
//
// Their twins already in the suite: the WS-down version gate and the
// optimistic-running write-back are ws_fallback_state.test.js tests 1 and 3,
// and the pending card's basename group is sidebar_p3_2431.test.js item 4.
const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');

test.use({ viewport: { width: 1280, height: 800 } });

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'desktop sidebar and keyboard shortcuts; desktop-chrome only');
  }
});

const KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';
const PROJ = '/home/user/workspace/myproject';

test('the fallback reconcile does not re-apply a main state it already applied', async ({ page }) => {
  const mock = await startMockServer();
  try {
    await page.goto(mock.url + '/dashboard');
    await page.click(`.session-card[data-key="${KEY}"]`);
    await page.waitForSelector('#msg-input');
    expect(await page.evaluate(() => window.nz.test.wsm.state)).not.toBe('connected');
    const display = (id) => page.evaluate((i) => document.getElementById(i).style.display, id);
    await expect.poll(() => display('btn-send')).toBe('flex');

    // updateSendButton is the only writer of the buttons' display; a sentinel
    // value survives exactly as long as nothing re-applies the main state.
    await page.evaluate(() => window.nz.test.fetchSessions());
    await page.evaluate(() => { document.getElementById('btn-send').style.display = 'contents'; });
    await page.evaluate(() => window.nz.test.fetchSessions());
    expect(await display('btn-send')).toBe('contents');

    // REST now differs from what was applied: the reconcile must apply it.
    mock.setSessionStateWithoutVersionBump(KEY, 'running');
    await page.evaluate(() => window.nz.test.fetchSessions());
    expect(await display('btn-stop')).toBe('flex');
    expect(await display('btn-send')).toBe('none');
    mock.setSessionStateWithoutVersionBump(KEY, 'ready');
    await page.evaluate(() => window.nz.test.fetchSessions());
    expect(await display('btn-send')).toBe('flex');
    expect(await display('btn-stop')).toBe('none');
  } finally { mock.server.close(); }
});

test('a poll that applies a new snapshot repaints the header CLI label', async ({ page }) => {
  const sessions = defaultSessions();
  const mock = await startMockServer({ sessions });
  try {
    await page.goto(mock.url + '/dashboard');
    await page.click(`.session-card[data-key="${KEY}"]`);
    const cli = page.locator('.main-header .detail-left #header-cli');
    await expect(cli).toHaveAttribute('title', 'claude v1.0.30');

    // Only the version moves. renderMainShell does not run again, so the
    // label changes only if the post-poll hook repaints it.
    sessions.sessions.find((s) => s.key === KEY).cli_version = '1.0.31';
    sessions.stats.version++;
    await page.evaluate(() => window.nz.test.fetchSessions());
    await expect(cli).toHaveAttribute('title', 'claude v1.0.31');
    await expect(cli).toHaveText('claude');
  } finally { mock.server.close(); }
});

// lagRunningBehindVisibleBanner leaves the banner up, as refreshBanner does
// for background agents after a turn ends, then has REST say 'running' with a
// bumped version so the poll gets past the short-circuit. It returns what
// fetchSessions returned: true means the payload was applied and the hooks ran.
async function lagRunningBehindVisibleBanner(page, sessions) {
  await page.click(`.session-card[data-key="${KEY}"]`);
  await page.waitForSelector('#msg-input');
  const display = (id) => page.evaluate((i) => document.getElementById(i).style.display, id);
  await expect.poll(() => display('btn-send')).toBe('flex');
  await expect(page.locator('#running-banner')).toHaveClass(/nz-hidden/);
  await page.evaluate(() => document.getElementById('running-banner').classList.remove('nz-hidden'));
  sessions.sessions.find((s) => s.key === KEY).state = 'running';
  sessions.stats.version++;
  const applied = await page.evaluate(() => window.nz.test.fetchSessions());
  await expect(page.locator(`.session-card[data-key="${KEY}"] .sc-dot`)).toHaveClass(/dot-running/);
  return { applied, stop: await display('btn-stop'), send: await display('btn-send') };
}

test('over a live socket, a lagging REST running does not re-run the running transition', async ({ page }) => {
  const sessions = defaultSessions();
  const mock = await startMockServer({ sessions, ws: true });
  try {
    await page.goto(mock.url + '/dashboard');
    await page.waitForFunction(() => window.nz.test.wsm.state === 'connected');
    const got = await lagRunningBehindVisibleBanner(page, sessions);
    expect(got).toEqual({ applied: true, stop: 'none', send: 'flex' });
    expect(await page.evaluate(() => window.nz.test.wsm.state)).toBe('connected');
  } finally { mock.server.close(); }
});

test('with the socket down, the same REST running is applied', async ({ page }) => {
  const sessions = defaultSessions();
  const mock = await startMockServer({ sessions });
  try {
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    expect(await page.evaluate(() => window.nz.test.wsm.state)).not.toBe('connected');
    const got = await lagRunningBehindVisibleBanner(page, sessions);
    expect(got).toEqual({ applied: true, stop: 'flex', send: 'none' });
  } finally { mock.server.close(); }
});

test('a pending session the backend lists is dropped from localStorage, so a reload shows no ghost', async ({ page }) => {
  const sessions = defaultSessions();
  const mock = await startMockServer({ sessions });
  try {
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    const blob = () => page.evaluate(() => JSON.parse(localStorage.getItem('nz:pending_sessions') || '{}'));
    const key = await page.evaluate((proj) => {
      window.nz.test.doCreateInProject(proj, 'myproject', 'local', undefined, 'general', { mode: 'new' });
      return window.nz.test.selectedKey;
    }, PROJ);
    expect((await blob())[key]).toEqual({ ws: PROJ });
    await expect(page.locator(`.session-card.new-card[data-key="${key}"]`)).toHaveCount(1);

    // The first send spawned it: the backend lists the key now.
    sessions.sessions.push({ ...sessions.sessions[0], key, state: 'ready', last_prompt: 'spawned', created_at: Date.now() });
    await page.evaluate(() => window.nz.test.fetchSessions());
    expect(await page.evaluate((k) => window.nz.test.sessionWorkspaces[k], key)).toBeUndefined();
    expect(await blob()).not.toHaveProperty(key);
    await expect(page.locator(`.session-card[data-key="${key}"]`)).not.toHaveClass(/new-card/);

    // It is gone from the backend again (dismissed elsewhere). A stale blob
    // entry would re-inject it on reload as a pending "new" card.
    sessions.sessions.pop();
    await page.reload();
    await page.waitForSelector('.session-card');
    await page.evaluate(() => window.nz.test.fetchSessions());
    await expect(page.locator(`.session-card[data-key="${KEY}"]`)).toHaveCount(1);
    await expect(page.locator(`.session-card[data-key="${key}"]`)).toHaveCount(0);
    expect(await page.evaluate((k) => window.nz.test.sessionWorkspaces[k], key)).toBeUndefined();
  } finally { mock.server.close(); }
});

// One project whose sessions the server lists out of created_at order, plus
// a neighbouring project the walk must not enter.
function orderedSessions() {
  const s = defaultSessions();
  const T = Date.UTC(2026, 0, 1, 12, 0, 0);
  const mk = (id, created, project, prompt) => ({
    key: `dashboard:direct:2026-01-01-${id}:general`, state: 'ready', platform: 'dashboard', agent: 'general',
    cli_name: 'claude', workspace: '/home/user/workspace/' + project, project, node: 'local',
    created_at: T + created * 60000, last_active: T + 90 * 60000, last_prompt: prompt,
  });
  s.sessions = [
    mk('120004-a', 4, 'myproject', 'fourth'),
    mk('120001-b', 1, 'myproject', 'first'),
    mk('120003-c', 3, 'myproject', 'third'),
    mk('120002-d', 2, 'myproject', 'second'),
    mk('120001-e', 1.5, 'otherproject', 'other'),
  ];
  return s;
}
const K = (id) => `dashboard:direct:2026-01-01-${id}:general`;

test('Cmd+↓, Cmd+↑ and Cmd+N walk the group in created_at order', async ({ page }) => {
  const mock = await startMockServer({ sessions: orderedSessions() });
  try {
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    // Sidebar: oldest first within a group, whatever order the server listed
    // them in; myproject's group is anchored by its oldest session.
    const cards = await page.locator('#session-list .session-card').evaluateAll((els) => els.map((el) => el.getAttribute('data-key')));
    expect(cards).toEqual([K('120001-b'), K('120002-d'), K('120003-c'), K('120004-a'), K('120001-e')]);

    const active = page.locator('#session-list .session-card.active');
    const press = async (combo) => {
      await page.evaluate(() => { if (document.activeElement instanceof HTMLElement) document.activeElement.blur(); });
      await page.keyboard.press(combo);
    };
    await page.click(`.session-card[data-key="${K('120001-b')}"]`);
    await expect(active).toHaveAttribute('data-key', K('120001-b'));

    await press('Meta+ArrowDown');
    await expect(active).toHaveAttribute('data-key', K('120002-d'));
    await press('Meta+ArrowDown');
    await expect(active).toHaveAttribute('data-key', K('120003-c'));
    await press('Meta+ArrowDown');
    await expect(active).toHaveAttribute('data-key', K('120004-a'));
    // Wraps inside the group; otherproject's card is never visited.
    await press('Meta+ArrowDown');
    await expect(active).toHaveAttribute('data-key', K('120001-b'));
    await press('Meta+ArrowUp');
    await expect(active).toHaveAttribute('data-key', K('120004-a'));

    await press('Meta+2');
    await expect(active).toHaveAttribute('data-key', K('120002-d'));
    await press('Meta+4');
    await expect(active).toHaveAttribute('data-key', K('120004-a'));
    await press('Meta+1');
    await expect(active).toHaveAttribute('data-key', K('120001-b'));
  } finally { mock.server.close(); }
});
