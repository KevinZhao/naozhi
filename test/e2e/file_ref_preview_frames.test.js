// @ts-check
//
// The preview drawer a chat file-ref opens (file_refs.js openFilePreview),
// driven from a path in a reply through the existence check to the click.
// Two frame shapes are security properties:
//
//   - A PDF frame carries sandbox="" (no capabilities). serveRaw forces PDFs
//     to download today; the sandbox is what holds if a proxy strips
//     Content-Disposition, since an embedded PDF could otherwise run script
//     same-origin to the dashboard.
//   - HTML renders in an allow-scripts-only sandbox pointed at the server's
//     inline render form (mode=render&inline=1). That response carries its
//     own sandbox CSP. A document built client-side would inherit the
//     dashboard's policy, which has no script-src 'unsafe-inline'.
//
// The files browser has the same two shapes; files_view_behaviour.test.js
// covers it.
//
// 跑法：cd test/e2e && npx playwright test file_ref_preview_frames.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const SESSION_KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'viewport-independent; desktop-chrome only');
  }
});

test('chat file-ref previews: PDF frame has no capabilities, HTML uses the inline render form', async ({ browser }) => {
  const now = Date.now();
  const mock = await startMockServer({
    eventsByKey: {
      [SESSION_KEY]: [
        { time: now - 2000, type: 'user', summary: 'make them', detail: 'make them' },
        { time: now - 1000, type: 'text', summary: 'wrote `docs/report.pdf` and `docs/page.html`', detail: 'wrote `docs/report.pdf` and `docs/page.html`' },
      ],
    },
    projectFiles: {
      myproject: {
        exists: {
          'docs/report.pdf': { exists: true, size: 10, mime: 'application/pdf' },
          'docs/page.html': { exists: true, size: 10, mime: 'text/html' },
        },
      },
    },
  });
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  const page = await ctx.newPage();
  /** @type {string[]} */
  const pageErrors = [];
  page.on('pageerror', (e) => pageErrors.push(String(e)));
  try {
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click(`.session-card[data-key="${SESSION_KEY}"]`);

    const previewBtn = (/** @type {string} */ p) =>
      page.locator(`.fr-slot.fr-verified[data-path="${p}"] .fr-btn-preview`);
    await expect(previewBtn('docs/report.pdf'), 'the existence check must verify the path').toHaveCount(1, { timeout: 8000 });

    await previewBtn('docs/report.pdf').click();
    const frame = page.locator('#fv-body iframe');
    await expect(frame).toHaveCount(1);
    await expect(frame).toHaveAttribute('sandbox', '');
    expect(await frame.getAttribute('src')).toContain('mode=raw');

    await previewBtn('docs/page.html').click();
    await expect(frame).toHaveAttribute('sandbox', 'allow-scripts');
    const src = await frame.getAttribute('src');
    expect(src).toContain('mode=render');
    expect(src).toContain('inline=1');
    expect(await frame.getAttribute('srcdoc')).toBeNull();

    expect(pageErrors).toEqual([]);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('chat file-ref previews: images load raw, text gets a gutter, binary content a placeholder or the HTML frame', async ({ browser }) => {
  const now = Date.now();
  const files = ['docs/shot.png', 'src/main.go', 'docs/page.bin', 'bin/tool'];
  const reply = 'wrote ' + files.map((f) => '`' + f + '`').join(' and ');
  const mock = await startMockServer({
    eventsByKey: {
      [SESSION_KEY]: [
        { time: now - 2000, type: 'user', summary: 'make them', detail: 'make them' },
        { time: now - 1000, type: 'text', summary: reply, detail: reply },
      ],
    },
    projectFiles: {
      myproject: {
        exists: {
          'docs/shot.png': { exists: true, size: 10, mime: 'image/png' },
          'src/main.go': { exists: true, size: 30, mime: 'text/plain' },
          'docs/page.bin': { exists: true, size: 10, mime: 'application/octet-stream' },
          'bin/tool': { exists: true, size: 10, mime: 'application/octet-stream' },
        },
        previews: {
          'src/main.go': { content: 'package main\nfunc main() {}\n', mime: 'text/plain', size: 30 },
          'docs/page.bin': { binary: true, mime: 'text/html', size: 10 },
          'bin/tool': { binary: true, mime: 'application/x-mach-binary', size: 10 },
        },
      },
    },
  });
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  const page = await ctx.newPage();
  try {
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click(`.session-card[data-key="${SESSION_KEY}"]`);
    const previewBtn = (/** @type {string} */ p) =>
      page.locator(`.fr-slot.fr-verified[data-path="${p}"] .fr-btn-preview`);
    await expect(previewBtn('bin/tool')).toHaveCount(1, { timeout: 8000 });
    const body = page.locator('#fv-body');

    // An image goes straight to the raw endpoint, no preview JSON.
    await previewBtn('docs/shot.png').click();
    await expect(body.locator('img')).toHaveCount(1);
    expect(await body.locator('img').getAttribute('src')).toContain('mode=raw');
    await expect(page.locator('#fv-title')).toHaveText('docs/shot.png');

    // Text: the preview JSON's content in a line-numbered listing.
    await previewBtn('src/main.go').click();
    await expect(body.locator('pre.fv-lined code.fv-code')).toHaveText('package main\nfunc main() {}\n');
    await expect(body.locator('.fv-gutter')).toHaveText('1\n2\n3');

    // Binary HTML (sniffed by the server) renders in the inline render frame.
    await previewBtn('docs/page.bin').click();
    const frame = body.locator('iframe');
    await expect(frame).toHaveAttribute('sandbox', 'allow-scripts');
    expect(await frame.getAttribute('src')).toContain('mode=render');

    // Any other binary: a download placeholder naming the MIME.
    await previewBtn('bin/tool').click();
    await expect(body.locator('.fv-binary .fv-mime')).toHaveText('application/x-mach-binary');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
