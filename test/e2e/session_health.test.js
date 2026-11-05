// E2E: a session list row's status dot is the session's health (planner RFC
// §15.7 S8): failed red, active blue, idle grey, the state in words only in
// the title. A session that failed before a restart comes back reading
// "ready" with no death_reason; its health still holds the failure, so the
// dot, the 异常退出 chip and the home page's 最近会话 keep showing it (Q19).
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const now = Date.now();
const FAILED = 'dashboard:direct:2026-01-01-120000-1:myproject';
const IDLE = 'dashboard:direct:2026-01-01-120002-3:myproject';
const RUNNING = 'dashboard:direct:2026-01-01-120001-2:otherproject';

function row(key, state, prompt, project, extra) {
  return Object.assign({
    key, state, platform: 'dashboard', agent: 'general', cli_name: 'claude', node: 'local',
    workspace: '/home/user/workspace/' + project, project, last_prompt: prompt, last_active: now - 600000,
  }, extra);
}

const SESSIONS = {
  sessions: [
    row(FAILED, 'ready', 'iso 失败演示', 'myproject', {
      health: { state: 'failed', since: now - 300000, death_reason: 'cli_exited_code_-1', error_class: 'other' },
    }),
    row(RUNNING, 'running', 'iso 长回合', 'otherproject', { health: { state: 'active', since: now - 60000 } }),
    row(IDLE, 'ready', 'iso 销售数据', 'myproject', { health: { state: 'idle', since: now - 60000 } }),
  ],
  stats: { total: 3, running: 1, ready: 2, active: 3, uptime: '1h', backend: 'cc', max_procs: 20, default_workspace: '/home/user/workspace', agents: ['general'], projects: [], version: 1 },
  nodes: { local: { display_name: 'Local', status: 'ok' } },
};

for (const [name, viewport] of [['desktop', { width: 1440, height: 900 }], ['phone', { width: 390, height: 844 }]]) {
  test(`${name}: the status dot is the health, and a failure outlives a restart`, async ({ browser }) => {
    const mock = await startMockServer({ sessions: SESSIONS });
    const ctx = await browser.newContext({ viewport });
    const page = await ctx.newPage();
    try {
      await page.goto(mock.url + '/dashboard');
      const failed = page.locator(`.session-card[data-key="${FAILED}"]`);
      await expect(failed.locator('.sc-dot')).toHaveClass(/dot-failed/);
      await expect(failed.locator('.sc-dot')).toHaveAttribute('title', '失败：CLI 进程被信号终止');
      await expect(failed.locator('.sc-exit')).toHaveText('⚠ 异常退出');
      await expect(page.locator(`.session-card[data-key="${RUNNING}"] .sc-dot`)).toHaveClass(/dot-running/);
      await expect(page.locator(`.session-card[data-key="${IDLE}"] .sc-dot`)).toHaveClass(/dot-ready/);
      await expect(page.locator(`.session-card[data-key="${IDLE}"] .sc-dot`)).toHaveAttribute('title', '空闲');
      // No state word on any row, and no 已回收 chip.
      for (const word of ['ready', 'running', '已回收']) await expect(page.locator('#session-list')).not.toContainText(word);
      // Relative times are written one way.
      await expect(failed.locator('.sc-time')).toHaveText('10 分钟前');
      if (name === 'desktop') {
        const recent = page.locator(`#recent-sessions-panel .recent-row[data-key="${FAILED}"]`);
        await expect(recent.locator('.recent-dot')).toHaveClass(/dot-failed/);
        await expect(recent.locator('.sc-exit')).toHaveText('⚠ 异常退出');
      }
      await page.screenshot({ path: test.info().outputPath(`session-health-${name}.png`) });
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
}

// The session header is built with its 异常退出 chip on switching to a
// failed session from another one, with and without a socket: read as the
// header first shows the session, before a later poll or push can paint it.
for (const ws of [false, true]) {
  test(`header: the 异常退出 chip shows on switching to a failed session (ws: ${ws})`, async ({ page }) => {
    const mock = await startMockServer({ ws, sessions: SESSIONS });
    try {
      await page.goto(mock.url + '/dashboard');
      await page.locator(`.session-card[data-key="${IDLE}"]`).click();
      await expect(page.locator('.main-header h2')).toContainText('iso 销售数据');
      await expect(page.locator('#header-exit')).toHaveText('');
      await page.locator(`.session-card[data-key="${FAILED}"]`).click();
      const built = await page.waitForFunction(() => {
        const h = document.querySelector('.main-header h2');
        return h && h.textContent.includes('iso 失败演示') && { chip: document.getElementById('header-exit').textContent };
      });
      expect(await built.jsonValue()).toEqual({ chip: '⚠ 异常退出' });
    } finally {
      mock.server.close();
    }
  });
}
