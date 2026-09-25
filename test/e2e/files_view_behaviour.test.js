// @ts-check
//
// The workspace file browser (files_view.js), driven against a mock file
// tree. Each case is a bug that shipped once:
//
//   - A name with a quote in it built data-name / data-dir with the
//     non-quote-escaping esc(), which truncated the attribute, so the click
//     went to the wrong path.
//   - A non-text file answers preview as {content:"", binary:true}. Gating the
//     hint on `content == null` never fired, and an empty <pre> rendered.
//   - The 空目录 hint was gated on the root dir, so an empty subdirectory
//     showed nothing but the ↑ row.
//   - renderEmpty escapes its argument. A caller that pre-escaped showed
//     `&lt;` literally.
//
// Two preview shapes are security properties, checked here for this view:
// a PDF frame has sandbox="" (no capabilities), and HTML renders in an
// allow-scripts-only sandboxed iframe pointed at the server's inline render
// form rather than a client-built document.
//
// 跑法：cd test/e2e && npx playwright test files_view_behaviour.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'viewport-independent; desktop-chrome only');
  }
});

function tree() {
  return {
    myproject: {
      dirs: {
        '': [
          { name: 'a"b.txt', size: 12 },
          { name: 'sub"dir', is_dir: true },
          { name: 'empty', is_dir: true },
          { name: 'broken', is_dir: true },
          { name: 'blob.bin', size: 100 },
          { name: 'doc.pdf', size: 10 },
          { name: 'page.html', size: 10 },
        ],
        'sub"dir': [{ name: 'x.txt', size: 1 }],
        empty: [],
      },
      listErrors: { broken: 'a<b>' },
      previews: {
        'a"b.txt': { content: 'hello from a quote' },
        'blob.bin': { content: '', binary: true },
      },
    },
  };
}

test.describe('files view', () => {
  /** @type {Awaited<ReturnType<typeof startMockServer>>} */
  let mock;
  /** @type {import('@playwright/test').Page} */
  let page;
  /** @type {string[]} */
  const pageErrors = [];

  test.beforeAll(async ({ browser }) => {
    mock = await startMockServer({ projectFiles: tree() });
    const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
    page = await ctx.newPage();
    page.on('pageerror', (e) => pageErrors.push(String(e)));
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click('#abnav-files');
  });
  test.afterAll(async () => {
    expect(pageErrors).toEqual([]);
    await page.context().close();
    mock.server.close();
  });

  /** @param {string} name */
  const row = (name) => page.locator('#files-list .files-row').filter({ has: page.locator('.files-name', { hasText: new RegExp('^' + name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&') + '$') }) });
  const toRoot = async () => {
    await page.locator('#files-crumbs .files-crumb').first().click();
    await expect(row('a"b.txt')).toHaveCount(1);
  };

  test('a quoted file name survives into data-name and previews the right path', async () => {
    await toRoot();
    await expect(row('a"b.txt')).toHaveAttribute('data-name', 'a"b.txt');
    await row('a"b.txt').click();
    await expect(page.locator('#files-main-body pre')).toHaveText('hello from a quote');
    expect(mock.fileRequests.filter(r => r.mode === 'preview').map(r => r.path)).toContain('a"b.txt');
  });

  test('a quoted directory name survives into the crumb data-dir', async () => {
    await toRoot();
    await row('sub"dir').click();
    await expect(row('x.txt')).toHaveCount(1);
    expect(mock.fileRequests.filter(r => r.mode === 'list').map(r => r.dir)).toContain('sub"dir');
    await expect(page.locator('#files-crumbs .files-crumb').last()).toHaveAttribute('data-dir', 'sub"dir');
  });

  test('a binary file shows the not-previewable hint, not an empty <pre>', async () => {
    await toRoot();
    await row('blob.bin').click();
    await expect(page.locator('#files-main-body')).toContainText('该文件不可预览');
    await expect(page.locator('#files-main-body pre')).toHaveCount(0);
  });

  test('an empty subdirectory shows 空目录 and keeps the ↑ row', async () => {
    await toRoot();
    await row('empty').click();
    await expect(page.locator('#files-list .files-empty')).toHaveText('空目录');
    await expect(page.locator('#files-list .files-up')).toHaveCount(1);
  });

  test('a load error is escaped exactly once', async () => {
    await toRoot();
    await row('broken').click();
    const msg = page.locator('#files-list .files-empty');
    await expect(msg).toContainText('a<b>');
    await expect(msg).not.toContainText('&lt;');
  });

  test('a PDF previews in a frame with no sandbox capabilities', async () => {
    await toRoot();
    await row('doc.pdf').click();
    const frame = page.locator('#files-main-body iframe');
    await expect(frame).toHaveAttribute('sandbox', '');
  });

  test('HTML previews in an allow-scripts sandbox pointed at the inline render form', async () => {
    await toRoot();
    await row('page.html').click();
    const frame = page.locator('#files-main-body iframe');
    await expect(frame).toHaveAttribute('sandbox', 'allow-scripts');
    const src = await frame.getAttribute('src');
    expect(src).toContain('mode=render');
    expect(src).toContain('inline=1');
    expect(await frame.getAttribute('srcdoc')).toBeNull();
  });
});
