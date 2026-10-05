// @ts-check
//
// A takeover the server refuses before SIGTERM (internal/dashboard/discovery
// writeTakeoverRefusal) names its cause, keeps the discovered card (the
// external CLI is still running) and gives the composer back. The bodies are
// the ones writeTakeoverRefusal sends.
//
// 跑法：cd test/e2e && npx playwright test takeover_refused.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, '仅 desktop-chrome project 跑');
  }
});

/** @type {Awaited<ReturnType<typeof startMockServer>>} */
let mock;
test.beforeAll(async () => {
  mock = await startMockServer({
    discovered: [
      { pid: 778, session_id: 'disc-refused', cwd: '/home/user/workspace/busyproj', proc_start_time: 1,
        node: 'local', cli_name: 'claude-code', type_label: 'Claude Terminal', state: 'ready', started_at: Date.now() - 70000 },
    ],
  });
});
test.afterAll(() => mock.server.close());

for (const c of [
  { status: 503, error: 'takeover refused: max concurrent processes reached', want: '接管进程失败：进程数已满，外部进程未被终止；请先关闭一个空闲会话后重试' },
  { status: 409, error: 'takeover already in progress', want: '接管进程失败：该会话正在被接管，请稍候' },
  { status: 503, error: 'takeover refused: router is shutting down', want: '接管进程失败：服务正在重启，外部进程未被终止，请稍后重试' },
]) {
  test(`takeover refused with HTTP ${c.status} "${c.error}" names the cause and keeps the card`, async ({ page }) => {
    let takeovers = 0;
    await page.route('**/api/discovered/takeover', (route) => {
      takeovers++;
      return route.fulfill({ status: c.status, contentType: 'text/plain; charset=utf-8', body: c.error + '\n' });
    });
    await page.goto(mock.url + '/dashboard');
    const card = page.locator('.session-card[data-key^="_discovered:778"]');
    await card.click();
    const input = page.locator('#msg-input');
    await expect(input).toHaveAttribute('data-placeholder', 'send a message to take over...');
    await page.fill('#msg-input', 'take it over');
    await page.click('#btn-send');

    const toast = page.locator('.toast', { hasText: '接管进程失败' });
    await expect(toast).toContainText(c.want);
    await expect(toast).not.toContainText('服务暂时不可用');
    await expect(toast).not.toContainText('状态冲突');
    expect(takeovers).toBe(1);
    await expect(card).toHaveCount(1);
    await expect(input).toHaveAttribute('contenteditable', 'true');
    await expect(input).toHaveAttribute('data-placeholder', 'send a message to take over...');
  });
}
