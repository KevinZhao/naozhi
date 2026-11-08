// E2E: what waits on you, where you look first (planner RFC §15.13 v0.7.0,
// UX review 1, 5 and 10).
//   1. The Planner page's left column: 等你's heading counts what it lists
//      (the badge's number), 任务's counts every task, and on a phone 等你
//      comes first.
//   5. The idle Home: "等你 N · 运行中 N · 失败 N" over the latest three items
//      that wait on you, each a click away from its card or session; with
//      the Planner off (no inbox) only 运行中 and 失败; nothing with every
//      count 0. The card comes back with the empty main area.
//   10. A dispatched session's header: its task's badge and 在 Planner 中查看,
//      which opens the page on the card, lit; no "(模型未配置)".
// Crowded and sparse data, desktop 1440×900 and phone 390×844; the shots go
// to test-results and, with NZ_SHOTS_DIR set, there too.
const fs = require('fs');
const path = require('path');
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');
const { openSessionCard } = require('./session_nav');

const now = Date.now();
const iso = (ms) => new Date(ms).toISOString();
const M = 60000, H = 3600000;
const PLANNER = 'planner:main';
const DESKTOP = { viewport: { width: 1440, height: 900 } };
const PHONE = { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true };
const VIEWPORTS = [['desktop', DESKTOP], ['phone', PHONE]];

async function shot(page, name) {
  await page.waitForTimeout(300);
  const file = test.info().outputPath(name + '.png');
  await page.screenshot({ path: file });
  const dir = process.env.NZ_SHOTS_DIR;
  if (dir) {
    fs.mkdirSync(dir, { recursive: true });
    fs.copyFileSync(file, path.join(dir, name + '.png'));
  }
}

// ---- seed data (after the UX review's crowded and sparse states) ----

const projects = ['naozhi', 'checkout-service', 'data-pipeline', 'docs-site', 'mobile-app', 'infra-terraform'];
const titles = [
  '修复登录页在 Safari 下的跳转循环问题并补上回归测试',
  '整理 11 月第二周的周报，附上 CI 通过率和 P0 进展',
  '把 checkout-service 的超时重试从 3 次改成指数退避，并在预发环境验证',
  '调研 Qwen3-ASR 在会议录音场景下的中文准确率，和 Whisper large-v3 对比',
  '给 data-pipeline 的 nightly 任务加上失败告警',
  '升级 terraform provider 到 6.x 并修所有 deprecation',
  '写 hello.md',
  '对比三家向量数据库的价格与延迟，给出一页纸结论',
  'review PR #3899：托管退避逻辑',
  '回复 Slack 上 @alice 关于发版窗口的问题',
];
const healthOf = ['active', 'waiting', 'failed', 'idle', 'stalled', 'idle', 'active', 'idle'];
const taskKey = (i) => 'task:t' + String(i).padStart(10, '0');
const dashKey = (i) => 'dashboard:direct:2026-11-0' + (1 + (i % 7)) + '-1200' + String(i).padStart(2, '0') + '-' + i + ':general';

function sess(key, extra) {
  return Object.assign({ key, state: 'ready', platform: 'dashboard', agent: 'general', cli_name: 'claude', cli_version: '2.1.0', node: 'local', last_active: now - 10 * M }, extra);
}

