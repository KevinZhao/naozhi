// E2E: the sidebar changes the moment a session is removed, put away or
// renamed, before the request that does it server-side answers (each test
// holds that request in the browser, and the list's own reads with it, so
// only the local repaint can show the change). Everything derived from the
// rows comes along: the project group and its header, the 只看需要我 chip,
// the Home 最近会话. A read answering meanwhile with the list from before
// does not bring the old state back; a refused request puts it back at once
// and says why.
//   × (移除): gone at once, 撤销 back at once, a refused DELETE back at once.
//   × on a discovered CLI: gone at once, a discovery scan meanwhile keeps it
//     gone, a refused close puts it back.
//   任务 标为已完成 / 停止: the card moves at once; a session the gateway
//     puts away right after (nothing of it runs, D137) leaves the sidebar at
//     once, one still running stays; a refused POST puts both back.
//   单独聊 结束: the view closes, its sidebar and 你开的 cards go at once; a
//     refused end opens it again, one failed after it ended reads the lists
//     again.
//   A read of 任务 sent before a POST answered, landing after it, keeps the
//     change shown.
//   重命名 (the session header's, a 单独聊's): the title shows at once, a
//     refused PATCH puts the old one back.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const now = Date.now();
const M = 60000;
const iso = (ms) => new Date(ms).toISOString();
const DESKTOP = { viewport: { width: 1280, height: 860 } };
const PLANNER = 'planner:main';
const planner = { enabled: true, stopped: false, key: PLANNER, alive: true };

const ALONE = 'dashboard:direct:2026-11-22-090000-1:general';
const BETA1 = 'dashboard:direct:2026-11-22-090001-2:general';
const BETA2 = 'dashboard:direct:2026-11-22-090002-3:general';

function row(key, project, prompt, extra) {
  return Object.assign({
    key, state: 'ready', platform: 'dashboard', agent: 'general', cli_name: 'claude', node: 'local',
    workspace: '/home/user/workspace/' + project, project, last_prompt: prompt, last_active: now - 10 * M,
    created_at: now - 60 * M + key.length, health: { state: 'idle', since: now - 5 * M },
  }, extra);
}

function sessionsOf(rows) {
  return {
    sessions: rows,
    stats: { total: rows.length, running: 0, ready: rows.length, active: rows.length, uptime: '1h', backend: 'cc', max_procs: 20, default_workspace: '/home/user/workspace', agents: ['general'], projects: [], version: 1 },
  };
}

// hold keeps the page's requests to glob with method from the mock until
// release: release() lets them through, release(status) fails them.
async function hold(page, glob, method) {
  const held = [];
  let on = true;
  await page.route(glob, (route) => {
    if (!on || route.request().method() !== method) return route.fallback();
    held.push(route);
  });
  return {
    held,
    on: () => { on = true; },
    async release(status) {
      on = false;
      for (const r of held.splice(0)) {
        if (status) await r.fulfill({ status, contentType: 'application/json', body: JSON.stringify({ error: 'iso 服务端拒绝' }) });
        else await r.continue();
      }
    },
  };
}

// stalePoll has the sidebar read the list again now, with a version bumped
// so a connected socket does not skip it, while the mock still lists what it
// listed before.
async function stalePoll(page, data) {
  data.stats.version++;
  await page.evaluate(() => window.nz.test.fetchSessions());
}

const card = (page, key) => page.locator('#session-list .session-card[data-key="' + key + '"]');
const groupHead = (page, name) => page.locator('#session-list .section-header [data-action="project-collapse"][data-key="local:' + name + '"]');

