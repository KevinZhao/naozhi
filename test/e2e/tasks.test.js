// E2E: the Planner page's "任务" group (tasks.js, planner RFC §15.4, D85,
// §15.7) renders GET /api/tasks in three parts — 进行中 (等你 and 失败 first),
// 待提醒, and 已完成 folded — the group's heading (what is in progress) and each part's counted, each card's state said
// once (its badge) with who set it only when not the default (the Planner's
// judgment and its reason); the card's "⋯" marks a task done as yours
// through POST /api/tasks/state, a done one reopens, and a dispatched
// session's title opens it in the middle column, whose header has 停止
// while it is in progress.
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
        // 任务 counts what 进行中 lists: the one waiting on you too.
        await expect(page.locator('#pl-left-toggle')).toHaveText('等你 0 · 任务 2');
        await page.click('#pl-left-toggle');
        // Unfolded, what waits on you comes first, as on a desktop (§15.13
        // v0.7.0, UX review 1; Q15 put the tasks first).
        await expect(page.locator('#pl-task .tk-sec-doing')).toBeVisible();
        const [tk, wait] = await Promise.all([page.locator('#pl-task').boundingBox(), page.locator('#pl-wait').boundingBox()]);
        expect(wait.y).toBeLessThan(tk.y);
      }
      const group = page.locator('#pl-task');
      // The heading counts what is in progress, the folded block's number
      // (one 任务 number on the screen, §15.8 F1); each part its own.
      await expect(group.locator('.tk-head')).toHaveText('任务 2');
      await expect(group.locator('.tk-sec > .tk-sec-h')).toHaveText(['进行中 2', '待提醒 1', '已完成 1']);
      const doing = group.locator('.tk-sec-doing .tk-card');
      await expect(doing.locator('.tk-title')).toHaveText(['iso 写 hello', 'iso CI 绿了告诉你']);
      // The state is the badge's, said once; the default source goes unsaid.
      await expect(doing.locator('.tk-badge')).toHaveText(['等你', '进行中']);
      await expect(doing.nth(0).locator('.tk-open')).toHaveCount(1);
      await expect(doing.nth(0)).not.toContainText('按健康状态');
      await expect(doing.nth(1).locator('.tk-open')).toHaveCount(0);
      await expect(doing.nth(1).locator('.tk-now')).toHaveText('完成条件：master CI 绿');
      // No control shows until "⋯" opens the card's row.
      await expect(group.locator('.tk-act, select')).toHaveCount(0);
      // A point in time reads M/D HH:mm, as everywhere on the page (D92).
      await expect(group.locator('.tk-sec-remind .tk-card .tk-now')).toHaveText(/到点提醒 \d{1,2}\/\d{1,2} \d\d:\d\d/);
      // 已完成 is folded; unfolded, the Planner's judgment and its reason.
      await expect(group.locator('.tk-sec-done .tk-card')).toBeHidden();
      await group.locator('.tk-sec-done summary').click();
      const done = group.locator('.tk-sec-done .tk-card');
      await expect(done.locator('.tk-now')).toHaveText('Planner 判断：它汇报测试全部通过');
      await expect(done.locator('.tk-badge')).toHaveText('已完成');
      await done.locator('.tk-more').click();
      await expect(done.locator('.tk-more')).toHaveAttribute('aria-expanded', 'true');
      await expect(done.locator('.tk-act')).toHaveText(['重新打开']);
      await page.screenshot({ path: test.info().outputPath(`tasks-${name}.png`) });

      // Marking the promise done from its "⋯": yours, and folded away.
      await doing.nth(1).locator('.tk-more').click();
      // One card's row is open at a time.
      await expect(group.locator('.tk-acts')).toHaveCount(1);
      await expect(doing.nth(1).locator('.tk-act')).toHaveText(['标为已完成']);
      await doing.nth(1).locator('.tk-done').click();
      await expect.poll(() => mock.tasksCalls).toEqual([{ id: 'pr_cond', state: 'done' }]);
      await expect(group.locator('.tk-sec-done')).toHaveAttribute('open', '');
      await expect(group.locator('.tk-sec-done .tk-card[data-id="pr_cond"] .tk-now')).toHaveText('你标记');
      await expect(group.locator('.tk-acts')).toHaveCount(0);
      // Reopening the session the Planner judged done.
      const done1 = group.locator('.tk-card[data-id="task:done1"]');
      await done1.locator('.tk-more').click();
      await done1.locator('.tk-reopen').click();
      await expect.poll(() => mock.tasksCalls.length).toBe(2);
      expect(mock.tasksCalls[1]).toEqual({ id: 'task:done1', state: 'doing' });
      await expect(group.locator('.tk-sec-doing .tk-card[data-id="task:done1"]')).toBeVisible();
      // The session waiting on you, marked back in progress from its "⋯",
      // which offers 停止 too while it runs.
      const waiting = group.locator('.tk-card[data-id="' + TASK + '"]');
      await waiting.locator('.tk-more').click();
      await expect(waiting.locator('.tk-act')).toHaveText(['停止', '标为进行中', '标为已完成']);
      await waiting.locator('.tk-doing').click();
      await expect.poll(() => mock.tasksCalls.length).toBe(3);
      expect(mock.tasksCalls[2]).toEqual({ id: TASK, state: 'doing' });
      await expect(waiting.locator('.tk-badge')).toHaveText('进行中');
      await expect(waiting.locator('.tk-meta')).toContainText('你标记');

      // A dispatched session's title opens it in the middle column, whose
      // header stops it while it is in progress.
      await group.locator('.tk-card[data-id="' + TASK + '"] .tk-open').click();
      await expect(page.locator('#planner-main')).toHaveClass(/pl-sess-open/);
      await expect(page.locator('#pl-sess-stream')).toContainText('iso hello.md 写好了');
      const stop = page.locator('#pl-sess-stop');
      await expect(stop).toBeVisible();
      await page.screenshot({ path: test.info().outputPath(`tasks-session-${name}.png`) });
      await stop.click();
      await expect.poll(() => mock.taskStopCalls).toEqual([{ id: TASK }]);
      await expect(stop).toBeHidden();
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
}

