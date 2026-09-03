// @ts-check
// Regressions for #2431 P3 items 4/6/7/8 (dashboard.js sidebar + mobile):
//  4. a pending session in an unregistered workspace groups under the folder
//     basename (like /api/sessions does) instead of 未分组;
//  6. discovered cards are keyed by pid AND node — two nodes with the same pid
//     must render two cards, select the right one, and dismiss only one;
//  7. switching sessions while already in mobile chat view must not stack
//     history entries, and the back button pops the entry it pushed;
//  8. selecting a discovered card from a non-chat view returns to chat.
const { test, expect, devices } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };
const iPhone = devices['iPhone 13'];

const SAME_PID = 4242;
function discoveredFixture() {
  const now = Date.now();
  return [
    { pid: SAME_PID, session_id: 'disc-local', cwd: '/home/user/workspace/myproject', proc_start_time: 100,
      node: 'local', cli_name: 'claude-code', state: 'ready', started_at: now - 5000, summary: 'local terminal' },
    { pid: SAME_PID, session_id: 'disc-remote', cwd: '/home/user/workspace/otherproject', proc_start_time: 200,
      node: 'remote1', cli_name: 'claude-code', state: 'ready', started_at: now - 4000, summary: 'remote terminal' },
  ];
}

function multiNodeSessions() {
  const s = defaultSessions();
  s.nodes = {
    local: { display_name: 'Local', status: 'ok' },
    remote1: { display_name: 'Remote 1', status: 'ok' },
  };
  return s;
}

/** @type {Awaited<ReturnType<typeof startMockServer>>} */
let mock;
test.beforeAll(async () => {
  mock = await startMockServer({ sessions: multiNodeSessions(), discovered: discoveredFixture() });
});
test.afterAll(async () => {
  await new Promise(r => mock.server.close(r));
});

const DISC_CARDS = '.session-card[data-key^="_discovered:"]';

async function openDesktop(browser) {
  const ctx = await browser.newContext({ ...desktop });
  const page = await ctx.newPage();
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await expect(page.locator(DISC_CARDS)).toHaveCount(2);
  return { ctx, page };
}

test.describe('#2431 item 6: discovered cards keyed by node + pid', () => {
  test('same pid on two nodes renders two distinct keys', async ({ browser }) => {
    const { ctx, page } = await openDesktop(browser);
    try {
      const keys = await page.locator(DISC_CARDS).evaluateAll(els => els.map(e => e.getAttribute('data-key')));
      expect(new Set(keys).size).toBe(2);
    } finally { await ctx.close(); }
  });

  test('clicking the remote card previews the remote process, not the local one', async ({ browser }) => {
    const { ctx, page } = await openDesktop(browser);
    try {
      await page.locator(DISC_CARDS + '[data-node="remote1"]').click();
      const active = page.locator('.session-card.active');
      await expect(active).toHaveCount(1);
      await expect(active).toHaveAttribute('data-node', 'remote1');
      // The header shows the previewed cwd basename — remote's, not local's.
      await expect(page.locator('#main .main-header h2')).toHaveText('otherproject');
    } finally { await ctx.close(); }
  });

  test('dismissing the local card keeps the remote card with the same pid', async ({ browser }) => {
    const { ctx, page } = await openDesktop(browser);
    try {
      const localKey = await page.locator(DISC_CARDS + '[data-node="local"]').getAttribute('data-key');
      // eslint-disable-next-line no-eval
      await page.evaluate(k => eval('dismissSession')(k, 'local'), localKey);
      await expect.poll(() => mock.discoveredCloseCalls.length).toBe(1);
      const closeBody = JSON.parse(mock.discoveredCloseCalls[0]);
      expect(closeBody.session_id).toBe('disc-local');
      await expect(page.locator(DISC_CARDS + '[data-node="local"]')).toHaveCount(0);
      // Give the debounced re-render a chance to run, then the remote card must survive.
      await page.waitForTimeout(800);
      await expect(page.locator(DISC_CARDS + '[data-node="remote1"]')).toHaveCount(1);
      // eslint-disable-next-line no-eval
      const left = await page.evaluate(() => eval('discoveredItems').map(d => d.session_id));
      expect(left).toEqual(['disc-remote']);
    } finally { await ctx.close(); mock.resetCalls(); }
  });
});

