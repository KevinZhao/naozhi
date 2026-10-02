// @ts-check
//
// renderCronList reconciles the cron rows by data-cron-id (S20b, #3026)
// instead of rewriting the list with innerHTML. A row whose markup did not
// change keeps its DOM node, so:
//
//   1. a repaint with nothing changed (a status-chip click) keeps every node;
//   2. a run_started frame for job A replaces A's row and no other;
//   3. a keyboard user focused on row B stays focused while A changes;
//   4. a sort switch moves the nodes into the new order instead of rebuilding;
//   5. deleting a job removes its row and leaves the others' nodes alone;
//   6. the list, the filtered-empty notice and the empty state replace each
//      other cleanly, with exactly one .cj-list under the host when listing.
//
// Node identity is checked with a page-side Map from id to element: a data-*
// marker would itself make the row unequal to the fresh markup.
//
// The jobs carry no next_run / last_run_at, so a row's text has no clock in it
// and two paints a millisecond apart produce the same markup.
//
// 跑法：cd test/e2e && npx playwright test cron_list_reconcile.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'desktop-chrome only');
  }
});

// Six jobs: more than five puts the search row and the sort select in the
// shell. created_desc (the default) shows them f, e, d, c, b, a; title_asc
// shows them in a different order (see TITLE_ORDER).
function jobs() {
  const day = 86400000;
  const now = Date.now();
  const titles = { a: 'delta', b: 'alpha', c: 'foxtrot', d: 'bravo', e: 'echo', f: 'charlie' };
  return ['a', 'b', 'c', 'd', 'e', 'f'].map((k, i) => ({
    id: 'cron-' + k,
    title: titles[k],
    schedule: '0 6 * * *',
    prompt: 'job ' + k,
    work_dir: '/home/user/workspace/myproject',
    paused: false,
    created_at: now - (10 - i) * day,
    recent_runs: [],
    stats: { total: 0, succeeded: 0 },
  }));
}

const CREATED_ORDER = ['cron-f', 'cron-e', 'cron-d', 'cron-c', 'cron-b', 'cron-a'];
const TITLE_ORDER = ['cron-b', 'cron-d', 'cron-f', 'cron-a', 'cron-e', 'cron-c'];

/** @param {import('@playwright/test').Browser} browser */
async function openPanel(browser) {
  const served = jobs();
  const mock = await startMockServer({ cronJobs: served });
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 } });
  const page = await ctx.newPage();
  /** @type {string[]} */
  const pageErrors = [];
  page.on('pageerror', (e) => pageErrors.push(String(e)));
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click('#abnav-cron');
  await expect(page.locator('#cron-list-items .cj-row')).toHaveCount(6);
  await expect(page.locator('#cron-search-input'), 'six jobs put the search row in the shell').toBeVisible();
  return { mock, served, ctx, page, pageErrors };
}

/** Remember the row element currently shown for every job. */
async function markRows(page) {
  await page.evaluate(() => {
    const marks = new Map();
    for (const el of document.querySelectorAll('#cron-list-items .cj-row')) {
      marks.set(el.getAttribute('data-cron-id'), el);
    }
    // @ts-ignore
    window.__cronMarks = marks;
  });
}

/** Ids whose current row is still the element markRows saw, in DOM order. */
async function keptRows(page) {
  return page.evaluate(() => {
    // @ts-ignore
    const marks = window.__cronMarks;
    return [...document.querySelectorAll('#cron-list-items .cj-row')]
      .filter((el) => marks.get(el.getAttribute('data-cron-id')) === el)
      .map((el) => el.getAttribute('data-cron-id'));
  });
}

/** @param {import('@playwright/test').Page} page */
async function rowOrder(page) {
  return page.locator('#cron-list-items .cj-row').evaluateAll((els) => els.map((el) => el.getAttribute('data-cron-id')));
}

/** @param {import('@playwright/test').Page} page @param {string} ownerId */
async function pushRunStarted(page, ownerId) {
  await page.evaluate((id) => {
    // @ts-ignore
    wsm.onMessage({
      type: 'run_started', subsystem: 'cron', owner_id: id, run_id: 'run-' + id,
      started_at: Date.now(), trigger: 'cron', session_id: 'sess-' + id,
    });
  }, ownerId);
}

