// E2E: a message to the Planner whose snapshot was unchanged carries only
// <naozhi-snapshot unchanged since="HH:MM"/> (planner/agent nextSnapshot);
// its bubble shows what you wrote, as with a full snapshot, also after a
// handed-over session.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const PLANNER = 'planner:main';
const SESSION = 'dashboard:direct:2026-01-01-120000-1:myproject';
const now = Date.now();
const UNCHANGED = '<naozhi-snapshot unchanged since="10:00"/>';
const REF = '<naozhi-session-ref>{"key":"' + SESSION + '","name":"iso-hello","title":"hello world"}</naozhi-session-ref>\n';

test('an unchanged snapshot mark is not shown in your bubble', async ({ browser }) => {
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const page = await ctx.newPage();
  const mock = await startMockServer({
    ws: true,
    plannerEvents: [
      { type: 'user', detail: UNCHANGED + '\n\niso 进展如何？', summary: UNCHANGED + ' iso 进展如何？', time: now - 60000, uuid: 'ps-u1' },
      { type: 'user', detail: REF + UNCHANGED + '\n\niso 帮我盯着', time: now - 50000, uuid: 'ps-u2' },
    ],
    planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
  });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.evaluate(() => document.getElementById('abnav-planner').click());
    const users = page.locator('#pl-stream .event.user');
    await expect(users).toHaveCount(2);
    await expect(users.nth(0)).toContainText('iso 进展如何？');
    await expect(users.nth(1)).toContainText('iso 帮我盯着');
    await expect(users.nth(1).locator('.pl-handoff .pl-scard')).toHaveAttribute('data-key', SESSION);
    for (const i of [0, 1]) {
      await expect(users.nth(i)).not.toContainText('naozhi-snapshot');
      await expect(users.nth(i)).not.toContainText('消息过长');
    }
  } finally {
    await ctx.close();
    await new Promise((r) => mock.server.close(r));
  }
});
