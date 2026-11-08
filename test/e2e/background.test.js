// E2E: 后台中 (planner RFC D109). A session whose turn is over while its
// background tasks (a Bash run_in_background, a Monitor, a subagent, a
// workflow) still run is neither 在跑 nor 空闲: its task card's badge says
// 后台中 with how many tasks, the first two by their own descriptions
// (escaped, never rendered) and how long since their latest progress; its
// sidebar row says the same in one line. A dispatched session's card and a
// session you opened (你开的) read it alike. The task's end (the next list)
// brings 空闲 back. The Planner's custody block turning observing off reads
// 不再观察.
const fs = require('fs');
const path = require('path');
const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const SHOTS = process.env.NZ_SHOTS_DIR || '';
const PLANNER = 'planner:main';
const TASK = 'task:0a1b2c3d4e5f';
const YOURS = 'dashboard:direct:2026-01-01-120000-1:myproject';
const now = Date.now();
const iso = (ms) => new Date(ms).toISOString();
const M = 60000;
const VIEWPORTS = [
  { name: 'desktop', viewport: { width: 1440, height: 900 } },
  { name: 'phone', viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true },
];
const block = (a) => '```naozhi-action\n' + JSON.stringify(a) + '\n```';

const bench = () => ({ count: 3, tasks: [{ id: 'b1', summary: 'ssh trn2 ./bench.sh <img src=x onerror="window.__bgXSS=1">' }, { id: 'b2', summary: 'gh run watch 4242' }], last_progress: iso(now - 3 * M) });

function tasks() {
  return {
    tasks: [
      { id: TASK, kind: 'session', title: 'iso 跑 trn2 基准', state: 'doing', at: iso(now - 60 * M), created: iso(now - 60 * M), health: 'idle', health_since: iso(now - 20 * M),
        live: { running: false, reply: '基准已在远端跑，等结果', last_active: iso(now - 20 * M), background: bench() } },
      { id: YOURS, kind: 'session', group: 'yours', title: 'iso 盯 CI', state: 'doing', at: iso(now - 30 * M), health: 'idle', health_since: iso(now - 30 * M),
        live: { running: false, reply: '我在后台盯着 CI', last_active: iso(now - 30 * M), background: { count: 1, tasks: [{ id: 'm1', summary: 'watch CI #4242' }], last_progress: iso(now - 30 * M) } } },
    ],
  };
}

function sessions() {
  const s = defaultSessions();
  const yours = s.sessions.find((x) => x.key === YOURS);
  yours.health = { state: 'idle', since: now - 30 * M, background_n: 1, background_tasks: ['watch CI #4242'], background_progress: now - 30 * M };
  s.sessions.push({ key: TASK, state: 'ready', platform: 'task', agent: 'general', cli_name: 'claude', purpose: 'task', user_label: 'iso 跑 trn2 基准',
    purpose_title: 'iso 跑 trn2 基准', workspace: '/home/user/workspace/myproject', project: 'myproject', node: 'local', last_active: now - 20 * M,
    health: { state: 'idle', since: now - 20 * M, background_n: 3, background_tasks: bench().tasks.map((t) => t.summary), background_progress: now - 3 * M } });
  return s;
}

function server(data, extra = {}) {
  return startMockServer({
    ws: true, sessions: sessions(), tasks: data, inbox: { open: [], closed: [], counts: { open: 0, badge: 0 } },
    planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
    plannerEvents: [
      { type: 'text', time: now - 5000, uuid: 'bg-t1', detail: '好，不再看它。\n\n' + block({ type: 'custody', key: YOURS, watch: false }) },
    ],
    ...extra,
  });
}

async function shot(page, name, locator) {
  const file = test.info().outputPath(name + '.png');
  await (locator || page).screenshot({ path: file });
  if (SHOTS) {
    fs.mkdirSync(SHOTS, { recursive: true });
    fs.copyFileSync(file, path.join(SHOTS, name + '.png'));
  }
}

const card = (page, id) => page.locator('#pl-task .tk-card[data-id="' + id + '"]');
const row = (page, key) => page.locator('#session-list .session-card[data-key="' + key + '"]');

