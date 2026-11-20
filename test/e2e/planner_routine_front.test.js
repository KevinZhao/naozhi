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
        block({ type: 'front', id: 'jd-tgw', name: 'JD TGW 迁移', state: 'EC2 VXLAN 方案被否', next: '改迁移路线图', waiting: '客户', decision: '先迁回 JDOS VPC' }) + '\n' +
        block({ type: 'front', id: 'blog', close: true }) },
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
    await expect(reply.locator('div.pl-act-routine')).toHaveText('例行 · 暂停 · 1cc584f384db82fc');

    const fronts = reply.locator('.pl-act-front');
    await expect(fronts).toHaveCount(2);
    await expect(fronts.nth(0)).toHaveText('工作线 · JD TGW 迁移 · EC2 VXLAN 方案被否 → 改迁移路线图 （等 客户） 定了：先迁回 JDOS VPC');
    await expect(fronts.nth(1)).toHaveText('工作线 · blog · 已结束');
  } finally {
    mock.server.close();
  }
});
