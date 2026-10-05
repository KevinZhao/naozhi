// @ts-check
// The history popover's list comes from GET /api/sessions/history (#3014);
// /api/sessions carries only stats.history_tag, the content tag naming it.
//  - the popover fills from the endpoint;
//  - a poll whose tag is unchanged makes no history request, and one whose tag
//    moved refetches the list;
//  - a failed history fetch, or an older one resolving last, drops the
//    sessions validator, so a 304 cannot keep the next poll from retrying it.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };
const OLD = { session_id: 'hist-001', workspace: '/home/user/workspace/myproject', project: 'myproject', last_prompt: 'old task from yesterday', last_active: Date.now() - 86400000 };
const NEW = { session_id: 'hist-002', workspace: '/home/user/workspace/myproject', project: 'myproject', last_prompt: 'closed an hour ago', last_active: Date.now() - 3600000 };

// popoverText opens the history popover, reads it and closes it again.
async function popoverText(page) {
  await page.click('#btn-history');
  const text = await page.locator('.history-popover').textContent();
  await page.evaluate(() => closeHistoryPopover());
  await expect(page.locator('.history-popover')).toHaveCount(0);
  return text || '';
}

async function open(browser, mock) {
  const ctx = await browser.newContext({ ...desktop });
  const page = await ctx.newPage();
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.waitForFunction(() => historyTag !== '');
  return { ctx, page };
}

// openConnected also waits out the poll the socket's connect schedules 300 ms
// later: one landing mid-test would spend a failNextHistory/staleNextHistory
// one-shot or move a call counter. The tag is unchanged, so it fetches no history.
async function openConnected(browser, mock) {
  const { ctx, page } = await open(browser, mock);
  await page.waitForFunction(() => wsm.state === WS_STATES.CONNECTED);
  const calls = mock.historyGetCalls;
  await page.evaluate(() => debouncedFetchSessions());
  expect(mock.historyGetCalls, 'the connect poll fetched the history').toBe(calls);
  return { ctx, page };
}

test('the history popover fills from /api/sessions/history', async ({ browser }) => {
  const mock = await startMockServer({ historySessions: [OLD] });
  const { ctx, page } = await open(browser, mock);
  expect(mock.historyGetCalls).toBeGreaterThanOrEqual(1);
  const text = await popoverText(page);
  expect(text).toContain('old task from yesterday');
  expect(text).toContain('(1)');
  await ctx.close();
  mock.server.close();
});

test('a poll refetches the history only when its tag moved', async ({ browser }) => {
  const mock = await startMockServer({ ws: true, historySessions: [OLD] });
  const { ctx, page } = await openConnected(browser, mock);
  const conn = mock.wsConnections[mock.wsConnections.length - 1];
  await page.evaluate(() => fetchSessions());
  const calls = mock.historyGetCalls;

  const polls = mock.sessionsGetCalls;
  conn.send({ type: 'sessions_update' });
  await expect.poll(() => mock.sessionsGetCalls).toBe(polls + 1);
  await page.evaluate(() => fetchSessions());
  expect(mock.historyGetCalls, 'an unchanged tag fetched the history').toBe(calls);

  mock.setHistorySessions([NEW, OLD]);
  conn.send({ type: 'sessions_update' });
  await expect.poll(() => mock.historyGetCalls).toBe(calls + 1);
  await expect.poll(() => popoverText(page)).toContain('closed an hour ago');
  await ctx.close();
  mock.server.close();
});

test('a failed history fetch is retried by the next poll despite the sessions validator', async ({ browser }) => {
  const mock = await startMockServer({ ws: true, sessionsETag: true, historySessions: [OLD] });
  const { ctx, page } = await openConnected(browser, mock);
  const tag = await page.evaluate(() => historyTag);

  mock.failNextHistory(1);
  mock.setHistorySessions([NEW, OLD]);
  const calls = mock.historyGetCalls;
  await page.evaluate(() => fetchSessions());
  await expect.poll(() => mock.historyGetCalls).toBe(calls + 1);
  expect(await page.evaluate(() => historyTag), 'the failed fetch replaced the list').toBe(tag);

  // Had the validator survived, every poll would get a 304 and none retry.
  const n = mock.sessionsValidators.length;
  await expect.poll(async () => {
    await page.evaluate(() => fetchSessions());
    return mock.historyGetCalls;
  }).toBe(calls + 2);
  expect(mock.sessionsValidators.slice(n), 'no poll after the failure was unconditional').toContain('');
  await expect.poll(() => popoverText(page)).toContain('closed an hour ago');
  await ctx.close();
  mock.server.close();
});

// Chrome serialises concurrent GETs of one URL behind its cache lock, so the
// mock stands in for the race: the poll names the latest list, the history
// response carries the one before it.
test('an older history fetch resolving last is corrected by the next poll despite the sessions validator', async ({ browser }) => {
  const MID = { ...NEW, session_id: 'hist-003', last_prompt: 'superseded list' };
  const mock = await startMockServer({ ws: true, sessionsETag: true, historySessions: [OLD] });
  const { ctx, page } = await openConnected(browser, mock);

  mock.setHistorySessions([MID, OLD]);
  const stale = mock.historyTag;
  mock.setHistorySessions([NEW, OLD]);
  const latest = mock.historyTag;
  mock.staleNextHistory([MID, OLD]);
  await page.evaluate(() => fetchSessions());
  await page.waitForFunction(t => historyTag === t, stale);

  // Had the validator survived, every poll would get a 304 and the stale list stick.
  await expect.poll(async () => {
    await page.evaluate(() => fetchSessions());
    return page.evaluate(() => historyTag);
  }).toBe(latest);
  const text = await popoverText(page);
  expect(text).toContain('closed an hour ago');
  expect(text).not.toContain('superseded list');
  await ctx.close();
  mock.server.close();
});
