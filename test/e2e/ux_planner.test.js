// E2E: the Planner page's UX review fixes (11-18):
//   - on a phone a session opened in the middle column, and the unfolded
//     left block, push a history entry: the system back closes them as the
//     breadcrumb does, and does not leave the dashboard; the block unfolded
//     over an open session folds back onto it, and the Planner's backs leave
//     a chat opened from 会话 open;
//   - a 等你 whose first read failed is read again;
//   - the one input box keeps each target's draft (each node's Planner its
//     own): a 单独聊's half-written text does not ride to the Planner or to
//     another session;
//   - an ended 单独聊's line opens it under its title, saying it ended;
//   - 结束's toast is a success, not an error's red;
//   - filtered to the new results, 全部已读 shows once.
// Screenshots go to test-results, and to $NZ_SHOTS_DIR when set.
const fs = require('fs');
const path = require('path');
const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { waitForWs } = require('./shim_wait');
const { openSessionCard } = require('./session_nav');

const SHOTS = process.env.NZ_SHOTS_DIR || '';
const PLANNER = 'planner:main';
const planner = { enabled: true, stopped: false, key: PLANNER, alive: true };
const now = Date.now();
const iso = (ms) => new Date(ms).toISOString();
const PHONE = { viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true };
const DESK = { viewport: { width: 1440, height: 900 } };
const TASK_A = 'task:uxa000000001';
const TASK_B = 'task:uxb000000001';

// A reply over 500 characters has its ↗ 追问.
const LONG = 'iso 两个任务在跑。' + '其中一个在等 CI，另一个在等评审。'.repeat(30);
const plannerEvents = () => [
  { type: 'user', detail: '<naozhi-snapshot at="x">\n</naozhi-snapshot>\n\niso 现在都在干什么？', time: now - 120000, uuid: 'ux-u1' },
  { type: 'text', detail: LONG, time: now - 110000, uuid: 'ux-t1' },
  { type: 'result', time: now - 109000, uuid: 'ux-r1' },
];

const ask = { id: 'ux-q1', kind: 'question', level: 'timely', unit: 'task:uxa000000001', title: 'iso 周报发给谁？', why: '它问你',
  created: iso(now - 300000), state: 'open', source: 'ask:ux-q1', decision: { handler: 'ask', actions: ['发给团队', '先别发'] } };
const inbox = () => ({ open: [Object.assign({}, ask)], closed: [], counts: { open: 1, badge: 1 } });

// taskData lists two dispatched sessions, A with a new result.
function taskData() {
  return { tasks: [
    { id: TASK_A, kind: 'session', title: 'iso 周报', state: 'doing', at: iso(now - 600000), created: iso(now - 900000), health: 'idle',
      result_at: iso(now - 600000), new: true, live: { running: false, reply: '写好了', last_active: iso(now - 600000) } },
    { id: TASK_B, kind: 'session', title: 'iso 压测', state: 'doing', at: iso(now - 500000), created: iso(now - 900000), health: 'idle',
      live: { running: false, reply: '跑完一轮', last_active: iso(now - 500000) } },
  ] };
}

function sessionsWithTasks() {
  const s = defaultSessions();
  for (const [key, label] of [[TASK_A, 'iso 周报'], [TASK_B, 'iso 压测']]) {
    s.sessions.push({ key, state: 'ready', platform: 'task', agent: 'general', cli_name: 'claude', user_label: label,
      workspace: '/home/user/workspace', node: 'local', last_active: now - 60000, created_at: now - 900000 });
  }
  return s;
}

async function shot(page, name) {
  const file = test.info().outputPath(name + '.png');
  await page.screenshot({ path: file });
  if (SHOTS) {
    fs.mkdirSync(SHOTS, { recursive: true });
    fs.copyFileSync(file, path.join(SHOTS, name + '.png'));
  }
}

async function openPlanner(page, mock) {
  await page.goto(mock.url + '/dashboard');
  await waitForWs(page, 'CONNECTED');
  await page.evaluate(() => document.getElementById('abnav-planner').click());
  await expect(page.locator('body')).toHaveClass(/nz-view-planner/);
}


