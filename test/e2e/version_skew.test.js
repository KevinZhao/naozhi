// @ts-check
// Version skew (#3006): the page's nz-asset-version meta names the assets it
// booted with, auth_ok's asset_version those the server serves now. A
// difference shows a banner with no close control; an idle tab reloads itself,
// at most once per server version, and never over typed text or while the
// operator watches a running turn.
//
// 跑法：cd test/e2e && npx playwright test version_skew.test.js --project=desktop-chrome
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };
const READY_KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';
const RUNNING_KEY = 'dashboard:direct:2026-01-01-120001-2:otherproject';

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') testInfo.skip(true, 'engine-neutral DOM + WS logic');
});

// hide makes the tab report itself hidden, as a background tab does.
function hide() {
  Object.defineProperty(document, 'hidden', { configurable: true, get: () => true });
  Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'hidden' });
}

async function openDashboard(browser, overrides, { hidden = false, clock = false } = {}) {
  const mock = await startMockServer({ ws: true, ...overrides });
  const ctx = await browser.newContext(desktop);
  const page = await ctx.newPage();
  if (clock) await page.clock.install();
  if (hidden) await page.addInitScript(hide);
  const loads = [];
  page.on('request', (r) => { if (new URL(r.url()).pathname === '/dashboard') loads.push(r.url()); });
  const errors = [];
  page.on('pageerror', (e) => errors.push(e.message));
  // A throwing frame handler is caught by ws_manager's onmessage and logged.
  page.on('console', (m) => { if (m.type() === 'error' && !/Failed to load resource|require-sri-for/.test(m.text())) errors.push(m.text()); });
  await page.goto(mock.url + '/dashboard');
  const close = async () => { await ctx.close(); mock.server.close(); };
  return { mock, page, loads, errors, close };
}

const banner = (page) => page.locator('#asset-skew-banner');
// noReload: no reload has started (loads still holds only the first load) and
// none starts within a second; the decision is taken synchronously, so a
// second is ample, and the count catches one that started before the wait.
const noReload = async (page, loads) => {
  await expect(page.waitForRequest((r) => new URL(r.url()).pathname === '/dashboard', { timeout: 1000 })).rejects.toThrow();
  expect(loads).toHaveLength(1);
};
const connected = (page) => page.waitForFunction(() => !!window.wsm && window.wsm.state === window.WS_STATES.CONNECTED);

test('a skewed visible tab shows a banner with no close control; clicking it reloads', async ({ browser }) => {
  const { page, loads, close } = await openDashboard(browser, { assetVersion: 'aaaa', wsAssetVersion: 'bbbb' });
  await connected(page);
  await expect(banner(page)).toBeVisible();
  await expect(banner(page).locator('button')).toHaveCount(1);
  await expect(banner(page)).toHaveText('naozhi 已更新，点击刷新');
  await noReload(page, loads); // fresh input on a visible tab holds it
  await banner(page).click();
  await expect.poll(() => loads.length).toBe(2);
  await close();
});

for (const [name, overrides] of [
  ['matching versions', { assetVersion: 'aaaa', wsAssetVersion: 'aaaa' }],
  ['a page without the meta', { wsAssetVersion: 'bbbb' }],
  ['an auth_ok without asset_version', { assetVersion: 'aaaa' }],
]) {
  test(`${name}: no banner`, async ({ browser }) => {
    const { page, errors, close } = await openDashboard(browser, overrides, { hidden: true });
    await connected(page);
    await expect(banner(page)).toBeHidden();
    expect(errors).toEqual([]);
    await close();
  });
}

test('a hidden idle tab reloads once; still skewed after it, it keeps the banner instead of looping', async ({ browser }) => {
  const { page, loads, close } = await openDashboard(browser, { assetVersion: 'aaaa', wsAssetVersion: 'bbbb' }, { hidden: true });
  await expect.poll(() => loads.length).toBe(2);
  await connected(page);
  await expect(banner(page)).toBeVisible();
  expect(await page.evaluate(() => sessionStorage.getItem('nz-asset-reload'))).toBe('bbbb');
  expect(loads).toHaveLength(2);
  await close();
});

for (const [name, hold, release] of [
  ['typed composer text', (page) => page.locator('#msg-input').fill('draft'), (page) => page.locator('#msg-input').fill('')],
  ['an open dialog', (page) => page.evaluate(() => { window.promptDialog({ title: 'x' }); }), (page) => page.click('.prompt-cancel')],
  // A switch parks the composer text in memory only; the composer itself is empty.
  ['a draft parked on another session', async (page) => {
    await page.locator('#msg-input').fill('draft');
    await page.click(`.session-card[data-key="${RUNNING_KEY}"]`);
    await expect(page.locator('#msg-input')).toHaveText('');
  }, async (page) => {
    await page.click(`.session-card[data-key="${READY_KEY}"]`);
    await expect(page.locator('#msg-input')).toHaveText('draft');
    await page.locator('#msg-input').fill('');
    await page.click(`.session-card[data-key="${RUNNING_KEY}"]`);
  }],
]) {
  test(`${name} holds the reload of a hidden tab until it goes`, async ({ browser }) => {
    const { mock, page, loads, close } = await openDashboard(browser, { assetVersion: 'aaaa', wsHoldAuth: true });
    await page.click(`.session-card[data-key="${READY_KEY}"]`);
    await hold(page);
    await page.evaluate(hide);
    mock.wsConnections[mock.wsConnections.length - 1].send({ type: 'auth_ok', asset_version: 'bbbb' });
    await expect(banner(page)).toBeVisible();
    await noReload(page, loads);
    await release(page);
    await page.evaluate(() => document.dispatchEvent(new Event('visibilitychange')));
    await expect.poll(() => loads.length).toBe(2);
    await close();
  });
}

test('a visible tab reloads after a minute without input, but not while its session runs', async ({ browser }) => {
  const { mock, page, loads, close } = await openDashboard(browser, { assetVersion: 'aaaa', wsHoldAuth: true }, { clock: true });
  await page.click(`.session-card[data-key="${RUNNING_KEY}"]`);
  mock.wsConnections[mock.wsConnections.length - 1].send({ type: 'auth_ok', asset_version: 'bbbb' });
  await expect(banner(page)).toBeVisible();
  await page.clock.runFor(91000);
  await noReload(page, loads); // the running turn holds it
  await page.click(`.session-card[data-key="${READY_KEY}"]`);
  await page.clock.runFor(91000);
  await expect.poll(() => loads.length).toBe(2);
  await close();
});

test('wheel scrolling on a visible tab counts as input', async ({ browser }) => {
  const { mock, page, loads, close } = await openDashboard(browser, { assetVersion: 'aaaa', wsHoldAuth: true }, { clock: true });
  await page.click(`.session-card[data-key="${READY_KEY}"]`);
  mock.wsConnections[mock.wsConnections.length - 1].send({ type: 'auth_ok', asset_version: 'bbbb' });
  await expect(banner(page)).toBeVisible();
  // Wheel reaches the page asynchronously; wait for each before moving the clock.
  await page.evaluate(() => { window.__wheels = 0; document.addEventListener('wheel', () => { window.__wheels++; }, { passive: true }); });
  await page.mouse.move(640, 400);
  for (let i = 1; i <= 3; i++) {
    await page.clock.runFor(50000);
    await page.mouse.wheel(0, -200);
    await expect.poll(() => page.evaluate(() => window.__wheels)).toBe(i);
  }
  await noReload(page, loads); // 150s on the clock, never 60s without a wheel
  await page.clock.runFor(91000);
  await expect.poll(() => loads.length).toBe(2);
  await close();
});