test('× repaints the whole sidebar at once; 撤销 and a refused DELETE put it all back at once', async ({ page }) => {
  test.setTimeout(60000);
  const data = sessionsOf([
    row(ALONE, 'alpha', 'iso 独苗会话', { health: { state: 'waiting', since: now - 5 * M } }),
    row(BETA1, 'beta', 'iso 乙一'),
    row(BETA2, 'beta', 'iso 乙二'),
  ]);
  const mock = await startMockServer({ sessions: data });
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  try {
    await page.setViewportSize(DESKTOP.viewport);
    await page.goto(mock.url + '/dashboard');
    await expect(card(page, ALONE)).toHaveCount(1);
    await expect(groupHead(page, 'alpha')).toHaveCount(1);
    const chip = page.locator('.sb-filter-chip[data-action="sidebar-only-need"]');
    await expect(chip).toHaveText('只看需要我 1');
    const recent = page.locator('#recent-sessions-panel .recent-row[data-key="' + ALONE + '"]');
    await expect(recent).toHaveCount(1);

    // No read of the list answers from here on: what changes is the local repaint.
    const reads = await hold(page, '**/api/sessions', 'GET');
    await card(page, ALONE).hover();
    await card(page, ALONE).locator('.btn-dismiss').click();
    await expect(card(page, ALONE)).toHaveCount(0);
    await expect(groupHead(page, 'alpha')).toHaveCount(0);
    await expect(chip).toHaveCount(0);
    await expect(recent).toHaveCount(0);
    await expect(groupHead(page, 'beta')).toHaveCount(1);

    // 撤销: all of it back, still with no read answered.
    await page.locator('#undo-toast').getByRole('button', { name: '撤销' }).click();
    await expect(card(page, ALONE)).toHaveCount(1);
    await expect(groupHead(page, 'alpha')).toHaveCount(1);
    await expect(chip).toHaveText('只看需要我 1');
    await expect(recent).toHaveCount(1);
    await reads.release();

    // A read answering before the DELETE (the mock still lists it) keeps it gone.
    await card(page, ALONE).hover();
    await card(page, ALONE).locator('.btn-dismiss').click();
    await expect(groupHead(page, 'alpha')).toHaveCount(0);
    await stalePoll(page, data);
    await expect(card(page, ALONE)).toHaveCount(0);
    await expect(groupHead(page, 'alpha')).toHaveCount(0);
    await expect(chip).toHaveCount(0);
    await expect.poll(() => mock.deleteCalls, { timeout: 10000 }).toEqual([{ key: ALONE }]);
    await expect(card(page, ALONE)).toHaveCount(0);

    // A refused DELETE puts the card back at once and says why.
    const del = await hold(page, '**/api/sessions', 'DELETE');
    reads.on();
    await card(page, BETA1).hover();
    await card(page, BETA1).locator('.btn-dismiss').click();
    await expect(card(page, BETA1)).toHaveCount(0);
    await expect.poll(() => del.held.length, { timeout: 10000 }).toBe(1);
    await del.release(500);
    await expect(card(page, BETA1)).toHaveCount(1);
    await expect(page.locator('#toast')).toContainText('删除会话');
    await reads.release();
    expect(errors).toEqual([]);
  } finally {
    mock.server.close();
  }
});

// scan runs the page's discovery scan now (the same module instance the
// page loaded: one URL, one module).
const scan = (page) => page.evaluate(async () => (await import('/static/discovery.js')).scanDiscovered());

test('× on a discovered CLI hides it at once, through a discovery scan; a refused close puts it back', async ({ page }) => {
  test.setTimeout(60000);
  const mock = await startMockServer({ sessions: sessionsOf([row(BETA1, 'beta', 'iso 乙一')]) });
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  let listed = [{ pid: 4242, cwd: '/home/user/workspace/gamma', project: 'gamma', state: 'ready', cli_name: 'claude', session_id: 'iso-ext-1', proc_start_time: 7, started_at: now - 30 * M, last_active: now - 2 * M, last_prompt: 'iso 外部会话' }];
  let scans = 0;
  await page.route('**/api/discovered', (route) => {
    if (route.request().method() !== 'GET') return route.fallback();
    scans++;
    return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(listed) });
  });
  try {
    await page.setViewportSize(DESKTOP.viewport);
    await page.goto(mock.url + '/dashboard');
    const ext = page.locator('#session-list .session-card[data-key="_discovered:4242:local"]');
    await expect(ext).toHaveCount(1);
    await expect(groupHead(page, 'gamma')).toHaveCount(1);

    const close = await hold(page, '**/api/discovered/close', 'POST');
    await ext.hover();
    await ext.locator('.btn-dismiss').click();
    await expect(ext).toHaveCount(0);
    await expect(groupHead(page, 'gamma')).toHaveCount(0);
    // A scan answering while the close is out still lists it.
    const before = scans;
    await scan(page);
    expect(scans).toBeGreaterThan(before);
    await page.evaluate(() => window.nz.test.fetchSessions());
    await expect(ext).toHaveCount(0);
    expect(close.held).toHaveLength(1);

    await close.release(500);
    await expect(ext).toHaveCount(1);
    await expect(groupHead(page, 'gamma')).toHaveCount(1);
    await expect(page.locator('#toast')).toContainText('关闭外部会话');

    // Closed for real: gone, and gone after the next scan too.
    close.on();
    await ext.hover();
    await ext.locator('.btn-dismiss').click();
    await expect(ext).toHaveCount(0);
    listed = [];
    await close.release();
    await expect.poll(() => close.held.length).toBe(0);
    await scan(page);
    await page.evaluate(() => window.nz.test.fetchSessions());
    await expect(ext).toHaveCount(0);
    expect(errors).toEqual([]);
  } finally {
    mock.server.close();
  }
});

