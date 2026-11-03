// E2E: the Planner's dispatch (planner RFC §6.2, §15.1 PR ⑪): its open
// cards also stand at the end of the Planner conversation, a dispatch card
// answers 批准 / 驳回 through /api/inbox/decide and a question's options are
// its buttons; the sessions it dispatched (purpose task) are their own
// "Planner 派出" group on top of the session list; the gateway's receipt to
// the Planner is a gateway line, not your bubble. Desktop and phone.
const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const PLANNER = 'planner:main';
const TASK = 'task:0a1b2c3d4e5f';
const now = Date.now();
const iso = (ms) => new Date(ms).toISOString();

function inbox() {
  return {
    open: [
      {
        id: 'in_dispatch', kind: 'decision', level: 'timely', unit: PLANNER, ws: 'ws_iso01', title: '派出会话：iso 写 hello',
        why: '工作区 iso-scratch · 模型 默认 · 第一条消息：在工作区里写一个 hello.md', created: iso(now - 2 * 60000), state: 'open',
        source: 'dispatch:1', decision: { handler: 'dispatch', actions: ['approve', 'reject'] },
      },
      {
        id: 'in_ask', kind: 'question', level: 'timely', unit: PLANNER, title: '今天发版吗？', why: 'Planner 问你',
        created: iso(now - 60000), state: 'open', source: 'ask:1', decision: { handler: 'ask', actions: ['发', '不发'] },
      },
      { id: 'in_err', kind: 'error', level: 'timely', unit: 'cron:nightly', title: 'iso nightly 失败', why: '定时任务运行失败', created: iso(now - 3600000), state: 'open', source: 'health:cron:nightly' },
    ],
    closed: [],
    counts: { open: 3, badge: 2 },
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

for (const vp of [{ name: 'desktop', width: 1280, height: 800 }, { name: 'phone', width: 390, height: 844, isMobile: true, hasTouch: true }]) {
  test(vp.name + ': dispatch card in the conversation, question options, the Planner 派出 group', async ({ browser }) => {
    test.setTimeout(60000);
    const ctx = await browser.newContext({ viewport: { width: vp.width, height: vp.height }, isMobile: !!vp.isMobile, hasTouch: !!vp.hasTouch });
    const page = await ctx.newPage();
    const box = inbox();
    const mock = await startMockServer({
      ws: true, inbox: box, sessions: sessions(),
      plannerEvents: [{ type: 'user', detail: '<naozhi-receipt>\n已派出：iso 写 hello（' + TASK + '）\n</naozhi-receipt>', time: now - 1000, uuid: 'pl-r1' }],
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
        await page.screenshot({ path: test.info().outputPath('sidebar-' + vp.name + '.png') });
      }

      await page.click('#abnav-planner');
      await expect(page.locator('body')).toHaveClass(/nz-view-planner/);
      await expect(page.locator('#pl-stream')).toContainText('回执给 Planner：已派出：iso 写 hello');
      await expect(page.locator('#pl-stream .event.user')).toHaveCount(0);
      await expect(page.locator('#pl-stream')).not.toContainText('naozhi-receipt');
      const inline = page.locator('#pl-cards');
      await expect(inline).toBeVisible();
      await expect(inline.locator('.pl-cards-h')).toHaveText('待你确认 · 2');
      // Only the Planner's own cards stand in the conversation.
      await expect(inline.locator('.pl-card')).toHaveCount(2);
      await expect(inline.locator('.pl-card[data-id="in_err"]')).toHaveCount(0);
      const dispatch = inline.locator('.pl-card[data-id="in_dispatch"]');
      await expect(dispatch.locator('.pl-card-title')).toHaveText('派出会话：iso 写 hello');
      await expect(dispatch.locator('.pl-card-why')).toContainText('hello.md');
      await expect(dispatch.locator('.pl-card-acts button')).toHaveText(['批准', '驳回']);
      await expect(inline.locator('.pl-card[data-id="in_ask"] .pl-card-acts button')).toHaveText(['发', '不发']);
      await page.screenshot({ path: test.info().outputPath('cards-' + vp.name + '.png') });

      // 批准 from the conversation answers the card; the inbox refetch drops it.
      await dispatch.locator('button', { hasText: '批准' }).click();
      await expect.poll(() => mock.inboxCalls.length).toBe(1);
      expect(mock.inboxCalls[0]).toEqual({ path: '/api/inbox/decide', method: 'POST', body: { id: 'in_dispatch', action: 'approve' } });
      await expect(inline.locator('.pl-card')).toHaveCount(1);

      // A question's option is its answer.
      await inline.locator('.pl-card[data-id="in_ask"] button', { hasText: '不发' }).click();
      await expect.poll(() => mock.inboxCalls.length).toBe(2);
      expect(mock.inboxCalls[1].body).toEqual({ id: 'in_ask', action: '不发' });
      await expect(inline).toBeHidden();
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
}
