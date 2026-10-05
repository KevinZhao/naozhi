// @ts-check
// The cron-live channel's claims on subscribed / session_state / error, its
// reset when the drawer switches jobs, its resume after a reconnect (S18,
// #3024), and the freeze after a failed run (cron_state's frozen-run set,
// S20j #3026). Pins the behaviour cron_live.js took over from wsm. A handler that
// throws on a socket frame is caught by onmessage and logged as 'ws parse
// error', not raised as a pageerror, so both are collected.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

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
const collectErrors = (page) => {
  const errs = [];
  page.on('pageerror', (e) => errs.push(e.message));
  page.on('console', (m) => { if (m.text().includes('ws parse error')) errs.push(m.text()); });
  return errs;
};

test('cron-live claims subscribed / session_state / error without touching the session subscription', async ({ browser }) => {
  const mock = await startMockServer({ ws: true, cronJobs: [job('cron-001')] });
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  const pageErrors = collectErrors(page);
  await page.goto(mock.url + '/dashboard');
  await waitForWs(page);
  await page.click('#abnav-cron');
  await page.click('.cj-row[data-cron-id="cron-001"]');
  await page.waitForSelector('#cron-live-events');
  const conn = mock.wsConnections[mock.wsConnections.length - 1];
  await expect.poll(() => subs(conn, 'cron:cron-001')).toBe(1);
  const status = page.locator('#cron-live-status');
  const mainKey = await page.evaluate(() => sessionStream.subscribedKey);

  conn.send({ type: 'subscribed', key: 'cron:cron-001', reason: 'suspended' });
  await expect(status).toHaveText('等待事件…');
  expect(await page.evaluate(() => sessionStream.subscribedKey), 'the cron ack must not become the session subscription').toBe(mainKey);

  // An event the suspended sub already holds sets the re-sub threshold: after
  // is its time (T), not the run start (about now - 5s).
  conn.send({ type: 'event', key: 'cron:cron-001', event: ev('held', T) });
  conn.send({ type: 'session_state', key: 'cron:cron-001', state: 'running' });
  await expect.poll(() => conn.messages.filter((m) => m.type === 'subscribe' && m.key === 'cron:cron-001').slice(1),
    { message: 'a suspended cron sub re-subscribes on running, after the last event it holds' })
    .toEqual([{ type: 'subscribe', key: 'cron:cron-001', after: T }]);

  conn.send({ type: 'error', key: 'cron:cron-001', error: 'session not found' });
  await expect(status).toHaveText('已停止');
  expect(pageErrors).toEqual([]);
  await ctx.close();
  mock.server.close();
});

test('switching the drawer to another running job drops the previous job\'s live events, closing it unsubscribes', async ({ browser }) => {
  const mock = await startMockServer({ ws: true, cronJobs: [job('cron-001'), job('cron-002')] });
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  const pageErrors = collectErrors(page);
  await page.goto(mock.url + '/dashboard');
  await waitForWs(page);
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

  // Closing the drawer ends the stream on the server too.
  await page.click('#cron-detail-pane [data-action="cron-detail-close"]');
  await expect.poll(() => conn.messages.filter((m) => m.type === 'unsubscribe').map((m) => m.key))
    .toEqual(['cron:cron-001', 'cron:cron-002']);
  expect(pageErrors).toEqual([]);
  await ctx.close();
  mock.server.close();
});

