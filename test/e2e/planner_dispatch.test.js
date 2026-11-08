// E2E: the Planner's dispatch (planner RFC §6.2, §15.1 PR ⑪, D88): a
// dispatch runs at once, so its card in the conversation has nothing to
// approve and no gateway line repeats it; the gateway's receipt to the
// Planner is not shown, and the turn cards the session once, where the
// Planner first names it, by its name even when it is no longer listed
// (D91); only the Planner's
// questions with options stand at the end of the conversation; the
// dispatched session is a 任务 with 停止, which posts /api/tasks/stop and
// leaves it 已完成 as yours (已停止); a session it dispatched (purpose task)
// lists in its workspace's group, marked 派出.
// Desktop and phone.
const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const PLANNER = 'planner:main';
const TASK = 'task:0a1b2c3d4e5f';
// GONE is a dispatched session no longer listed: 任务 still names it.
const GONE = 'task:9f8e7d6c5b4a';
const now = Date.now();
const iso = (ms) => new Date(ms).toISOString();
const DISPATCH = { type: 'dispatch', workspace: 'iso-scratch', title: 'iso 写 hello', model: '', message: '在工作区里写一个 hello.md' };

function inbox() {
  return {
    open: [
      {
        id: 'in_ask', kind: 'question', level: 'timely', unit: PLANNER, title: '今天发版吗？', why: 'Planner 问你',
        created: iso(now - 60000), state: 'open', source: 'ask:1', decision: { handler: 'ask', actions: ['发', '不发'] },
      },
      { id: 'in_err', kind: 'error', level: 'timely', unit: 'cron:nightly', title: 'iso nightly 失败', why: '定时任务运行失败', created: iso(now - 3600000), state: 'open', source: 'health:cron:nightly' },
    ],
    closed: [],
    counts: { open: 2, badge: 2 },
  };
}

function sessions() {
  const s = defaultSessions();
  s.sessions.push({
    key: TASK, state: 'running', platform: 'task', agent: 'general', cli_name: 'claude', purpose: 'task',
    user_label: 'iso 写 hello', workspace: '/home/user/workspace/myproject', project: 'myproject',
    last_active: now - 10000, last_prompt: '在工作区里写一个 hello.md', node: 'local',
  });
  return s;
}

const PROMISE = 'pr_iso00001';
const tasks = () => ({ tasks: [
  { id: TASK, kind: 'session', title: 'iso 写 hello', state: 'doing', at: iso(now - 4000), created: iso(now - 4000) },
  { id: GONE, kind: 'session', title: 'iso 旧任务', state: 'done', at: iso(now - 90000), created: iso(now - 99000) },
  { id: PROMISE, kind: 'promise', title: 'iso 周五前提醒你看周报', state: 'done', at: iso(now - 2000), created: iso(now - 9000) },
] });
const STATUS = { type: 'status', task: PROMISE, state: 'done', why: 'iso 你看过周报了' };

const events = () => [
  { type: 'text', detail: '好，我派一个会话去写。\n\n```naozhi-action\n' + JSON.stringify(DISPATCH) + '\n```', time: now - 5000, uuid: 'pl-t1' },
  { type: 'user', detail: '<naozhi-receipt>\n已派出：iso 写 hello（' + TASK + '），工作区 iso-scratch。\n</naozhi-receipt>', time: now - 3000, uuid: 'pl-r1' },
  { type: 'text', detail: '派出了 iso 写 hello（`' + TASK + '`），做完我告诉你。', time: now - 2500, uuid: 'pl-t2' },
  { type: 'text', detail: '`' + TASK + '` 已经在写了。\n\n```naozhi-action\n' + JSON.stringify(STATUS) + '\n```', time: now - 2000, uuid: 'pl-t3' },
  { type: 'text', detail: 'iso 旧任务（`' + GONE + '`）昨天写完了。', time: now - 1500, uuid: 'pl-t4' },
];