const IDLE_TASK = 'task:idle00000001';
const RUN_TASK = 'task:run000000002';
const FAIL_TASK = 'task:fail00000003';
const tkCard = (page, id) => page.locator('#pl-task .tk-card[data-id="' + id + '"]');

test('任务: 标为已完成 and 停止 show at once, the session put away leaves the sidebar at once; a refused POST puts both back', async ({ page }) => {
  test.setTimeout(60000);
  const taskRow = (key, project, prompt, extra) => row(key, project, prompt, Object.assign({ purpose: 'task', purpose_title: prompt }, extra));
  const sessions = sessionsOf([
    taskRow(IDLE_TASK, 'alpha', 'iso 查容量'),
    taskRow(RUN_TASK, 'beta', 'iso 跑压测', { state: 'running', health: { state: 'active', since: now - M } }),
    taskRow(FAIL_TASK, 'gamma', 'iso 写周报'),
  ]);
  const tasks = { tasks: [
    { id: IDLE_TASK, kind: 'session', title: 'iso 查容量', state: 'doing', at: iso(now - 40 * M), created: iso(now - 40 * M), health: 'idle',
      live: { running: false, reply: '容量够', last_active: iso(now - 10 * M) } },
    { id: RUN_TASK, kind: 'session', title: 'iso 跑压测', state: 'doing', at: iso(now - 30 * M), created: iso(now - 30 * M), health: 'active', health_since: iso(now - M),
      live: { running: true, activity: 'Bash: 跑压测', last_active: iso(now - M) } },
    { id: FAIL_TASK, kind: 'session', title: 'iso 写周报', state: 'doing', at: iso(now - 20 * M), created: iso(now - 20 * M), health: 'idle',
      live: { running: false, last_active: iso(now - 15 * M) } },
  ] };
  const mock = await startMockServer({
    ws: true, sessions, tasks, plannerEvents: [], inbox: { open: [], closed: [], counts: { open: 0, badge: 0 } }, planner,
  });
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  let taskReads = 0;
  page.on('response', (r) => { if (new URL(r.url()).pathname === '/api/tasks') taskReads++; });
  try {
    await page.setViewportSize(DESKTOP.viewport);
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await expect(card(page, IDLE_TASK)).toHaveCount(1);
    await page.click('#abnav-planner');
    await expect(page.locator('#pl-task .tk-head')).toHaveText('任务 3');

    const post = await hold(page, '**/api/tasks/state', 'POST');
    await tkCard(page, IDLE_TASK).hover();
    await tkCard(page, IDLE_TASK).locator('.tk-more').click();
    await tkCard(page, IDLE_TASK).locator('[data-action="task-done"]').click();
    // The card, the counts and the sidebar, before the POST reached the gateway.
    await expect(page.locator('#pl-task .tk-sec-doing .tk-card[data-id="' + IDLE_TASK + '"]')).toHaveCount(0);
    await expect(page.locator('#pl-task .tk-sec-done .tk-card[data-id="' + IDLE_TASK + '"]')).toHaveCount(1);
    await expect(page.locator('#pl-task .tk-head')).toHaveText('任务 2');
    await expect(page.locator('#pl-task .tk-acts')).toHaveCount(0);
    await expect(card(page, IDLE_TASK)).toHaveCount(0);
    await expect(groupHead(page, 'alpha')).toHaveCount(0);
    expect(mock.tasksCalls).toEqual([]);

    // A read of 任务 answering before the POST (still 进行中 there) does not flip it back,
    // nor does a read of the list bring the card back.
    const before = taskReads;
    for (const c of mock.wsConnections) c.send({ type: 'session_state', key: RUN_TASK, state: 'running' });
    await expect.poll(() => taskReads, { timeout: 8000 }).toBeGreaterThan(before);
    await expect(page.locator('#pl-task .tk-sec-done .tk-card[data-id="' + IDLE_TASK + '"]')).toHaveCount(1);
    await expect(page.locator('#pl-task .tk-head')).toHaveText('任务 2');
    await stalePoll(page, sessions);
    await expect(card(page, IDLE_TASK)).toHaveCount(0);
    await post.release();
    await expect.poll(() => mock.tasksCalls).toEqual([{ id: IDLE_TASK, state: 'done' }]);
    await expect(page.locator('#pl-task .tk-sec-done .tk-card[data-id="' + IDLE_TASK + '"]')).toHaveCount(1);
    await expect(card(page, IDLE_TASK)).toHaveCount(0);

    // 停止 a running one: 已停止 at once; its turn still runs, so the gateway
    // does not put it away yet and its sidebar card stays. Refused: back.
    const stop = await hold(page, '**/api/tasks/stop', 'POST');
    await tkCard(page, RUN_TASK).hover();
    await tkCard(page, RUN_TASK).locator('.tk-more').click();
    await tkCard(page, RUN_TASK).locator('[data-action="task-stop"]').click();
    await expect(tkCard(page, RUN_TASK).locator('.tk-badge')).toHaveText('已停止');
    await expect(page.locator('#pl-task .tk-head')).toHaveText('任务 1');
    await expect(card(page, RUN_TASK)).toHaveCount(1);
    await expect.poll(() => stop.held.length).toBe(1);
    await stop.release(500);
    await expect(page.locator('#pl-task .tk-sec-doing .tk-card[data-id="' + RUN_TASK + '"] .tk-badge')).toHaveText('在跑');
    await expect(page.locator('#toast')).toContainText('没有停下');

    // A refused 标为已完成 of one the gateway would put away: the card back
    // in 进行中 and in the sidebar, its group too.
    post.on();
    await tkCard(page, FAIL_TASK).hover();
    await tkCard(page, FAIL_TASK).locator('.tk-more').click();
    await tkCard(page, FAIL_TASK).locator('[data-action="task-done"]').click();
    await expect(card(page, FAIL_TASK)).toHaveCount(0);
    await expect(groupHead(page, 'gamma')).toHaveCount(0);
    await expect.poll(() => post.held.length).toBe(1);
    await post.release(500);
    await expect(card(page, FAIL_TASK)).toHaveCount(1);
    await expect(groupHead(page, 'gamma')).toHaveCount(1);
    await expect(page.locator('#pl-task .tk-sec-doing .tk-card[data-id="' + FAIL_TASK + '"]')).toHaveCount(1);
    await expect(page.locator('#toast')).toContainText('没有改成');
    expect(errors).toEqual([]);
  } finally {
    mock.server.close();
  }
});

