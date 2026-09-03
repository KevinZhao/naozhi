// @ts-check
// E2E (#2437): the model label / tuning anchor must survive a sessions poll.
//
// renderMainShell paints #header-cli + #header-model into .detail-left on
// session open. updateHeaderCLI then runs on every /api/sessions poll that
// passes fetchSessions' stats.version short-circuit; it used to overwrite
// .detail-left's innerHTML with "name vVersion", which erased #header-model
// (model gone, switch-model entry gone) and leaked the version into the text.
const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');

const KEY = 'dashboard:direct:2026-01-01-120001-2:otherproject';

let mock;

test.beforeAll(async () => {
  const sessions = defaultSessions();
  sessions.sessions[1].model = 'us.anthropic.claude-fable-5-1[1m]';
  mock = await startMockServer({ sessions });
});

test.afterAll(async () => {
  await new Promise(r => mock.server.close(r));
});

test('#header-model survives a poll that re-runs updateHeaderCLI', async ({ page }) => {
  await page.goto(`${mock.url}/dashboard`);
  await page.waitForSelector(`.session-card[data-key="${KEY}"]`, { timeout: 5000 });
  await page.locator(`.session-card[data-key="${KEY}"]`).click();

  const model = page.locator('.main-header .detail-left #header-model');
  await expect(model).toHaveText(/claude-fable-5\.1 1m/);

  // Bump stats.version (mock starts at 1 → 2) so the next 5 s poll gets past
  // the short-circuit and reaches updateHeaderCLI (same workspace: only the
  // version moves). Selecting a session resets the dashboard's lastVersion to
  // 0, so seeing it reach the bumped value proves a poll fully processed the
  // new snapshot rather than short-circuiting.
  mock.setSessionWorkspace(KEY, '/home/user/workspace/otherproject');
  await expect.poll(() => page.evaluate(() => lastVersion), { timeout: 12000 }).toBe(2);

  // The anchor is still there and still says the model.
  await expect(model).toHaveCount(1);
  await expect(model).toHaveText(/claude-fable-5\.1 1m/);
  await expect(model).toHaveAttribute('data-action', 'tuning-model');

  // The cli label kept renderMainShell's shape: name in text, version in title.
  const cli = page.locator('.main-header .detail-left #header-cli');
  await expect(cli).toHaveText('claude');
  await expect(cli).toHaveAttribute('title', 'claude v1.0.30');
  await expect(page.locator('.main-header .detail-left')).not.toContainText('v1.0.30');
});
