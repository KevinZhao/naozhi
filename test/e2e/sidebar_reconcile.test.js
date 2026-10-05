// @ts-check
//
// renderSidebar reconciles the session list against the DOM as it stands,
// keyed by session (and project header):
//  - a render with nothing new touches no DOM, so the card nodes survive;
//  - a changed session replaces its own card and no other;
//  - a card patched in place (the WS state dot) follows the data on the next
//    render instead of being left as the patch drew it;
//  - reordering and removal follow the data, moving the surviving nodes;
//  - the open session's live agent count survives a sidebar render;
//  - after its card is replaced, switching away clears its highlight;
//  - a remote node's card, whose badge colour is applied through CSSOM, still
//    compares equal to its markup.
//
// Run: cd test/e2e && npx playwright test sidebar_reconcile.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');

const A = 'dashboard:direct:2026-01-01-120000-1:myproject';
const B = 'dashboard:direct:2026-01-01-120002-3:myproject';

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'desktop-chrome only');
  }
});

/** @param {import('@playwright/test').Page} page @param {any} [overrides] */
async function open(page, overrides) {
  const mock = await startMockServer(overrides);
  // The fixture's 30s-old session renders a seconds-granular "30s ago", so
  // two renders of the same data a second apart differ in markup. Date is
  // frozen (timers still run); a test needing elapsed time uses page.clock.
  await page.clock.setFixedTime(Date.now());
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector(`.session-card[data-key="${A}"]`);
  return mock;
}

test('a render with nothing new touches no sidebar DOM', async ({ page }) => {
  const mock = await open(page);
  const result = await page.evaluate((a) => {
    const w = /** @type {any} */ (window);
    const list = document.getElementById('session-list');
    const before = list.querySelector(`.session-card[data-key="${a}"]`);
    const records = [];
    const mo = new MutationObserver((r) => records.push(...r));
    mo.observe(list, { subtree: true, childList: true, attributes: true, characterData: true });
    w.renderSidebar(JSON.parse(JSON.stringify(w._lastSidebarData)));
    w.renderSidebar(JSON.parse(JSON.stringify(w._lastSidebarData)));
    const pending = mo.takeRecords();
    mo.disconnect();
    return {
      mutations: records.length + pending.length,
      same: list.querySelector(`.session-card[data-key="${a}"]`) === before,
    };
  }, A);
  expect(result).toEqual({ mutations: 0, same: true });
  mock.server.close();
});

test('a changed session replaces its own card and no other', async ({ page }) => {
  const mock = await open(page);
  const result = await page.evaluate(([a, b]) => {
    const w = /** @type {any} */ (window);
    const list = document.getElementById('session-list');
    const card = (/** @type {string} */ k) => list.querySelector(`.session-card[data-key="${k}"]`);
    const beforeA = card(a);
    const beforeB = card(b);
    const data = JSON.parse(JSON.stringify(w._lastSidebarData));
    data.sessions.find((/** @type {any} */ s) => s.key === b).last_prompt = 'renamed prompt';
    w.renderSidebar(data);
    return {
      aKept: card(a) === beforeA,
      bReplaced: card(b) !== beforeB,
      bText: card(b).querySelector('.sc-prompt').textContent,
    };
  }, [A, B]);
  expect(result).toEqual({ aKept: true, bReplaced: true, bText: 'renamed prompt' });
  mock.server.close();
});

test('a card patched in place follows the data on the next render', async ({ page }) => {
  const mock = await open(page);
  const dot = page.locator(`.session-card[data-key="${A}"] .sc-dot`);
  await expect(dot).toHaveClass(/dot-ready/);
  await page.evaluate((a) => {
    const w = /** @type {any} */ (window);
    // An optimistic flip patches the dot; the send is then refused and the
    // data still says ready.
    const d = document.querySelector(`.session-card[data-key="${a}"] .sc-dot`);
    d.className = 'sc-dot dot-running';
    w.renderSidebar(JSON.parse(JSON.stringify(w._lastSidebarData)));
  }, A);
  await expect(dot).toHaveClass(/dot-ready/);
  mock.server.close();
});

