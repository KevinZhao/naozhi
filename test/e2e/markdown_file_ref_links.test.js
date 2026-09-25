// @ts-check
//
// File-ref <code> rendering in renderMd.
//
// claude CLI often names a generated file as a markdown link,
// `[数学/专题/foo.html](数学/专题/foo.html)`, instead of a backtick span. safeUrl
// rejects the non-http target, and without a rescue the link collapses to
// plain text. The file-ref scanner only walks <code>, so that text never gets
// its preview/download buttons. The rescue re-renders a path-shaped target as
// inline <code class="md-code">.
//
// The link target has already been through earlier inlineMd passes by the
// time the link pass sees it. Those passes can leave naozhi's own
// <strong>/<em> or \x00 tokenizer sentinels in it, so the rescue has to
// refuse both, and it has to refuse slash-shaped non-files (dates,
// fractions). The same <code> shape serves backtick spans and the rows of a
// fenced path list. Each call site escapes its own input, and the shared
// helper must not escape it a second time.
//
// Assertions parse the HTML and query the DOM, so they hold however the
// markup is spelled.
//
// 跑法：cd test/e2e && npx playwright test markdown_file_ref_links.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, '渲染逻辑与 viewport 无关，仅 desktop-chrome 跑一次');
  }
});

test.describe('renderMd file-ref <code>', () => {
  /** @type {Awaited<ReturnType<typeof startMockServer>>} */
  let mock;
  /** @type {import('@playwright/test').Page} */
  let page;

  test.beforeAll(async ({ browser }) => {
    mock = await startMockServer();
    const ctx = await browser.newContext();
    page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForFunction(() => typeof (/** @type {any} */ (window)).renderMd === 'function');
  });
  test.afterAll(async () => {
    await page.context().close();
    mock.server.close();
  });

  /**
   * Renders src and reports the shape of what came out.
   * @param {string} src
   */
  const shape = (src) => page.evaluate((s) => {
    const div = document.createElement('div');
    div.innerHTML = /** @type {any} */ (window).renderMd(s);
    const codes = [...div.querySelectorAll('code')];
    return {
      html: div.innerHTML,
      text: div.textContent,
      codes: codes.map(c => ({ cls: c.className, text: c.textContent, inner: c.innerHTML })),
      nestedInCode: div.querySelectorAll('code *').length,
      anchors: [...div.querySelectorAll('a')].map(a => ({ href: a.getAttribute('href'), cls: a.className })),
      pathlines: [...div.querySelectorAll('.md-pathline > code')].map(c => ({ cls: c.className, text: c.textContent })),
    };
  }, src);

  test('本地文件链接渲染为 <code class="md-code">，显示路径本身', async () => {
    const r = await shape('见 [数学/专题/foo.html](数学/专题/foo.html)');
    expect(r.codes).toEqual([{ cls: 'md-code', text: '数学/专题/foo.html', inner: '数学/专题/foo.html' }]);
    expect(r.anchors).toEqual([]);
  });

  test('http 链接仍渲染为 <a class="md-link">，不走救援', async () => {
    const r = await shape('[site](https://example.com/a.html)');
    expect(r.anchors).toEqual([{ href: 'https://example.com/a.html', cls: 'md-link' }]);
    expect(r.codes).toEqual([]);
  });

  test('目标里被粗体 pass 插入的 <strong> 不会进入 <code>', async () => {
    const r = await shape('[x](a**b**/c.html)');
    expect(r.codes).toEqual([]);
    expect(r.text).toContain('x');
  });

  test('目标里的反引号 sentinel 不会在 <code> 里还原成嵌套 <code>', async () => {
    const r = await shape('[x](`a`/b.html)');
    expect(r.nestedInCode).toBe(0);
    expect(r.html).not.toContain('\x00');
    expect(r.codes.filter(c => c.text.includes('b.html'))).toEqual([]);
  });

  for (const [name, src, label] of [
    ['日期', '[发布日](2024/01/02)', '发布日'],
    ['分数', '[一半](1/2)', '一半'],
    ['无扩展名的 slug', '[文档](docs/guide)', '文档'],
  ]) {
    test(`斜杠形状但不是文件（${name}）：保留作者的标签，不变成 <code>`, async () => {
      const r = await shape(src);
      expect(r.codes).toEqual([]);
      expect(r.text).toContain(label);
    });
  }

  test('反引号路径走同一个 <code class="md-code">', async () => {
    const r = await shape('打开 `docs/a.md`');
    expect(r.codes).toEqual([{ cls: 'md-code', text: 'docs/a.md', inner: 'docs/a.md' }]);
  });

  test('反引号内容只转义一次', async () => {
    const r = await shape('`a<b>&c.md`');
    expect(r.codes.map(c => c.text)).toEqual(['a<b>&c.md']);
    expect(r.codes[0].inner).toBe('a&lt;b&gt;&amp;c.md');
  });

  test('路径列表 fence 的每行是无 class 的 <code>，路径只转义一次', async () => {
    const r = await shape('```\nsrc/a.go\nweb/<x>.html\n```');
    expect(r.pathlines).toEqual([
      { cls: '', text: 'src/a.go' },
      { cls: '', text: 'web/<x>.html' },
    ]);
  });
});
