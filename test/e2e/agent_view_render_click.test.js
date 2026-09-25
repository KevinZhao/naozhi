// @ts-check
//
// The sub-agent view, driven instead of grepped.
//
// 1. Its transcript renders through the dashboard's shared bubble renderer.
//    A past revision called a window.renderEvent that dashboard.js never
//    defined and silently fell back to a plain-text stub, dropping markdown
//    in the sub-agent panel. A markdown entry must therefore come out as
//    markup, not as its source text.
// 2. An agent row drills in through one delegated click listener that reads
//    the task id from data-task. The CSP carries no 'unsafe-inline', so an
//    inline onclick regression would make the click do nothing. A row
//    without a task id is a dead tap and leaves the current view alone. The
//    row's label is escaped text, never markup.
//
// Rows are produced by the real agentRowHtml, so the markup under test is
// the markup the running banner shows.
//
// 跑法：cd test/e2e && npx playwright test agent_view_render_click.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };
const SESSION_KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';
const TASK_ID = 'taskrender1';
const BASE = 1750000000000;

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'viewport-independent; desktop-chrome only');
  }
});

/** @param {import('@playwright/test').Browser} browser @param {any} mock */
async function openSession(browser, mock) {
  const ctx = await browser.newContext({ ...desktop });
  const page = await ctx.newPage();
  /** @type {string[]} */
  const pageErrors = [];
  page.on('pageerror', (e) => pageErrors.push(String(e)));
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click(`.session-card[data-key="${SESSION_KEY}"]`);
  await page.waitForSelector('#events-scroll');
  return { ctx, page, pageErrors };
}

test('sub-agent transcript renders markdown through the shared renderer', async ({ browser }) => {
  const mock = await startMockServer({
    agentEvents: {
      [TASK_ID]: [
        { time: BASE, type: 'user', summary: 'go', detail: 'go' },
        { time: BASE + 1000, type: 'text', summary: 'found **the bug** in `store.go`', detail: 'found **the bug** in `store.go`' },
      ],
    },
  });
  const { ctx, page, pageErrors } = await openSession(browser, mock);
  try {
    await page.evaluate((id) => (/** @type {any} */ (window)).nz.views.agent.switchTo(id), TASK_ID);
    const bubble = page.locator('#events-scroll > .event').filter({ hasText: 'the bug' });
    await expect(bubble).toHaveCount(1);
    await expect(bubble.locator('strong'), 'markdown bold must be markup, not literal asterisks').toHaveText('the bug');
    await expect(bubble.locator('code')).toHaveText('store.go');
    expect(await bubble.innerText()).not.toContain('**');
    expect(pageErrors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('agent row: delegated click drills in by data-task; no-task row is a dead tap; label is text', async ({ browser }) => {
  const mock = await startMockServer({ agentEvents: { [TASK_ID]: [] } });
  const { ctx, page, pageErrors } = await openSession(browser, mock);
  try {
    await page.evaluate((id) => {
      const av = (/** @type {any} */ (window)).nz.views.agent;
      const host = document.createElement('div');
      host.id = 'agent-row-host';
      host.innerHTML =
        av.agentRowHtml({ taskId: id, name: '<img src=x id=injected>', status: 'running' }) +
        av.agentRowHtml({ taskId: '', name: 'spawning', status: 'spawned' });
      document.body.appendChild(host);
    }, TASK_ID);

    const activeTask = () => page.evaluate(() => (/** @type {any} */ (window)).nz.views.agent.activeTaskID());
    const rows = page.locator('#agent-row-host .rb-agent-row');
    await expect(rows).toHaveCount(2);

    // Label is escaped text: the angle-bracket name shows literally.
    await expect(page.locator('#injected')).toHaveCount(0);
    await expect(rows.nth(0).locator('.sa-name')).toHaveText('<img src=x id=injected>');

    await rows.nth(0).click();
    await expect.poll(activeTask, { timeout: 3000 }).toBe(TASK_ID);

    // A dead tap is a no-op. switchTo('') means "back to the parent view", so
    // a row with no task id that reached it would throw the user out of the
    // drill-in they are in.
    await rows.nth(1).click();
    expect(await activeTask(), 'a row without a task id must leave the current view alone').toBe(TASK_ID);
    expect(pageErrors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
