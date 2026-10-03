// @ts-check
//
// The token prompt's two guards, as the operator sees them. showAuthModal and
// dismissAuthModal live in utilities.js with the cooldown they share;
// auth_modal.js's saveToken clears it.
//
//   - Cancel starts a cooldown: a background session poll answered 401
//     (showAuthModal({ auto: true })) does not reopen the prompt.
//   - A user action ignores the cooldown: an upload answered 401 prompts.
//   - A successful login clears the cooldown: the next background 401 prompts.
//
// /api/sessions, /api/sessions/upload and /api/auth/login are answered by
// page.route; fetchSessions is called directly so each poll is one request.
//
// 跑法：cd test/e2e && npx playwright test auth_modal_cooldown.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.use({ viewport: { width: 1280, height: 800 } });

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'the prompt logic does not depend on the viewport; desktop-chrome only');
  }
});

const KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';
const PROMPT = '.modal-overlay [aria-label="Dashboard API token"]';

/** @type {Awaited<ReturnType<typeof startMockServer>>} */
let mock;
test.beforeAll(async () => { mock = await startMockServer(); });
test.afterAll(async () => { await new Promise((r) => mock.server.close(r)); });

test('cancel holds back the background prompt, a user action still prompts, and a login clears the hold', async ({ page }) => {
  await page.goto(mock.url + '/dashboard');
  await page.click(`.session-card[data-key="${KEY}"]`);
  await page.waitForSelector('#msg-input');

  let refused = 0;
  const refuseList = (/** @type {import('@playwright/test').Route} */ route) => {
    refused++;
    return route.fulfill({ status: 401, contentType: 'application/json', body: '{"error":"unauthorized"}' });
  };
  const isList = (/** @type {URL} */ u) => u.pathname === '/api/sessions';
  const poll = async () => {
    const before = refused;
    await page.evaluate(() => window.nz.test.fetchSessions());
    // The poll must have reached the server, or "no prompt" proves nothing.
    expect(refused).toBeGreaterThan(before);
  };
  await page.route(isList, refuseList);

  // No cooldown yet: the background poll's 401 prompts.
  await poll();
  await expect(page.locator(PROMPT)).toBeVisible();

  // Cancel starts the cooldown; the next background 401 stays quiet.
  await page.click('.modal-btns button[data-action="auth-dismiss"]');
  await expect(page.locator('.modal-overlay')).toHaveCount(0);
  await poll();
  await expect(page.locator('.modal-overlay')).toHaveCount(0);

  // An upload is the operator's own action: its 401 prompts through the cooldown.
  await page.route('**/api/sessions/upload', (route) => route.fulfill({ status: 401, contentType: 'application/json', body: '{"error":"unauthorized"}' }));
  await page.evaluate(() => window.nz.test.handleFiles([new File([new Uint8Array(16)], 'a.pdf', { type: 'application/pdf' })]));
  await expect(page.locator(PROMPT)).toBeVisible();

  // A successful login closes the prompt and clears the cooldown.
  await page.unroute(isList, refuseList);
  await page.route('**/api/auth/login', (route) => route.fulfill({ status: 200, contentType: 'application/json', body: '{}' }));
  await page.fill('#token-input', 'secret-token');
  await page.click('.modal-btns button[data-action="token-save"]');
  await expect(page.locator('.modal-overlay')).toHaveCount(0);

  // So the next background 401 prompts again.
  await page.route(isList, refuseList);
  await poll();
  await expect(page.locator(PROMPT)).toBeVisible();
});