test('phone: the system back closes a session opened from 任务 and folds the left block, staying in the dashboard', async ({ browser }) => {
  test.setTimeout(90000);
  const ctx = await browser.newContext(PHONE);
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  const mock = await startMockServer({ ws: true, sessions: sessionsWithTasks(), tasks: taskData(), plannerEvents: plannerEvents(), planner,
    inbox: { open: [], closed: [], counts: { open: 0, badge: 0 } }, custody: { paused: false, sessions: [] } });
  try {
    // A page before the dashboard: a back that leaves it lands there.
    await page.goto('about:blank');
    await openPlanner(page, mock);
    await expect(page.locator('#pl-stream .event.text')).toHaveCount(1);
    const pane = page.locator('#pl-sess');
    const main = page.locator('#planner-main');

    // 任务 title → the session takes the screen; back closes it.
    await page.click('#pl-left-toggle');
    await expect(main).toHaveClass(/pl-left-open/);
    await page.locator(`#pl-task .tk-card[data-id="${TASK_A}"] .tk-open`).click();
    await expect(pane).toBeVisible();
    await expect(page.locator('#pl-sess-name')).toHaveText('iso 周报');
    await shot(page, 'phone-sess-open');
    await page.goBack();
    await expect(pane).toBeHidden();
    expect(page.url()).toContain('/dashboard');
    await expect(main).not.toHaveClass(/pl-left-open/);
    await expect(page.locator('#pl-events')).toBeVisible();
    await shot(page, 'phone-sess-back');

    // The unfolded left block: back folds it.
    await page.click('#pl-left-toggle');
    await expect(main).toHaveClass(/pl-left-open/);
    await page.goBack();
    await expect(main).not.toHaveClass(/pl-left-open/);
    expect(page.url()).toContain('/dashboard');

    // The breadcrumb takes its entry along: one back after it leaves the page,
    // as it did before the session opened.
    await page.click('#pl-left-toggle');
    await page.locator(`#pl-task .tk-card[data-id="${TASK_B}"] .tk-open`).click();
    await expect(pane).toBeVisible();
    await page.click('#pl-sess .pl-crumb [data-action="planner-close-session"]');
    await expect(pane).toBeHidden();
    await expect.poll(() => page.evaluate(() => history.state && history.state.view)).toBeFalsy();
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('a 等你 whose first read failed is read again with the socket up', async ({ browser }) => {
  test.setTimeout(90000);
  const ctx = await browser.newContext(DESK);
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  const mock = await startMockServer({ ws: true, plannerEvents: plannerEvents(), planner, inbox: inbox(), tasks: { tasks: [] }, custody: { paused: false, sessions: [] } });
  try {
    let failing = true;
    await page.route((u) => u.pathname === '/api/inbox', (route) =>
      failing ? route.fulfill({ status: 500, contentType: 'application/json', body: '{"error":"boom"}' }) : route.fallback());
    await openPlanner(page, mock);
    const wait = page.locator('#pl-wait');
    await expect(wait.locator('.pl-empty')).toHaveText('连上后显示');
    await shot(page, 'inbox-failed');

    // The socket stays up and the list does not change: no frame re-reads it.
    failing = false;
    await expect(wait).toContainText('iso 周报发给谁？', { timeout: 40000 });
    await expect(wait).not.toContainText('连上后显示');
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('the input box keeps each target\'s draft: a 单独聊\'s, a task\'s, the Planner\'s', async ({ browser }) => {
  test.setTimeout(90000);
  const ctx = await browser.newContext(DESK);
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  const mock = await startMockServer({ ws: true, sessions: sessionsWithTasks(), tasks: taskData(), plannerEvents: plannerEvents(), planner, sendCreates: true,
    inbox: { open: [], closed: [], counts: { open: 0, badge: 0 } }, custody: { paused: false, sessions: [] } });
  try {
    await openPlanner(page, mock);
    const input = page.locator('#pl-input');
    await input.fill('iso 给 Planner 的半句');
    await page.locator('#pl-to').selectOption('new');
    // 发给 新对话 is picked with the text typed: it sends it.
    await input.fill('iso 第一句');
    await input.press('Enter');
    await expect(page.locator('#pl-sess-name')).toHaveText('单独聊：iso 第一句');
    await expect(input).toHaveValue('');
    await input.fill('iso 写到一半的话');
    await page.click('#pl-sess .pl-crumb [data-action="planner-close-session"]');
    await expect(page.locator('#pl-sess')).toBeHidden();
    await expect(input).toHaveValue('');
    await shot(page, 'draft-back-planner');
    await input.press('Enter');
    expect(mock.plannerSendCalls).toEqual([]);

    // Task A, then task B straight from it: each its own.
    await page.locator(`#pl-task .tk-card[data-id="${TASK_A}"] .tk-open`).click();
    await expect(page.locator('#pl-sess-name')).toHaveText('iso 周报');
    await expect(input).toHaveValue('');
    await input.fill('iso 给 A 的话');
    await page.locator(`#pl-task .tk-card[data-id="${TASK_B}"] .tk-open`).click();
    await expect(page.locator('#pl-sess-name')).toHaveText('iso 压测');
    await expect(input).toHaveValue('');
    await page.locator(`#pl-task .tk-card[data-id="${TASK_A}"] .tk-open`).click();
    await expect(input).toHaveValue('iso 给 A 的话');

    // The 单独聊's comes back with it.
    await page.click('#pl-sess .pl-crumb [data-action="planner-close-session"]');
    await expect(page.locator('#pl-stream .pl-dyn')).not.toHaveCount(0, { timeout: 15000 });
    await page.locator('#pl-stream .pl-dyn-h').click();
    await page.locator('#pl-stream .pl-dyn-line').first().click();
    await expect(page.locator('#pl-sess-name')).toHaveText('单独聊：iso 第一句');
    await expect(input).toHaveValue('iso 写到一半的话');
    expect(mock.plannerSendCalls).toEqual([]);
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('an ended 单独聊\'s line opens it under its title, saying it ended; 结束\'s toast is a success', async ({ browser }) => {
  test.setTimeout(60000);
  for (const [name, opts] of [['desk-light', Object.assign({ colorScheme: 'light' }, DESK)], ['phone-dark', Object.assign({ colorScheme: 'dark' }, PHONE)]]) {
    const ctx = await browser.newContext(opts);
    const page = await ctx.newPage();
    const errors = [];
    page.on('pageerror', (e) => errors.push(e.message));
    const mock = await startMockServer({ ws: true, plannerEvents: plannerEvents(), planner, sendCreates: true });
    try {
      await openPlanner(page, mock);
      await page.locator('#pl-to').selectOption('new');
      await page.fill('#pl-input', 'iso 今天几号？顺便看看天气');
      await page.locator('#pl-send').click();
      await expect(page.locator('#pl-sess-end')).toBeVisible();
      await page.click('#pl-sess-end');
      const toast = page.locator('#toast');
      await expect(toast).toHaveText('单独聊已结束，移到了历史');
      await expect(toast).toHaveClass(/\bsuccess\b/);
      await shot(page, 'aside-ended-toast-' + name);

      await expect(page.locator('#pl-stream .pl-dyn')).not.toHaveCount(0, { timeout: 15000 });
      await page.locator('#pl-stream .pl-dyn-h').click();
      await page.locator('#pl-stream .pl-dyn-line').first().click();
      await expect(page.locator('#pl-sess')).toBeVisible();
      await expect(page.locator('#pl-sess-name')).toHaveText('单独聊：iso 今天几号？');
      await expect(page.locator('#pl-target-to')).toHaveText('→ 单独聊：iso 今天几号？');
      await expect(page.locator('#pl-target-note')).toHaveText('已结束，在历史里');
      await expect(page.locator('#pl-input')).toBeDisabled();
      await shot(page, 'aside-ended-reopen-' + name);
      expect(errors).toEqual([]);
    } finally {
      await ctx.close();
      mock.server.close();
    }
  }
});

test('desktop: filtered to the new results, 全部已读 shows once', async ({ browser }) => {
  test.setTimeout(60000);
  const ctx = await browser.newContext(DESK);
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  const mock = await startMockServer({ ws: true, tasks: taskData(), plannerEvents: plannerEvents(), planner,
    inbox: { open: [], closed: [], counts: { open: 0, badge: 0 } }, custody: { paused: false, sessions: [] } });
  try {
    await openPlanner(page, mock);
    const news = page.locator('#pl-news [data-action="tasks-only-new"]');
    await expect(news).toHaveText('1 件新结果');
    const seenAll = page.locator('[data-action="tasks-seen-all"]:visible');
    await expect(seenAll).toHaveCount(1);
    await news.click();
    await expect(page.locator('#pl-task .tk-filter')).toBeVisible();
    await expect(seenAll).toHaveCount(1);
    await expect(page.locator('#pl-task .tk-filter [data-action="tasks-seen-all"]')).toBeVisible();
    await shot(page, 'news-filtered');
    // Back to all: the header has it again.
    await page.locator('#pl-task .tk-filter [data-action="tasks-only-new"]').click();
    await expect(page.locator('#pl-task .tk-filter')).toHaveCount(0);
    await expect(page.locator('#pl-news .pl-news-all')).toBeVisible();
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('phone: folding the left block unfolded over a session shows the session again', async ({ browser }) => {
  test.setTimeout(90000);
  const ctx = await browser.newContext(PHONE);
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  const mock = await startMockServer({ ws: true, sessions: sessionsWithTasks(), tasks: taskData(), plannerEvents: plannerEvents(), planner,
    inbox: { open: [], closed: [], counts: { open: 0, badge: 0 } }, custody: { paused: false, sessions: [] } });
  try {
    await page.goto('about:blank');
    await openPlanner(page, mock);
    const pane = page.locator('#pl-sess');
    const main = page.locator('#planner-main');
    await page.click('#pl-left-toggle');
    await page.locator(`#pl-task .tk-card[data-id="${TASK_A}"] .tk-open`).click();
    await expect(page.locator('#pl-sess-name')).toHaveText('iso 周报');

    // The toggle folds it: the session is back.
    await page.click('#pl-left-toggle');
    await expect(main).toHaveClass(/pl-left-open/);
    await page.click('#pl-left-toggle');
    await expect(main).not.toHaveClass(/pl-left-open/);
    await expect(pane).toBeVisible();
    await expect(page.locator('#pl-sess-name')).toHaveText('iso 周报');
    await shot(page, 'phone-left-fold-over-sess');

    // So does the system back.
    await page.click('#pl-left-toggle');
    await expect(main).toHaveClass(/pl-left-open/);
    await page.goBack();
    await expect(main).not.toHaveClass(/pl-left-open/);
    await expect(pane).toBeVisible();
    await expect(page.locator('#pl-sess-name')).toHaveText('iso 周报');

    // Another session opened from the block takes its place: one back closes it.
    await page.click('#pl-left-toggle');
    await page.locator(`#pl-task .tk-card[data-id="${TASK_B}"] .tk-open`).click();
    await expect(page.locator('#pl-sess-name')).toHaveText('iso 压测');
    await page.goBack();
    await expect(pane).toBeHidden();
    expect(page.url()).toContain('/dashboard');
    await expect.poll(() => page.evaluate(() => history.state && history.state.view)).toBeFalsy();
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('phone: closing a session on the Planner page leaves the chat opened from 会话 open', async ({ browser }) => {
  test.setTimeout(90000);
  const ctx = await browser.newContext(PHONE);
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  const mock = await startMockServer({ ws: true, sessions: sessionsWithTasks(), tasks: taskData(), plannerEvents: plannerEvents(), planner,
    inbox: { open: [], closed: [], counts: { open: 0, badge: 0 } }, custody: { paused: false, sessions: [] } });
  const CHAT = 'dashboard:direct:2026-01-01-120002-3:myproject';
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await openSessionCard(page, page.locator(`.session-card[data-key="${CHAT}"]`));
    const body = page.locator('body');
    await expect(body).toHaveClass(/mobile-chat-view/);
    await page.evaluate(() => document.getElementById('abnav-planner').click());
    await expect(body).toHaveClass(/nz-view-planner/);
    const pane = page.locator('#pl-sess');

    // The breadcrumb, then the system back.
    for (const how of ['crumb', 'back']) {
      await page.click('#pl-left-toggle');
      await page.locator(`#pl-task .tk-card[data-id="${TASK_A}"] .tk-open`).click();
      await expect(pane).toBeVisible();
      if (how === 'crumb') await page.click('#pl-sess .pl-crumb [data-action="planner-close-session"]');
      else await page.goBack();
      await expect(pane).toBeHidden();
      await expect(body).toHaveClass(/mobile-chat-view/);
    }
    await page.evaluate(() => document.getElementById('abnav-chat').click());
    await expect(body).toHaveClass(/mobile-chat-view/);
    await expect(page.locator(`.session-card[data-key="${CHAT}"]`)).toHaveClass(/\bactive\b/);
    await shot(page, 'phone-chat-kept');
    // Its own back still leaves it.
    await page.goBack();
    await expect(body).toHaveClass(/mobile-list-view/);
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

const NODE = 'pc';
function sessionsWithNode() {
  const s = sessionsWithTasks();
  s.nodes = { local: { display_name: 'EC2', status: 'ok' }, [NODE]: { display_name: 'MBP', status: 'ok', caps: ['files', 'planner', 'subscribe-history'] } };
  return s;
}
const remoteNode = (inboxOpen) => ({ [NODE]: {
  planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
  tasks: { tasks: [] }, custody: { paused: false, sessions: [] },
  inbox: { open: inboxOpen || [], closed: [], counts: { open: 0, badge: 0 } },
  events: { [PLANNER]: [{ type: 'text', detail: 'iso 这里是 MBP 的 Planner。', time: now - 50000, uuid: 'ux-m1' }] },
} });

test('desktop: each node\'s Planner keeps its own draft', async ({ browser }) => {
  test.setTimeout(90000);
  const ctx = await browser.newContext(DESK);
  const page = await ctx.newPage();
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  const mock = await startMockServer({ ws: true, sessions: sessionsWithNode(), tasks: taskData(), plannerEvents: plannerEvents(), planner,
    remote: remoteNode(), inbox: { open: [], closed: [], counts: { open: 0, badge: 0 } }, custody: { paused: false, sessions: [] } });
  try {
    await openPlanner(page, mock);
    const pick = page.locator('#pl-host select');
    const input = page.locator('#pl-input');
    const stream = page.locator('#pl-stream');
    await expect(pick).toHaveValue('local');
    await input.fill('iso 给 EC2 的半句');
    await page.locator(`#pl-task .tk-card[data-id="${TASK_A}"] .tk-open`).click();
    await expect(page.locator('#pl-sess-name')).toHaveText('iso 周报');
    await expect(input).toHaveValue('');

    // The page flips to MBP's Planner with the session open (the header's
    // switch is behind it; a sessions list re-settling the node does it): the
    // box is MBP's, never EC2's text.
    await page.evaluate(async () => (await import('/static/planner_host.js')).choosePlannerNode('pc'));
    await expect(page.locator('#pl-sess')).toBeHidden();
    await expect(stream).toContainText('iso 这里是 MBP 的 Planner。');
    await expect(pick).toHaveValue(NODE);
    await expect(input).toHaveValue('');
    await shot(page, 'draft-node-switch');
    // From one Planner to the other with the header's switch.
    await input.fill('iso 给 MBP 的半句');
    await pick.selectOption('local');
    await expect(stream).toContainText('iso 现在都在干什么');
    await expect(input).toHaveValue('iso 给 EC2 的半句');
    await pick.selectOption(NODE);
    await expect(stream).toContainText('iso 这里是 MBP 的 Planner。');
    await expect(input).toHaveValue('iso 给 MBP 的半句');
    expect(mock.plannerSendCalls).toEqual([]);
    expect(mock.remoteCalls.filter((c) => c.path === '/api/planner/send')).toEqual([]);
    expect(errors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