function crowdedSessions() {
  const list = [];
  for (let i = 0; i < 12; i++) {
    const h = healthOf[i % healthOf.length];
    const p = projects[i % projects.length];
    const t = titles[i % titles.length];
    const extra = {
      platform: 'task', purpose: 'task', user_label: t, purpose_title: t, workspace: '/Users/me/workspace/' + p, project: p,
      last_active: now - (i * 17 + 2) * M, last_prompt: t, state: h === 'active' ? 'running' : 'ready',
      health: { state: h, since: now - (i * 7 + 1) * M },
    };
    if (h === 'failed') Object.assign(extra.health, { death_reason: 'cli_exited_code_-1', error_class: 'rate_limit' });
    list.push(sess(taskKey(i), extra));
  }
  for (let i = 0; i < 16; i++) {
    const p = projects[i % projects.length];
    const h = ['idle', 'idle', 'active', 'idle', 'failed', 'idle', 'waiting', 'idle'][i % 8];
    list.push(sess(dashKey(i), {
      workspace: '/Users/me/workspace/' + p, project: p, last_prompt: titles[(i + 5) % titles.length],
      state: h === 'active' ? 'running' : 'ready', last_active: now - (i * 41 + 5) * M,
      health: { state: h, since: now - (i * 13 + 1) * M, ...(h === 'failed' ? { death_reason: 'cli_exited_code_1', error_class: 'other' } : {}) },
    }));
  }
  const running = list.filter((s) => s.state === 'running').length;
  return {
    sessions: list,
    stats: { total: list.length, running, ready: list.length - running, active: list.length, uptime: '3d4h', backend: 'cc', max_procs: 20, default_workspace: '/Users/me/workspace', agents: ['general'],
      projects: projects.map((n) => ({ name: n, path: '/Users/me/workspace/' + n, favorite: false, github: true, git_remote_url: '' })), version: 1 },
  };
}

function crowdedInbox() {
  const open = [
    { id: 'in_d1', kind: 'decision', level: 'timely', title: '周报用哪个模板？新版模板多了"风险"一栏', why: 'Planner 问你', created: iso(now - 5 * M), state: 'open', source: 'planner:d1', decision: { handler: 'ask', actions: ['新版模板', '旧版模板'] } },
    { id: 'in_d2', kind: 'question', level: 'timely', unit: PLANNER, title: '今天下午发 v0.6.2 吗？', why: 'Planner 问你', created: iso(now - 25 * M), state: 'open', source: 'ask:2', decision: { handler: 'ask', actions: ['发', '不发'] } },
    { id: 'in_q1', kind: 'question', level: 'timely', unit: taskKey(1), title: '「整理 11 月第二周的周报」在等你回复', why: '有提问等你回答超过 10 分钟', created: iso(now - 40 * M), state: 'open', source: 'waiting:' + taskKey(1), auto_close: true },
    { id: 'in_q2', kind: 'question', level: 'timely', unit: dashKey(6), title: '会话在等你回复：要不要把相关的文档也一起更新？', why: '有提问等你回答超过 10 分钟', created: iso(now - 2 * H), state: 'open', source: 'waiting:x', auto_close: true },
    { id: 'in_e1', kind: 'error', level: 'timely', unit: taskKey(2), title: '会话失败：把 checkout-service 的超时重试改成指数退避', why: '限流（rate_limit）', created: iso(now - 3 * H), state: 'open', source: 'health:' + taskKey(2) },
    { id: 'in_e3', kind: 'error', level: 'timely', unit: dashKey(4), title: '会话失败：data-pipeline', why: 'CLI 退出码 1', created: iso(now - 7 * H), state: 'open', source: 'health:y' },
    { id: 'in_n', kind: 'notice', level: 'info', title: '巡检：一切正常', created: iso(now - 9 * H), state: 'open', source: 'planner:n' },
  ];
  return { open, closed: [], counts: { open: open.length, badge: 6 } };
}

function crowdedTasks() {
  const tasks = [];
  for (let i = 0; i < 12; i++) {
    const h = healthOf[i % healthOf.length];
    const state = h === 'waiting' ? 'waiting' : h === 'failed' ? 'failed' : (i > 9 ? 'done' : 'doing');
    tasks.push({ id: taskKey(i), kind: 'session', title: titles[i % titles.length], state, by: state === 'done' ? 'planner' : 'health', why: state === 'done' ? '它汇报已完成并贴了 PR 链接' : '',
      at: iso(now - (i * 9 + 1) * M), created: iso(now - (i * 30 + 60) * M), health: h, live: { running: h === 'active', activity: h === 'active' ? '运行 go test ./internal/...' : '', last_active: iso(now - (i * 17 + 2) * M) } });
  }
  for (let i = 0; i < 3; i++) tasks.push({ id: 'pr_r' + i, kind: 'promise', title: ['明早 9 点提醒你看压测结果', '周五前提醒你交季度 OKR 自评', '下午 3 点提醒你和 Bob 对一下发版窗口'][i], state: 'remind', at: iso(now - i * H), created: iso(now - i * H), due: iso(now + (i + 1) * 5 * H) });
  for (let i = 0; i < 5; i++) tasks.push({ id: 'task:done' + i, kind: 'session', title: '已完成的任务 ' + i, state: 'done', by: 'planner', why: '它汇报已完成', at: iso(now - (i + 1) * H), created: iso(now - (i + 3) * H) });
  return { tasks };
}

