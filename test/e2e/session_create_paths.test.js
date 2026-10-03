// @ts-check
//
// auth_modal.js's three ways to start a session, each of which must paint the
// main shell itself (shell.renderMainShell, registered by dashboard.js) rather
// than wait for the next session poll:
//
//   - a palette project row (doCreateInProject) and the custom-workspace
//     modal (doCreateSession) mount the composer and focus it;
//   - the empty state's quick-ask (createQuickSession) mounts the composer,
//     writes the question into it and sends it (sendMessage, imported from
//     send_message.js) in one step.
//
// The mock rejects /ws, so the quick-ask send goes over HTTP and lands in
// mock.sendCalls.
//
// 跑法：cd test/e2e && npx playwright test session_create_paths.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.use({ viewport: { width: 1280, height: 800 } });

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'the create paths do not depend on the viewport; desktop-chrome only');
  }
});

/** @type {Awaited<ReturnType<typeof startMockServer>>} */
let mock;
test.beforeAll(async () => { mock = await startMockServer(); });
test.afterAll(async () => { await new Promise((r) => mock.server.close(r)); });
test.beforeEach(() => mock.resetCalls());

/** @param {import('@playwright/test').Page} page */
async function openPalette(page) {
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click('.hdr-btn[title="New Session"]');
  await page.waitForSelector('.cmd-palette-item');
}

test('a palette project row mounts the composer and focuses it', async ({ page }) => {
  await openPalette(page);
  await page.locator('.cmd-palette-item', { hasText: 'myproject' }).first().click();
  await expect(page.locator('#msg-input')).toBeFocused();
});

test('the custom-workspace modal mounts the composer and focuses it', async ({ page }) => {
  await openPalette(page);
  await page.locator('.cmd-palette-item', { hasText: '打开自定义工作目录' }).click();
  await page.fill('#new-workspace', '/tmp/elsewhere');
  await page.click('.modal-overlay .modal-btns button.primary');
  await expect(page.locator('.modal-overlay')).toHaveCount(0);
  await expect(page.locator('#msg-input')).toBeFocused();
});

test('quick-ask mounts the composer and sends the question', async ({ page }) => {
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('#quick-ask-input');
  await page.fill('#quick-ask-input', 'what changed today?');
  await page.press('#quick-ask-input', 'Enter');
  await expect.poll(() => mock.sendCalls.map((b) => JSON.parse(b).text)).toContain('what changed today?');
  await expect(page.locator('#msg-input')).toBeVisible();
  await expect(page.locator('#quick-ask-input')).toHaveCount(0);
});
