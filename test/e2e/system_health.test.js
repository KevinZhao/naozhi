// E2E: the 系统 view's 全会话健康 table renders GET /api/system/health
// (counts, state labels, error classes) and a row opens its session.
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
    { id: 'cron:nightly', kind: 'cron', workspace: 'ws_a', workspace_name: 'myproject', name: '夜间构建', source: 'cron', coverage: 'full',
      state: 'idle', since: iso(now - 3600000), last_active: iso(now - 3600000) },
    { id: 'external:abc', kind: 'external', source: 'discovery', coverage: 'snapshot',
      state: 'idle', since: iso(now - 3600000) },
    { id: 'planner:main', kind: 'session', source: 'router', coverage: 'full',
      state: 'idle', since: iso(now - 3600000), last_active: iso(now - 7200000) },
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
  await expect(section.locator('.hl-table tbody tr')).toHaveCount(5);
  // No daemons (sysession off): neither they nor their introduction show,
  // and the overview leaves the session counts to the table.
  await expect(page.locator('#system-main .system-intro')).toHaveCount(0);
  await expect(page.locator('#system-main .system-body > .system-empty')).toHaveCount(0);
  await expect(page.locator('#system-main .svc-overview')).not.toContainText('就绪');
  // A unit with no workspace is not counted as a problem (Q22).
  await expect(section.locator('[data-count="unowned"]')).toHaveCount(0);
  await expect(section.locator('.hl-counts')).toContainText('失败 1');
  await expect(section.locator('.hl-counts')).toContainText('空闲 3');

  const first = section.locator('tbody tr').first();
  await expect(first.locator('.hl-state')).toHaveText('失败');
  await expect(first.locator('.hl-state')).toHaveClass(/fail/);
  await expect(first.locator('.hl-state')).toHaveAttribute('title', /last turn ended in an API error/);
  // The class is in the state's title, not a column of its own.
  await expect(first.locator('.hl-state')).toHaveAttribute('title', /凭证 \/ 权限拒绝/);
  await expect(section.locator('thead')).not.toContainText('错误类别');
  await expect(first).toContainText('myproject');
  await expect(first.locator('.hl-open')).toHaveText('hello world');

  const lost = section.locator(`tr[data-unit="gpu-box/dashboard:direct:x:general"]`);
  await expect(lost.locator('.hl-state')).toHaveText('失联');
  await expect(lost).toContainText('无工作区');
  await expect(section).not.toContainText('无人负责');
  // Nothing on the dashboard to open for these: plain text, no button.
  await expect(lost.locator('.hl-open')).toHaveCount(0);
  // A cron job reads by its title; naozhi's own units belong to no workspace.
  await expect(section.locator('tr[data-unit="cron:nightly"] .hl-name')).toHaveText('夜间构建');
  await expect(section.locator('tr[data-unit="planner:main"] .hl-col-ws')).toHaveText('—');
  await expect(section.locator('.hl-at')).toContainText('更新于');
  await page.screenshot({ path: test.info().outputPath('system-health.png'), fullPage: true });

  await first.locator('.hl-open').click();
  await expect(page.locator('#system-main')).toBeHidden();
  await expect(page.locator(`.session-card.active[data-key="${KEY}"]`)).toHaveCount(1);
});