function sparseSessions() {
  return {
    sessions: [sess('dashboard:direct:2026-11-08-090000-1:general', { workspace: '/Users/me/workspace/naozhi', project: 'naozhi', last_prompt: 'hello', health: { state: 'idle', since: now - M } })],
    stats: { total: 1, running: 0, ready: 1, active: 1, uptime: '5m', backend: 'cc', max_procs: 20, default_workspace: '/Users/me/workspace', agents: ['general'], projects: [], version: 1 },
  };
}

const planner = { enabled: true, stopped: false, key: PLANNER, alive: true };
const crowded = () => ({ ws: true, sessions: crowdedSessions(), inbox: crowdedInbox(), tasks: crowdedTasks(), plannerEvents: [], planner,
  custody: { paused: false, sessions: [{ key: taskKey(0), on: true, state: 'on', sent: 0, max: 8 }, { key: taskKey(1), on: true, state: 'on', sent: 0, max: 8 }] } });
const sparse = () => ({ ws: true, sessions: sparseSessions(), inbox: { open: [], closed: [], counts: { open: 0, badge: 0 } }, tasks: { tasks: [] }, plannerEvents: [], planner,
  custody: { paused: false, sessions: [] } });

// ---- 1. the left column ----

for (const [name, vp] of VIEWPORTS) {
  test(`the Planner page's left column counts 等你 and 任务, 等你 first (${name})`, async ({ browser }) => {
    for (const [label, data] of [['crowded', crowded()], ['sparse', sparse()]]) {
      const ctx = await browser.newContext(vp);
      const page = await ctx.newPage();
      const mock = await startMockServer(data);
      try {
        await page.goto(mock.url + '/dashboard');
        await waitForWs(page, 'CONNECTED');
        const n = label === 'crowded' ? 6 : 0;
        await expect(page.locator('#abnav-planner-count')).toHaveText(String(n));
        await page.click('#abnav-planner');
        const toggle = page.locator('#pl-left-toggle');
        if (name === 'phone') {
          // 任务 on the folded block counts what is in progress: 等你 2,
          // 失败 2 and the 7 running, stalled or idle ones.
          await expect(toggle).toHaveText(label === 'crowded' ? '等你 6 · 任务 11' : '等你 0 · 任务 0');
          await toggle.click();
          await expect(toggle).toHaveAttribute('aria-expanded', 'true');
        }
        const head = page.locator('#pl-wait-h');
        const tkHead = page.locator('#pl-task .tk-head');
        if (label === 'crowded') {
          // The same number as the badge and the folded block's.
          await expect(head).toHaveText('等你 6');
          await expect(page.locator('#pl-wait .pl-card')).toHaveCount(6);
          await expect(tkHead).toHaveText('任务 ' + crowdedTasks().tasks.length);
          await expect(page.locator('#pl-task .tk-sec > .tk-sec-h')).toHaveText(['进行中 11', '待提醒 3', '已完成 6']);
        } else {
          // An empty group says so; its heading has no 0.
          await expect(head).toHaveText('等你');
          await expect(tkHead).toHaveText('任务');
        }
        // 等你 comes first on a phone too: it is the first thing unfolded.
        const [wait, tk] = await Promise.all([page.locator('#pl-wait').boundingBox(), page.locator('#pl-task').boundingBox()]);
        expect(wait.y).toBeLessThan(tk.y);
        if (name === 'phone') await expect(head).toBeInViewport();
        await shot(page, `left-${label}-${name}`);
      } finally {
        await ctx.close();
        mock.server.close();
      }
    }
  });
}

