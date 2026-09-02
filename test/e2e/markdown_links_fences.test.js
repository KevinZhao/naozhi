// @ts-check
//
// renderMd 链接 / fence / 表格降级 / 斜体 回归测试。
//
// 覆盖的线上实测 bug：
//   1. 链接 href 中 `&` 双重转义（esc 后再 escAttr → `&amp;amp;`）
//   2. 自动链接吞中文标点（`https://x.com/a。然后` 整句进链接）
//   3. fence 语言标识含非 \w 字符时残留进代码体（```c++ → lang=c，体多出 ++）
//   4. 未闭合 fence（流式尾巴）语言行进入代码体、末尾 3 字符被截
//   5. `![alt](url)` 多输出一个 `!`
//   6. 表格降级路径用 `\n` 拼接塌成一行
//   7. `2 * 3 * 4` 误斜体
//
// 跑法：cd test/e2e && npx playwright test markdown_links_fences.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, '渲染逻辑与 viewport 无关，仅 desktop-chrome 跑一次');
  }
});

test.describe('renderMd 链接 / fence / 表格 / 斜体', () => {
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

  // ===== 1. 链接 href `&` 只转义一次 =====

  test('[text](url) 的 href 中 & 只转义一次', async () => {
    const html = await render('[api](https://x.com/?a=1&b=2)');
    expect(html).toContain('href="https://x.com/?a=1&amp;b=2"');
    expect(html).not.toContain('&amp;amp;');
    expect(html).toMatch(/>api<\/a>/);
  });

  test('源文本里的字面 &amp; 反解后再转义一次仍是 &amp;amp;（不会多解一层）', async () => {
    // 作者写的是四个字符 `&amp;`，浏览器解出 href 应仍是字面 `&amp;`。
    const html = await render('[x](https://x.com/?q=&amp;)');
    expect(html).toContain('href="https://x.com/?q=&amp;amp;"');
  });

  test('&amp;lt; 单次反解不会塌成 <', async () => {
    const html = await render('[x](https://x.com/?q=&lt;)');
    // 源里是字面 `&lt;` → 单次反解只处理 esc 产出的那一层
    expect(html).toContain('href="https://x.com/?q=&amp;lt;"');
    expect(html).not.toContain('href="https://x.com/?q=<');
  });

  test('自动链接的 href 中 & 只转义一次，可见文本保持转义', async () => {
    const html = await render('见 https://x.com/?a=1&b=2 结束');
    expect(html).toContain('href="https://x.com/?a=1&amp;b=2"');
    expect(html).not.toContain('&amp;amp;');
    expect(html).toMatch(/>https:\/\/x\.com\/\?a=1&amp;b=2<\/a> 结束/);
  });

  test('自动链接在 &lt; / &gt; 实体边界处结束', async () => {
    const html = await render('看 <https://x.com/a> 吧');
    expect(html).toContain('href="https://x.com/a"');
    expect(html).toMatch(/<\/a>&gt; 吧/);
  });

  test('javascript: 链接仍被拒绝（安全契约不变）', async () => {
    const html = await render('[click](javascript:alert(1))');
    expect(html).not.toContain('href="javascript');
    expect(html).toContain('click');
  });

  // ===== 2. 自动链接不吞中文标点 =====

  test('自动链接在中文句号处截断', async () => {
    const html = await render('见 https://x.com/a。然后继续');
    expect(html).toContain('href="https://x.com/a"');
    expect(html).toMatch(/<\/a>。然后继续/);
  });

  test('自动链接在全角括号 / 逗号 / 书名号处截断', async () => {
    const h1 = await render('（https://x.com/b）');
    expect(h1).toContain('href="https://x.com/b"');
    expect(h1).toMatch(/<\/a>）/);
    const h2 = await render('访问 https://x.com/c，再说');
    expect(h2).toContain('href="https://x.com/c"');
    const h3 = await render('《https://x.com/d》');
    expect(h3).toContain('href="https://x.com/d"');
  });

  test('URL 路径里的汉字不被截断', async () => {
    const html = await render('看 https://zh.wikipedia.org/wiki/中文 页面');
    expect(html).toContain('href="https://zh.wikipedia.org/wiki/中文"');
  });

  // ===== 3. fence 语言标识 =====

  test('```c++ 保留完整语言名，代码体不残留 ++', async () => {
    const html = await render('```c++\nint x;\n```');
    expect(html).toContain('data-lang="c++"');
    expect(html).toMatch(/<code[^>]*>int x;<\/code>/);
    expect(html).not.toMatch(/<code[^>]*>\+\+/);
  });

  test('fence 信息串只取首词作 lang', async () => {
    const html = await render('```js title=demo.js\nlet a = 1;\n```');
    expect(html).toContain('data-lang="js"');
    expect(html).toMatch(/<code[^>]*>let a = 1;<\/code>/);
  });

  test('fence lang 里的引号 / 尖括号被清洗，不进入 data-lang', async () => {
    const html = await render('```py"><b\nx\n```');
    expect(html).not.toContain('data-lang="py&quot;');
    expect(html).toMatch(/data-lang="py[a-z]*"/);
    expect(html).toMatch(/<code[^>]*>x<\/code>/);
  });

  test('普通 ```python fence 行为不变', async () => {
    const html = await render('```python\nprint(1)\n```');
    expect(html).toContain('data-lang="python"');
    expect(html).toMatch(/<code[^>]*>print\(1\)<\/code>/);
  });

  // ===== 4. 未闭合 fence =====

  test('未闭合 fence：语言行不进代码体，末尾字符不被截', async () => {
    const html = await render('```python\nprint(1)\nfoo');
    expect(html).toMatch(/<code[^>]*>print\(1\)\nfoo<\/code>/);
    expect(html).not.toMatch(/<code[^>]*>python/);
  });

  test('未闭合无语言 fence 保留全部内容', async () => {
    const html = await render('```\nabcdef');
    expect(html).toMatch(/<code[^>]*>abcdef<\/code>/);
  });

  // ===== 5. ![alt](url) =====

  test('![alt](远程 url) 不输出 !，渲染为带 alt 文本的链接', async () => {
    const html = await render('![图片](https://x.com/a.png)');
    expect(html).not.toContain('!');
    expect(html).toContain('href="https://x.com/a.png"');
    expect(html).toMatch(/>图片<\/a>/);
  });

  test('![alt](本地路径) 不输出 !，复用 file-ref <code> 预览路径', async () => {
    const html = await render('![chart](out/chart.png)');
    expect(html).not.toContain('!');
    expect(html).toMatch(/<code[^>]*>out\/chart\.png<\/code>/);
  });

  test('感叹号后有空格再接链接时，感叹号保留', async () => {
    const html = await render('太好了! [x](https://x.com/)');
    expect(html).toContain('太好了! <a');
  });

  // ===== 6. 表格降级 =====

  test('无分隔行的表格降级时逐行 <br>，不用 \\n 塌成一行', async () => {
    const html = await render('| a | b |\n| c | d |');
    expect(html).toMatch(/\| a \| b \|<br>/);
    expect(html).not.toMatch(/\|\n\|/);
  });

  test('单列表格（|---|）也能渲染为 table', async () => {
    const html = await render('| h |\n|---|\n| v |');
    expect(html).toContain('<table');
    expect(html).toContain('<th>h</th>');
    expect(html).toContain('<td>v</td>');
  });

  test('两列表格仍正常渲染', async () => {
    const html = await render('| h1 | h2 |\n|---|---|\n| v1 | v2 |');
    expect(html).toContain('<table');
    expect(html).toContain('<td>v2</td>');
  });

  // ===== 7. 斜体 =====

  test('2 * 3 * 4 不误斜体', async () => {
    const html = await render('2 * 3 * 4');
    expect(html).not.toContain('<em>');
    expect(html).toContain('2 * 3 * 4');
  });

  test('*强调* 仍斜体，**a** 不受影响', async () => {
    const h1 = await render('*强调*');
    expect(h1).toContain('<em>强调</em>');
    const h2 = await render('**a**');
    expect(h2).toContain('<strong>a</strong>');
    expect(h2).not.toContain('<em>');
    const h3 = await render('a *b* and **c** and 1 * 2');
    expect(h3).toContain('<em>b</em>');
    expect(h3).toContain('<strong>c</strong>');
    expect(h3).toContain('1 * 2');
  });
});