test.describe('#2431 item 8: selecting a discovered card leaves non-chat views', () => {
  test('selectSession on a discovered key from cron view flips back to chat', async ({ browser }) => {
    const { ctx, page } = await openDesktop(browser);
    try {
      // eslint-disable-next-line no-eval
      await page.evaluate(() => eval('setActivityView')('cron'));
      await expect(page.locator('body')).toHaveClass(/nz-view-cron/);
      const card = page.locator(DISC_CARDS + '[data-node="remote1"]');
      const key = await card.getAttribute('data-key');
      // Keyboard (sessionCardKey) and mouse (onclick) both route through selectSession.
      // eslint-disable-next-line no-eval
      await page.evaluate(k => eval('selectSession')(k, 'remote1'), key);
      // eslint-disable-next-line no-eval
      expect(await page.evaluate(() => eval('activeView'))).toBe('chat');
      await expect(page.locator('body')).not.toHaveClass(/nz-view-cron/);
      await expect(page.locator('.session-card.active')).toHaveAttribute('data-node', 'remote1');
    } finally { await ctx.close(); }
  });
});

test.describe('#2431 item 4: pending card groups by workspace basename', () => {
  test('pending session in an unregistered folder lands under its basename header', async ({ browser }) => {
    const { ctx, page } = await openDesktop(browser);
    try {
      const ws = '/home/user/workspace/scratch-dir';
      const key = await page.evaluate((w) => {
        // eslint-disable-next-line no-eval
        eval('doCreateInProject')(w, 'scratch-dir', 'local', undefined, 'general', { mode: 'new' });
        // eslint-disable-next-line no-eval
        return eval('selectedKey');
      }, ws);
      const card = page.locator('.session-card[data-key="' + key + '"]');
      await expect(card).toHaveCount(1);
      // Walk back to the nearest section header above the card.
      const header = await card.evaluate(el => {
        let n = el.previousElementSibling;
        while (n && !n.classList.contains('section-header')) n = n.previousElementSibling;
        return n ? (n.textContent || '').trim() : '';
      });
      expect(header).not.toContain('未分组');
      expect(header).toContain('scratch-dir');
    } finally {
      await page.evaluate(() => localStorage.removeItem('nz:pending_sessions')).catch(() => {});
      await ctx.close();
    }
  });
});

test.describe('#2431 item 7: mobile chat history stack', () => {
  test('switching sessions in chat view does not grow history; back pops the chat entry', async ({ browser }) => {
    const ctx = await browser.newContext({ ...iPhone });
    const page = await ctx.newPage();
    try {
      await page.goto(mock.url + '/dashboard');
      await page.waitForSelector('.session-card');
      const keys = await page.locator('.session-card:not([data-key^="_discovered:"])')
        .evaluateAll(els => els.map(e => e.getAttribute('data-key')));
      expect(keys.length).toBeGreaterThanOrEqual(2);

      const len0 = await page.evaluate(() => history.length);
      // eslint-disable-next-line no-eval
      await page.evaluate(k => eval('selectSession')(k, 'local'), keys[0]);
      await expect(page.locator('body')).toHaveClass(/mobile-chat-view/);
      const len1 = await page.evaluate(() => history.length);
      expect(len1).toBe(len0 + 1);

      // Switch twice more while already in chat view.
      // eslint-disable-next-line no-eval
      await page.evaluate(k => eval('selectSession')(k, 'local'), keys[1]);
      // eslint-disable-next-line no-eval
      await page.evaluate(k => eval('selectSession')(k, 'local'), keys[0]);
      await expect(page.locator('body')).toHaveClass(/mobile-chat-view/);
      expect(await page.evaluate(() => history.length)).toBe(len1);

      // The in-app back button flips to list view AND pops the pushed entry.
      await page.locator('.btn-mobile-back').first().click();
      await expect(page.locator('body')).toHaveClass(/mobile-list-view/);
      await expect(page.locator('body')).not.toHaveClass(/mobile-chat-view/);
      await expect.poll(() => page.evaluate(() => (history.state && history.state.view) || null)).toBe(null);
    } finally { await ctx.close(); }
  });
});