// holdGets keeps the page's GET /api/tasks from the mock: answer(body)
// answers the oldest held one with body, pass() lets them through and stops
// holding.
async function holdGets(page) {
  const held = [];
  let on = true;
  await page.route('**/api/tasks', (route) => {
    if (!on || route.request().method() !== 'GET') return route.fallback();
    held.push(route);
  });
  return {
    held,
    answer: (body) => held.shift().fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) }),
    async pass() {
      on = false;
      for (const r of held.splice(0)) await r.continue();
    },
  };
}

test('任务: a read sent before the POST answered and landing after it does not flip the card back; the phone toggle counts at once', async ({ page }) => {
  test.setTimeout(60000);
  const tasks = { tasks: [
    { id: IDLE_TASK, kind: 'session', title: 'iso 查容量', state: 'doing', at: iso(now - 40 * M), created: iso(now - 40 * M), health: 'idle',
      live: { running: false, last_active: iso(now - 10 * M) } },
    { id: FAIL_TASK, kind: 'session', title: 'iso 写周报', state: 'doing', at: iso(now - 20 * M), created: iso(now - 20 * M), health: 'idle',
      live: { running: false, last_active: iso(now - 15 * M) } },
  ] };
  const mock = await startMockServer({
    ws: true, sessions: sessionsOf([]), tasks, plannerEvents: [], inbox: { open: [], closed: [], counts: { open: 0, badge: 0 } }, planner,
  });
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  try {
    await page.setViewportSize(DESKTOP.viewport);
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click('#abnav-planner');
    await expect(page.locator('#pl-task .tk-head')).toHaveText('任务 2');
    await expect(page.locator('#pl-left-toggle span')).toHaveText('等你 0 · 任务 2');

    // A read goes out (a session_state frame of a listed task) and is held:
    // the list as it is before the change.
    const gets = await holdGets(page);
    const stale = JSON.parse(JSON.stringify(tasks));
    for (const c of mock.wsConnections) c.send({ type: 'session_state', key: IDLE_TASK, state: 'ready' });
    await expect.poll(() => gets.held.length, { timeout: 8000 }).toBe(1);

    const post = await hold(page, '**/api/tasks/state', 'POST');
    await tkCard(page, IDLE_TASK).hover();
    await tkCard(page, IDLE_TASK).locator('.tk-more').click();
    await tkCard(page, IDLE_TASK).locator('[data-action="task-done"]').click();
    await expect(page.locator('#pl-task .tk-head')).toHaveText('任务 1');
    await expect(page.locator('#pl-left-toggle span')).toHaveText('等你 0 · 任务 1');
    await expect.poll(() => post.held.length).toBe(1);
    await post.release();
    await expect.poll(() => mock.tasksCalls).toEqual([{ id: IDLE_TASK, state: 'done' }]);

    // The read from before lands after the POST answered. The read the
    // change asked for goes out only once it landed, and is held too: what
    // shows meanwhile is what the old read was painted as.
    await gets.answer(stale);
    await expect.poll(() => gets.held.length, { timeout: 8000 }).toBe(1);
    await expect(page.locator('#pl-task .tk-sec-done .tk-card[data-id="' + IDLE_TASK + '"]')).toHaveCount(1);
    await expect(page.locator('#pl-task .tk-head')).toHaveText('任务 1');
    await expect(page.locator('#pl-left-toggle span')).toHaveText('等你 0 · 任务 1');
    await gets.pass();
    await expect(page.locator('#pl-task .tk-sec-done .tk-card[data-id="' + IDLE_TASK + '"]')).toHaveCount(1);
    await expect(page.locator('#pl-task .tk-head')).toHaveText('任务 1');
    expect(errors).toEqual([]);
  } finally {
    mock.server.close();
  }
});

