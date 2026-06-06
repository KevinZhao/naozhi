// @ts-check
// E2E: pending-session workspace persistence (new-session cwd-fallback fix).
//
// Regression guard for the bug where creating a session in a project and then
// reloading the page BEFORE sending the first message dropped the chosen
// workspace — the next send carried no workspace, the backend never wrote a
// per-chat override, and the session spawned in defaultCWD (workspace root)
// instead of the project dir.
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
  // Clear any pending blob left by a prior test in this browser context.
  await page.evaluate(() => localStorage.removeItem('nz:pending_sessions'));
});

const PROJ = '/home/user/workspace/myproject';

test('create persists pending workspace to localStorage and eager-binds', async ({ page }) => {
  const key = await page.evaluate((proj) => {
    // Drive the real create funnel directly (palette UI is covered elsewhere).
    doCreateInProject(proj, 'myproject', 'local', undefined, 'general', { mode: 'new' });
    return selectedKey;
  }, PROJ);

  // Durable blob written with the project workspace.
  const blob = await page.evaluate(() => JSON.parse(localStorage.getItem('nz:pending_sessions') || '{}'));
  expect(blob[key]).toBeTruthy();
  expect(blob[key].ws).toBe(PROJ);

  // Eager-bind fired to the backend with the workspace (cross-browser robustness).
  await expect.poll(() => mock.bindCalls.length).toBeGreaterThan(0);
  const bound = JSON.parse(mock.bindCalls[mock.bindCalls.length - 1]);
  expect(bound.key).toBe(key);
  expect(bound.workspace).toBe(PROJ);
  expect(bound.node).toBe('local');
});

test('reload-before-send still carries the workspace on the first send', async ({ page }) => {
  const key = await page.evaluate((proj) => {
    doCreateInProject(proj, 'myproject', 'local', undefined, 'general', { mode: 'new' });
    return selectedKey;
  }, PROJ);

  // Simulate the proven trigger: reload BEFORE sending the first message.
  await page.reload();
  await page.waitForSelector('.session-card', { timeout: 5000 });
  // Let fetchSessions merge the restored pending session into the sidebar.
  await page.waitForFunction((k) => !!sessionWorkspaces[k], key, { timeout: 5000 });

  // The in-memory map was wiped by reload, but restorePending() rehydrated it.
  const restoredWs = await page.evaluate((k) => sessionWorkspaces[k], key);
  expect(restoredWs).toBe(PROJ);

  // Open the restored session (renders the chat shell + #msg-input), seed the
  // composer via the real setter, then send. The payload MUST carry the
  // rehydrated workspace.
  await page.evaluate((k) => {
    selectSession(k, 'local');
    const input = document.getElementById('msg-input');
    if (input) setMsgValue(input, 'hello');
    return sendMessage();
  }, key);
  await expect.poll(() => mock.sendCalls.length, { timeout: 8000 }).toBeGreaterThan(0);
  const sent = JSON.parse(mock.sendCalls[mock.sendCalls.length - 1]);
  expect(sent.workspace).toBe(PROJ);
});

test('consumed send clears the durable localStorage entry', async ({ page }) => {
  const key = await page.evaluate((proj) => {
    doCreateInProject(proj, 'myproject', 'local', undefined, 'general', { mode: 'new' });
    return selectedKey;
  }, PROJ);

  await page.evaluate((k) => {
    selectSession(k, 'local');
    const input = document.getElementById('msg-input');
    if (input) setMsgValue(input, 'hello');
    return sendMessage();
  }, key);
  await expect.poll(() => mock.sendCalls.length, { timeout: 8000 }).toBeGreaterThan(0);

  const blob = await page.evaluate(() => JSON.parse(localStorage.getItem('nz:pending_sessions') || '{}'));
  expect(blob[key]).toBeFalsy();
});

test('malformed pending blob is ignored without throwing', async ({ page }) => {
  // Seed junk + a non-absolute workspace entry, then reload.
  await page.evaluate(() => {
    localStorage.setItem('nz:pending_sessions', JSON.stringify({
      'dashboard:direct:evil:general': { ws: 'etc/passwd' },   // relative → rejected
      'dashboard:direct:ok:general': { ws: '/home/user/workspace/myproject' },
    }));
  });
  await page.reload();
  await page.waitForSelector('.session-card', { timeout: 5000 });

  // Page is interactive (no throw), the relative entry was dropped, the
  // absolute one survived.
  const maps = await page.evaluate(() => ({
    evil: sessionWorkspaces['dashboard:direct:evil:general'],
    ok: sessionWorkspaces['dashboard:direct:ok:general'],
  }));
  expect(maps.evil).toBeUndefined();
  expect(maps.ok).toBe('/home/user/workspace/myproject');
});
