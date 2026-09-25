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
