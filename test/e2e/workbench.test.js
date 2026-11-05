// E2E: the workbench (workbench.js / workbench_panes.js) — it never opens by
// itself nor collapses the session sidebar (D91): a session's new output
// raises the "产出 N" on its 工作台 button; its 产出 · 文件 · 详情 tabs; and
// the files view's 成品库.
const { test, expect, devices } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const A = 'dashboard:direct:2026-01-01-120000-1:myproject';
const B = 'dashboard:direct:2026-01-01-120001-2:otherproject';
const WS = '/home/user/workspace/myproject';
const now = Date.now();
const file = (key, p, ago) => ({ key, time: new Date(now - ago).toISOString(), type: 'output', summary: p, path: p, dir: WS, root: 'session:' + key });
const pr = (key, n, ago) => ({ key, time: new Date(now - ago).toISOString(), type: 'pr_create', summary: 'gh pr create', url: 'https://github.com/acme/myproject/pull/' + n, state: 'opened' });

// routeFiles answers the file API: a listing with one folder and one file,
// and a preview of whatever path is asked for that echoes it.
async function routeFiles(page) {
  await page.route('**/api/projects/files/list?*', (route) => {
    const dir = new URL(route.request().url()).searchParams.get('dir') || '';
    route.fulfill({
      contentType: 'application/json',
      body: JSON.stringify({ root_name: 'myproject', entries: dir ? [{ name: 'notes.txt', size: 9 }] : [{ name: 'docs', is_dir: true }, { name: 'main.go', size: 120 }] }),
    });
  });
  await page.route('**/api/projects/file?*', (route) => {
    const p = new URL(route.request().url()).searchParams.get('path');
    route.fulfill({ contentType: 'application/json', body: JSON.stringify({ content: 'content of ' + p, size: 20, mime: 'text/plain' }) });
  });
}

const drawer = (page) => page.locator('#fv-drawer');
const tab = (page, name) => page.locator(`#fv-drawer .wb-tab[data-wb-tab="${name}"]`);

test('a new output counts on the 工作台 button; the workbench never opens by itself', async ({ page }) => {
  const outputs = [file(A, 'old.md', 3600000)];
  const mock = await startMockServer({ ws: true, outputs });
  await routeFiles(page);
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click(`.session-card[data-key="${A}"]`);
    const conn = mock.wsConnections[mock.wsConnections.length - 1];
    await expect.poll(() => conn.messages.filter((m) => m.type === 'subscribe' && m.key === A).length).toBe(1);
    const btn = page.locator('.main-header .btn-workbench');
    await expect(btn).toHaveText('工作台产出 1');

    // Live events of a local session come as history frames, as the hub pushes them.
    const event = (ev) => conn.send({ type: 'history', key: A, events: [{ time: Date.now(), uuid: 'wb-' + Math.random(), ...ev }] });
    outputs.unshift(file(A, 'docs/report.md', 0));
    event({ type: 'tool_use', tool: 'Write', summary: 'docs/report.md', path: 'docs/report.md', activity: 'output' });
    await expect(btn).toHaveText('工作台产出 2');
    // One written by Bash, found at turn end.
    outputs.unshift(file(A, 'chart.png', 0));
    event({ type: 'result', summary: 'done' });
    await expect(btn).toHaveText('工作台产出 3');
    await expect(drawer(page)).not.toHaveClass(/fv-open/);

    // The button opens it, on 产出 with nothing previewed yet.
    await btn.click();
    await expect(drawer(page)).toHaveClass(/fv-open/);
    await expect(tab(page, 'outputs')).toHaveAttribute('aria-selected', 'true');
    await expect(page.locator('#wb-outputs .files-row')).toHaveCount(3);
    await expect(page.locator('#wb-outputs-count')).toHaveText('3');
    await page.click('#fv-btn-close');

    // Another session's button counts its own outputs.
    await page.click(`.session-card[data-key="${B}"]`);
    await expect(btn).toHaveText('工作台');
  } finally {
    // Not awaited: the page's socket keeps the server open.
    mock.server.close();
  }
});

