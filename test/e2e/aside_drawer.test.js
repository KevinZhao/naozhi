// @ts-check
//
// 追问 drawer (aside_drawer.js) wiring that crosses module lines:
//  1. An AskUserQuestion card answered inside #aside-drawer is sent to the
//     scratch session's key, not the parent session's: ask_card reads the key
//     aside_drawer mirrors into selection.scratchKey.
//  2. Leaving the chat view closes the drawer and clears selection.scratchKey.
//  3. 保存为正式会话 (the 'scratch-promote' action on #ad-save) promotes the
//     scratch and selects the session the server returns.
//
// Run: cd test/e2e && npx playwright test aside_drawer.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';
const SCRATCH_KEY = 'dashboard:scratch:2026-01-01-130000-9:myproject';
const PROMOTED_KEY = 'dashboard:direct:2026-01-01-120002-3:myproject';

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'desktop-chrome only');
  }
});

// The ↗ button renders only on text events over 500 characters.
function drawerMock(scratchEvents, extra = {}) {
  const now = Date.now();
  const reply = '这是一条足够长的回复。' + '内容填充，凑到追问按钮的长度闸以上。'.repeat(30);
  return startMockServer({
    eventsByKey: {
      [KEY]: [
        { type: 'user', detail: 'hello', time: now - 8000, uuid: 'ev-usr-1' },
        { type: 'text', detail: reply, time: now - 5000, uuid: 'ev-txt-2' },
      ],
      [SCRATCH_KEY]: scratchEvents(now),
    },
    ...extra,
  });
}

async function openDrawer(browser, mock) {
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  await page.goto(mock.url + '/dashboard');
  await page.click(`.session-card[data-key="${KEY}"]`);
  const askBtn = page.locator('#events-scroll .event-ask-btn').first();
  await askBtn.hover({ force: true });
  await askBtn.click({ force: true });
  await expect(page.locator('#aside-drawer')).toBeVisible();
  return { ctx, page };
}

const selectionField = (page, field) => page.evaluate(async (f) => {
  const { selection } = await import('/static/state.js');
  return selection[f];
}, field);

test('an ask card answered inside the drawer is sent to the scratch key', async ({ browser }) => {
  const mock = await drawerMock((now) => [
    { type: 'ask_question', time: now - 1000, uuid: 'ask-1', ask_question: { tool_use_id: 'tu-aside', items: [
      { header: 'Color', question: 'Pick one', options: [{ label: 'Red' }, { label: 'Blue' }] },
    ] } },
  ]);
  const { ctx, page } = await openDrawer(browser, mock);
  try {
    const card = page.locator('#aside-drawer .event.ask_question[data-tool-use-id="tu-aside"]');
    await expect(card).toBeVisible();
    await card.locator('.ask-opt').first().click();
    await card.locator('.ask-submit').click();
    await expect.poll(() => mock.sendCalls.length).toBe(1);
    expect(JSON.parse(mock.sendCalls[0]).key, 'the answer must reach the scratch CLI, not the parent session').toBe(SCRATCH_KEY);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('leaving the chat view closes the drawer and clears selection.scratchKey', async ({ browser }) => {
  const mock = await drawerMock(() => []);
  const { ctx, page } = await openDrawer(browser, mock);
  try {
    await expect.poll(() => selectionField(page, 'scratchKey')).toBe(SCRATCH_KEY);
    await page.evaluate(() => window.nz.test.setActivityView('cron'));
    await expect(page.locator('#aside-drawer')).not.toHaveClass(/visible/);
    expect(await selectionField(page, 'scratchKey')).toBe('');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('保存为正式会话 promotes the scratch and selects the returned session', async ({ browser }) => {
  const mock = await drawerMock((now) => [
    { type: 'text', detail: 'scratch reply', time: now - 1000, uuid: 'scr-txt-1' },
  ], { scratchPromoteKey: PROMOTED_KEY });
  const { ctx, page } = await openDrawer(browser, mock);
  try {
    const save = page.locator('#ad-save');
    await expect(save).toHaveClass(/visible/);
    await save.click();
    await expect.poll(() => mock.scratchPromoteCalls).toEqual(['scr-0001']);
    await expect(page.locator('#aside-drawer')).not.toHaveClass(/visible/);
    await expect.poll(() => selectionField(page, 'key')).toBe(PROMOTED_KEY);
    expect(await selectionField(page, 'scratchKey')).toBe('');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
