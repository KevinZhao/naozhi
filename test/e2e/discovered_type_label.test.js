// @ts-check
// A discovered terminal session's type chip shows the label the backend sent
// (backend.Profile.TerminalLabel), escaped, and falls back to "CLI" when an
// older peer sends none (#2944).
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test('the type chip shows the backend-sent label, escaped, and CLI without one', async ({ browser }) => {
  const now = Date.now();
  const mock = await startMockServer({
    discovered: [
      { pid: 101, session_id: 'd-vs', cwd: '/home/user/a', proc_start_time: 1, node: 'local', cli_name: 'claude-code',
        type_label: 'Claude VS Extension', state: 'ready', started_at: now - 3000 },
      { pid: 102, session_id: 'd-x', cwd: '/home/user/b', proc_start_time: 2, node: 'local', cli_name: 'claude-code',
        type_label: '<img src=x>', state: 'ready', started_at: now - 2000 },
      { pid: 103, session_id: 'd-old', cwd: '/home/user/c', proc_start_time: 3, node: 'local', cli_name: 'kiro',
        state: 'ready', started_at: now - 1000 },
    ],
  });
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  try {
    await page.goto(mock.url + '/dashboard');
    const card = (pid) => page.locator(`.session-card[data-key^="_discovered:${pid}"] .sc-type-tag`);
    await expect(card(101)).toHaveText('Claude VS Extension');
    await expect(card(102)).toHaveText('<img src=x>');
    expect(await page.locator('.session-card img[src="x"]').count()).toBe(0);
    await expect(card(103)).toHaveText('CLI');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