// ---- 5. the idle Home ----

// sidebarDots counts the sidebar's running and failed status dots.
const sidebarDots = (page) => page.evaluate(() => ({
  running: document.querySelectorAll('#session-list .session-card .sc-dot.dot-running').length,
  failed: document.querySelectorAll('#session-list .session-card .sc-dot.dot-failed').length,
}));

test('the idle Home sums up what needs you and goes straight to it (desktop)', async ({ browser }) => {
  const ctx = await browser.newContext(DESKTOP);
  const page = await ctx.newPage();
  const mock = await startMockServer(crowded());
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    const card = page.locator('#home-attn .ha-card');
    await expect(card).toBeVisible();
    await expect(page.locator('#abnav-planner-count')).toHaveText('6');
    const dots = await sidebarDots(page);
    expect(dots.running).toBeGreaterThan(0);
    expect(dots.failed).toBeGreaterThan(0);
    // 等你 is the badge's number, not a second count; 运行中 and 失败 the
    // sidebar's dots.
    await expect(card.locator('.ha-n')).toHaveText(['等你 6', '运行中 ' + dots.running, '失败 ' + dots.failed]);
    // The numbers are text: the rail badge stays the one red count.
    const color = await card.locator('.ha-n-wait b').evaluate((el) => getComputedStyle(el).color);
    const red = await page.locator('#abnav-planner-count').evaluate((el) => getComputedStyle(el).backgroundColor);
    expect(color).not.toBe(red);
    // The latest three, newest first; the rest are a link away.
    const rows = card.locator('.ha-row');
    await expect(rows.locator('.ha-title')).toHaveText(['周报用哪个模板？新版模板多了"风险"一栏', '今天下午发 v0.6.2 吗？', '「整理 11 月第二周的周报」在等你回复']);
    await expect(rows.locator('.ha-kind')).toHaveText(['待你决定', '提问', '提问']);
    await expect(card.locator('.ha-all')).toHaveText('在 Planner 页看全部 6 条');
    await shot(page, 'home-crowded-desktop');

    // An item about a listed session opens that session.
    await rows.nth(2).click();
    await expect(page.locator('.session-card.active')).toHaveAttribute('data-key', taskKey(1));
    await expect(page.locator('#home-attn')).toHaveCount(0);

    // Removing the open session repaints the empty main area: the card comes back.
    await page.locator('.session-card.active').hover();
    await page.locator('.session-card.active .btn-dismiss').click();
    await expect(page.locator('#quick-ask-input')).toBeVisible();
    await expect(page.locator('#home-attn .ha-n-wait')).toHaveText('等你 6');

    // The Planner's own question opens the Planner page on its card, lit.
    await page.locator('#home-attn .ha-row[data-id="in_d2"]').click();
    await expect(page.locator('body')).toHaveClass(/nz-view-planner/);
    await expect(page.locator('#pl-wait .pl-card[data-id="in_d2"]')).toHaveClass(/inbox-lit/);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('the idle Home: Planner off says 运行中 and 失败 only; nothing when every count is 0', async ({ browser }) => {
  const ctx = await browser.newContext(DESKTOP);
  const page = await ctx.newPage();
  // No inbox, no Planner: GET /api/inbox answers 404.
  const off = crowded();
  delete off.inbox;
  delete off.planner;
  delete off.tasks;
  delete off.custody;
  let mock = await startMockServer(off);
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    const card = page.locator('#home-attn .ha-card');
    await expect(card).toBeVisible();
    const dots = await sidebarDots(page);
    await expect(card.locator('.ha-n')).toHaveText(['运行中 ' + dots.running, '失败 ' + dots.failed]);
    await expect(card.locator('.ha-row')).toHaveCount(0);
    await shot(page, 'home-planner-off-desktop');
  } finally {
    mock.server.close();
  }
  // Sparse: nothing waits, runs or failed.
  mock = await startMockServer(sparse());
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await expect(page.locator('#abnav-planner-count')).toBeHidden();
    await expect(page.locator('#quick-ask-input')).toBeVisible();
    await page.waitForTimeout(500);
    await expect(page.locator('#home-attn')).toBeEmpty();
    await expect(page.locator('#home-attn')).toBeHidden();
    await shot(page, 'home-sparse-desktop');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('the phone Home is the session list: its 等你 bar holds the same count', async ({ browser }) => {
  for (const [label, data] of [['crowded', crowded()], ['sparse', sparse()]]) {
    const ctx = await browser.newContext(PHONE);
    const page = await ctx.newPage();
    const mock = await startMockServer(data);
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      if (label === 'crowded') await expect(page.locator('#sw-n')).toHaveText('6');
      else await expect(page.locator('#side-wait')).toBeHidden();
      await shot(page, `home-${label}-phone`);
    } finally {
      await ctx.close();
      mock.server.close();
    }
  }
});

// ---- 10. a dispatched session's header ----

for (const [name, vp] of VIEWPORTS) {
  test(`a dispatched session's header says its task's state and opens its card (${name})`, async ({ browser }) => {
    const ctx = await browser.newContext(vp);
    const page = await ctx.newPage();
    const mock = await startMockServer(crowded());
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      // Before the Planner page was ever open: the header reads the list.
      await openSessionCard(page, page.locator('.session-card[data-key="' + taskKey(1) + '"]'));
      const slot = page.locator('#header-task');
      await expect(slot.locator('.tk-badge')).toHaveText('等你');
      await expect(slot.locator('.tk-badge')).toHaveClass(/tk-b-warn/);
      await expect(slot.locator('.tk-reveal')).toHaveText('在 Planner 中查看');
      // A model not known yet says nothing.
      await expect(page.locator('.main-header')).not.toContainText('模型未配置');
      await expect(page.locator('#header-model')).toHaveCount(0);
      await shot(page, `header-task-waiting-${name}`);

      // The link opens the Planner page on the card, lit; on a phone the
      // left block it is in unfolds.
      await slot.locator('.tk-reveal').click();
      await expect(page.locator('body')).toHaveClass(/nz-view-planner/);
      const tcard = page.locator('#pl-task .tk-card[data-id="' + taskKey(1) + '"]');
      await expect(tcard).toBeInViewport();
      await expect(tcard).toHaveClass(/inbox-lit/);
      if (name === 'phone') await expect(page.locator('#pl-left-toggle')).toHaveAttribute('aria-expanded', 'true');
      await shot(page, `header-task-reveal-${name}`);
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
}

test('a done task\'s link unfolds 已完成; a session the Planner did not dispatch has no task badge', async ({ browser }) => {
  const ctx = await browser.newContext(DESKTOP);
  const page = await ctx.newPage();
  const mock = await startMockServer(crowded());
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await openSessionCard(page, page.locator('.session-card[data-key="' + dashKey(1) + '"]'));
    await expect(page.locator('.main-header')).toBeVisible();
    await page.waitForTimeout(500);
    await expect(page.locator('#header-task')).toBeEmpty();
    // A running task says 在跑.
    await openSessionCard(page, page.locator('.session-card[data-key="' + taskKey(0) + '"]'));
    await expect(page.locator('#header-task .tk-badge')).toHaveText('在跑');
    // A done one: 已完成, its card in the folded part.
    await openSessionCard(page, page.locator('.session-card[data-key="' + taskKey(11) + '"]'));
    await expect(page.locator('#header-task .tk-badge')).toHaveText('已完成');
    await page.locator('#header-task .tk-reveal').click();
    await expect(page.locator('#pl-task .tk-sec-done')).toHaveAttribute('open', '');
    await expect(page.locator('#pl-task .tk-card[data-id="' + taskKey(11) + '"]')).toBeInViewport();
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