test('a reconnect resumes the live stream of a running job and leaves a finished one stopped', async ({ browser }) => {
  const mock = await startMockServer({ ws: true, cronJobs: [job('cron-001')] });
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  const pageErrors = collectErrors(page);
  await page.goto(mock.url + '/dashboard');
  await waitForWs(page);
  await page.click('#abnav-cron');
  await page.click('.cj-row[data-cron-id="cron-001"]');
  await page.waitForSelector('#cron-live-events');
  let conn = mock.wsConnections[mock.wsConnections.length - 1];
  await expect.poll(() => subs(conn, 'cron:cron-001')).toBe(1);
  const live = () => page.locator('#cron-live-events .event').evaluateAll((els) => els.map((e) => (e.textContent || '').trim()));
  conn.send({ type: 'subscribed', key: 'cron:cron-001' });
  conn.send({ type: 'history', key: 'cron:cron-001', events: [ev('one-1', T)] });
  await expect.poll(live).toEqual(['one-1']);

  const reconnect = async () => {
    const before = mock.wsConnections.length;
    conn.close();
    await expect.poll(() => mock.wsConnections.length, { timeout: 5000 }).toBe(before + 1);
    conn = mock.wsConnections[before];
    await waitForWs(page);
  };
  await reconnect();
  await expect.poll(() => conn.messages.filter((m) => m.type === 'subscribe' && m.key === 'cron:cron-001'),
    { message: 'the running job re-subscribes once, after the last event it holds' })
    .toEqual([{ type: 'subscribe', key: 'cron:cron-001', after: T }]);
  conn.send({ type: 'subscribed', key: 'cron:cron-001' });
  conn.send({ type: 'event', key: 'cron:cron-001', event: ev('one-2', T + 1) });
  await expect.poll(live).toEqual(['one-1', 'one-2']);

  // The run ends (the list the refetch returns agrees): the stream stops and
  // keeps its events, and the next reconnect must not resume it.
  mock.setCronJobs([{ ...job('cron-001'), current_run: null }]);
  conn.send({ type: 'run_ended', subsystem: 'cron', owner_id: 'cron-001', run_id: 'run-cron-001', state: 'succeeded', ended_at: Date.now() });
  const status = page.locator('#cron-live-status');
  await expect(status).toHaveText('已停止');
  await reconnect();
  // onReady runs inside the auth_ok handler, so anything it sends is on the
  // socket ahead of a ping sent once CONNECTED is observed.
  await page.evaluate(() => wsm.send({ type: 'ping' }));
  await expect.poll(() => conn.messages.some((m) => m.type === 'ping')).toBe(true);
  expect(subs(conn, 'cron:cron-001'), 'a finished job is not re-subscribed').toBe(0);
  await expect(status).toHaveText('已停止');
  await expect.poll(live).toEqual(['one-1', 'one-2']);
  expect(pageErrors).toEqual([]);
  await ctx.close();
  mock.server.close();
});

test('a run that ends failed freezes its live stream until the next run starts', async ({ browser }) => {
  const idle = { ...job('cron-002'), current_run: null };
  const mock = await startMockServer({ ws: true, cronJobs: [job('cron-001'), idle] });
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  const pageErrors = collectErrors(page);
  await page.goto(mock.url + '/dashboard');
  await waitForWs(page);
  await page.click('#abnav-cron');
  await page.click('.cj-row[data-cron-id="cron-001"]');
  await page.waitForSelector('#cron-live-events');
  const conn = mock.wsConnections[mock.wsConnections.length - 1];
  await expect.poll(() => subs(conn, 'cron:cron-001')).toBe(1);
  const live = () => page.locator('#cron-live-events .event').evaluateAll((els) => els.map((e) => (e.textContent || '').trim()));
  conn.send({ type: 'subscribed', key: 'cron:cron-001' });
  conn.send({ type: 'history', key: 'cron:cron-001', events: [ev('one-1', T)] });
  await expect.poll(live).toEqual(['one-1']);

  mock.setCronJobs([{ ...job('cron-001'), current_run: null }, idle]);
  conn.send({ type: 'run_ended', subsystem: 'cron', owner_id: 'cron-001', run_id: 'run-cron-001', state: 'failed', error_class: 'session_error', ended_at: Date.now() });
  conn.send({ type: 'event', key: 'cron:cron-001', event: ev('ghost', T + 1) });
  // Frames are handled in order: once cron-002's row runs, the ghost event
  // before it has been handled too.
  conn.send({ type: 'run_started', subsystem: 'cron', owner_id: 'cron-002', run_id: 'run-b', started_at: Date.now() });
  await expect(page.locator('.cj-row.is-running[data-cron-id="cron-002"]')).toHaveCount(1);
  await expect.poll(live, { message: 'a failed run drops the events its CLI still emits' }).toEqual(['one-1']);

  conn.send({ type: 'run_started', subsystem: 'cron', owner_id: 'cron-001', run_id: 'run-2', started_at: Date.now() });
  conn.send({ type: 'event', key: 'cron:cron-001', event: ev('two-1', T + 2) });
  await expect.poll(live, { message: 'the next run of the job streams again' }).toEqual(['two-1']);
  expect(pageErrors).toEqual([]);
  await ctx.close();
  mock.server.close();
});
