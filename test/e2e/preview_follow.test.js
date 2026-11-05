// E2E: the workbench preview of an HTML output loads its directory (relative
// CSS, JS and images, under POST /api/preview's link), and a previewed file
// follows the session's writes to it (file_refs.js), a growing log landing at
// its end until the operator scrolls up.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const A = 'dashboard:direct:2026-01-01-120000-1:myproject';
const WS = '/home/user/workspace/myproject';
const file = (p) => ({ key: A, time: new Date().toISOString(), type: 'output', summary: p, path: p, dir: WS, root: 'session:' + A });
const drawer = (page) => page.locator('#fv-drawer');

// The directory the preview link serves, with the policy files_preview.go sends.
const SITE = {
  'index.html': ['text/html; charset=utf-8', '<!doctype html><html><head><link rel="stylesheet" href="css/style.css"><script src="app.js"></script></head><body><h1 id="x">plain</h1><img id="logo" src="logo.svg" alt=""></body></html>'],
  'css/style.css': ['text/css; charset=utf-8', '#x{color:rgb(255,0,0)}'],
  'app.js': ['text/javascript; charset=utf-8', "document.addEventListener('DOMContentLoaded',()=>{document.getElementById('x').textContent='script ran'})"],
  'logo.svg': ['image/svg+xml', '<svg xmlns="http://www.w3.org/2000/svg" width="12" height="12"><rect width="12" height="12"/></svg>'],
};

function previewCSP(host, token) {
  const p = host + '/api/preview/' + token + '/';
  const src = 'http://' + p + ' https://' + p;
  return ["default-src 'none'", 'script-src ' + src + " 'unsafe-inline' 'unsafe-eval'", 'style-src ' + src + " 'unsafe-inline'",
    'img-src ' + src + ' data: blob:', 'font-src ' + src + ' data:', 'media-src ' + src + ' data: blob:', 'connect-src ' + src,
    "form-action 'none'", "base-uri 'none'", "frame-ancestors 'self'", 'sandbox allow-scripts'].join('; ');
}

test('an HTML output previews with its directory: relative CSS, JS and images load in the sandboxed frame', async ({ page }) => {
  const mock = await startMockServer({ outputs: [file('site/index.html')] });
  const minted = [];
  await page.route('**/api/preview', async (route) => {
    minted.push(route.request().postDataJSON());
    await route.fulfill({ contentType: 'application/json', body: JSON.stringify({ url: '/api/preview/tok1/index.html' }) });
  });
  await page.route('**/api/preview/tok1/**', (route) => {
    const u = new URL(route.request().url());
    const f = SITE[u.pathname.slice('/api/preview/tok1/'.length)];
    if (!f) return route.fulfill({ status: 404, body: '' });
    return route.fulfill({ contentType: f[0], body: f[1], headers: { 'Content-Security-Policy': previewCSP(u.host, 'tok1'), 'X-Content-Type-Options': 'nosniff', 'Access-Control-Allow-Origin': '*' } });
  });
  try {
    await page.goto(mock.url + '/dashboard');
    await page.click(`.session-card[data-key="${A}"]`);
    await page.click('.main-header .btn-workbench');
    await page.locator('#wb-outputs .files-row').first().click();
    const frame = page.locator('#fv-body iframe');
    await expect(frame).toHaveAttribute('sandbox', 'allow-scripts');
    await expect(frame).toHaveAttribute('src', '/api/preview/tok1/index.html');
    expect(minted).toEqual([{ root: 'session:' + A, path: 'site/index.html' }]);
    const doc = page.frameLocator('#fv-body iframe');
    await expect(doc.locator('#x')).toHaveText('script ran');
    await expect(doc.locator('#x')).toHaveCSS('color', 'rgb(255, 0, 0)');
    await expect.poll(() => doc.locator('#logo').evaluate((img) => img.naturalWidth)).toBe(12);
    await page.screenshot({ path: test.info().outputPath('dir-preview.png') });
  } finally {
    await new Promise((r) => mock.server.close(r));
  }
});

