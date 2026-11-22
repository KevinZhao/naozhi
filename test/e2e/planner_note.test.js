// E2E: the Planner's note block (planner RFC §15.22 ②, D139). In its
// conversation the block is one line, 旁注 · the session · the note, what it
// goes by in the tooltip, never the "看不懂的动作块" card.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const PLANNER = 'planner:main';
const A = 'dashboard:direct:2026-01-01-120000-1:myproject';
const now = Date.now();
const block = (a) => '```naozhi-action\n' + JSON.stringify(a) + '\n```';

test('note card', async ({ page }) => {
  const mock = await startMockServer({
    ws: true,
    planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
    plannerEvents: [
      { type: 'text', time: now - 5000, uuid: 'nt-t1', detail: '它和另一个会话的结论冲突，我在那个会话里写了一条旁注。\n\n' +
        block({ type: 'note', key: A, text: '这个改法和 task:ab12 的结论冲突', why: 'task:ab12 的摘要：已改成方案 B' }) },
    ],
  });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click('#abnav-planner');
    const reply = page.locator('#pl-stream .event.text').first();
    await expect(reply.locator('.pl-act-bad')).toHaveCount(0);
    const card = reply.locator('.pl-act-note');
    await expect(card).toHaveClass(/pl-act-line/);
    await expect(card.locator('.pl-act-k')).toHaveText('旁注');
    await expect(card.locator('.pl-act-name')).toHaveAttribute('data-key', A);
    await expect(card.locator('.pl-act-goal')).toHaveText(' · 这个改法和 task:ab12 的结论冲突');
    await expect(card).toHaveAttribute('title', '写在那个会话里，只给你看，不进会话上下文。依据：task:ab12 的摘要：已改成方案 B');
    await expect(card.locator('button')).toHaveCount(0);
  } finally {
    mock.server.close();
  }
});
