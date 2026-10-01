// @ts-check
// The cron-live channel's claims on subscribed / session_state / error, and its
// reset when the drawer switches jobs (S18, #3024). Pins the behaviour the
// cron-live state keeps when it moves out of wsm.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') testInfo.skip(true, 'desktop-chrome only');
});

const T = Date.now() - 60000;
const ev = (text, time) => ({ type: 'text', detail: text, summary: text, time, uuid: 'u-' + text });
const job = (id) => ({
  id, schedule: '@every 1h', prompt: 'job ' + id, work_dir: '/home/user/workspace/myproject', paused: false,
  created_at: Date.now() - 86400000, next_run: Date.now() + 3600000,
  current_run: { run_id: 'run-' + id, started_at: Date.now() - 5000, state: 'running' },
  stats: { total: 1, succeeded: 0, failed: 0, skipped: 0 },
});
const subs = (conn, key) => conn.messages.filter((m) => m.type === 'subscribe' && m.key === key).length;

test('cron-live claims subscribed / session_state / error without touching the session subscription', async ({ browser }) => {
  const mock = await startMockServer({ ws: true, cronJobs: [job('cron-001')] });
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  const pageErrors = [];
  page.on('pageerror', (e) => pageErrors.push(e.message));
  await page.goto(mock.url + '/dashboard');
  await page.waitForFunction(() => wsm.state === WS_STATES.CONNECTED);
  await page.click('#abnav-cron');
  await page.click('.cj-row[data-cron-id="cron-001"]');
  await page.waitForSelector('#cron-live-events');
  const conn = mock.wsConnections[mock.wsConnections.length - 1];
  await expect.poll(() => subs(conn, 'cron:cron-001')).toBe(1);
  const status = page.locator('#cron-live-status');
  const mainKey = await page.evaluate(() => wsm.subscribedKey);

  conn.send({ type: 'subscribed', key: 'cron:cron-001', reason: 'suspended' });
  await expect(status).toHaveText('等待事件…');
  expect(await page.evaluate(() => wsm.subscribedKey), 'the cron ack must not become the session subscription').toBe(mainKey);

  conn.send({ type: 'session_state', key: 'cron:cron-001', state: 'running' });
  await expect.poll(() => subs(conn, 'cron:cron-001'), { message: 'a suspended cron sub re-subscribes on running' }).toBe(2);

  conn.send({ type: 'error', key: 'cron:cron-001', error: 'session not found' });
  await expect(status).toHaveText('已停止');
  expect(pageErrors).toEqual([]);
  await ctx.close();
  mock.server.close();
});

test('switching the drawer to another running job drops the previous job\'s live events', async ({ browser }) => {
  const mock = await startMockServer({ ws: true, cronJobs: [job('cron-001'), job('cron-002')] });
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  const pageErrors = [];
  page.on('pageerror', (e) => pageErrors.push(e.message));
  await page.goto(mock.url + '/dashboard');
  await page.waitForFunction(() => wsm.state === WS_STATES.CONNECTED);
  await page.click('#abnav-cron');
  await page.click('.cj-row[data-cron-id="cron-001"]');
  await page.waitForSelector('#cron-live-events');
  const conn = mock.wsConnections[mock.wsConnections.length - 1];
  await expect.poll(() => subs(conn, 'cron:cron-001')).toBe(1);
  const live = () => page.locator('#cron-live-events .event').evaluateAll((els) => els.map((e) => (e.textContent || '').trim()));
  conn.send({ type: 'subscribed', key: 'cron:cron-001' });
  conn.send({ type: 'history', key: 'cron:cron-001', events: [ev('one-1', T)] });
  await expect.poll(live).toEqual(['one-1']);

  await page.click('.cj-row[data-cron-id="cron-002"]');
  await expect.poll(() => subs(conn, 'cron:cron-002')).toBe(1);
  conn.send({ type: 'subscribed', key: 'cron:cron-002' });
  conn.send({ type: 'history', key: 'cron:cron-002', events: [ev('two-1', T + 5)] });
  await expect.poll(live).toEqual(['two-1']);
  expect(pageErrors).toEqual([]);
  await ctx.close();
  mock.server.close();
});