test('an HTML preview follows writes to the directory it serves, not to the rest of the tree', async ({ page }) => {
  const mock = await startMockServer({ ws: true, outputs: [file('site/index.html')] });
  const site = { ...SITE };
  let minted = 0;
  await page.route('**/api/preview', (route) => {
    minted++;
    route.fulfill({ contentType: 'application/json', body: JSON.stringify({ url: '/api/preview/tok1/index.html' }) });
  });
  await page.route('**/api/preview/tok1/**', (route) => {
    const u = new URL(route.request().url());
    const f = site[u.pathname.slice('/api/preview/tok1/'.length)];
    if (!f) return route.fulfill({ status: 404, body: '' });
    return route.fulfill({ contentType: f[0], body: f[1], headers: { 'Content-Security-Policy': previewCSP(u.host, 'tok1'), 'Cache-Control': 'no-store' } });
  });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click(`.session-card[data-key="${A}"]`);
    const conn = mock.wsConnections[mock.wsConnections.length - 1];
    await expect.poll(() => conn.messages.filter((m) => m.type === 'subscribe' && m.key === A).length).toBe(1);
    const event = (ev) => conn.send({ type: 'history', key: A, events: [{ time: Date.now(), uuid: 'pd-' + Math.random(), ...ev }] });
    await page.click('.main-header .btn-workbench');
    await page.locator('#wb-outputs .files-row').first().click();
    const doc = page.frameLocator('#fv-body iframe');
    await expect(doc.locator('#x')).toHaveCSS('color', 'rgb(255, 0, 0)');
    expect(minted).toBe(1);

    // A stylesheet outside the served directory leaves it alone.
    event({ type: 'tool_use', tool: 'Write', summary: 'other/style.css', path: 'other/style.css' });
    await page.waitForTimeout(1500);
    expect(minted).toBe(1);
    // Its own stylesheet changing reloads it.
    site['css/style.css'] = ['text/css; charset=utf-8', '#x{color:rgb(0,128,0)}'];
    event({ type: 'tool_use', tool: 'Edit', summary: 'site/css/style.css', path: 'site/css/style.css' });
    await expect(doc.locator('#x')).toHaveCSS('color', 'rgb(0, 128, 0)');
    expect(minted).toBe(2);
  } finally {
    mock.server.close();
  }
});

test('a previewed file follows the session: a write refreshes it, a growing log stays at its end until scrolled up', async ({ page }) => {
  const outputs = [file('notes.md'), file('build.log')];
  const mock = await startMockServer({ ws: true, outputs });
  const content = { 'notes.md': 'first draft', 'build.log': Array.from({ length: 200 }, (_, i) => 'line ' + i).join('\n') };
  const reads = {};
  await page.route('**/api/projects/file?*', (route) => {
    const p = new URL(route.request().url()).searchParams.get('path');
    reads[p] = (reads[p] || 0) + 1;
    route.fulfill({ contentType: 'application/json', body: JSON.stringify({ content: content[p], size: content[p].length, mime: 'text/plain' }) });
  });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click(`.session-card[data-key="${A}"]`);
    const conn = mock.wsConnections[mock.wsConnections.length - 1];
    await expect.poll(() => conn.messages.filter((m) => m.type === 'subscribe' && m.key === A).length).toBe(1);
    const event = (ev) => conn.send({ type: 'history', key: A, events: [{ time: Date.now(), uuid: 'pf-' + Math.random(), ...ev }] });
    const body = page.locator('#fv-body');

    await page.click('.main-header .btn-workbench');
    await page.locator('#wb-outputs .files-row[data-i="0"]').click();
    await expect(body).toContainText('first draft');

    // A write to another file leaves it alone; a write to it refreshes it.
    content['notes.md'] = 'second draft';
    event({ type: 'tool_use', tool: 'Write', summary: 'other.md', path: 'other.md' });
    await page.waitForTimeout(1500);
    await expect(body).toContainText('first draft');
    event({ type: 'tool_use', tool: 'Edit', summary: 'notes.md', path: 'notes.md' });
    await expect(body).toContainText('second draft');

    // The event comes before the call runs: a write that lands after the
    // refresh (the Edit queued behind a slow Bash) shows on the session's
    // next event, and at its turn end.
    event({ type: 'tool_use', tool: 'Bash', summary: 'Bash sleep 30' });
    event({ type: 'tool_use', tool: 'Edit', summary: 'notes.md', path: 'notes.md' });
    await page.waitForTimeout(1500);
    content['notes.md'] = 'third draft';
    event({ type: 'text', summary: 'edited' });
    await expect(body).toContainText('third draft');
    content['notes.md'] = 'final draft';
    event({ type: 'result', summary: 'done' });
    await expect(body).toContainText('final draft');
    const settled = reads['notes.md'];
    content['notes.md'] = 'after the turn';
    event({ type: 'text', summary: 'next turn' });
    await page.waitForTimeout(1500);
    expect(reads['notes.md']).toBe(settled);

    // A log written by Bash (no path on the event) follows the session's
    // tool calls and lands at its end while it grows.
    await page.locator('#fv-drawer .wb-tab[data-wb-tab="outputs"]').click();
    await page.locator('#wb-outputs .files-row[data-i="1"]').click();
    await expect(body).toContainText('line 199');
    const atEnd = () => body.evaluate((el) => el.scrollTop + el.clientHeight >= el.scrollHeight - 30);
    content['build.log'] += '\n' + Array.from({ length: 100 }, (_, i) => 'more ' + i).join('\n');
    event({ type: 'tool_use', tool: 'Bash', summary: 'Bash run the build' });
    await expect(body).toContainText('more 99');
    await expect.poll(atEnd).toBe(true);

    // Scrolled up, it keeps the operator's place.
    await body.evaluate((el) => { el.scrollTop = 0; });
    await page.waitForTimeout(100);
    const before = reads['build.log'];
    content['build.log'] += '\nlast line';
    event({ type: 'result', summary: 'done' });
    await expect(body).toContainText('last line');
    expect(reads['build.log']).toBe(before + 1);
    expect(await body.evaluate((el) => el.scrollTop)).toBe(0);
  } finally {
    mock.server.close();
  }
});
