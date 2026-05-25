// @ts-check
//
// renderMd list 渲染回归测试。
//
// 触发场景（dashboard 截图里的"三段都是 1."）：
//   - ordered list 的源数字被丢弃 → CSS decimal 强制从 1 重新计数
//   - 每一段无序列表都把上一段 ordered list 截断
//   - 不支持嵌套
//
// 跑法：cd test/e2e && npx playwright test markdown_lists.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, '渲染逻辑与 viewport 无关，仅 desktop-chrome 跑一次');
  }
});

test.describe('renderMd list 渲染', () => {
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

  test('源数字被保留为 ol start', async () => {
    const html = await render('5. 五\n6. 六\n7. 七\n');
    expect(html).toMatch(/<ol[^>]*\bstart="5"/);
    // 三个项都在同一 ol 里
    expect((html.match(/<ol/g) || []).length).toBe(1);
    expect((html.match(/<li>/g) || []).length).toBe(3);
  });

  test('1. 起始的 ol 不写多余 start 属性', async () => {
    const html = await render('1. a\n2. b\n');
    expect(html).toMatch(/<ol class="md-ol">/);
    expect(html).not.toMatch(/start=/);
  });

  test('夹在 ol 中的同深度 ul 不再 mid-段截断 ol', async () => {
    // 不带段间空行的混排：旧实现每遇到 `-` 行立即关 ol、开 ul；下一个
    // 顶格 `1.` 又开新 ol → 截图里"全是 1." 的根因。新实现把 ul 嵌套
    // 进父 li，ol 全程只开一次。
    const src =
      '1. 必须 source\n' +
      '- 强依赖 VPC_ID\n' +
      '- 这次走 terraform\n' +
      '2. 必须先有 SG\n' +
      '- 这个 SG\n' +
      '3. 脚本同时干两件事\n';
    const html = await render(src);
    // 同段内 ol 只开一次（旧实现会切成 3 个）
    expect((html.match(/<ol/g) || []).length).toBe(1);
    // 三个 ol 父项 + 中间嵌套的 ul
    const olInner = html.match(/<ol[^>]*>([\s\S]*)<\/ol>/)[1];
    expect((olInner.match(/<li>必须 source|<li>必须先有|<li>脚本/g) || []).length).toBe(3);
    expect((olInner.match(/<ul/g) || []).length).toBeGreaterThanOrEqual(1);
  });

  test('嵌套 list（缩进 3 空格）作为父 li 的子节点', async () => {
    const src = '1. 父项一\n   - 子项 a\n   - 子项 b\n2. 父项二\n';
    const html = await render(src);
    // 父 ol 内：父 li 的内部包含 ul + 两个 li，然后 ol 内再有第二个 li
    expect(html).toMatch(/<ol[^>]*><li>父项一<ul[^>]*><li>子项 a<\/li><li>子项 b<\/li><\/ul><\/li><li>父项二<\/li><\/ol>/);
  });

  test('嵌套 list（tab 缩进）等价于 4 列', async () => {
    const src = '1. 父\n\t- 子\n';
    const html = await render(src);
    expect(html).toMatch(/<li>父<ul[^>]*><li>子<\/li><\/ul><\/li>/);
  });

  test('空行不切断同质 list', async () => {
    const html = await render('1. a\n\n2. b\n');
    expect((html.match(/<ol/g) || []).length).toBe(1);
  });

  test('空行后非 list 行切断 list', async () => {
    const html = await render('1. a\n\nplain\n');
    expect(html).toMatch(/<\/ol>.*<div class="md-blank"><\/div>plain/);
  });

  test('lazy continuation 把缩进续行折叠到上一 li', async () => {
    const html = await render('1. 父\n   续行\n2. 兄\n');
    expect(html).toMatch(/<li>父 续行<\/li><li>兄<\/li>/);
  });

  test('深度上限 6（不爆栈）', async () => {
    // 14 层缩进：14 / 2 = 7，应被截到 6
    const indents = Array.from({ length: 7 }, (_, i) => ' '.repeat(i * 2) + '- L' + i).join('\n');
    const html = await render(indents + '\n');
    // 只要 render 完成且没抛错就算 pass；顺便检查最后一层不再加深
    expect(html).toContain('L6');
  });

  test('fence code 不被误识为 list', async () => {
    const src = '1. a\n```\n2. fake\n```\n';
    const html = await render(src);
    expect(html).toMatch(/<li>a<\/li><\/ol>.*<div class="md-code-wrap">/s);
  });

  test('list 行后非空行的 table 仍能渲染', async () => {
    // 注：空行后的 table 检测在旧实现里就走不到（已有局限，与本 PR 无关）。
    // 这里测"无空行衔接"的情形，确认 list 关闭后 table 路径仍触发。
    // renderTable 要求至少两列（|---|---|）才算合法 table，所以 fixture 给两列。
    const src = '1. a\nplain\n| h1 | h2 |\n|---|---|\n| v1 | v2 |\n';
    const html = await render(src);
    expect(html).toMatch(/<\/ol>/);
    expect(html).toMatch(/<table/);
  });

  test('list item 内 inline markdown（bold/code）正常 token 还原', async () => {
    const html1 = await render('1. **bold**\n');
    expect(html1).toMatch(/<li><strong>bold<\/strong><\/li>/);
    const html2 = await render('1. `code`\n');
    expect(html2).toMatch(/<li><code[^>]*>code<\/code><\/li>/);
  });

  test('headings 仍然关闭 list', async () => {
    const html = await render('1. a\n# H\n');
    expect(html).toMatch(/<\/ol>.*<strong class="md-h1">/);
  });

  // ===== Code-review fix-up coverage =====

  test('CRLF 输入仍能正确识别为 list', async () => {
    // Windows-paste / IM 文本会保留 \r。renderMd 入口的 CRLF→LF 归一化
    // 必须吃掉 \r，否则 LIST_ITEM_RE 全段 miss → list 完全不渲染。
    const html = await render('1. a\r\n2. b\r\n');
    expect(html).toMatch(/<ol class="md-ol"><li>a<\/li><li>b<\/li><\/ol>/);
    expect(html).not.toMatch(/\r/);
  });

  test('CRLF 不会让 ordered list 退化为 br 段', async () => {
    const html = await render('5. five\r\n6. six\r\n');
    expect(html).toMatch(/<ol[^>]*\bstart="5"/);
    expect(html).not.toMatch(/<br>/);
  });

  test('cross-kind 空行：两个独立 list 之间保留 md-blank 分隔', async () => {
    // 用户截图回归：- a + 空行 + 1. b 旧实现产 ul + md-blank + ol，
    // 第一版修复误把它嵌套；现在恢复"同 kind 才续接"语义。
    const html = await render('- a\n\n1. b\n');
    expect(html).toMatch(/<ul class="md-ul"><li>a<\/li><\/ul>/);
    expect(html).toMatch(/<ol class="md-ol"><li>b<\/li><\/ol>/);
    expect(html).toMatch(/<\/ul><div class="md-blank"><\/div><ol/);
  });

  test('同 kind 空行仍不切断 list', async () => {
    const html = await render('1. a\n\n2. b\n');
    expect((html.match(/<ol/g) || []).length).toBe(1);
  });

  test('lazy continuation 在 lenient promotion 之后仍能折叠 2-cols 续行', async () => {
    // 1. parent + - detail（lenient 把 ul 推到 depth=1）+ 2-col 缩进续行 cont。
    // 阈值原本用 (top.depth+1)*2=4，永远不命中；改用 top.cols+STEP 后命中。
    const html = await render('1. parent\n- detail\n  cont\n');
    expect(html).toMatch(/<li>detail cont<\/li>/);
  });

  test('start 数字过长（年份 / 版本号开头段）不被识别为 ol', async () => {
    // "2024. 关于新需求" 不应渲染为 <ol start="2024">；走普通行 + br 即可。
    const html = await render('2024. 关于新需求\n');
    expect(html).not.toMatch(/<ol[^>]*start="2024"/);
    expect(html).not.toMatch(/<ol\b/);
  });

  test('start=0 / start=1 都不写 start 属性（避免 <ol start="0"> 异常）', async () => {
    const h0 = await render('0. zero\n');
    expect(h0).not.toMatch(/<ol[^>]*start="0"/);
    const h1 = await render('1. one\n');
    expect(h1).not.toMatch(/start=/);
  });

  test('全空白行（仅空格）等价于空行，不污染 list', async () => {
    const html = await render('1. a\n   \n2. b\n');
    // 中间不该产生空格污染 li 内容
    expect(html).not.toMatch(/<li>a <\/li>/);
    expect((html.match(/<ol/g) || []).length).toBe(1);
  });

  test('CSS：嵌套 list 在 .crs-text.md 容器下拿到紧凑 16px 缩进', async () => {
    const m = await page.evaluate(() => {
      const w = /** @type {any} */ (window);
      const html = w.renderMd('1. 父项\n   - 子项\n');
      const host = document.createElement('div');
      host.className = 'crs-text md';
      host.innerHTML = html;
      document.body.appendChild(host);
      const innerUl = host.querySelector('.md-ol > li > .md-ul');
      const cs = innerUl ? getComputedStyle(innerUl) : null;
      const result = {
        found: !!innerUl,
        marginLeft: cs ? cs.marginLeft : null,
        listStyle: cs ? cs.listStyleType : null,
      };
      host.remove();
      return result;
    });
    expect(m.found).toBe(true);
    // 期望 16px（PR 紧凑规则），而非 22px（.crs-text.md ul 的覆盖）
    expect(m.marginLeft).toBe('16px');
    expect(m.listStyle).toBe('circle');
  });

  test('list 末尾不带 \\n 仍能正确闭合', async () => {
    const html = await render('1. a');
    expect(html).toMatch(/<ol class="md-ol"><li>a<\/li><\/ol>/);
  });

  test('深度 cap 验证：超过 MAX_LIST_DEPTH 的栈帧不再加深', async () => {
    // 14 列 → depth=7 被截到 6；再深的 list item 不会让栈再长。
    const src = '- L0\n  - L1\n    - L2\n      - L3\n        - L4\n          - L5\n            - L6\n              - L7\n                - L8\n';
    const html = await render(src);
    // 实际 ul 嵌套层数 ≤ MAX_LIST_DEPTH+1 = 7（depth 0..6，每层一个 ul）
    const ulCount = (html.match(/<ul\b/g) || []).length;
    expect(ulCount).toBeLessThanOrEqual(7);
  });
});
