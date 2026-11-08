// E2E: the sessions you opened that the gateway observes (planner RFC
// §15.12 ⑥, D104). 任务 has a part "你开的" (group "yours") with the same
// card as a dispatched session's: the state by health (等你, else 在跑 /
// 空闲 from health and live), a blue dot when a turn ended with you away,
// no custody mark (off is their default), "⋯" only marking it done or back
// in progress (no 停止, no 重新打开); the group's heading counts what the
// Planner dispatched, not these. A turn of one starting or ending (its
// session_state frame) refetches the list as a dispatched one's does. The
// session header has 观察 beside the custody chip (not on a dispatched
// session): a click turns it off through PUT /api/custody {key, watch}, a
// toast saying what that means, and the card leaves 你开的; the Planner
// turning it off is a note in the session.
const fs = require('fs');
const path = require('path');
const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const SHOTS = process.env.NZ_SHOTS_DIR || '';
const PLANNER = 'planner:main';
const A = 'dashboard:direct:2026-01-01-120000-1:myproject';
const B = 'dashboard:direct:2026-01-01-120001-2:otherproject';
const IM = 'feishu:group:oc_1#t9:general';
const TASK = 'task:0a1b2c3d4e5f';
const DONE = 'slack:direct:u1:general';
const now = Date.now();
const iso = (ms) => new Date(ms).toISOString();
const M = 60000;

function tasks() {
  return {
    tasks: [
      { id: TASK, kind: 'session', title: 'iso 修登录页', state: 'doing', at: iso(now - 60 * M), created: iso(now - 60 * M), health: 'active', health_since: iso(now - 5 * M),
        live: { running: true, activity: 'Bash: go test ./...', last_active: iso(now - 10000) } },
      { id: IM, kind: 'session', group: 'yours', title: 'iso 飞书里问的 trn2', state: 'waiting', by: 'health', at: iso(now - 8 * M), health: 'waiting', health_since: iso(now - 8 * M),
        live: { running: false, reply: '要用哪个区域？', last_active: iso(now - 8 * M) } },
      { id: A, kind: 'session', group: 'yours', title: 'iso 压测 trn2-moe-large', state: 'doing', at: iso(now - 20 * M), health: 'active', health_since: iso(now - 20 * M),
        live: { running: true, activity: 'Bash: python bench.py', last_active: iso(now - 20000) } },
      { id: B, kind: 'session', group: 'yours', title: 'iso 调研 ASR', state: 'doing', at: iso(now - 40 * M), health: 'idle', health_since: iso(now - 40 * M),
        result_at: iso(now - 40 * M), new: true, live: { running: false, reply: 'Qwen3-ASR 中文最好', last_active: iso(now - 40 * M) } },
      { id: DONE, kind: 'session', group: 'yours', title: 'iso 写周报', state: 'done', by: 'planner', why: '周报已发', at: iso(now - 90 * M), health: 'idle', health_since: iso(now - 90 * M) },
    ],
  };
}

function sessions() {
  const s = defaultSessions();
  s.sessions.push({ key: TASK, state: 'ready', platform: 'task', agent: 'general', cli_name: 'claude', purpose: 'task', user_label: 'iso 修登录页',
    purpose_title: 'iso 修登录页', workspace: '/home/user/workspace/myproject', project: 'myproject', node: 'local', last_active: now - 10000 });
  return s;
}

function server(data, extra = {}) {
  return startMockServer({
    ws: true, sessions: sessions(), tasks: data, plannerEvents: [], inbox: { open: [], closed: [], counts: { open: 0, badge: 0 } },
    planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
    // Custody on for the dispatched session only: a session you opened has
    // it off by default, which its card does not mark.
    custody: { paused: false, sessions: [{ key: TASK, on: true, state: 'on', sent: 0, max: 8 }] },
    ...extra,
  });
}

async function shot(page, name) {
  const file = test.info().outputPath(name + '.png');
  await page.screenshot({ path: file });
  if (SHOTS) {
    fs.mkdirSync(SHOTS, { recursive: true });
    fs.copyFileSync(file, path.join(SHOTS, name + '.png'));
  }
}

const card = (page, id) => page.locator('#pl-task .tk-card[data-id="' + id + '"]');
const lastConn = (mock) => mock.wsConnections[mock.wsConnections.length - 1];

async function openSession(page, mock, key, events = []) {
  await page.evaluate((k) => window.nz.test.selectSession(k, 'local'), key);
  const conn = lastConn(mock);
  await expect.poll(() => conn.messages.some((m) => m.type === 'subscribe' && m.key === key)).toBe(true);
  conn.send({ type: 'subscribed', key, state: 'ready' });
  conn.send({ type: 'history', key, initial: true, events });
}

const VIEWPORTS = [['desktop', { viewport: { width: 1440, height: 900 } }], ['mobile', { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true }]];