const sent = (mock) => mock.sendCalls.map((b) => JSON.parse(b));

async function openAside(page, mock, text) {
  await page.locator('#pl-to').selectOption('new');
  await page.fill('#pl-input', text);
  await page.press('#pl-input', 'Enter');
  await expect.poll(() => sent(mock).length).toBeGreaterThan(0);
  const key = sent(mock).at(-1).key;
  await expect(page.locator('#pl-sess')).toBeVisible();
  await expect(card(page, key)).toHaveCount(1);
  await expect.poll(() => mock.labelCalls.filter((c) => c.key === key && c.status === 200).length).toBe(1);
  return key;
}

test('单独聊: 结束 closes it and drops its card at once; refused, it comes back open. Its rename shows at once, refused it goes back', async ({ page }) => {
  test.setTimeout(90000);
  const data = sessionsOf([row(BETA1, 'beta', 'iso 乙一')]);
  const mock = await startMockServer({ ws: true, sessions: data, plannerEvents: [], planner, sendCreates: true });
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  try {
    await page.setViewportSize(DESKTOP.viewport);
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click('#abnav-planner');
    const key = await openAside(page, mock, 'iso 先问一个问题');
    const name = page.locator('#pl-sess-name');
    await expect(name).toHaveText('单独聊：iso 先问一个问题');

    // Rename: the breadcrumb and the sidebar at once; refused, back.
    const label = await hold(page, '**/api/sessions/label', 'PATCH');
    await page.click('#pl-sess-rename');
    await page.fill('.prompt-input', 'iso 新标题');
    await page.click('.prompt-ok');
    await expect(name).toHaveText('单独聊：iso 新标题');
    await expect(card(page, key)).toContainText('单独聊：iso 新标题');
    await expect.poll(() => label.held.length).toBe(1);
    await stalePoll(page, data);
    await expect(name).toHaveText('单独聊：iso 新标题');
    await label.release(500);
    await expect(name).toHaveText('单独聊：iso 先问一个问题');
    await expect(card(page, key)).toContainText('单独聊：iso 先问一个问题');
    await expect(page.locator('#toast')).toHaveText('没有改成标题');

    // 结束 refused: the view is gone at once, then back with its card.
    const end = await hold(page, '**/api/planner/aside/end', 'POST');
    await page.click('#pl-sess-end');
    await expect(page.locator('#pl-sess')).toBeHidden();
    await expect(card(page, key)).toHaveCount(0);
    await expect.poll(() => end.held.length).toBe(1);
    await end.release(500);
    await expect(card(page, key)).toHaveCount(1);
    await expect(page.locator('#pl-sess')).toBeVisible();
    await expect(name).toHaveText('单独聊：iso 先问一个问题');
    await expect(page.locator('#toast')).toContainText('没有结束单独聊');

    // 结束 for real: gone at once, a read answering before the end keeps it
    // gone, and it stays gone once it ended.
    end.on();
    await page.click('#pl-sess-end');
    await expect(page.locator('#pl-sess')).toBeHidden();
    await expect(card(page, key)).toHaveCount(0);
    await expect.poll(() => end.held.length).toBe(1);
    await stalePoll(page, data);
    await expect(card(page, key)).toHaveCount(0);
    await end.release();
    await expect.poll(() => mock.plannerAsideEndCalls).toEqual([{ key }]);
    await expect(page.locator('#toast')).toHaveText('单独聊已结束，移到了历史');
    await expect(card(page, key)).toHaveCount(0);
    expect(errors).toEqual([]);
  } finally {
    mock.server.close();
  }
});

