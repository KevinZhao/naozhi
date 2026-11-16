// E2E: the Planner's park block (planner RFC §15.14 ⑤, D116). In its
// conversation the block is one line, 放下 · the item · its source, never
// the "看不懂的动作块" card.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const PLANNER = 'planner:main';
const now = Date.now();
const block = (a) => '```naozhi-action\n' + JSON.stringify(a) + '\n```';

test('park card', async ({ page }) => {
  const mock = await startMockServer({
    ws: true,
    planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
    plannerEvents: [
      { type: 'text', time: now - 5000, uuid: 'pk-t1', detail: '好，先放下，交接里不再提。\n\n' +
        block({ type: 'park', text: '家里网络换不换路由器', source: '11-14 15:25 用户说' }) },
    ],
  });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click('#abnav-planner');
    const reply = page.locator('#pl-stream .event.text').first();
    await expect(reply.locator('.pl-act-bad')).toHaveCount(0);
    const card = reply.locator('.pl-act-park');
    await expect(card).toHaveText('放下 · 家里网络换不换路由器 · 出处 11-14 15:25 用户说');
    await expect(card).toHaveAttribute('title', '移进搁置区，交接里不再重抄');
  } finally {
    mock.server.close();
  }
});
