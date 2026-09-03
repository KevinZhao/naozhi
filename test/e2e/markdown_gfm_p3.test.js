// @ts-check
//
// renderMd GFM P3 回归测试（#2428 第 4/5/6 项）。
//
//   4. `file:line:col` 识别为文件引用（预览仍跳 line）
//   5. blockquote / `__bold__` / `~~del~~` / `+ ` 列表 / `- [ ]` 任务框
//   6. 链接 URL 含 `)` 或 title、链接文本含 URL 不生成嵌套 <a>
//
// 跑法：cd test/e2e && npx playwright test markdown_gfm_p3.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, '渲染逻辑与 viewport 无关，仅 desktop-chrome 跑一次');
  }
});

test.describe('renderMd GFM P3 (#2428)', () => {
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

  /** @param {string} src */
  const render = (src) => page.evaluate((s) => /** @type {any} */ (window).renderMd(s), src);
  /** @param {string} src */
  const isRef = (src) => page.evaluate((s) => /** @type {any} */ (window).isFileRefCandidate(s), src);
  /** @param {string} src */
  const split = (src) => page.evaluate((s) => /** @type {any} */ (window).splitPathLine(s), src);

  // ===== 4. file:line:col =====

  test('src/foo.go:42:10 识别为文件引用，line 取 42', async () => {
    expect(await isRef('src/foo.go:42:10')).toBe(true);
    expect(await split('src/foo.go:42:10')).toEqual({ path: 'src/foo.go', line: '42' });
  });

  test('裸文件名 foo.go:42:10 识别为文件引用', async () => {
    expect(await isRef('foo.go:42:10')).toBe(true);
    expect(await split('foo.go:42-50')).toEqual({ path: 'foo.go', line: '42-50' });
  });

  test('时间 12:30:45 / 三段冒号 a:b:c 不是文件引用', async () => {
    expect(await isRef('12:30:45')).toBe(false);
    expect(await isRef('a:b:c')).toBe(false);
    expect(await isRef('src/foo.go:42:10:3')).toBe(false);
  });

  test('[p](src/foo.go:42:10) 本地链接走 file-ref <code> 救援', async () => {
    const html = await render('[p](src/foo.go:42:10)');
    expect(html).toContain('<code class="md-code">src/foo.go:42:10</code>');
  });

  // ===== 5a. blockquote =====

  test('连续 > 行合并为一个 blockquote，内容走 esc', async () => {
    const html = await render('> a <b>x</b>\n> b\nc');
    expect(html).toContain('<blockquote class="md-quote">a &lt;b&gt;x&lt;/b&gt;<br>b</blockquote>');
    expect(html).not.toContain('<b>x</b>');
    expect(html).toMatch(/<\/blockquote>c<br>$/);
  });

  test('行中 > 不是 blockquote', async () => {
    const html = await render('a > b');
    expect(html).not.toContain('<blockquote');
    expect(html).toContain('a &gt; b');
  });

  test('blockquote 关闭前面的列表', async () => {
    const html = await render('- x\n> q');
    expect(html).toContain('</ul><blockquote class="md-quote">q</blockquote>');
  });

  // ===== 5b. __bold__ =====

  test('__bold__ 渲染为 <strong>', async () => {
    const html = await render('a __bold__ b');
    expect(html).toContain('a <strong>bold</strong> b');
  });

  test('snake_case_name / foo__bar__baz 不误伤', async () => {
    const html = await render('snake_case_name foo__bar__baz');
    expect(html).not.toContain('<strong>');
    expect(html).not.toContain('<em>');
    expect(html).toContain('snake_case_name foo__bar__baz');
  });

  test('链接目标 / 裸 URL / 本地路径中的 __x__ 不被加粗（F1）', async () => {
    const a = await render('[src](https://github.com/o/r/blob/main/pkg/__init__.py)');
    expect(a).toContain('href="https://github.com/o/r/blob/main/pkg/__init__.py"');
    expect(a).not.toContain('<strong>');
    const b = await render('see https://x.com/pkg/__init__.py now');
    expect(b).toContain('href="https://x.com/pkg/__init__.py"');
    expect(b).toContain('>https://x.com/pkg/__init__.py</a> now');
    const c = await render('[init](pkg/__init__.py)');
    expect(c).toContain('<code class="md-code">pkg/__init__.py</code>');
  });

  test('foo.__init__() 保持字面，__all__ = [] 仍加粗', async () => {
    expect(await render('foo.__init__()')).toContain('foo.__init__()');
    expect(await render('foo.__init__()')).not.toContain('<strong>');
    expect(await render('__all__ = []')).toContain('<strong>all</strong> = []');
  });

  test('__ 最坏输入不二次扫描（F2）', async () => {
    const ms = await page.evaluate(() => {
      const w = /** @type {any} */ (window);
      const inputs = [' __a'.repeat(10000), ' __a__b'.repeat(10000), ' ~~a'.repeat(10000)];
      let worst = 0;
      for (const src of inputs) {
        let best = Infinity;
        for (let k = 0; k < 3; k++) {
          const t0 = performance.now();
          w.renderMd(src + String(k)); // vary input to defeat the render cache
          best = Math.min(best, performance.now() - t0);
        }
        worst = Math.max(worst, best);
      }
      return worst;
    });
    expect(ms).toBeLessThan(50);
  });

  // ===== 5c. ~~del~~ =====

  test('~~del~~ 渲染为 <del>', async () => {
    const html = await render('a ~~gone~~ b');
    expect(html).toContain('a <del>gone</del> b');
  });

  test('~/.config 与孤立 ~~ 不误伤', async () => {
    const html = await render('~/.config and a ~~ b');
    expect(html).not.toContain('<del>');
    expect(html).toContain('~/.config and a ~~ b');
  });

  test('URL 中的 a~~b~~c 不被删除线截断（F1）', async () => {
    const html = await render('see https://x.com/a~~b~~c now');
    expect(html).toContain('href="https://x.com/a~~b~~c"');
    expect(html).not.toContain('<del>');
  });

  // ===== 5d. `+ ` 列表 =====

  test('+ 项渲染为 <ul>', async () => {
    const html = await render('+ a\n+ b');
    expect(html).toContain('<ul class="md-ul"><li>a</li><li>b</li></ul>');
  });

  test('1 + 2 / +1 不是列表', async () => {
    expect(await render('1 + 2')).not.toContain('<ul');
    expect(await render('+1 vote')).not.toContain('<ul');
  });

  // ===== 5e. 任务框 =====

  test('- [ ] / - [x] 渲染为 disabled checkbox', async () => {
    const html = await render('- [ ] todo\n- [x] done');
    expect(html).toContain('<li><input type="checkbox" class="md-task" disabled> todo</li>');
    expect(html).toContain('<li><input type="checkbox" class="md-task" disabled checked> done</li>');
  });

  test('- [link](url) / - [a] b 不是任务框', async () => {
    const html = await render('- [link](https://x.com)\n- [a] b');
    expect(html).not.toContain('type="checkbox"');
    expect(html).toContain('<a href="https://x.com"');
  });

  // ===== 6a. 链接 URL 含 ) =====

  test('[t](https://x/a_(b)) href 保留括号', async () => {
    const html = await render('[t](https://x/a_(b))');
    expect(html).toContain('href="https://x/a_(b)"');
    expect(html).toMatch(/>t<\/a><br>$/);
  });

  test('(see [t](https://x.com)) 外层括号留在链接外', async () => {
    const html = await render('(see [t](https://x.com))');
    expect(html).toContain('href="https://x.com"');
    expect(html).toMatch(/>t<\/a>\)/);
  });

  // ===== 6b. title 语法 =====

  test('[t](url "title") href 不含 title，title 进属性', async () => {
    const html = await render('[t](https://x.com/a "My Title")');
    expect(html).toContain('href="https://x.com/a"');
    expect(html).toContain('title="My Title"');
    expect(html).toMatch(/>t<\/a><br>$/);
  });

  test('title 中的 " 与 < 走 escAttr', async () => {
    const html = await render("[t](https://x.com/a 'a<b')");
    expect(html).toContain('href="https://x.com/a"');
    expect(html).toContain('title="a&lt;b"');
  });

  test('[t]( url ) / [t](url "t" ) 允许两侧空白填充（F3）', async () => {
    const a = await render('[t]( https://x.com )');
    expect(a).toContain('href="https://x.com"');
    expect(a).toMatch(/>t<\/a><br>$/);
    const b = await render('[t](https://x.com "tt" )');
    expect(b).toContain('href="https://x.com"');
    expect(b).toContain('title="tt"');
  });

  // ===== 6c. 链接文本含 URL 不嵌套 <a> =====

  test('[see https://x.com](https://x.com) 只生成一个 <a>', async () => {
    const html = await render('[see https://x.com](https://x.com)');
    expect(html.match(/<a /g) || []).toHaveLength(1);
    expect(html).toContain('>see https://x.com</a>');
  });

  test('链接外的裸 URL 仍自动链接', async () => {
    const html = await render('[a](https://x.com) and https://y.com done');
    expect(html.match(/<a /g) || []).toHaveLength(2);
    expect(html).toContain('href="https://y.com"');
  });
});