for (const vp of VIEWPORTS) {
  test(`${vp.name}: the sidebar row and the task card say 后台中 with the tasks and their latest progress`, async ({ browser }) => {
    const ctx = await browser.newContext({ viewport: vp.viewport, isMobile: !!vp.isMobile, hasTouch: !!vp.hasTouch });
    const page = await ctx.newPage();
    const data = tasks();
    const mock = await server(data);
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      // The sidebar rows, one line each.
      await expect(row(page, TASK).locator('.sc-response')).toHaveText('后台中 · 3 个任务 · ssh trn2 ./bench.sh <img src=x onerror="…、gh run watch 4242 等 · 最近进度 3 分钟前');
      await expect(row(page, YOURS).locator('.sc-response')).toHaveText('后台中 · 1 个任务 · watch CI #4242 · 最近进度 30 分钟前');
      // Cut on a narrow row, whole in its tooltip.
      await expect(row(page, TASK).locator('.sc-response')).toHaveAttribute('title', /^后台中 · 3 个任务 · .* · 最近进度 3 分钟前$/);
      await shot(page, 'sidebar-' + vp.name);

      await page.click('#abnav-planner');
      if (vp.name === 'phone') await page.click('#pl-left-toggle');
      const line = (id) => card(page, id).locator('.tk-status');
      await expect(line(TASK).locator('.tk-badge')).toHaveText('后台中');
      await expect(line(TASK).locator('.tk-now')).toHaveText('3 个任务 · ssh trn2 ./bench.sh <img src=x onerror="…、gh run watch 4242 等');
      await expect(line(TASK).locator('.tk-time')).toHaveText('最近进度 3 分钟前');
      await expect(card(page, TASK)).toHaveAttribute('data-status', 'bg');
      await expect(line(YOURS).locator('.tk-badge')).toHaveText('后台中');
      await expect(line(YOURS).locator('.tk-now')).toHaveText('1 个任务 · watch CI #4242');
      await expect(line(YOURS).locator('.tk-time')).toHaveText('最近进度 30 分钟前');
      // The badge is the run colour, its dot a ring: work goes on, no turn.
      const color = await line(TASK).locator('.tk-badge').evaluate((el) => getComputedStyle(el).color);
      const running = await page.evaluate(() => { const d = document.createElement('span'); d.style.color = 'var(--nz-status-running)'; document.body.append(d); const c = getComputedStyle(d).color; d.remove(); return c; });
      expect(color).toBe(running);
      await expect(page.locator('#pl-task img')).toHaveCount(0);
      expect(await page.evaluate(() => window.__bgXSS)).toBeUndefined();
      await shot(page, 'tasks-' + vp.name, vp.name === 'desktop' ? page.locator('#pl-left') : undefined);

      // The tasks end: the next list reads 空闲 again.
      for (const t of data.tasks) delete t.live.background;
      for (const c of mock.wsConnections) c.send({ type: 'session_state', key: TASK, state: 'ready' });
      await expect(line(TASK).locator('.tk-badge')).toHaveText('空闲');
      await expect(line(TASK).locator('.tk-now')).toHaveText('最后回复：基准已在远端跑，等结果');
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
}

test('the Planner turning observing off reads 不再观察, naming the session', async ({ browser }) => {
  for (const vp of VIEWPORTS) {
    const ctx = await browser.newContext({ viewport: vp.viewport, isMobile: !!vp.isMobile, hasTouch: !!vp.hasTouch });
    const page = await ctx.newPage();
    const mock = await server(tasks());
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      await page.click('#abnav-planner');
      const reply = page.locator('#pl-stream .event.text').first();
      const c = reply.locator('.pl-act-custody');
      await expect(c.locator('.pl-act-h')).toHaveText('不再观察');
      await expect(c.locator('.pl-scard[data-key="' + YOURS + '"]')).toHaveCount(1);
      await expect(reply.locator('.pl-act-bad')).toHaveCount(0);
      await expect(reply).not.toContainText('watch');
      await shot(page, 'watch-off-' + vp.name, reply);
    } finally {
      await ctx.close();
      mock.server.close();
    }
  }
});