test('(1) a repaint with nothing changed keeps every row node', async ({ browser }) => {
  const { mock, ctx, page, pageErrors } = await openPanel(browser);
  try {
    await markRows(page);
    // 全部 is already the active chip: the click repaints the same list.
    await page.click('.cron-status-chip[data-status="all"]');
    await expect(page.locator('.cron-status-chip[data-status="all"]')).toHaveAttribute('aria-pressed', 'true');
    expect(await keptRows(page), 'an unchanged repaint must keep every node').toEqual(CREATED_ORDER);
    // 运行中 then 全部: no job is paused, so the list never changes.
    await page.click('.cron-status-chip[data-status="active"]');
    await expect(page.locator('.cron-status-chip[data-status="active"]')).toHaveAttribute('aria-pressed', 'true');
    await page.click('.cron-status-chip[data-status="all"]');
    await expect(page.locator('.cron-status-chip[data-status="all"]')).toHaveAttribute('aria-pressed', 'true');
    expect(await keptRows(page)).toEqual(CREATED_ORDER);
    expect(pageErrors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('(2) run_started for one job replaces that row only', async ({ browser }) => {
  const { mock, ctx, page, pageErrors } = await openPanel(browser);
  try {
    await markRows(page);
    await pushRunStarted(page, 'cron-c');
    await expect(page.locator('.cj-row[data-cron-id="cron-c"]')).toHaveClass(/is-running/);
    expect(await keptRows(page), 'every row but cron-c keeps its node')
      .toEqual(CREATED_ORDER.filter((id) => id !== 'cron-c'));
    expect(await rowOrder(page), 'the replaced row stays in place').toEqual(CREATED_ORDER);
    expect(pageErrors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('(3) the focused row keeps focus while another row changes', async ({ browser }) => {
  const { mock, ctx, page, pageErrors } = await openPanel(browser);
  try {
    const rowB = page.locator('.cj-row[data-cron-id="cron-b"]');
    await rowB.focus();
    await expect(rowB).toBeFocused();
    await pushRunStarted(page, 'cron-e');
    await expect(page.locator('.cj-row[data-cron-id="cron-e"]')).toHaveClass(/is-running/);
    await expect(rowB, 'a run on cron-e must not take focus from cron-b').toBeFocused();
    await page.evaluate(() => {
      // @ts-ignore
      wsm.onMessage({
        type: 'run_ended', subsystem: 'cron', owner_id: 'cron-e', run_id: 'run-cron-e',
        state: 'succeeded', ended_at: Date.now(), duration_ms: 900, trigger: 'cron',
      });
    });
    await expect(page.locator('.cj-row[data-cron-id="cron-e"]')).not.toHaveClass(/is-running/);
    await expect(rowB).toBeFocused();
    expect(pageErrors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('(4) a sort switch keeps the nodes and follows the new order', async ({ browser }) => {
  const { mock, ctx, page, pageErrors } = await openPanel(browser);
  try {
    expect(await rowOrder(page)).toEqual(CREATED_ORDER);
    await markRows(page);
    await page.selectOption('.cron-sort-select', 'title_asc');
    await expect.poll(() => rowOrder(page)).toEqual(TITLE_ORDER);
    expect(await keptRows(page), 'the sort moves nodes, it does not rebuild them').toEqual(TITLE_ORDER);
    await page.selectOption('.cron-sort-select', 'created_desc');
    await expect.poll(() => rowOrder(page)).toEqual(CREATED_ORDER);
    expect(await keptRows(page)).toEqual(CREATED_ORDER);
    expect(pageErrors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('(5) deleting a job removes its row and keeps the other nodes', async ({ browser }) => {
  const { mock, served, ctx, page, pageErrors } = await openPanel(browser);
  try {
    await markRows(page);
    // cron-d sits third: positional matching would shift every row after it.
    await page.locator('.cj-row[data-cron-id="cron-d"] [data-action="cron-menu-toggle"]').click();
    await page.locator('.cj-menu-item[data-menu-action="delete"]').click();
    const ok = page.locator('.confirm-dialog .confirm-ok');
    await expect(ok).toBeEnabled({ timeout: 6000 });
    // The mock's DELETE leaves its list alone; drop the job from what GET
    // serves so the refetch after the delete no longer has it.
    served.splice(served.findIndex((j) => j.id === 'cron-d'), 1);
    await ok.click();
    await expect.poll(() => mock.cronDeleteCalls).toEqual(['cron-d']);
    const rest = CREATED_ORDER.filter((id) => id !== 'cron-d');
    await expect.poll(() => rowOrder(page)).toEqual(rest);
    expect(await keptRows(page), 'only the deleted row goes').toEqual(rest);
    expect(pageErrors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('(6) list, filtered-empty and empty state replace each other', async ({ browser }) => {
  const { mock, served, ctx, page, pageErrors } = await openPanel(browser);
  try {
    const host = page.locator('#cron-list-items');
    const lists = host.locator(':scope > .cj-list');
    const hostChildren = host.locator(':scope > *');
    await expect(lists).toHaveCount(1);
    await expect(hostChildren).toHaveCount(1);

    await page.fill('#cron-search-input', 'no such job');
    await expect(host.locator(':scope > .cron-filter-empty')).toHaveCount(1);
    await expect(hostChildren).toHaveCount(1);
    await expect(host.locator('.cj-row')).toHaveCount(0);

    await page.click('.cron-search-clear');
    await expect(lists).toHaveCount(1);
    await expect(hostChildren).toHaveCount(1);
    expect(await rowOrder(page)).toEqual(CREATED_ORDER);

    // A run frame for a job the page does not know makes it refetch the list.
    const saved = served.splice(0, served.length);
    await pushRunStarted(page, 'cron-unknown-1');
    await expect(host.locator(':scope > .cron-empty')).toHaveCount(1);
    await expect(hostChildren).toHaveCount(1);
    await expect(host.locator('.cj-row')).toHaveCount(0);

    served.push(...saved);
    await pushRunStarted(page, 'cron-unknown-2');
    await expect(lists).toHaveCount(1);
    await expect(hostChildren).toHaveCount(1);
    await expect.poll(() => rowOrder(page)).toEqual(CREATED_ORDER);

    await page.fill('#cron-search-input', 'alpha');
    await expect.poll(() => rowOrder(page)).toEqual(['cron-b']);
    await expect(lists).toHaveCount(1);
    await expect(hostChildren).toHaveCount(1);
    expect(pageErrors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
