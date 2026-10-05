// @ts-check
// shim_wait.js against a page whose e2e shim is late or missing (#3425). A
// bare `wsm` read in that window throws, and waitForFunction does not retry a
// throwing predicate; the helpers must keep polling, and say what they saw
// when the shim never arrives.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForShim, waitForWs } = require('./shim_wait');

let mock;
test.beforeAll(async () => { mock = await startMockServer({ ws: true }); });
test.afterAll(() => mock.server.close());

test('waitForWs keeps polling while the shim has not run, then resolves on its key or value', async ({ page }) => {
  /** @type {() => void} */
  let release = () => {};
  const held = new Promise((r) => { release = () => r(undefined); });
  let requested = false;
  await page.route('**/e2e-shim.js', async (route) => { requested = true; await held; await route.continue(); });
  await page.goto(mock.url + '/dashboard', { waitUntil: 'commit' });
  await expect.poll(() => requested).toBe(true);

  const connected = waitForWs(page);
  const settled = connected.then(() => 'resolved', (e) => 'rejected: ' + e.message);
  // The window the bare form fails in: the document is up, the mirror is not.
  // Two frames give the rAF-polled predicate time to run in it.
  expect(await page.evaluate(() => typeof (/** @type {any} */ (window)).wsm)).toBe('undefined');
  await page.evaluate(() => new Promise((r) => requestAnimationFrame(() => requestAnimationFrame(r))));
  expect(await Promise.race([settled, 'pending'])).toBe('pending');

  release();
  expect(await settled).toBe('resolved');
  await waitForWs(page, 'connected');
  expect(await page.evaluate(() => /** @type {any} */ (window).nz.test.wsm.state)).toBe('connected');
});

test('a shim that never loads fails waitForShim with what the page got to', async ({ page }) => {
  await page.route('**/e2e-shim.js', (route) => route.fulfill({ status: 404, body: '' }));
  await page.goto(mock.url + '/dashboard');
  const err = await waitForShim(page, { timeout: 1000 }).then(() => null, (e) => e);
  expect(err && err.message).toMatch(/^e2e shim not installed: \{"url":"\/dashboard","shimTag":true,.*"nz":true,"nzTest":false\}; page errors: /);
});

test('waitForWs rejects a state that WS_STATES does not have', async ({ page }) => {
  await page.goto(mock.url + '/dashboard');
  await expect(waitForWs(page, 'CONECTED', { timeout: 2000 })).rejects.toThrow(/no WS state CONECTED/);
});
