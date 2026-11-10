// E2E: the Planner's remember block (planner RFC §15.12 ⑧, D106). In its
// conversation the block is one line, 记下了 · where · the text's first
// sentence, its rows and 撤回 in the fold; 撤回 asks the gateway to take the
// memory back and the button says it was.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const PLANNER = 'planner:main';
const now = Date.now();
const block = (a) => '```naozhi-action\n' + JSON.stringify(a) + '\n```';

test('remember card and 撤回', async ({ page }) => {
  const mock = await startMockServer({
    ws: true,
    planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
    plannerEvents: [
      { type: 'text', time: now - 5000, uuid: 'rm-t1', detail: '好，记下了。\n\n' +
        block({ type: 'remember', workspace: 'myproject', name: 'frankfurt-az', text: '法兰克福用 euc1-az2。别用 az1，容量不够。' }) },
    ],
  });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click('#abnav-planner');
    const reply = page.locator('#pl-stream .event.text').first();
    await expect(reply.locator('.pl-act-bad')).toHaveCount(0);
    const card = reply.locator('.pl-act-remember');
    await expect(card.locator('summary')).toHaveText('记下了 · myproject · 法兰克福用 euc1-az2。');
    await expect(card).not.toHaveAttribute('open', /.*/);
    await card.locator('summary').click();
    await expect(card).toContainText('frankfurt-az');
    const undo = card.locator('button[data-action="planner-memory-forget"]');
    await undo.click();
    await expect(undo).toHaveText('已撤回');
    await expect(undo).toBeDisabled();
    expect(mock.plannerForgetCalls).toEqual([{ workspace: 'myproject', name: 'frankfurt-az' }]);
  } finally {
    mock.server.close();
  }
});