// A done dispatched session is put away only once it has been idle 2 hours
// (D105): while that alone holds it, its status line says about when
// (reap_at) next to who marked it done, and its card says nothing more;
// held by anything else it says what; once put away, that it was.
const SHOTS = process.env.NZ_SHOTS_DIR || '';
for (const [name, opts] of [['desktop', { viewport: { width: 1440, height: 900 } }], ['phone', { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true }]]) {
  test(`a done task says about when it is put away (${name})`, async ({ browser }) => {
    const ctx = await browser.newContext(opts);
    const page = await ctx.newPage();
    // The countdown rounds up to a minute: each reap_at sits 20s short of
    // a whole one, read from this test's own clock.
    const now = Date.now();
    const done = (id, title, extra) => ({ id, kind: 'session', title, state: 'done', by: 'planner', why: 'PR 已合并', at: iso(now - 30 * 60000), created: iso(now - 7200000), ...extra });
    const data = {
      tasks: [
        done('task:idle1', 'iso 修 reap', { reap_wait: '空闲不到 2 小时', reap_at: iso(now + 90 * 60000 - 20000) }),
        done('task:soon1', 'iso 补文档', { reap_wait: '空闲不到 2 小时', reap_at: iso(now + 12 * 60000 - 20000) }),
        done('task:ask1', 'iso 跑基准', { reap_wait: '有提问没回答' }),
        // Stopped by you: 你停的 in place of who judged it done.
        done('task:stop1', 'iso 停掉的', { by: 'user', why: '已停止', reap_wait: '空闲不到 2 小时', reap_at: iso(now + 45 * 60000 - 20000) }),
        // reap_at passed, the next look not run yet: never 0 or less.
        done('task:late1', 'iso 到点了', { reap_wait: '空闲不到 2 小时', reap_at: iso(now - 5 * 60000) }),
        done('task:gone1', 'iso 改配置', { reap: { at: iso(now - 60000), by: 'planner', session_id: 's1', worktrees: [] }, retired: iso(now - 60000) }),
      ],
    };
    const mock = await startMockServer({
      ws: true, tasks: data, plannerEvents: [], inbox: { open: [], closed: [], counts: { open: 0, badge: 0 } },
      planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
    });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      await page.click('#abnav-planner');
      if (name === 'phone') await page.click('#pl-left-toggle');
      const group = page.locator('#pl-task');
      await group.locator('.tk-sec-done summary').click();
      const card = (id) => group.locator('.tk-card[data-id="' + id + '"]');
      await expect(card('task:idle1').locator('.tk-badge')).toHaveText('已完成');
      await expect(card('task:idle1').locator('.tk-now')).toHaveText('约 1 小时 30 分后收起 · Planner 判断：PR 已合并');
      await expect(card('task:soon1').locator('.tk-now')).toHaveText('约 12 分钟后收起 · Planner 判断：PR 已合并');
      await expect(card('task:stop1').locator('.tk-badge')).toHaveText('已停止');
      await expect(card('task:stop1').locator('.tk-now')).toHaveText('约 45 分钟后收起 · 你停的');
      await expect(card('task:late1').locator('.tk-now')).toHaveText('约 1 分钟后收起 · Planner 判断：PR 已合并');
      // Said once: not again as 等收起 under it.
      await expect(card('task:idle1')).not.toContainText('等收起');
      await expect(card('task:idle1').locator('.tk-meta')).toHaveCount(0);
      // Held by something more specific: what holds it, no time.
      await expect(card('task:ask1').locator('.tk-now')).toHaveText('Planner 判断：PR 已合并');
      await expect(card('task:ask1').locator('.tk-meta')).toHaveText('等收起：有提问没回答');
      await expect(card('task:gone1').locator('.tk-meta')).toHaveText('会话已收起');
      await expect(card('task:gone1')).not.toContainText('后收起');
      await card('task:idle1').scrollIntoViewIfNeeded();
      const shot = SHOTS ? `${SHOTS}/tasks_reap_eta_${name}.png` : test.info().outputPath(`tasks-reap-eta-${name}.png`);
      await page.screenshot({ path: shot });
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
}
