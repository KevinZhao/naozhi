// E2E: an ask naming a task expires (planner RFC §15.14 ④, D115): once you
// wrote to the task's session yourself or the task is done, the question
// closes without an answer. 等你 keeps it a while as a quiet card saying
// why, with no answer to press, counted nowhere; an answer that comes too
// late is refused with that reason, and the card turns into the expired one.
// Desktop and phone width.
const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const PLANNER = 'planner:main';
const now = Date.now();
const iso = (ms) => new Date(ms).toISOString();
const TASK = defaultSessions().sessions[0].key;

function inbox() {
  return {
    open: [
      {
        id: 'in_late', kind: 'question', level: 'timely', unit: PLANNER, task: TASK, title: 'iso 拆不拆测试栈？', why: 'Planner 问你',
        created: iso(now - 20 * 60000), state: 'open', source: 'ask:late',
        decision: { handler: 'ask', actions: ['拆', '先不拆'] }, expire: '任务已完成',
      },
    ],
    closed: [
      {
        id: 'in_done', kind: 'question', level: 'timely', unit: PLANNER, task: TASK, title: 'iso 录音怎么处理？', why: 'Planner 问你',
        created: iso(now - 30 * 60000), state: 'closed', closed_by: 'auto', closed_at: iso(now - 25 * 60000), closed_why: '你已在会话里处理',
        source: 'ask:done', decision: { handler: 'ask', actions: ['清理掉并提交'] },
      },
      // Long past, or answered by you: not shown.
      {
        id: 'in_old', kind: 'question', level: 'timely', unit: PLANNER, task: TASK, title: 'iso 昨天的提问', why: 'Planner 问你',
        created: iso(now - 30 * 3600000), state: 'closed', closed_by: 'auto', closed_at: iso(now - 26 * 3600000), closed_why: '任务已完成',
        source: 'ask:old', decision: { handler: 'ask', actions: ['是'] },
      },
      {
        id: 'in_answered', kind: 'question', level: 'timely', unit: PLANNER, title: 'iso 答过的提问', why: 'Planner 问你',
        created: iso(now - 40 * 60000), state: 'closed', closed_by: 'user', closed_at: iso(now - 35 * 60000),
        source: 'ask:answered', decision: { handler: 'ask', actions: ['是'], answer: '是' },
      },
    ],
    counts: { open: 1, badge: 1 },
  };
}

for (const vp of [{ name: 'desktop', width: 1280, height: 800 }, { name: 'phone', width: 390, height: 844 }]) {
  test(`${vp.name}: an expired ask says why in 等你 and a late answer is refused`, async ({ browser }) => {
    const ctx = await browser.newContext({ viewport: { width: vp.width, height: vp.height } });
    const page = await ctx.newPage();
    const mock = await startMockServer({
      ws: true, plannerEvents: [], inbox: inbox(), tasks: { tasks: [] },
      planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
    });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page, 'CONNECTED');
      await page.click('#abnav-planner');
      await expect(page.locator('body')).toHaveClass(/nz-view-planner/);

      const expired = page.locator('#pl-wait .pl-card-expired');
      if (vp.name === 'phone') {
        // The fold counts what waits on you: the expired one is not.
        await expect(page.locator('#pl-left-toggle')).toHaveText('等你 1 · 任务 0');
        await page.click('#pl-left-toggle');
      }
      await expect(expired).toHaveCount(1);
      const done = expired.first();
      await expect(done).toHaveAttribute('data-id', 'in_done');
      await expect(done.locator('.pl-kind')).toHaveText('提问 · 已过期');
      await expect(done.locator('.pl-card-why')).toHaveText('你已在会话里处理，不用再回答');
      await expect(done.locator('.pl-card-acts, [data-action="planner-inbox-close"]')).toHaveCount(0);
      await expect(done.locator('[data-action="session-select"]')).toHaveAttribute('data-key', TASK);
      await expect(page.locator('#pl-wait-h')).toContainText('1');
      await page.screenshot({ path: test.info().outputPath(`planner-ask-expired-${vp.name}.png`) });

      // The open ask, answered after its task is done: refused, with why.
      if (vp.name === 'phone') await page.click('#pl-left-toggle');
      const ask = page.locator('#pl-cards .pl-card[data-id="in_late"]');
      await expect(ask).toBeVisible();
      await ask.locator('.pl-card-acts button', { hasText: '先不拆' }).click();
      await expect(page.locator('#toast')).toHaveText('这条提问已过期（任务已完成），回答没有送出');
      await expect(page.locator('#toast')).toHaveClass(/warning/);
      await expect(page.locator('#pl-cards .pl-card')).toHaveCount(0);
      if (vp.name === 'phone') {
        // Nothing waits on you now, yet the fold stays for the cards that
        // say why.
        await expect(page.locator('#pl-left-toggle')).toHaveText('等你 0 · 任务 0');
        await page.click('#pl-left-toggle');
      }
      await expect(expired).toHaveCount(2);
      await expect(page.locator('#pl-wait .pl-card-expired[data-id="in_late"] .pl-card-why')).toHaveText('任务已完成，不用再回答');
      await expect(page.locator('#pl-wait .pl-card:not(.pl-card-expired)')).toHaveCount(0);
      await expect(page.locator('#pl-wait .pl-empty')).toHaveText('没有等你的事');
      expect(mock.inboxCalls.filter((c) => c.path.endsWith('/decide'))).toHaveLength(1);
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
}
