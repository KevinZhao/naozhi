// overlay_drift_chip.test.js — header "配置漂移" chip (#2543). A session whose
// /api/sessions row carries overlay_drift entries shows the chip with each
// field's stored→current in the tooltip; a clean session (overlay_drift: [])
// collapses the mount.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

function driftSessions() {
  const base = {
    state: 'ready',
    platform: 'dashboard',
    agent: 'general',
    cli_name: 'claude',
    cli_version: '2.1.143',
    backend: 'claude',
    model: 'claude-opus-4.7',
    workspace: '/home/user/workspace/myproject',
    last_active: Date.now() - 60000,
    node: 'local',
    project: 'myproject',
  };
  return {
    sessions: [
      {
        ...base,
        key: 'dashboard:direct:2026-01-01-120000-1:myproject',
        overlay_drift: [
          { field: 'model', stored: 'claude-opus-4.7', current: 'claude-fable-5' },
        ],
      },
      {
        ...base,
        key: 'dashboard:direct:2026-01-01-120001-2:clean',
        overlay_drift: [],
      },
    ],
    stats: { version: 1, running: 0, ready: 2, max_procs: 10 },
    nodes: {},
    history_sessions: [],
  };
}

async function selectByKey(page, keySuffix) {
  const card = page.locator(`.session-card[data-key*="${keySuffix}"]`).first();
  await card.click();
  await page.waitForTimeout(350);
}

test.describe('overlay-drift chip', () => {
  let server;
  let baseURL;

  test.beforeAll(async () => {
    server = await startMockServer({ sessions: driftSessions() });
    baseURL = server.url;
  });

  test.afterAll(async () => {
    if (server) await new Promise(r => server.server.close(r));
  });

  test('drifted session shows the chip; tooltip carries stored→current', async ({ page }) => {
    await page.goto(baseURL + '/dashboard');
    await page.waitForSelector('.session-card', { timeout: 8000 });
    await selectByKey(page, '120000-1:myproject');

    const chip = page.locator('.main-header .detail-overlaydrift .overlay-drift-tag');
    await expect(chip).toHaveCount(1);
    await expect(chip).toContainText('配置漂移');
    const title = await chip.getAttribute('title');
    expect(title).toContain('重启会话以应用新配置');
    expect(title).toContain('model: claude-opus-4.7 → claude-fable-5');
  });

  test('clean session collapses the mount', async ({ page }) => {
    await page.goto(baseURL + '/dashboard');
    await page.waitForSelector('.session-card', { timeout: 8000 });
    await selectByKey(page, 'clean');

    await expect(page.locator('.main-header .detail-overlaydrift .overlay-drift-tag')).toHaveCount(0);
    const display = await page.locator('.main-header .detail-overlaydrift').evaluate(
      el => getComputedStyle(el).display
    );
    expect(display).toBe('none');
  });
});
