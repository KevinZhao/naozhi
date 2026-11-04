// E2E: the Planner page's "任务" group (tasks.js, planner RFC §15.4, D85)
// renders GET /api/tasks in three parts — 进行中 (等你 and 失败 first), 待提醒,
// and 已完成 folded — with each card's source, state and who set it (the
// Planner's judgment and its reason); the state menu marks a task done as
// yours through POST /api/tasks/state, a done one reopens, and a dispatched
// session's title opens it in the middle column.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const PLANNER = 'planner:main';
const TASK = 'task:ab12cd34ef56';
const now = Date.now();
const iso = (ms) => new Date(ms).toISOString();

function tasks() {
  return {
    tasks: [
      { id: TASK, kind: 'session', title: 'iso 写 hello', state: 'waiting', by: 'health', at: iso(now - 5 * 60000), created: iso(now - 3600000), health: 'waiting' },
      { id: 'pr_cond', kind: 'promise', title: 'iso CI 绿了告诉你', state: 'doing', at: iso(now - 7200000), created: iso(now - 7200000), close_when: 'master CI 绿' },
      { id: 'pr_later', kind: 'promise', title: 'iso 明早提醒你看压测结果', state: 'remind', at: iso(now - 600000), created: iso(now - 600000), due: iso(now + 20 * 3600000) },
      { id: 'task:done1', kind: 'session', title: 'iso 跑测试', state: 'done', by: 'planner', why: '它汇报测试全部通过', at: iso(now - 1800000), created: iso(now - 7200000) },
    ],
  };
}

for (const [name, viewport] of [['desktop', { width: 1280, height: 800 }], ['mobile', { width: 390, height: 844 }]]) {
  test(`任务 on the Planner page (${name})`, async ({ browser }) => {
    const ctx = await browser.newContext({ viewport });
    const page = await ctx.newPage();
    const data = tasks();
    const mock = await startMockServer({
      ws: true, tasks: data, plannerEvents: [], inbox: { open: [], closed: [], counts: { open: 0, badge: 0 } },
      planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
      keyEvents: { [TASK]: [{ type: 'text', detail: 'iso hello.md 写好了', time: now - 60000, uuid: 'tk-t1' }] },
    });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      await page.click('#abnav-planner');
      if (name === 'mobile') {
        await expect(page.locator('#pl-left-toggle')).toContainText('通知 0 · 任务 3');
        await page.click('#pl-left-toggle');
      }
      const group = page.locator('#pl-task');
      await expect(group.locator('.tk-head')).toHaveText('任务 · 3');
      await expect(group.locator('.tk-sec > .tk-sec-h')).toHaveText(['进行中 · 2', '待提醒 · 1', '已完成 · 1']);
      const doing = group.locator('.tk-sec-doing .tk-card');
      await expect(doing.locator('.tk-title')).toHaveText(['iso 写 hello', 'iso CI 绿了告诉你']);
      await expect(doing.nth(0).locator('.tk-state')).toHaveText('等你');
      await expect(doing.nth(0).locator('.tk-src')).toHaveText('派出的会话');
      await expect(doing.nth(0)).toContainText('按健康状态');
      await expect(doing.nth(1).locator('.tk-src')).toHaveText('承诺');
      await expect(doing.nth(1)).toContainText('完成条件：master CI 绿');
      await expect(group.locator('.tk-sec-remind .tk-card')).toContainText('到点提醒');
      // 已完成 is folded; unfolded, the Planner's judgment and its reason.
      await expect(group.locator('.tk-sec-done .tk-card')).toBeHidden();
      await group.locator('.tk-sec-done summary').click();
      const done = group.locator('.tk-sec-done .tk-card');
      await expect(done).toContainText('Planner 判断：它汇报测试全部通过');
      await expect(done.locator('.tk-reopen')).toHaveText('重新打开');
      await page.screenshot({ path: test.info().outputPath(`tasks-${name}.png`) });

      // Marking the promise done from its menu: yours, and folded away.
      await doing.nth(1).locator('.tk-menu').selectOption('done');
      await expect.poll(() => mock.tasksCalls).toEqual([{ id: 'pr_cond', state: 'done' }]);
      await expect(group.locator('.tk-head')).toHaveText('任务 · 2');
      await expect(group.locator('.tk-sec-done')).toHaveAttribute('open', '');
      await expect(group.locator('.tk-sec-done .tk-card[data-id="pr_cond"]')).toContainText('你标记');
      // Reopening the session the Planner judged done.
      await group.locator('.tk-card[data-id="task:done1"] .tk-reopen').click();
      await expect.poll(() => mock.tasksCalls.length).toBe(2);
      expect(mock.tasksCalls[1]).toEqual({ id: 'task:done1', state: 'doing' });
      await expect(group.locator('.tk-sec-doing .tk-card[data-id="task:done1"]')).toBeVisible();
      await expect(page.locator('#pl-task-n')).toHaveText('3');

      // A dispatched session's title opens it in the middle column.
      await group.locator('.tk-card[data-id="' + TASK + '"] .tk-open').click();
      await expect(page.locator('#planner-main')).toHaveClass(/pl-sess-open/);
      await expect(page.locator('#pl-sess-stream')).toContainText('iso hello.md 写好了');
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
}