for (const vp of [{ name: 'desktop', width: 1280, height: 800 }, { name: 'phone', width: 390, height: 844, isMobile: true, hasTouch: true }]) {
  test(vp.name + ': a dispatch runs at once, shows its card and stops from 任务', async ({ browser }) => {
    test.setTimeout(60000);
    const ctx = await browser.newContext({ viewport: { width: vp.width, height: vp.height }, isMobile: !!vp.isMobile, hasTouch: !!vp.hasTouch });
    const page = await ctx.newPage();
    const mock = await startMockServer({
      ws: true, inbox: inbox(), sessions: sessions(), tasks: tasks(), plannerEvents: events(),
      planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
    });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');

      // The session list: the dispatched session is in its workspace's
      // group, marked 派出; no group of its own.
      if (vp.name === 'desktop') {
        const list = page.locator('#session-list');
        await expect(list.locator('.section-header', { hasText: 'Planner 派出' })).toHaveCount(0);
        const card = list.locator('.session-card[data-key="' + TASK + '"]');
        await expect(card).toHaveCount(1);
        await expect(card.locator('.sc-origin.kind-planner')).toHaveText('派出');
      }

      await page.click('#abnav-planner');
      await expect(page.locator('body')).toHaveClass(/nz-view-planner/);
      const stream = page.locator('#pl-stream');
      // The dispatch card: what was dispatched, nothing to approve.
      const card = stream.locator('.pl-act-dispatch');
      await expect(card.locator('.pl-act-h')).toHaveText('派出会话 · iso 写 hello');
      await expect(card).toContainText('在工作区里写一个 hello.md');
      await expect(card.locator('dd').first()).toHaveText('iso-scratch');
      await expect(card).not.toContainText('可在"任务"里停止');
      await expect(stream.locator('button', { hasText: '批准' })).toHaveCount(0);
      // No gateway line says it again; the receipt it wrote for the Planner
      // is not shown.
      await expect(stream.locator('.event.system')).toHaveCount(0);
      await expect(stream.locator('.event.user')).toHaveCount(0);
      await expect(stream).not.toContainText('naozhi-receipt');
      await expect(stream).not.toContainText('回执');
      // One card for the turn, where the Planner first names the session;
      // named again, the key reads as its name. No key shows.
      await expect(stream.locator('.pl-scard[data-key="' + TASK + '"]')).toHaveCount(1);
      const reply = stream.locator('.event.text').nth(1);
      await expect(reply.locator('.pl-scard[data-key="' + TASK + '"] .pl-scard-name')).toHaveText('iso 写 hello');
      await expect(stream.locator('.event.text').nth(2)).toHaveText(/iso 写 hello 已经在写了。/);
      await expect(stream).not.toContainText(TASK);
      // One no longer listed is named by 任务.
      const gone = stream.locator('.event.text').nth(3);
      await expect(gone.locator('.pl-scard[data-key="' + GONE + '"] .pl-scard-name')).toHaveText('iso 旧任务');
      await expect(gone).toHaveText(/iso 旧任务昨天写完了。/);
      await expect(stream).not.toContainText(GONE);
      // A judgment names its task by the title 任务 lists, not its ID.
      await expect(stream.locator('.pl-act-status dd')).toHaveText(['iso 周五前提醒你看周报', 'iso 你看过周报了']);
      await expect(stream).not.toContainText(PROMISE);
      // Only the Planner's question stands at the end of the conversation.
      const inline = page.locator('#pl-cards');
      await expect(inline.locator('.pl-cards-h')).toHaveText('待你回答 · 1');
      await expect(inline.locator('.pl-card')).toHaveCount(1);
      await expect(inline.locator('.pl-card[data-id="in_ask"] .pl-card-acts button')).toHaveText(['发', '不发']);
      await page.screenshot({ path: test.info().outputPath('dispatch-' + vp.name + '.png') });

      // 任务: the dispatched session runs, with 停止.
      if (vp.isMobile) await page.click('#pl-left-toggle');
      const task = page.locator('#pl-task .tk-card[data-id="' + TASK + '"]');
      await expect(task.locator('.tk-menu option:checked')).toHaveText('进行中');
      await expect(task.locator('.tk-stop')).toHaveText('停止');
      await task.scrollIntoViewIfNeeded();
      await page.screenshot({ path: test.info().outputPath('dispatch-task-' + vp.name + '.png') });
      await task.locator('.tk-stop').click();
      await expect.poll(() => mock.taskStopCalls).toEqual([{ id: TASK }]);
      // Stopping is your decision (§15.7 S2): 已完成, said as 已停止 · 你停的.
      const stopped = page.locator('#pl-task .tk-sec-done .tk-card[data-id="' + TASK + '"]');
      await expect(stopped.locator('.tk-state')).toHaveText('已停止');
      await expect(stopped.locator('.tk-meta')).toHaveText('你停的');
      await expect(stopped).not.toContainText('失败');
      await expect(stopped.locator('.tk-stop')).toHaveCount(0);
      await expect(stopped.locator('.tk-reopen')).toHaveText('重新打开');
      await page.locator('#pl-task .tk-sec-done summary').click();
      await stopped.scrollIntoViewIfNeeded();
      await page.screenshot({ path: test.info().outputPath('dispatch-stopped-' + vp.name + '.png') });

      // A question's option is its answer.
      if (vp.isMobile) await page.click('#pl-left-toggle');
      await inline.locator('.pl-card[data-id="in_ask"] button', { hasText: '不发' }).click();
      await expect.poll(() => mock.inboxCalls.length).toBe(1);
      expect(mock.inboxCalls[0]).toEqual({ path: '/api/inbox/decide', method: 'POST', body: { id: 'in_ask', action: '不发' } });
      await expect(inline).toBeHidden();
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
}
