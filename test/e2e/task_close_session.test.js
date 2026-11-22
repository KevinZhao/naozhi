// E2E: marking a session done yourself in 任务 also closes it in 会话
// (planner RFC D137). A session you opened says so on its 标为已完成, and
// once put away its card stays in 已完成 saying 会话已收起, with 重新打开; one
// the Planner marked done is only listed as done, with 标为进行中.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const now = Date.now();
const M = 60000;
const iso = (ms) => new Date(ms).toISOString();
const OPEN = 'dashboard:direct:opn:general';
const AWAY = 'dashboard:direct:awy:general';
const NOTED = 'dashboard:direct:ntd:general';
const TASK = 'task:tsk000000001';
const card = (page, id) => page.locator('#pl-task .tk-card[data-id="' + id + '"]');

test('marking a session done yourself closes it, and it reopens from its card', async ({ page }) => {
  const data = { tasks: [
    { id: OPEN, kind: 'session', group: 'yours', title: 'iso 分析 TGW 迁移', state: 'doing', at: iso(now - 30 * M), created: iso(now - 90 * M), health: 'idle',
      live: { running: false, reply: '路线图改好了', last_active: iso(now - 20 * M) } },
    { id: TASK, kind: 'session', title: 'iso 查容量', state: 'doing', at: iso(now - 40 * M), created: iso(now - 40 * M), health: 'idle',
      live: { running: false, last_active: iso(now - 10 * M) } },
    { id: AWAY, kind: 'session', group: 'yours', title: 'iso trn2 blog', state: 'done', by: 'user', at: iso(now - 120 * M), created: iso(now - 300 * M),
      retired: iso(now - 119 * M), reap: { at: iso(now - 119 * M), by: 'user', worktrees: [] } },
    { id: NOTED, kind: 'session', group: 'yours', title: 'iso 内网调研', state: 'done', by: 'planner', why: '你说做完了', at: iso(now - 60 * M), created: iso(now - 200 * M),
      health: 'idle', seen_at: iso(now - 50 * M), result_at: iso(now - 60 * M) },
  ] };
  const mock = await startMockServer({
    ws: true, tasks: data, plannerEvents: [], inbox: { open: [], closed: [], counts: { open: 0, badge: 0 } },
    planner: { enabled: true, stopped: false, key: 'planner:main', alive: true },
  });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click('#abnav-planner');

    // 你开的, in progress: 标为已完成 says it closes the session too.
    await card(page, OPEN).hover();
    await card(page, OPEN).locator('.tk-more').click();
    const done = card(page, OPEN).locator('[data-action="task-done"]');
    await expect(done).toHaveAttribute('title', /关掉“会话”里的这个会话/);
    await done.click();
    await expect.poll(() => mock.tasksCalls).toContainEqual({ id: OPEN, state: 'done' });

    // A dispatched one says the same.
    await card(page, TASK).hover();
    await card(page, TASK).locator('.tk-more').click();
    await expect(card(page, TASK).locator('[data-action="task-done"]')).toHaveAttribute('title', /关掉“会话”里的这个会话/);

    // Put away: listed in 已完成 as 会话已收起, reopened from the card.
    await page.locator('#pl-task .tk-sec-done summary').click();
    await expect(card(page, AWAY).locator('.tk-meta')).toContainText('会话已收起');
    await card(page, AWAY).hover();
    await card(page, AWAY).locator('.tk-more').click();
    await expect(card(page, AWAY).locator('[data-action="task-doing"]')).toHaveCount(0);
    const reopen = card(page, AWAY).locator('[data-action="task-reopen"]');
    await expect(reopen).toHaveText('重新打开');
    await reopen.click();
    await expect.poll(() => mock.tasksCalls).toContainEqual({ id: AWAY, state: 'doing' });

    // The Planner's mark on a session you opened: only listed as done.
    await card(page, NOTED).hover();
    await card(page, NOTED).locator('.tk-more').click();
    await expect(card(page, NOTED).locator('[data-action="task-doing"]')).toHaveText('标为进行中');
    await expect(card(page, NOTED).locator('[data-action="task-reopen"]')).toHaveCount(0);
    await expect(card(page, NOTED).locator('.tk-meta')).toHaveCount(0);
  } finally {
    mock.server.close();
  }
});
