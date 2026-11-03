// E2E: the 系统 view's 全会话健康 table renders GET /api/system/health
// (counts, unowned, state labels, error classes) and a row opens its session.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';
const now = Date.now();
const iso = (ms) => new Date(ms).toISOString();

// Already in the server's order: abnormal first.
const HEALTH = {
  at: iso(now - 20000),
  unowned: 2,
  units: [
    { id: KEY, kind: 'session', workspace: 'ws_a', workspace_name: 'myproject', source: 'router', coverage: 'full',
      state: 'failed', evidence: 'last turn ended in an API error', error_class: 'auth', since: iso(now - 60000), last_active: iso(now - 60000) },
    { id: 'gpu-box/dashboard:direct:x:general', kind: 'remote', source: 'node:gpu-box', coverage: 'snapshot',
      state: 'lost', evidence: 'node gpu-box unreachable', since: iso(now - 120000) },
    { id: 'cron:nightly', kind: 'cron', workspace: 'ws_a', workspace_name: 'myproject', source: 'cron', coverage: 'full',
      state: 'idle', since: iso(now - 3600000), last_active: iso(now - 3600000) },
    { id: 'external:abc', kind: 'external', source: 'discovery', coverage: 'snapshot',
      state: 'idle', since: iso(now - 3600000) },
  ],
};

let mock;

test.beforeAll(async () => {
  mock = await startMockServer({ systemHealth: HEALTH });
});

test.afterAll(async () => {
  await new Promise(r => mock.server.close(r));
});

test('health table lists units and a row opens its session', async ({ page }) => {
  await page.goto(`${mock.url}/dashboard`);
  await page.waitForSelector('.session-card');
  await page.click('#abnav-system');

  const section = page.locator('.sys-health');
  await expect(section.locator('.sh-table tbody tr')).toHaveCount(4);
  await expect(section.locator('[data-count="unowned"] b')).toHaveText('2');
  await expect(section.locator('.sh-counts')).toContainText('失败 1');
  await expect(section.locator('.sh-counts')).toContainText('空闲 2');

  const first = section.locator('tbody tr').first();
  await expect(first.locator('.sh-state')).toHaveText('失败');
  await expect(first.locator('.sh-state')).toHaveClass(/fail/);
  await expect(first.locator('.sh-state')).toHaveAttribute('title', /last turn ended in an API error/);
  await expect(first).toContainText('凭证 / 权限拒绝');
  await expect(first).toContainText('myproject');
  await expect(first.locator('.sh-open')).toHaveText('hello world');

  const lost = section.locator(`tr[data-unit="gpu-box/dashboard:direct:x:general"]`);
  await expect(lost.locator('.sh-state')).toHaveText('失联');
  await expect(lost).toContainText('无人负责');
  // Nothing on the dashboard to open for these: plain text, no button.
  await expect(lost.locator('.sh-open')).toHaveCount(0);
  await expect(section.locator('tr[data-unit="cron:nightly"] .sh-name')).toContainText('nightly');

  await first.locator('.sh-open').click();
  await expect(page.locator('#system-main')).toBeHidden();
  await expect(page.locator(`.session-card.active[data-key="${KEY}"]`)).toHaveCount(1);
});
