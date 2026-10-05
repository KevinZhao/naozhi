// @ts-check
//
// The backend catalog (backend_catalog.js) as its readers see it, against
// /api/cli/backends, /api/access-profiles and /api/projects/config answered
// by page.route (the mock server serves none of them):
//
//   - The composer's feature gates follow the backends manifest even when it
//     lands after the session opened: fetchCLIBackends re-applies them, so a
//     kiro session's image button turns disabled without another render.
//   - Project settings builds its backend and access-profile pickers from the
//     catalog, preselects the project's saved values, shows a profile whose
//     secret is missing as a disabled option, and labels the effective-link
//     preview with the profile's display name.
//
// 跑法：cd test/e2e && npx playwright test backend_catalog.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { openSessionCard } = require('./session_nav');

const KIRO_KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';

const MANIFEST = {
  backends: [
    { id: 'claude', display_name: 'claude-code', protocol: 'stream-json', available: true, features: { image_input: true } },
    { id: 'kiro', display_name: 'kiro', protocol: 'acp', available: true, features: { image_input: false } },
  ],
  default: 'claude',
  detected: [],
};

const PROFILES = {
  profiles: [
    { id: 'broken', display_name: 'Broken', secret_ok: false },
    { id: 'team', display_name: 'Team Bedrock', default_model: 'opus', secret_ok: true },
  ],
  default: '',
};

/** @param {object} body */
function json(body) {
  return { status: 200, contentType: 'application/json', body: JSON.stringify(body) };
}

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'viewport-independent; desktop-chrome only');
  }
});

let mock;

test.beforeAll(async () => {
  const sessions = defaultSessions();
  Object.assign(sessions.sessions[0], { backend: 'kiro', cli_name: 'kiro' });
  mock = await startMockServer({ sessions });
});

test.afterAll(async () => {
  await new Promise(r => mock.server.close(r));
});

test('a backends manifest that lands after the session opened still gates the image button', async ({ page }) => {
  /** @type {() => void} */
  let release = () => {};
  const held = new Promise(r => { release = () => r(undefined); });
  await page.route(url => url.pathname === '/api/cli/backends', async route => {
    await held;
    await route.fulfill(json(MANIFEST));
  });
  await page.goto(`${mock.url}/dashboard`);
  await openSessionCard(page, page.locator(`.session-card[data-key="${KIRO_KEY}"]`));
  const btn = page.locator('button[data-action="file-picker"]');
  await expect(btn).toBeVisible();
  // No manifest yet: the gates have nothing to read and leave the button alone.
  await expect(btn).toBeEnabled();

  release();
  await expect(btn).toBeDisabled();
  await expect(btn).toHaveAttribute('title', '当前后端 (kiro) 不支持图片上传');
});

test('project settings draws its pickers from the catalog and preselects the saved values', async ({ page }) => {
  await page.route(url => url.pathname === '/api/cli/backends', route => route.fulfill(json(MANIFEST)));
  await page.route(url => url.pathname === '/api/access-profiles', route => route.fulfill(json(PROFILES)));
  await page.route(url => url.pathname === '/api/projects/config', route => route.fulfill(json({ access_profile: 'team', backend: 'kiro' })));
  await page.goto(`${mock.url}/dashboard`);
  await page.locator('[data-action="project-settings"][data-name="myproject"]').click();

  const backend = page.locator('#ps-backend');
  await expect(backend).toHaveValue('kiro');
  await expect(backend.locator('option')).toHaveText(['自动（claude-code）', 'claude-code', 'kiro']);
  const profile = page.locator('#ps-access-profile');
  await expect(profile).toHaveValue('team');
  await expect(profile.locator('option[value="broken"]')).toBeDisabled();
  await expect(profile.locator('option[value="broken"]')).toHaveText('Broken ⚠ 凭证缺失');
  await expect(page.locator('#ps-preview')).toHaveText('生效链路：Team Bedrock → opus');
});
