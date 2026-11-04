// E2E: the Planner's dispatch (planner RFC §6.2, §15.1 PR ⑪, D88): a
// dispatch runs at once, so its card in the conversation has nothing to
// approve and the gateway's line after it says 已派出 with the session's card;
// only the Planner's questions with options stand at the end of the
// conversation; the dispatched session is a 任务 with 停止, which posts
// /api/tasks/stop and leaves it 失败 as yours; the sessions it dispatched
// (purpose task) are their own "Planner 派出" group on top of the session
// list; the gateway's receipt to the Planner is a gateway line, not your
// bubble. Desktop and phone.
const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const PLANNER = 'planner:main';
const TASK = 'task:0a1b2c3d4e5f';
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
    counts: { open: 2, badge: 1 },
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

const tasks = () => ({ tasks: [
  { id: TASK, kind: 'session', title: 'iso 写 hello', state: 'doing', at: iso(now - 4000), created: iso(now - 4000) },
] });

const events = () => [
  { type: 'text', detail: '好，我派一个会话去写。\n\n```naozhi-action\n' + JSON.stringify(DISPATCH) + '\n```', time: now - 5000, uuid: 'pl-t1' },
  { type: 'system', summary: '已派出：iso 写 hello（' + TASK + '）', time: now - 4000, uuid: 'pl-s1' },
  { type: 'user', detail: '<naozhi-receipt>\n已派出：iso 写 hello（' + TASK + '），工作区 iso-scratch。\n</naozhi-receipt>', time: now - 3000, uuid: 'pl-r1' },
];

for (const vp of [{ name: 'desktop', width: 1280, height: 800 }, { name: 'phone', width: 390, height: 844, isMobile: true, hasTouch: true }]) {
  test(vp.name + ': a dispatch runs at once, shows 已派出 and stops from 任务', async ({ browser }) => {
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

      // The session list: the dispatched session leads, under its own header.
      if (vp.name === 'desktop') {
        const list = page.locator('#session-list');
        await expect(list.locator('.section-header').first()).toHaveText('Planner 派出');
        await expect(list.locator('.session-card').first()).toHaveAttribute('data-key', TASK);
        await expect(list.locator('.session-card[data-key="' + TASK + '"]')).toHaveCount(1);
      }

      await page.click('#abnav-planner');
      await expect(page.locator('body')).toHaveClass(/nz-view-planner/);
      const stream = page.locator('#pl-stream');
      // The dispatch card: what was dispatched, nothing to approve.
      const card = stream.locator('.pl-act-dispatch');
      await expect(card.locator('.pl-act-h')).toHaveText('派出会话 · iso 写 hello');
      await expect(card).toContainText('在工作区里写一个 hello.md');
      await expect(card.locator('.pl-act-note')).toHaveText('派出后直接执行，可在"任务"里停止');
      await expect(stream.locator('button', { hasText: '批准' })).toHaveCount(0);
      // The gateway's line names the session, and its card opens it.
      const line = stream.locator('.event.system').filter({ hasText: '已派出：iso 写 hello' }).first();
      await expect(line.locator('.pl-scard[data-key="' + TASK + '"] .pl-scard-name')).toHaveText('iso 写 hello');
      await expect(stream).toContainText('回执给 Planner：已派出：iso 写 hello');
      await expect(stream.locator('.event.user')).toHaveCount(0);
      await expect(stream).not.toContainText('naozhi-receipt');
      // Only the Planner's question stands at the end of the conversation.
      const inline = page.locator('#pl-cards');
      await expect(inline.locator('.pl-cards-h')).toHaveText('待你回答 · 1');
      await expect(inline.locator('.pl-card')).toHaveCount(1);
      await expect(inline.locator('.pl-card[data-id="in_ask"] .pl-card-acts button')).toHaveText(['发', '不发']);
      await page.screenshot({ path: test.info().outputPath('dispatch-' + vp.name + '.png') });

      // 任务: the dispatched session runs, with 停止.
      if (vp.isMobile) await page.click('#pl-left-toggle');
      const task = page.locator('#pl-task .tk-card[data-id="' + TASK + '"]');
      await expect(task.locator('.tk-state')).toHaveText('进行中');
      await expect(task.locator('.tk-stop')).toHaveText('停止');
      await task.scrollIntoViewIfNeeded();
      await page.screenshot({ path: test.info().outputPath('dispatch-task-' + vp.name + '.png') });
      await task.locator('.tk-stop').click();
      await expect.poll(() => mock.taskStopCalls).toEqual([{ id: TASK }]);
      await expect(task.locator('.tk-state')).toHaveText('失败');
      await expect(task).toContainText('你标记：已停止');
      await expect(task.locator('.tk-stop')).toHaveCount(0);
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
