// @ts-check
//
// renderMd 行内数学 / 行内代码 / KaTeX pending 缓存 回归测试（#2428 P2 三项）。
//
//   1. 反引号内 `\(...\)` 被块级预提取当作行内公式，<code> 里出现 KaTeX span
//   2. `$x$` / `$AB$` 单/双字母纯变量不被 isMathInline 放行，原样输出
//   3. `_mdCache` 缓存含 `\begin{env}` 的 KaTeX pending 占位符：首帧 KaTeX
//      未就绪输出 `ktx-N` pending span 被缓存，二次命中返回同一 id 的陈旧 HTML
//
// 本文件屏蔽 cdn.jsdelivr.net，让 KaTeX 永不就绪 → 稳定走 pending 路径。
//
// 跑法：cd test/e2e && npx playwright test markdown_math_code.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, '渲染逻辑与 viewport 无关，仅 desktop-chrome 跑一次');
  }
});

test.describe('renderMd 行内数学 / 行内代码 / pending 缓存', () => {
  /** @type {Awaited<ReturnType<typeof startMockServer>>} */
  let mock;
  /** @type {import('@playwright/test').Page} */
  let page;

  test.beforeAll(async ({ browser }) => {
    mock = await startMockServer();
    const ctx = await browser.newContext();
    // KaTeX 走 CDN 懒加载；屏蔽后 katexReady 永为 false，renderKatex 稳定
    // 产出 `ktx-N` pending span，缓存 bug 才可确定性复现。
    await ctx.route(/cdn\.jsdelivr\.net/, route => route.abort());
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

  // ---- 1. 反引号内 \(...\) ------------------------------------------------

  test('行内代码里的 \\(...\\) 保持为代码，不进 KaTeX', async () => {
    const html = await render('正则 `\\(\\d+\\)` 匹配数字');
    expect(html).toContain('<code class="md-code">\\(\\d+\\)</code>');
    expect(html).not.toMatch(/katex|ktx-/);
  });

  test('行内代码里的 \\(...\\) 含 HTML 时仍按代码转义（无未转义 HTML）', async () => {
    const html = await render('看 `\\(<b>x</b>\\)` 这段');
    expect(html).toContain('<code class="md-code">\\(&lt;b&gt;x&lt;/b&gt;\\)</code>');
    expect(html).not.toContain('<b>');
    expect(html).not.toMatch(/katex|ktx-/);
  });

  test('代码段之外的 \\(...\\) 仍走 KaTeX；同一行内代码不受影响', async () => {
    const html = await render('公式 \\(a+b\\) 与代码 `\\(c\\)` 共存');
    expect(html).toMatch(/<span id="ktx-\d+" class="katex-pending">a\+b<\/span>/);
    expect(html).toContain('<code class="md-code">\\(c\\)</code>');
    expect((html.match(/katex-pending/g) || []).length).toBe(1);
  });

  test('跨行 \\(...\\) 预提取仍有效（不被代码保护误伤）', async () => {
    const html = await render('前 \\(x\n+y\\) 后');
    expect(html).toMatch(/class="katex-pending">x\s*\+y<\/span>/);
  });

  test('fenced code 里的 \\(...\\) 保持原样', async () => {
    const html = await render('```\n\\(\\d+\\)\n```');
    expect(html).toContain('<code>\\(\\d+\\)</code>');
    expect(html).not.toMatch(/katex|ktx-/);
  });

  // ---- 2. $x$ / $AB$ ------------------------------------------------------

  test('$x$ 与 $AB$ 单/双字母变量渲染为公式', async () => {
    const html = await render('设 $x$ 为未知数，线段 $AB$ 长');
    expect(html).toMatch(/class="katex-pending">x<\/span>/);
    expect(html).toMatch(/class="katex-pending">AB<\/span>/);
    expect(html).not.toContain('$x$');
    expect(html).not.toContain('$AB$');
  });

  test('金额 $5 和 $10 / shell $$ / $USD$ 不被误判', async () => {
    const amounts = await render('花了 $5 和 $10 买东西');
    expect(amounts).not.toMatch(/katex|ktx-/);
    expect(amounts).toContain('$5 和 $10');

    const shell = await render('进程号 $$ 与变量 $HOME$PATH');
    expect(shell).not.toMatch(/katex|ktx-/);

    const word = await render('结算 $USD$ 计价');
    expect(word).not.toMatch(/katex|ktx-/);
    expect(word).toContain('$USD$');

    const prose = await render('例子 $(test)$ 不是公式');
    expect(prose).not.toMatch(/katex|ktx-/);
  });

  test('反引号内的 $x$ 仍是代码', async () => {
    const html = await render('shell 变量 `$x$` 原样');
    expect(html).toContain('<code class="md-code">$x$</code>');
    expect(html).not.toMatch(/katex|ktx-/);
  });

  // ---- 3. _mdCache 与 \begin{env} pending -------------------------------

  test('\\begin{env} 块每次渲染都铸新 ktx id（不被 _mdCache 缓存）', async () => {
    const src = '推导：\n\\begin{aligned}a&=b\\\\c&=d\\end{aligned}\n完';
    const first = await render(src);
    const second = await render(src);
    const id1 = first.match(/id="(ktx-\d+)" class="katex-pending"/);
    const id2 = second.match(/id="(ktx-\d+)" class="katex-pending"/);
    expect(id1).not.toBeNull();
    expect(id2).not.toBeNull();
    // 缓存命中会返回同一份 HTML（同一 id），KaTeX 就绪后 katexPending 已删该
    // id，再命中的 bubble 永久显示原始 TeX。
    expect(/** @type {RegExpMatchArray} */ (id2)[1]).not.toBe(/** @type {RegExpMatchArray} */ (id1)[1]);
  });

  test('对照：$$ / \\[ / \\( / ``` 各构造同样不缓存', async () => {
    for (const src of ['$$a=b$$', '\\[a=b\\]', '看 \\(a=b\\) 呀', '```mermaid\ngraph TD\n```']) {
      const first = await render(src);
      const second = await render(src);
      expect(first, src).not.toBe(second);
    }
  });

  test('对照：纯文本仍命中缓存（同一输入返回同一 HTML）', async () => {
    const src = '普通一句话 **粗体** 而已';
    expect(await render(src)).toBe(await render(src));
  });
});
