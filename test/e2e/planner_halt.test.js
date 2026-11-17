// E2E: the Planner's halt and cancel blocks (planner RFC §15.15 ①, D118–
// D120). In its conversation each is one line, ⏸ 叫停 / ■ 停止 · the session
// by its name (never its key) · the why's first sentence, the why whole in
// the tooltip, never the "看不懂的动作块" card.
const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const PLANNER = 'planner:main';
const TASK = 'task:0a1b2c3d4e5f';
const now = Date.now();
const block = (a) => '```naozhi-action\n' + JSON.stringify(a) + '\n```';

function sessions() {
  const s = defaultSessions();
  s.sessions.push({ key: TASK, state: 'running', platform: 'task', agent: 'general', cli_name: 'claude', purpose: 'task', user_label: 'iso 跑基准',
    purpose_title: 'iso 跑基准', workspace: '/home/user/workspace/myproject', project: 'myproject', node: 'local', last_active: now - 10000 });
  return s;
}

test('halt and cancel cards', async ({ page }) => {
  const mock = await startMockServer({
    ws: true, sessions: sessions(),
    planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
    plannerEvents: [
      { type: 'text', time: now - 5000, uuid: 'ph-t1', detail: '好，先停下。\n\n' +
        block({ type: 'halt', task: TASK, why: '用户说先停下。等他看完结果再说' }) + '\n\n' +
        block({ type: 'cancel', task: TASK, why: '用户说基准不用跑了' }) },
    ],
  });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click('#abnav-planner');
    const reply = page.locator('#pl-stream .event.text').first();
    await expect(reply.locator('.pl-act-bad')).toHaveCount(0);
    const halt = reply.locator('.pl-act-halt');
    await expect(halt).toHaveText('⏸ 叫停 · iso 跑基准 · 用户说先停下。');
    await expect(halt).toHaveAttribute('title', /停掉托管、关掉进程.*依据：用户说先停下。等他看完结果再说$/);
    const cancel = reply.locator('.pl-act-cancel');
    await expect(cancel).toHaveText('■ 停止 · iso 跑基准 · 用户说基准不用跑了');
    await expect(cancel).toHaveAttribute('title', /记为已完成（已停止）。依据：用户说基准不用跑了$/);
    await expect(reply).not.toContainText(TASK);
  } finally {
    mock.server.close();
  }
});