test('单独聊: 结束 drops its 你开的 card at once too; refused, it comes back; failed after it ended, the lists are read again', async ({ page }) => {
  test.setTimeout(90000);
  const data = sessionsOf([row(BETA1, 'beta', 'iso 乙一')]);
  const tasks = { tasks: [] };
  const mock = await startMockServer({
    ws: true, sessions: data, tasks, plannerEvents: [], inbox: { open: [], closed: [], counts: { open: 0, badge: 0 } }, planner, sendCreates: true,
  });
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  let taskReads = 0;
  page.on('response', (r) => { if (new URL(r.url()).pathname === '/api/tasks') taskReads++; });
  // A session_state frame of a dispatched session has 任务 read again.
  const readTasks = async () => {
    const before = taskReads;
    for (const c of mock.wsConnections) c.send({ type: 'session_state', key: 'task:other0000009', state: 'ready' });
    await expect.poll(() => taskReads, { timeout: 8000 }).toBeGreaterThan(before);
  };
  try {
    await page.setViewportSize(DESKTOP.viewport);
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click('#abnav-planner');
    const key = await openAside(page, mock, 'iso 问个事');
    tasks.tasks.push({ id: key, kind: 'session', group: 'yours', title: '单独聊：iso 问个事', state: 'doing', at: iso(now - M), created: iso(now - M),
      health: 'idle', live: { running: false, last_active: iso(now - M) } });
    await readTasks();
    await expect(tkCard(page, key)).toHaveCount(1);

    // Refused: the 你开的 card goes with the view at once, a read of 任务
    // answering meanwhile keeps it gone, and it is back once refused.
    const end = await hold(page, '**/api/planner/aside/end', 'POST');
    await page.click('#pl-sess-end');
    await expect(tkCard(page, key)).toHaveCount(0);
    await expect(card(page, key)).toHaveCount(0);
    await expect.poll(() => end.held.length).toBe(1);
    await readTasks();
    await expect(tkCard(page, key)).toHaveCount(0);
    await end.release(500);
    await expect(tkCard(page, key)).toHaveCount(1);
    await expect(card(page, key)).toHaveCount(1);
    await expect(page.locator('#toast')).toContainText('没有结束单独聊');

    // The end went through but its answer did not come back: refused as far
    // as the page knows, so it puts the cards back, then reads both lists and
    // the session ended there is gone from them.
    await page.route('**/api/planner/aside/end', async (route) => {
      await route.fetch();
      await route.fulfill({ status: 504, contentType: 'application/json', body: JSON.stringify({ error: 'iso 网关超时' }) });
    });
    await expect(page.locator('#pl-sess')).toBeVisible();
    await page.click('#pl-sess-end');
    await expect.poll(() => mock.plannerAsideEndCalls).toEqual([{ key }]);
    await expect(page.locator('#toast')).toContainText('没有结束单独聊');
    await expect(card(page, key)).toHaveCount(0, { timeout: 5000 });
    await expect(tkCard(page, key)).toHaveCount(0, { timeout: 5000 });
    expect(errors).toEqual([]);
  } finally {
    mock.server.close();
  }
});

