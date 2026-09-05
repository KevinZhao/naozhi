// spawn_diag_chip.test.js — header "配置未生效" chip for spawn-gate rejections
// (#2532). A session whose /api/sessions row carries spawn_diags entries must
// show the warning chip with every ineffective item in its tooltip; a clean
// session (spawn_diags: []) must collapse the mount entirely.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

function diagSessions() {
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
        // The #2412 shape: config args smuggled --effort, the argv denylist
        // stripped it, and the session snapshot reports the drop.
        spawn_diags: [
          { layer: 'argv-denylist', key: '--effort', action: 'dropped', reason: 'flag is denied in ExtraArgs' },
          { layer: 'caps', key: 'effort', action: 'ignored', reason: 'backend does not support a thinking-effort tier' },
        ],
      },
      {
        ...base,
        key: 'dashboard:direct:2026-01-01-120001-2:clean',
        spawn_diags: [],
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

test.describe('spawn-diag warning chip', () => {
  let server;
  let baseURL;

  test.beforeAll(async () => {
    server = await startMockServer({ sessions: diagSessions() });
    baseURL = server.url;
  });

  test.afterAll(async () => {
    if (server) await new Promise(r => server.server.close(r));
  });

  test('session with spawn_diags shows the chip; tooltip names each item', async ({ page }) => {
    await page.goto(baseURL + '/dashboard');
    await page.waitForSelector('.session-card', { timeout: 8000 });
    await selectByKey(page, '120000-1:myproject');

    const chip = page.locator('.main-header .detail-spawndiag .spawn-diag-tag');
    await expect(chip).toHaveCount(1);
    await expect(chip).toContainText('配置未生效');
    const title = await chip.getAttribute('title');
    expect(title).toContain('以下配置未生效');
    expect(title).toContain('--effort (dropped)');
    expect(title).toContain('effort (ignored)');
  });

  test('clean session collapses the mount and switching back repaints it', async ({ page }) => {
    await page.goto(baseURL + '/dashboard');
    await page.waitForSelector('.session-card', { timeout: 8000 });

    await selectByKey(page, 'clean');
    await expect(page.locator('.main-header .detail-spawndiag .spawn-diag-tag')).toHaveCount(0);
    // :empty collapse — the empty mount must not occupy the row.
    const visible = await page.locator('.main-header .detail-spawndiag').evaluate(
      el => getComputedStyle(el).display
    );
    expect(visible).toBe('none');

    await selectByKey(page, '120000-1:myproject');
    await expect(page.locator('.main-header .detail-spawndiag .spawn-diag-tag')).toHaveCount(1);
  });
});