test('phone: an output only counts on the header button, which opens it full screen', async ({ browser }) => {
  const ctx = await browser.newContext({ ...devices['iPhone 13'] });
  const page = await ctx.newPage();
  const outputs = [];
  const mock = await startMockServer({ ws: true, outputs });
  await routeFiles(page);
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click(`.session-card[data-key="${A}"]`);
    await expect.poll(() => mock.outputsCalls.filter((k) => k === A).length).toBe(1);
    const conn = mock.wsConnections[mock.wsConnections.length - 1];
    outputs.unshift(file(A, 'chat.md', 0));
    conn.send({ type: 'event', key: A, event: { time: Date.now(), type: 'tool_use', tool: 'Write', summary: 'chat.md', path: 'chat.md', activity: 'output' } });
    const btn = page.locator('.main-header .btn-workbench');
    await expect(btn).toHaveText('工作台产出 1');
    await expect(btn).toBeInViewport();
    await expect(drawer(page)).not.toHaveClass(/fv-open/);
    await page.screenshot({ path: test.info().outputPath('workbench-count-mobile.png') });
    await btn.click();
    await expect(drawer(page)).toHaveClass(/fv-open/);
    const box = await drawer(page).boundingBox();
    expect(Math.round(box.width)).toBe(page.viewportSize().width);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('tabs: 产出 previews and links PRs, 文件 browses the session tree, 详情, 放大', async ({ page }) => {
  const mock = await startMockServer({
    outputs: [file(A, 'site/index.md', 60000), pr(A, 7, 120000), file(A, 'deck.pptx', 180000)],
    sessionDetails: {
      [A]: { key: A, name: 'iso-report', workspace: { id: 'w1', name: 'myproject' }, health: { id: A, state: 'stalled', since: new Date(now - 3600000).toISOString(), evidence: 'no output for 61m' } },
    },
  });
  await routeFiles(page);
  try {
    await page.goto(mock.url + '/dashboard');
    await page.click(`.session-card[data-key="${A}"]`);
    await page.click('.main-header .btn-workbench');
    await expect(drawer(page)).toHaveClass(/fv-open/);
    // The session sidebar stays (D91 S6), and the transcript beside it keeps
    // room for its composer.
    await expect(page.locator('body')).not.toHaveClass(/sidebar-collapsed/);
    await expect(page.locator('#sidebar')).toBeVisible();
    expect((await page.locator('#main').boundingBox()).width).toBeGreaterThanOrEqual(420);
    expect((await page.locator('#msg-input').boundingBox()).width).toBeGreaterThanOrEqual(120);
    // Nothing previewed yet: it opens on 产出.
    await expect(tab(page, 'outputs')).toHaveAttribute('aria-selected', 'true');
    const rows = page.locator('#wb-outputs .files-row');
    await expect(rows).toHaveCount(3);
    await expect(rows.nth(1)).toHaveAttribute('href', 'https://github.com/acme/myproject/pull/7');
    await expect(rows.nth(1)).toHaveAttribute('target', '_blank');
    await expect(rows.nth(2)).toHaveClass(/wb-out-off/);
    // The directory under the name, only when the path has one: a file at
    // the root does not repeat its name.
    await expect(rows.nth(0).locator('.wb-out-path')).toHaveText('site');
    await expect(rows.nth(2).locator('.wb-out-path')).toHaveCount(0);
    await expect(rows.nth(2)).toHaveAttribute('title', /^deck\.pptx/);
    await rows.nth(0).click();
    await expect(tab(page, 'preview')).toHaveAttribute('aria-selected', 'true');
    await expect(page.locator('#fv-body')).toContainText('content of site/index.md');

    await tab(page, 'files').click();
    await expect(page.locator('#wb-files-crumbs')).toHaveText('会话 · myproject');
    await page.locator('#wb-files-list .files-row[data-name="docs"]').click();
    await expect(page.locator('#wb-files-crumbs')).toContainText('docs');
    await page.locator('#wb-files-list .files-row[data-name="notes.txt"]').click();
    await expect(tab(page, 'preview')).toHaveAttribute('aria-selected', 'true');
    await expect(page.locator('#fv-body')).toContainText('content of docs/notes.txt');

    await tab(page, 'details').click();
    const details = page.locator('#wb-details');
    await expect(details).toContainText(A);
    await expect(details).toContainText('iso-report');
    await expect(details).toContainText('停滞');
    await expect(details).toContainText(WS);

    await page.click('#wb-btn-max');
    await expect(drawer(page)).toHaveClass(/wb-max/);
    const vw = page.viewportSize().width;
    expect(Math.round((await drawer(page).boundingBox()).width)).toBe(vw);
    await page.click('#wb-btn-max');
    await expect(drawer(page)).not.toHaveClass(/wb-max/);

    // Another session's tabs follow the selection.
    await page.locator(`.session-card[data-key="${B}"]`).click();
    await tab(page, 'outputs').click();
    await expect(page.locator('#wb-outputs')).toContainText('这个会话还没有产出');
  } finally {
    await new Promise((r) => mock.server.close(r));
  }
});

test('成品库 lists every session\'s outputs by day and previews them', async ({ page }) => {
  const mock = await startMockServer({
    outputs: [file(B, 'plan.md', 60000), pr(A, 9, 120000), file(A, 'old/report.md', 3 * 86400000)],
  });
  await routeFiles(page);
  try {
    await page.goto(mock.url + '/dashboard');
    await page.click('#abnav-files');
    await page.locator('#files-quickjump .files-qj[data-library]').click();
    await expect(page.locator('#files-crumbs')).toHaveText('成品库');
    await expect(page.locator('#files-upload-btn')).toBeHidden();
    const list = page.locator('#files-list');
    await expect(list.locator('.wb-group')).toHaveCount(2);
    await expect(list.locator('.wb-group').first()).toHaveText('今天');
    await expect(list.locator('.files-row')).toHaveCount(3);
    // Rows name the session that made them.
    await expect(list.locator('.files-row').first()).toContainText('review this code');
    await expect(list.locator('a.files-row')).toHaveAttribute('href', 'https://github.com/acme/myproject/pull/9');
    await list.locator('.files-row[data-kind="output"]').first().click();
    await expect(page.locator('#files-main-title')).toHaveText('plan.md');
    await expect(page.locator('#files-main-body')).toContainText('content of plan.md');
    expect(mock.outputsCalls).toContain('');
  } finally {
    await new Promise((r) => mock.server.close(r));
  }
});
