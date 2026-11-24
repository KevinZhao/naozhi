// E2E: the Planner's routine and front blocks (planner RFC §15.17, D131 /
// D132). In its conversation a new routine folds to 例行 · 标题 · 定时 · 工作区
// with the prompt inside, a switch and a front are one line each; never the
// "看不懂的动作块" card.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const PLANNER = 'planner:main';
const now = Date.now();
const block = (a) => '```naozhi-action\n' + JSON.stringify(a) + '\n```';

test('routine and front cards', async ({ page }) => {
  const mock = await startMockServer({
    ws: true,
    planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
    plannerEvents: [
      { type: 'text', time: now - 5000, uuid: 'rf-t1', detail: '好，每个工作日早上查一次，并记到这条线上。\n\n' +
        block({ type: 'routine', title: '内网模型动态', workspace: 'myproject', schedule: '0 9 * * 1-5', prompt: '查内网 Anthropic 新模型动态，第一行写结论' }) + '\n' +
        block({ type: 'routine', id: '1cc584f384db82fc', off: true }) + '\n' +
        block({ type: 'routine', id: 'aa11', schedule: '0 8 * * *' }) + '\n' +
        block({ type: 'routine', id: 'bb22', delete: true }) + '\n' +
        block({ type: 'front', id: 'jd-tgw', name: 'JD TGW 迁移', state: 'EC2 VXLAN 方案被否', next: '改迁移路线图', waiting: '客户', decision: '先迁回 JDOS VPC' }) + '\n' +
        block({ type: 'front', id: 'blog', close: true }) + '\n' +
        // The open items (§15.23): one filed under a line, ones closed and
        // one re-sourced with no line named.
        block({ type: 'front', id: 'jd-tgw', item: { text: '补上 reconnect 路径的单测', source: 'task:9f3c2a71d0' } }) + '\n' +
        block({ type: 'front', done: ['i7k2m9qa', 'i0c4x8zn'] }) + '\n' +
        block({ type: 'front', item: { id: 'i7k2m9qa', source: 'https://github.com/owner/repo/issues/7' } }) },
    ],
  });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click('#abnav-planner');
    const reply = page.locator('#pl-stream .event.text').first();
    await expect(reply.locator('.pl-act-bad')).toHaveCount(0);

    const made = reply.locator('details.pl-act-routine');
    await expect(made.locator('summary')).toHaveText('例行 · 内网模型动态 · 0 9 * * 1-5 · myproject');
    await made.locator('summary').click();
    await expect(made.locator('.pl-act-rows')).toContainText('查内网 Anthropic 新模型动态，第一行写结论');
    await expect(reply.locator('div.pl-act-routine')).toHaveText(
      ['例行 · 暂停 · 1cc584f384db82fc', '例行 · 改了定时 → 0 8 * * * · aa11', '例行 · 删除 · bb22']);

    const fronts = reply.locator('.pl-act-front');
    await expect(fronts).toHaveCount(5);
    await expect(fronts.nth(0)).toHaveText('工作线 · JD TGW 迁移 · EC2 VXLAN 方案被否 → 改迁移路线图 （等 客户） 定了：先迁回 JDOS VPC');
    await expect(fronts.nth(1)).toHaveText('工作线 · blog · 已结束');
    await expect(fronts.nth(2)).toHaveText('工作线 · jd-tgw · 未了：补上 reconnect 路径的单测');
    await expect(fronts.nth(3)).toHaveText('工作线 · 了结 2 条未了事项');
    await expect(fronts.nth(4)).toHaveText('工作线 · 改了一条未了事项的出处');
  } finally {
    mock.server.close();
  }
});
