// @ts-check
//
// dismissSession (tuning.js) has one branch per kind of sidebar card. Each
// removes the card; when the dismissed session is the one on screen the main
// panel falls back to the empty quick-ask state:
//   - managed: optimistic, DELETE /api/sessions fired without waiting, and
//     the card stays gone once the DELETE's re-sync has painted;
//   - pending (never sent): only the local record goes, no request;
//   - discovered: POST /api/discovered/close, the card goes once it lands;
//   - a cron stub (server-side filtered, so only a server bug shows one):
//     the card goes and NO DELETE is sent, since the scheduler owns it.
// dismiss_failure_restore.test.js covers the failed-DELETE re-sync.
//
// 跑法：cd test/e2e && npx playwright test dismiss_branches.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');

const KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';
const PROJ = '/home/user/workspace/myproject';

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'viewport-independent; desktop-chrome only');
  }
});

async function open(browser, overrides = {}) {
  const mock = await startMockServer(overrides);
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  const page = await ctx.newPage();
  /** @type {string[]} */
  const deletes = [];
  page.on('request', (r) => { if (r.method() === 'DELETE' && r.url().includes('/api/sessions')) deletes.push(r.postData() || ''); });
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  const close = async () => { await ctx.close(); mock.server.close(); };
  return { mock, page, deletes, close };
}

const dismiss = (page, key) => page.evaluate((k) => window.nz.test.dismissSession(k, 'local'), key);
const selectedKey = (page) => page.evaluate(() => window.nz.test.selectedKey);

test('the selected managed session: card and panel go at once, DELETE carries the key', async ({ browser }) => {
  const sessions = defaultSessions();
  const { page, deletes, close } = await open(browser, { sessions });
  try {
    await page.click(`.session-card[data-key="${KEY}"]`);
    await expect(page.locator('#main #events-scroll')).toHaveCount(1);
    const deleted = page.waitForEvent('requestfinished', (r) => r.method() === 'DELETE' && new URL(r.url()).pathname === '/api/sessions');
    await dismiss(page, KEY);
    await expect(page.locator(`.session-card[data-key="${KEY}"]`)).toHaveCount(0);
    await expect(page.locator('#main #quick-ask-input')).toHaveCount(1);
    expect(await selectedKey(page)).toBeNull();
    await expect.poll(() => deletes.length).toBe(1);
    expect(JSON.parse(deletes[0])).toEqual({ key: KEY });
    // The DELETE's settle stops hiding the key and zeroes lastVersion, then
    // re-syncs. Seeing both "no key hidden" and the server's post-DELETE
    // version therefore means a list fetched after the settle has painted
    // unfiltered: the card must still be gone and the panel still empty.
    await deleted;
    const version = sessions.stats.version;
    await expect.poll(() => page.evaluate(async () => {
      const { sessionList } = await import('/static/state.js');
      return { hidden: sessionList.optimisticDeleteKeys.size, version: sessionList.lastVersion };
    })).toEqual({ hidden: 0, version });
    await expect(page.locator(`.session-card[data-key="${KEY}"]`)).toHaveCount(0);
    await expect(page.locator('#main #quick-ask-input')).toHaveCount(1);
  } finally { await close(); }
});

test('the selected pending session: card, record and panel go, no request', async ({ browser }) => {
  const { page, deletes, close } = await open(browser);
  try {
    const key = await page.evaluate((proj) => {
      window.nz.test.doCreateInProject(proj, 'myproject', 'local', undefined, 'general', { mode: 'new' });
      return window.nz.test.selectedKey;
    }, PROJ);
    await expect(page.locator(`.session-card.new-card[data-key="${key}"]`)).toHaveCount(1);
    await dismiss(page, key);
    await expect(page.locator(`.session-card[data-key="${key}"]`)).toHaveCount(0);
    await expect(page.locator('#main #quick-ask-input')).toHaveCount(1);
    expect(await selectedKey(page)).toBeNull();
    expect(await page.evaluate(() => JSON.parse(localStorage.getItem('nz:pending_sessions') || '{}'))).toEqual({});
    await expect(page.locator(`.session-card[data-key="${KEY}"]`)).toHaveCount(1);
    expect(deletes).toEqual([]);
  } finally { await close(); }
});

test('the previewed discovered session: close lands, then card and preview go', async ({ browser }) => {
  const now = Date.now();
  const { mock, page, deletes, close } = await open(browser, {
    discovered: [
      { pid: 888, session_id: 'disc-dismiss', cwd: '/home/user/workspace/termproj', proc_start_time: 5,
        node: 'local', cli_name: 'claude-code', state: 'ready', started_at: now - 5000 },
    ],
  });
  try {
    const card = page.locator('.session-card[data-key^="_discovered:888"]');
    await card.click();
    await expect(page.locator('#main .main-header h2')).toHaveText('termproj');
    const key = await card.getAttribute('data-key');
    await dismiss(page, key);
    await expect.poll(() => mock.discoveredCloseCalls.length).toBe(1);
    expect(JSON.parse(mock.discoveredCloseCalls[0])).toMatchObject({ pid: 888, session_id: 'disc-dismiss' });
    await expect(card).toHaveCount(0);
    await expect(page.locator('#main #quick-ask-input')).toHaveCount(1);
    expect(deletes).toEqual([]);
  } finally { await close(); }
});

test('a leaked cron stub on screen: its card and the panel go, no DELETE is sent', async ({ browser }) => {
  const { page, deletes, close } = await open(browser);
  try {
    await page.click(`.session-card[data-key="${KEY}"]`);
    await expect(page.locator('#main #events-scroll')).toHaveCount(1);
    // Stand in for the server bug: the stub has a sidebar card and is the
    // selected session. The card is counted synchronously after the call,
    // before the resync's re-render could drop it on dismissSession's behalf.
    const left = await page.evaluate((k) => {
      const src = document.querySelector('.session-card[data-key="' + k + '"]');
      const stub = /** @type {HTMLElement} */ (src.cloneNode(true));
      stub.dataset.key = 'cron:job-leaked';
      src.parentNode.appendChild(stub);
      const count = () => document.querySelectorAll('.session-card[data-key="cron:job-leaked"]').length;
      const before = count();
      window.nz.test.selectedKey = 'cron:job-leaked';
      window.nz.test.dismissSession('cron:job-leaked', 'local');
      return [before, count()];
    }, KEY);
    expect(left, 'the stub card is there, then removed by the dismiss itself').toEqual([1, 0]);
    await expect(page.locator('.session-card[data-key="cron:job-leaked"]')).toHaveCount(0);
    await expect(page.locator('#main #quick-ask-input')).toHaveCount(1);
    expect(await selectedKey(page)).toBeNull();
    await expect(page.locator(`.session-card[data-key="${KEY}"]`)).toHaveCount(1);
    expect(deletes).toEqual([]);
  } finally { await close(); }
});
