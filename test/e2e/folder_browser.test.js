// @ts-check
// E2E: new-session command palette navigable folder browser.
// Opens on the default workspace, lets the user drill into a subfolder and
// back up, and creates a session in the browsed directory.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

let mock;

test.beforeAll(async () => {
  mock = await startMockServer();
});

test.afterAll(async () => {
  await new Promise(r => mock.server.close(r));
});

test.beforeEach(async ({ page }) => {
  mock.resetCalls();
  await page.goto(`${mock.url}/dashboard`);
  await page.waitForSelector('.session-card', { timeout: 5000 });
});

async function openPalette(page) {
  await page.click('#btn-new-session');
  await page.waitForSelector('.cmd-palette-overlay', { timeout: 5000 });
  // Wait for the async folder-browser load to populate the breadcrumb.
  await page.waitForSelector('.cmd-palette-overlay .cp-breadcrumb', { timeout: 5000 });
}

test('palette opens on the workspace folder browser with breadcrumb + entries', async ({ page }) => {
  await openPalette(page);
  // Breadcrumb shows the workspace path.
  await expect(page.locator('.cp-breadcrumb .cp-bc-path')).toContainText('workspace');
  // "create here" row is present and is the default-selected (active) row.
  const here = page.locator('.cmd-palette-item', { hasText: '在此目录新建会话' });
  await expect(here).toHaveCount(1);
  await expect(here).toHaveClass(/active/);
  // Root listing: src/ and docs/ dirs + README.md file.
  await expect(page.locator('.cmd-palette-item', { hasText: 'src' }).first()).toBeVisible();
  await expect(page.locator('.cmd-palette-item', { hasText: 'README.md' })).toBeVisible();
});

test('clicking a subfolder drills in and shows the up affordance', async ({ page }) => {
  await openPalette(page);
  await page.locator('.cmd-palette-item', { hasText: 'src' }).first().click();
  // Now inside src/: breadcrumb updates, "up" row appears, main.go listed.
  await expect(page.locator('.cp-breadcrumb .cp-bc-path')).toContainText('src');
  await expect(page.locator('.cmd-palette-item', { hasText: '上级目录' })).toHaveCount(1);
  await expect(page.locator('.cmd-palette-item', { hasText: 'main.go' })).toBeVisible();
});

test('up row navigates back to the workspace root', async ({ page }) => {
  await openPalette(page);
  await page.locator('.cmd-palette-item', { hasText: 'src' }).first().click();
  await page.waitForSelector('.cmd-palette-item:has-text("上级目录")');
  await page.locator('.cmd-palette-item', { hasText: '上级目录' }).click();
  // Back at root: no up row, README.md visible again.
  await expect(page.locator('.cmd-palette-item', { hasText: '上级目录' })).toHaveCount(0);
  await expect(page.locator('.cmd-palette-item', { hasText: 'README.md' })).toBeVisible();
});

test('create-here in a subfolder opens a session in that directory', async ({ page }) => {
  await openPalette(page);
  await page.locator('.cmd-palette-item', { hasText: 'src' }).first().click();
  await page.waitForSelector('.cmd-palette-item:has-text("在此目录新建会话")');
  await page.locator('.cmd-palette-item', { hasText: '在此目录新建会话' }).click();
  // Palette closes and the main chat shell renders for the new session.
  await expect(page.locator('.cmd-palette-overlay')).toHaveCount(0);
  await expect(page.locator('#msg-input')).toBeVisible({ timeout: 5000 });
});

test('typing a query hides the folder browser and restores project search', async ({ page }) => {
  await openPalette(page);
  await page.fill('#cp-input', 'myproject');
  // Browser breadcrumb gone; matching project row shown.
  await expect(page.locator('.cp-breadcrumb')).toHaveCount(0);
  await expect(page.locator('.cmd-palette-item', { hasText: 'myproject' })).toBeVisible();
});
