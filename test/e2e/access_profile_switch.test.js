// The header's access-profile chip moves a session that has already talked
// onto another access profile, the model riding along (RFC
// project-access-profile §7.1), against the mock server's
// /api/sessions/override and /api/access-profiles answered by page.route:
//
//   - A session on the global credentials reads 全局默认; the popover offers
//     the global default and each profile (one whose secret is missing
//     disabled), and 切换 stays off until another profile is picked.
//   - Picking a profile and a model posts access_profile + model; the chip
//     then names the profile without a reload.
//   - 保持当前 sends no model; switching back to 全局默认 posts "".
//   - A running session and a remote one get a notice, not the popover.
//
// 跑法：cd test/e2e && npx playwright test access_profile_switch.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { openSessionCard } = require('./session_nav');

const KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';
const RUNNING_KEY = 'dashboard:direct:2026-01-01-120001-2:otherproject';
const FABLE = 'global.anthropic.claude-fable-5-2[1m]';

const MANIFEST = {
  backends: [
    { id: 'claude', display_name: 'claude-code', protocol: 'stream-json', available: true,
      models: [{ id: 'claude-opus-5-5[1m]' }] },
  ],
  default: 'claude',
  detected: [],
};

const PROFILES = {
  profiles: [
    { id: 'broken', display_name: 'Broken', secret_ok: false },
    { id: 'isengard', display_name: 'Isengard · 788668107894', chip_color: 'var(--nz-amber)', secret_ok: true },
  ],
  default: '',
};

function json(body) {
  return { status: 200, contentType: 'application/json', body: JSON.stringify(body) };
}

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'viewport-independent; desktop-chrome only');
  }
});

let mock;

test.beforeEach(async ({ page }) => {
  const sessions = defaultSessions();
  sessions.sessions[0].model = 'claude-opus-5-5[1m]';
  mock = await startMockServer({ sessions });
  await page.route(url => url.pathname === '/api/cli/backends', route => route.fulfill(json(MANIFEST)));
  await page.route(url => url.pathname === '/api/access-profiles', route => route.fulfill(json(PROFILES)));
});

test.afterEach(async () => {
  await new Promise(r => mock.server.close(r));
});

test('a global session moves onto isengard with Fable 5.2, and back', async ({ page }) => {
  await page.goto(`${mock.url}/dashboard`);
  await openSessionCard(page, page.locator(`.session-card[data-key="${KEY}"]`));

  const chip = page.locator('#header-access-profile');
  await expect(chip).toHaveText('全局默认');
  await chip.click();

  const pop = page.locator('#tuning-popover');
  await expect(pop.locator('.ap-opt')).toHaveText(['● （全局默认）', '○ Broken ⚠ 凭证缺失', '○ Isengard · 788668107894']);
  const submit = pop.locator('#ap-submit');
  await expect(submit).toBeDisabled();
  // The current profile and a broken one leave 切换 off.
  await pop.locator('.ap-opt[data-profile=""]').click();
  await expect(submit).toBeDisabled();
  await pop.locator('.ap-opt[data-profile="broken"]').click();
  await expect(submit).toBeDisabled();

  await pop.locator('.ap-opt[data-profile="isengard"]').click();
  await expect(submit).toBeEnabled();
  await pop.locator('#ap-model-select').selectOption({ label: '手动输入…' });
  await pop.locator('#ap-model-input').fill(FABLE);
  await submit.click();

  await expect.poll(() => mock.overrideCalls.length).toBe(1);
  expect(mock.overrideCalls[0]).toEqual({ key: KEY, access_profile: 'isengard', model: FABLE });
  await expect(chip).toHaveText('Isengard · 788668107894');
  await expect(chip).toHaveAttribute('data-access-profile', 'isengard');

  // Back to the global credentials, keeping the model: no model is sent.
  await chip.click();
  await expect(pop.locator('.ap-opt[data-profile="isengard"]')).toHaveText('● Isengard · 788668107894');
  await pop.locator('.ap-opt[data-profile=""]').click();
  await expect(pop.locator('#ap-model-select')).toHaveValue('__keep__');
  await pop.locator('#ap-submit').click();
  await expect.poll(() => mock.overrideCalls.length).toBe(2);
  expect(mock.overrideCalls[1]).toEqual({ key: KEY, access_profile: '' });
  await expect(chip).toHaveText('全局默认');
});

test('a running session gets a notice instead of the popover', async ({ page }) => {
  await page.goto(`${mock.url}/dashboard`);
  await openSessionCard(page, page.locator(`.session-card[data-key="${RUNNING_KEY}"]`));
  await page.locator('#header-access-profile').click();
  await expect(page.locator('#tuning-toast')).toHaveText('会话正在运行：等这一轮结束再切换访问档');
  await expect(page.locator('#tuning-popover')).toHaveCount(0);
  expect(mock.overrideCalls).toHaveLength(0);
});

test('no access profile configured draws no chip', async ({ page }) => {
  await page.unroute(url => url.pathname === '/api/access-profiles');
  await page.route(url => url.pathname === '/api/access-profiles', route => route.fulfill(json({ profiles: [], default: '' })));
  await page.goto(`${mock.url}/dashboard`);
  await openSessionCard(page, page.locator(`.session-card[data-key="${KEY}"]`));
  await expect(page.locator('#header-model')).toBeVisible();
  await expect(page.locator('#header-access-profile')).toHaveCount(0);
});