for (const [name, opts] of VIEWPORTS) {
  test(`你开的 in 任务 (${name})`, async ({ browser }) => {
    test.setTimeout(60000);
    const ctx = await browser.newContext(opts);
    const page = await ctx.newPage();
    const data = tasks();
    const mock = await server(data);
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      await page.click('#abnav-planner');
      if (name === 'mobile') {
        // The folded block's 任务 counts what the Planner dispatched.
        await expect(page.locator('#pl-left-toggle')).toHaveText('等你 0 · 任务 1');
        await page.click('#pl-left-toggle');
      }
      const group = page.locator('#pl-task');
      await expect(group.locator('.tk-head')).toHaveText('任务 1');
      await expect(group.locator('.tk-sec > .tk-sec-h')).toHaveText(['进行中 1', '你开的 3', '已完成 1']);
      const yours = group.locator('.tk-sec-yours .tk-card');
      await expect(yours.locator('.tk-title')).toHaveText(['iso 飞书里问的 trn2', 'iso 压测 trn2-moe-large', 'iso 调研 ASR']);
      // The state by health, as a dispatched session's card says it.
      await expect(yours.locator('.tk-badge')).toHaveText(['等你', '在跑', '空闲']);
      await expect(card(page, A).locator('.tk-now')).toHaveText('Bash: python bench.py');
      await expect(card(page, B).locator('.tk-now')).toHaveText('最后回复：Qwen3-ASR 中文最好');
      // A turn ended with you away: the blue dot.
      await expect(card(page, B).locator('.tk-top > .tk-new:first-child')).toBeVisible();
      await expect(group.locator('.tk-sec-yours .tk-new')).toHaveCount(1);
      // Custody off is their default: no mark.
      await expect(group.locator('.tk-sec-yours .custody-chip')).toHaveCount(0);
      // The title opens the session in the middle column.
      await expect(card(page, A).locator('.tk-open')).toHaveAttribute('data-key', A);
      await shot(page, 'yours-' + name);

      // "⋯" only marks it: no 停止.
      await card(page, A).locator('.tk-more').click();
      await expect(card(page, A).locator('.tk-act')).toHaveText(['标为已完成']);
      await card(page, A).locator('.tk-done').click();
      await expect.poll(() => mock.tasksCalls).toEqual([{ id: A, state: 'done' }]);
      await expect(group.locator('.tk-sec > .tk-sec-h')).toHaveText(['进行中 1', '你开的 2', '已完成 2']);
      // A done one goes back in progress, and is not reopened.
      await group.locator('.tk-sec-done summary').click();
      await card(page, DONE).locator('.tk-more').click();
      await expect(card(page, DONE).locator('.tk-act')).toHaveText(['标为进行中']);
      await expect(card(page, DONE).locator('.tk-now')).toHaveText('Planner 判断：周报已发');

      // A turn of one ending refetches the list, as a dispatched one's does:
      // ended with you away, it has a dot now.
      const t = data.tasks.find((x) => x.id === IM);
      Object.assign(t, { state: 'doing', health: 'idle', by: '', new: true, result_at: iso(Date.now()) });
      for (const c of mock.wsConnections) c.send({ type: 'session_state', key: IM, state: 'ready' });
      await expect(card(page, IM).locator('.tk-new')).toBeVisible();
      await expect(card(page, IM).locator('.tk-badge')).toHaveText('空闲');
      await shot(page, 'yours-dot-' + name);
    } finally {
      await ctx.close();
      await mock.server.close();
    }
  });

  test(`观察 in the session header (${name})`, async ({ browser }) => {
    test.setTimeout(60000);
    const ctx = await browser.newContext(opts);
    const page = await ctx.newPage();
    const data = tasks();
    const notes = { [B]: [{ id: 'wn1', key: B, time: iso(now - 30 * M), kind: 'custody', event: 'watch_off', by: 'planner' }] };
    const mock = await server(data, { notes });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');

      // A session you opened: custody's chip and 观察 beside it, on.
      await openSession(page, mock, A, [{ type: 'text', detail: 'iso 压测跑着', time: now - 20000, uuid: 'w-t1' }]);
      const chips = page.locator('#header-custody > button');
      await expect(chips).toHaveText(['托管关', '观察开']);
      const watch = page.locator('#header-custody .watch-chip');
      await expect(watch).toHaveAttribute('aria-pressed', 'true');
      await expect(watch).toHaveAttribute('title', /网关看着它、写摘要、放进任务的「你开的」；不发任何消息/);
      // Beside the custody chip, on one row.
      const [c1, c2] = await Promise.all([chips.nth(0).boundingBox(), watch.boundingBox()]);
      expect(Math.abs(c1.y - c2.y)).toBeLessThan(4);
      expect(c2.x).toBeGreaterThan(c1.x);
      if (name === 'mobile') await expect(page.locator('body')).toHaveClass(/mobile-chat-view/);
      await expect(watch).toBeInViewport();
      await shot(page, 'watch-chip-' + name);

      // One click turns it off, custody untouched; the toast says what that
      // means.
      await watch.click();
      await expect.poll(() => mock.custodyCalls).toEqual([{ key: A, watch: false }]);
      await expect(page.locator('#toast')).toHaveText('观察已关：不进任务、不写摘要（托管不受影响）');
      await expect(watch).toHaveText('观察关');
      await expect(watch).toHaveClass(/watch-off/);
      await expect(page.locator('#header-custody .custody-chip')).toHaveText('托管关');
      await shot(page, 'watch-off-' + name);

      // The Planner turning it off is a note in that session.
      await openSession(page, mock, B, [{ type: 'text', detail: 'iso 调研完了', time: now - 40 * M, uuid: 'w-t2' }]);
      await expect(page.locator('#events-scroll .gw-note[data-note-id="wn1"]')).toContainText('观察：已关闭（Planner），不进任务、不写摘要');

      // A dispatched session is a task already: no 观察.
      await openSession(page, mock, TASK);
      await expect(page.locator('#header-custody > button')).toHaveText(['托管开']);
      await expect(page.locator('#header-custody .watch-chip')).toHaveCount(0);

      // The session turned off is out of 你开的.
      await page.click('#abnav-planner');
      if (name === 'mobile') await page.click('#pl-left-toggle');
      await expect(page.locator('#pl-task .tk-sec-yours .tk-card')).toHaveCount(2);
      await expect(card(page, A)).toHaveCount(0);
    } finally {
      await ctx.close();
      await mock.server.close();
    }
  });
}