test('重命名 in the session header shows at once, through a read from before; a refused PATCH puts the old title back', async ({ page }) => {
  const data = sessionsOf([row(BETA1, 'beta', 'iso 乙一'), row(BETA2, 'beta', 'iso 乙二')]);
  const mock = await startMockServer({ sessions: data });
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  try {
    await page.setViewportSize(DESKTOP.viewport);
    await page.goto(mock.url + '/dashboard');
    await card(page, BETA1).click();
    const title = page.locator('.hdr-title-t');
    await expect(title).toHaveText('iso 乙一');

    const label = await hold(page, '**/api/sessions/label', 'PATCH');
    await page.click('[data-action="session-rename"]');
    await page.fill('.prompt-input', 'iso 改过的名字');
    await page.press('.prompt-input', 'Enter');
    await expect(title).toHaveText('iso 改过的名字');
    await expect(card(page, BETA1)).toContainText('iso 改过的名字');
    await expect.poll(() => label.held.length).toBe(1);
    // The mock still has no label: a read now must not undo it.
    await stalePoll(page, data);
    await expect(card(page, BETA1)).toContainText('iso 改过的名字');
    await expect(title).toHaveText('iso 改过的名字');
    await label.release(500);
    await expect(title).toHaveText('iso 乙一');
    await expect(card(page, BETA1)).toContainText('iso 乙一');
    await expect(card(page, BETA1)).not.toContainText('iso 改过的名字');
    await expect(page.locator('#toast')).toContainText('重命名');

    // Accepted: kept, and the next read agrees.
    await page.click('[data-action="session-rename"]');
    await page.fill('.prompt-input', 'iso 终于改好');
    await page.press('.prompt-input', 'Enter');
    await expect(title).toHaveText('iso 终于改好');
    await expect.poll(() => mock.labelCalls.map((c) => c.label)).toEqual(['iso 终于改好']);
    await expect(page.locator('#toast')).toHaveText('已重命名');
    await stalePoll(page, data);
    await expect(card(page, BETA1)).toContainText('iso 终于改好');

    // A read sent before the PATCH answered and landing after it still has
    // the label from before: the new one stays. The read the rename asks for
    // after is held behind it, so only the old one has landed.
    label.on();
    await page.click('[data-action="session-rename"]');
    await page.fill('.prompt-input', 'iso 第三个名字');
    await page.press('.prompt-input', 'Enter');
    await expect.poll(() => label.held.length).toBe(1);
    const stale = JSON.parse(JSON.stringify(data));
    stale.stats.version += 10;
    stale.sessions[0].iso_stale = true;
    const gets = [];
    await page.route('**/api/sessions', (route) => {
      if (route.request().method() !== 'GET') return route.fallback();
      gets.push(route);
    });
    page.evaluate(() => { window.nz.test.fetchSessions(); });
    // At least: the sidebar's own 5 s poll may be held here too on a slow run.
    await expect.poll(() => gets.length).toBeGreaterThanOrEqual(1);
    const before = gets.length;
    await label.release();
    await expect(page.locator('#toast')).toHaveText('已重命名');
    await expect.poll(() => gets.length).toBeGreaterThan(before);
    await gets.shift().fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(stale) });
    await expect.poll(() => page.evaluate((k) => !!(window.nz.test.sessionsData[k] || {}).iso_stale, BETA1 + '\tlocal')).toBe(true);
    await expect(card(page, BETA1)).toContainText('iso 第三个名字');
    await expect(title).toHaveText('iso 第三个名字');
    await page.unroute('**/api/sessions');
    for (const g of gets.splice(0)) await g.continue();
    await expect(card(page, BETA1)).toContainText('iso 第三个名字');
    expect(errors).toEqual([]);
  } finally {
    mock.server.close();
  }
});