test('reordering and removal follow the data', async ({ page }) => {
  const mock = await open(page);
  const result = await page.evaluate(([a, b]) => {
    const w = /** @type {any} */ (window);
    const beforeA = document.querySelector(`.session-card[data-key="${a}"]`);
    const data = JSON.parse(JSON.stringify(w._lastSidebarData));
    const sa = data.sessions.find((/** @type {any} */ s) => s.key === a);
    const sb = data.sessions.find((/** @type {any} */ s) => s.key === b);
    const keys = () => [...document.querySelectorAll('#session-list .session-card')].map((c) => /** @type {HTMLElement} */ (c).dataset.key);
    // The sidebar orders by creation: swap A and B, and drop the
    // otherproject session.
    const aFirst = keys().indexOf(a) < keys().indexOf(b);
    sa.created_at = aFirst ? 2000 : 1000;
    sb.created_at = aFirst ? 1000 : 2000;
    data.sessions = data.sessions.filter((/** @type {any} */ s) => s.project !== 'otherproject');
    w.renderSidebar(data);
    const order = keys();
    return {
      swapped: aFirst ? order.indexOf(b) < order.indexOf(a) : order.indexOf(a) < order.indexOf(b),
      order,
      aMoved: document.querySelector(`.session-card[data-key="${a}"]`) === beforeA,
    };
  }, [A, B]);
  const order = result.order;
  expect(order).toContain(A);
  expect(order).toContain(B);
  expect(result.swapped).toBe(true);
  expect(result.aMoved, 'the surviving card is moved, not rebuilt').toBe(true);
  expect(order.some((k) => k && k.endsWith(':otherproject'))).toBe(false);
  // A render back to the original data restores both.
  await page.evaluate(async () => { await /** @type {any} */ (window).fetchSessions(); });
  await expect(page.locator('#session-list .session-card[data-key$=":otherproject"]')).toHaveCount(1);
  mock.server.close();
});

test('a remote node card keeps its node across an unchanged render', async ({ page }) => {
  const payload = defaultSessions();
  payload.nodes = { local: { display_name: 'Local', status: 'ok' }, gpu: { display_name: 'GPU 盒子', status: 'ok' } };
  const remote = { ...payload.sessions[0], key: 'dashboard:direct:2026-01-01-120009-9:myproject', node: 'gpu' };
  payload.sessions.push(remote);
  const mock = await open(page, { sessions: payload });
  const sel = `.session-card[data-key="${remote.key}"][data-node="gpu"]`;
  await expect(page.locator(`${sel} .sc-node`)).toBeVisible();
  const same = await page.evaluate((s) => {
    const w = /** @type {any} */ (window);
    const before = document.querySelector(s);
    w.renderSidebar(JSON.parse(JSON.stringify(w._lastSidebarData)));
    return document.querySelector(s) === before;
  }, sel);
  expect(same).toBe(true);
  mock.server.close();
});

test('the open session keeps its live agent count across a sidebar render', async ({ page }) => {
  const mock = await open(page);
  await page.click(`.session-card[data-key="${A}"]`);
  await expect(page.locator(`.session-card[data-key="${A}"]`)).toHaveClass(/active/);
  await page.evaluate(() => {
    const w = /** @type {any} */ (window);
    // A sub-agent spawned in the running turn; the server's list has not
    // caught up yet.
    w.turnState.agents.push({ toolUseId: 'tu-1', taskId: '', name: 'reviewer', status: 'spawned' });
    w.renderSidebar(JSON.parse(JSON.stringify(w._lastSidebarData)));
  });
  await expect(page.locator(`.session-card[data-key="${A}"] .sc-agents`)).toContainText('\u00d71');
  mock.server.close();
});

test('after the open card is replaced, switching away clears its highlight', async ({ page }) => {
  const mock = await open(page);
  await page.click(`.session-card[data-key="${A}"]`);
  await expect(page.locator(`.session-card[data-key="${A}"]`)).toHaveClass(/active/);
  const active = await page.evaluate(([a, b]) => {
    const w = /** @type {any} */ (window);
    const data = JSON.parse(JSON.stringify(w._lastSidebarData));
    data.sessions.find((/** @type {any} */ s) => s.key === a).last_prompt = 'fresh title';
    w.renderSidebar(data);
    w.selectSession(b, 'local');
    return [...document.querySelectorAll('#session-list .session-card.active')].map((c) => /** @type {HTMLElement} */ (c).dataset.key);
  }, [A, B]);
  expect(active).toEqual([B]);
  mock.server.close();
});
