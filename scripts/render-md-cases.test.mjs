// node --test scripts/render-md-cases.test.mjs
// renderMd's string-in, string-out cases: lists, GFM extensions, links,
// fences, table fallback, emphasis and the file-ref <code> rescue. Each one
// renders a source and reads the HTML, so plain node runs them on the same
// regex engine the browser would. Cases that need a real DOM, CSS, KaTeX,
// mermaid or the CSP stay in test/e2e (markdown_lists, markdown_math_code,
// markdown_csp_render, golden_render). The stubs are render-md.test.mjs's.
import { describe, test } from 'node:test';
import assert from 'node:assert/strict';

globalThis.window = globalThis;
globalThis.MutationObserver = class { observe() {} disconnect() {} };
globalThis.document = {
  documentElement: { dataset: {} },
  head: { appendChild() {} },
  addEventListener() {},
  getElementById: () => null,
  querySelector: () => null,
  querySelectorAll: () => [],
  createElement: () => ({ setAttribute() {} }),
};

const { renderMd } = await import('../internal/server/static/render_md.js');
const { isFileRefCandidate, splitPathLine } = await import('../internal/server/static/file_ref_parse.js');

const render = (src) => renderMd(src);
const count = (html, re) => (html.match(re) || []).length;
const has = (html, s) => assert.ok(html.includes(s), `want ${JSON.stringify(s)} in ${JSON.stringify(html)}`);
const lacks = (html, s) => assert.ok(!html.includes(s), `want no ${JSON.stringify(s)} in ${JSON.stringify(html)}`);

describe('renderMd list 渲染', () => {
  test('源数字被保留为 ol start', () => {
    const html = render('5. 五\n6. 六\n7. 七\n');
    assert.match(html, /<ol[^>]*\bstart="5"/);
    assert.equal(count(html, /<ol/g), 1);
    assert.equal(count(html, /<li>/g), 3);
  });

  test('1. 起始的 ol 不写多余 start 属性', () => {
    const html = render('1. a\n2. b\n');
    assert.match(html, /<ol class="md-ol">/);
    assert.doesNotMatch(html, /start=/);
  });

  test('夹在 ol 中的同深度 ul 不再 mid-段截断 ol', () => {
    // 同深度的 `-` 行嵌进父 li，ol 全程只开一次，而不是每遇 `-` 关 ol、
    // 下一个顶格 `1.` 再开新 ol（"三段都是 1."）。
    const src =
      '1. 必须 source\n' +
      '- 强依赖 VPC_ID\n' +
      '- 这次走 terraform\n' +
      '2. 必须先有 SG\n' +
      '- 这个 SG\n' +
      '3. 脚本同时干两件事\n';
    const html = render(src);
    assert.equal(count(html, /<ol/g), 1);
    const olInner = html.match(/<ol[^>]*>([\s\S]*)<\/ol>/)[1];
    assert.equal(count(olInner, /<li>必须 source|<li>必须先有|<li>脚本/g), 3);
    assert.ok(count(olInner, /<ul/g) >= 1, olInner);
  });

  test('嵌套 list（缩进 3 空格）作为父 li 的子节点', () => {
    const html = render('1. 父项一\n   - 子项 a\n   - 子项 b\n2. 父项二\n');
    assert.match(html, /<ol[^>]*><li>父项一<ul[^>]*><li>子项 a<\/li><li>子项 b<\/li><\/ul><\/li><li>父项二<\/li><\/ol>/);
  });

  test('嵌套 list（tab 缩进）等价于 4 列', () => {
    assert.match(render('1. 父\n\t- 子\n'), /<li>父<ul[^>]*><li>子<\/li><\/ul><\/li>/);
  });

  test('空行不切断同质 list', () => {
    assert.equal(count(render('1. a\n\n2. b\n'), /<ol/g), 1);
  });

  test('空行后非 list 行切断 list', () => {
    assert.match(render('1. a\n\nplain\n'), /<\/ol>.*<div class="md-blank"><\/div>plain/);
  });

  test('lazy continuation 把缩进续行折叠到上一 li', () => {
    assert.match(render('1. 父\n   续行\n2. 兄\n'), /<li>父 续行<\/li><li>兄<\/li>/);
  });

  test('深度上限 6（不爆栈）', () => {
    const indents = Array.from({ length: 7 }, (_, i) => ' '.repeat(i * 2) + '- L' + i).join('\n');
    has(render(indents + '\n'), 'L6');
  });

  test('fence code 不被误识为 list', () => {
    assert.match(render('1. a\n```\n2. fake\n```\n'), /<li>a<\/li><\/ol>.*<div class="md-code-wrap">/s);
  });

  test('list 行后非空行的 table 仍能渲染', () => {
    // 两列是 renderTable 认 table 的下限（|---|---|）。
    const html = render('1. a\nplain\n| h1 | h2 |\n|---|---|\n| v1 | v2 |\n');
    assert.match(html, /<\/ol>/);
    assert.match(html, /<table/);
  });

  test('list item 内 inline markdown（bold/code）正常 token 还原', () => {
    assert.match(render('1. **bold**\n'), /<li><strong>bold<\/strong><\/li>/);
    assert.match(render('1. `code`\n'), /<li><code[^>]*>code<\/code><\/li>/);
  });

  test('headings 仍然关闭 list', () => {
    assert.match(render('1. a\n# H\n'), /<\/ol>.*<strong class="md-h1">/);
  });

  test('CRLF 输入仍能正确识别为 list', () => {
    const html = render('1. a\r\n2. b\r\n');
    assert.match(html, /<ol class="md-ol"><li>a<\/li><li>b<\/li><\/ol>/);
    assert.doesNotMatch(html, /\r/);
  });

  test('CRLF 不会让 ordered list 退化为 br 段', () => {
    const html = render('5. five\r\n6. six\r\n');
    assert.match(html, /<ol[^>]*\bstart="5"/);
    assert.doesNotMatch(html, /<br>/);
  });

  test('cross-kind 空行：两个独立 list 之间保留 md-blank 分隔', () => {
    const html = render('- a\n\n1. b\n');
    assert.match(html, /<ul class="md-ul"><li>a<\/li><\/ul>/);
    assert.match(html, /<ol class="md-ol"><li>b<\/li><\/ol>/);
    assert.match(html, /<\/ul><div class="md-blank"><\/div><ol/);
  });

  test('同 kind 空行仍不切断 list', () => {
    assert.equal(count(render('1. a\n\n2. b\n'), /<ol/g), 1);
  });

  test('lazy continuation 在 lenient promotion 之后仍能折叠 2-cols 续行', () => {
    // lenient 把 ul 推到 depth=1；续行阈值是 top.cols+STEP，不是 (depth+1)*2。
    assert.match(render('1. parent\n- detail\n  cont\n'), /<li>detail cont<\/li>/);
  });

  test('start 数字过长（年份 / 版本号开头段）不被识别为 ol', () => {
    const html = render('2024. 关于新需求\n');
    assert.doesNotMatch(html, /<ol[^>]*start="2024"/);
    assert.doesNotMatch(html, /<ol\b/);
  });

  test('start=0 / start=1 都不写 start 属性（避免 <ol start="0"> 异常）', () => {
    assert.doesNotMatch(render('0. zero\n'), /<ol[^>]*start="0"/);
    assert.doesNotMatch(render('1. one\n'), /start=/);
  });

  test('全空白行（仅空格）等价于空行，不污染 list', () => {
    const html = render('1. a\n   \n2. b\n');
    assert.doesNotMatch(html, /<li>a <\/li>/);
    assert.equal(count(html, /<ol/g), 1);
  });

  test('list 末尾不带 \\n 仍能正确闭合', () => {
    assert.match(render('1. a'), /<ol class="md-ol"><li>a<\/li><\/ol>/);
  });

  test('深度 cap 验证：超过 MAX_LIST_DEPTH 的栈帧不再加深', () => {
    // depth 0..6 各一个 ul，L7、L8 不再加深。
    const src = '- L0\n  - L1\n    - L2\n      - L3\n        - L4\n          - L5\n            - L6\n              - L7\n                - L8\n';
    assert.ok(count(render(src), /<ul\b/g) <= 7);
  });

  test('lenient sibling：多个无序兄弟在父 ol 之下应共享一个 ul', () => {
    const html = render('1. parent\n- detail A\n- detail B\n2. next\n');
    assert.equal(count(html, /<ol\b/g), 1);
    assert.equal(count(html, /<ul\b/g), 1);
    assert.match(html, /<ul[^>]*><li>detail A<\/li><li>detail B<\/li><\/ul>/);
    assert.match(html, /<li>parent<ul[^>]*>.*<\/ul><\/li><li>next<\/li>/);
  });

  test('lenient sibling：多对父项各自含多兄弟', () => {
    const html = render('1. one\n- A\n- B\n2. two\n- X\n- Y\n');
    assert.equal(count(html, /<ol\b/g), 1);
    assert.equal(count(html, /<ul\b/g), 2);
    assert.match(html, /<li>A<\/li><li>B<\/li>/);
    assert.match(html, /<li>X<\/li><li>Y<\/li>/);
  });

  test('lenient sibling：ol 兄弟在父 ul 下也归并为单一 ol', () => {
    const html = render('- a\n1. b\n2. c\n');
    assert.equal(count(html, /<ol\b/g), 1);
    assert.equal(count(html, /<ul\b/g), 1);
    assert.match(html, /<ol[^>]*><li>b<\/li><li>c<\/li><\/ol>/);
  });

  test('缩进 table 不被 lazy-continuation 吞噬', () => {
    const html = render('1. parent\n   | h1 | h2 |\n   |---|---|\n   | a | b |\n');
    assert.match(html, /<\/ol>/);
    assert.match(html, /<table/);
    assert.doesNotMatch(html, /<li>parent[^<]*\| h1/);
  });

  test('缩进 heading 不被 lazy-continuation 吞噬', () => {
    const html = render('1. parent\n   ## sub\n');
    assert.doesNotMatch(html, /<li>parent[^<]*##/);
    assert.match(html, /<\/ol>/);
  });

  test('缩进的 4+ 位数字行不被 lazy-continuation 吞噬', () => {
    // parseListItem 拒绝 "2024."，LIST_SHAPE_RE 仍认出 list 形状，不折进父 li。
    const html = render('1. notes\n   2024. Q1 highlights\n');
    assert.doesNotMatch(html, /<li>notes[^<]*2024/);
    assert.match(html, /2024\. Q1 highlights/);
  });

  test('00100. 起步 ordinal 按数值（=100）接受', () => {
    assert.match(render('00100. installation\n'), /<ol[^>]*\bstart="100"/);
  });

  test('099. 接受 start=99（前导 0 不影响）', () => {
    assert.match(render('099. ninety-nine\n'), /<ol[^>]*\bstart="99"/);
  });

  test('2024. 仍按年份 token 拒绝（数值阈值守恒）', () => {
    const html = render('2024. year\n');
    assert.doesNotMatch(html, /<ol\b/);
    assert.match(html, /2024\. year/);
  });

  test('NBSP/U+00A0 缩进不再误识为缩进 bullet', () => {
    // LIST_ITEM_RE 只认 ASCII 缩进：NBSP 行是普通段落，不把已建的 list 弹空。
    const html = render('1. a\n\u00a0- weird\n');
    assert.match(html, /<ol[^>]*><li>a<\/li><\/ol>/);
    assert.doesNotMatch(html, /<li>weird<\/li>/);
  });
});

describe('renderMd GFM P3 (#2428)', () => {
  test('src/foo.go:42:10 识别为文件引用，line 取 42', () => {
    assert.equal(isFileRefCandidate('src/foo.go:42:10'), true);
    assert.deepEqual(splitPathLine('src/foo.go:42:10'), { path: 'src/foo.go', line: '42' });
  });

  test('裸文件名 foo.go:42:10 识别为文件引用', () => {
    assert.equal(isFileRefCandidate('foo.go:42:10'), true);
    assert.deepEqual(splitPathLine('foo.go:42-50'), { path: 'foo.go', line: '42-50' });
  });

  test('时间 12:30:45 / 三段冒号 a:b:c 不是文件引用', () => {
    assert.equal(isFileRefCandidate('12:30:45'), false);
    assert.equal(isFileRefCandidate('a:b:c'), false);
    assert.equal(isFileRefCandidate('src/foo.go:42:10:3'), false);
  });

  test('[p](src/foo.go:42:10) 本地链接走 file-ref <code> 救援', () => {
    has(render('[p](src/foo.go:42:10)'), '<code class="md-code">src/foo.go:42:10</code>');
  });

  test('连续 > 行合并为一个 blockquote，内容走 esc', () => {
    const html = render('> a <b>x</b>\n> b\nc');
    has(html, '<blockquote class="md-quote">a &lt;b&gt;x&lt;/b&gt;<br>b</blockquote>');
    lacks(html, '<b>x</b>');
    assert.match(html, /<\/blockquote>c<br>$/);
  });

  test('行中 > 不是 blockquote', () => {
    const html = render('a > b');
    lacks(html, '<blockquote');
    has(html, 'a &gt; b');
  });

  test('blockquote 关闭前面的列表', () => {
    has(render('- x\n> q'), '</ul><blockquote class="md-quote">q</blockquote>');
  });

  test('__bold__ 渲染为 <strong>', () => {
    has(render('a __bold__ b'), 'a <strong>bold</strong> b');
  });

  test('snake_case_name / foo__bar__baz 不误伤', () => {
    const html = render('snake_case_name foo__bar__baz');
    lacks(html, '<strong>');
    lacks(html, '<em>');
    has(html, 'snake_case_name foo__bar__baz');
  });

  test('链接目标 / 裸 URL / 本地路径中的 __x__ 不被加粗（F1）', () => {
    const a = render('[src](https://github.com/o/r/blob/main/pkg/__init__.py)');
    has(a, 'href="https://github.com/o/r/blob/main/pkg/__init__.py"');
    lacks(a, '<strong>');
    const b = render('see https://x.com/pkg/__init__.py now');
    has(b, 'href="https://x.com/pkg/__init__.py"');
    has(b, '>https://x.com/pkg/__init__.py</a> now');
    has(render('[init](pkg/__init__.py)'), '<code class="md-code">pkg/__init__.py</code>');
  });

  test('foo.__init__() 保持字面，__all__ = [] 仍加粗', () => {
    has(render('foo.__init__()'), 'foo.__init__()');
    lacks(render('foo.__init__()'), '<strong>');
    has(render('__all__ = []'), '<strong>all</strong> = []');
  });

  // 复杂度闸门，不是性能预算：带 `{1,300}` 正文上限的 inlineMd 在 N=40000
  // 时是几十毫秒，去掉任一上限退化成二次扫描要几秒。别收紧阈值，负载抖动会误报。
  test('__ 最坏输入不二次扫描（F2）', { timeout: 120000 }, () => {
    const CEILING_MS = 2000;
    const N = 40000;
    const inputs = [' __a'.repeat(N), ' __a__b'.repeat(N), ' ~~a'.repeat(N)];
    // 预热让 JIT 升层与首次 GC 不计时；N/10 免得回归时预热本身就耗掉几秒。
    render(' __a ~~a');
    render(' __a ~~a'.repeat(N / 10));
    for (const src of inputs) {
      let best = Infinity;
      for (let k = 0; k < 3; k++) {
        const t0 = performance.now();
        // 超过缓存输入上限的源本就不进 _mdCache，后缀只是保险。
        render(src + String(k));
        const dt = performance.now() - t0;
        best = Math.min(best, dt);
        // 一次低于阈值即证明不是二次扫描；单次超 4 倍阈值已是定论。
        if (best < CEILING_MS || dt >= 4 * CEILING_MS) break;
      }
      assert.ok(best < CEILING_MS,
        `input of ${src.length} chars took ${best.toFixed(1)}ms at best; a quadratic __/~~ scan is seconds at this size`);
    }
  });

  test('~~del~~ 渲染为 <del>', () => {
    has(render('a ~~gone~~ b'), 'a <del>gone</del> b');
  });

  test('~/.config 与孤立 ~~ 不误伤', () => {
    const html = render('~/.config and a ~~ b');
    lacks(html, '<del>');
    has(html, '~/.config and a ~~ b');
  });

  test('URL 中的 a~~b~~c 不被删除线截断（F1）', () => {
    const html = render('see https://x.com/a~~b~~c now');
    has(html, 'href="https://x.com/a~~b~~c"');
    lacks(html, '<del>');
  });

  test('+ 项渲染为 <ul>', () => {
    has(render('+ a\n+ b'), '<ul class="md-ul"><li>a</li><li>b</li></ul>');
  });

  test('1 + 2 / +1 不是列表', () => {
    lacks(render('1 + 2'), '<ul');
    lacks(render('+1 vote'), '<ul');
  });

  test('- [ ] / - [x] 渲染为 disabled checkbox', () => {
    const html = render('- [ ] todo\n- [x] done');
    has(html, '<li><input type="checkbox" class="md-task" disabled> todo</li>');
    has(html, '<li><input type="checkbox" class="md-task" disabled checked> done</li>');
  });

  test('- [link](url) / - [a] b 不是任务框', () => {
    const html = render('- [link](https://x.com)\n- [a] b');
    lacks(html, 'type="checkbox"');
    has(html, '<a href="https://x.com"');
  });

  test('[t](https://x/a_(b)) href 保留括号', () => {
    const html = render('[t](https://x/a_(b))');
    has(html, 'href="https://x/a_(b)"');
    assert.match(html, />t<\/a><br>$/);
  });

  test('(see [t](https://x.com)) 外层括号留在链接外', () => {
    const html = render('(see [t](https://x.com))');
    has(html, 'href="https://x.com"');
    assert.match(html, />t<\/a>\)/);
  });

  test('[t](url "title") href 不含 title，title 进属性', () => {
    const html = render('[t](https://x.com/a "My Title")');
    has(html, 'href="https://x.com/a"');
    has(html, 'title="My Title"');
    assert.match(html, />t<\/a><br>$/);
  });

  test('title 中的 " 与 < 走 escAttr', () => {
    const html = render("[t](https://x.com/a 'a<b')");
    has(html, 'href="https://x.com/a"');
    has(html, 'title="a&lt;b"');
  });

  test('[t]( url ) / [t](url "t" ) 允许两侧空白填充（F3）', () => {
    const a = render('[t]( https://x.com )');
    has(a, 'href="https://x.com"');
    assert.match(a, />t<\/a><br>$/);
    const b = render('[t](https://x.com "tt" )');
    has(b, 'href="https://x.com"');
    has(b, 'title="tt"');
  });

  test('[see https://x.com](https://x.com) 只生成一个 <a>', () => {
    const html = render('[see https://x.com](https://x.com)');
    assert.equal(count(html, /<a /g), 1);
    has(html, '>see https://x.com</a>');
  });

  test('链接外的裸 URL 仍自动链接', () => {
    const html = render('[a](https://x.com) and https://y.com done');
    assert.equal(count(html, /<a /g), 2);
    has(html, 'href="https://y.com"');
  });
});

describe('renderMd 链接 / fence / 表格 / 斜体', () => {
  test('[text](url) 的 href 中 & 只转义一次', () => {
    const html = render('[api](https://x.com/?a=1&b=2)');
    has(html, 'href="https://x.com/?a=1&amp;b=2"');
    lacks(html, '&amp;amp;');
    assert.match(html, />api<\/a>/);
  });

  test('源文本里的字面 &amp; 反解后再转义一次仍是 &amp;amp;（不会多解一层）', () => {
    // 作者写的是四个字符 `&amp;`，浏览器解出的 href 应仍是字面 `&amp;`。
    has(render('[x](https://x.com/?q=&amp;)'), 'href="https://x.com/?q=&amp;amp;"');
  });

  test('&amp;lt; 单次反解不会塌成 <', () => {
    const html = render('[x](https://x.com/?q=&lt;)');
    has(html, 'href="https://x.com/?q=&amp;lt;"');
    lacks(html, 'href="https://x.com/?q=<');
  });

  test('自动链接的 href 中 & 只转义一次，可见文本保持转义', () => {
    const html = render('见 https://x.com/?a=1&b=2 结束');
    has(html, 'href="https://x.com/?a=1&amp;b=2"');
    lacks(html, '&amp;amp;');
    assert.match(html, />https:\/\/x\.com\/\?a=1&amp;b=2<\/a> 结束/);
  });

  test('自动链接在 &lt; / &gt; 实体边界处结束', () => {
    const html = render('看 <https://x.com/a> 吧');
    has(html, 'href="https://x.com/a"');
    assert.match(html, /<\/a>&gt; 吧/);
  });

  test('javascript: 链接仍被拒绝（安全契约不变）', () => {
    const html = render('[click](javascript:alert(1))');
    lacks(html, 'href="javascript');
    has(html, 'click');
  });

  test('自动链接在中文句号处截断', () => {
    const html = render('见 https://x.com/a。然后继续');
    has(html, 'href="https://x.com/a"');
    assert.match(html, /<\/a>。然后继续/);
  });

  test('自动链接在全角括号 / 逗号 / 书名号处截断', () => {
    const h1 = render('（https://x.com/b）');
    has(h1, 'href="https://x.com/b"');
    assert.match(h1, /<\/a>）/);
    has(render('访问 https://x.com/c，再说'), 'href="https://x.com/c"');
    has(render('《https://x.com/d》'), 'href="https://x.com/d"');
  });

  test('URL 路径里的 〇 / 々 / 半角片假名不被截断（日文路径）', () => {
    const html = render('見る https://ja.example.org/wiki/〇々ｶﾅ ページ');
    has(html, 'href="https://ja.example.org/wiki/〇々ｶﾅ"');
    assert.match(html, /<\/a> ページ/);
  });

  test('URL 路径里的汉字不被截断', () => {
    has(render('看 https://zh.wikipedia.org/wiki/中文 页面'), 'href="https://zh.wikipedia.org/wiki/中文"');
  });

  test('```c++ 保留完整语言名，代码体不残留 ++', () => {
    const html = render('```c++\nint x;\n```');
    has(html, 'data-lang="c++"');
    assert.match(html, /<code[^>]*>int x;<\/code>/);
    assert.doesNotMatch(html, /<code[^>]*>\+\+/);
  });

  test('fence 信息串只取首词作 lang', () => {
    const html = render('```js title=demo.js\nlet a = 1;\n```');
    has(html, 'data-lang="js"');
    assert.match(html, /<code[^>]*>let a = 1;<\/code>/);
  });

  test('fence lang 里的引号 / 尖括号被清洗，不进入 data-lang', () => {
    const html = render('```py"><b\nx\n```');
    lacks(html, 'data-lang="py&quot;');
    assert.match(html, /data-lang="py[a-z]*"/);
    assert.match(html, /<code[^>]*>x<\/code>/);
  });

  test('单行 fence ```ls -la``` 内容整体作为代码保留', () => {
    const html = render('```ls -la```');
    assert.match(html, /<code[^>]*>ls -la<\/code>/);
    lacks(html, 'data-lang');
  });

  test('```python:main.py 的 lang 在 : 处截断', () => {
    const html = render('```python:main.py\nx = 1\n```');
    has(html, 'data-lang="python"');
    assert.match(html, /<code[^>]*>x = 1<\/code>/);
  });

  test('普通 ```python fence 行为不变', () => {
    const html = render('```python\nprint(1)\n```');
    has(html, 'data-lang="python"');
    assert.match(html, /<code[^>]*>print\(1\)<\/code>/);
  });

  test('未闭合 fence：语言行不进代码体，末尾字符不被截', () => {
    const html = render('```python\nprint(1)\nfoo');
    assert.match(html, /<code[^>]*>print\(1\)\nfoo<\/code>/);
    assert.doesNotMatch(html, /<code[^>]*>python/);
  });

  test('未闭合无语言 fence 保留全部内容', () => {
    assert.match(render('```\nabcdef'), /<code[^>]*>abcdef<\/code>/);
  });

  test('![alt](远程 url) 不输出 !，渲染为带 alt 文本的链接', () => {
    const html = render('![图片](https://x.com/a.png)');
    lacks(html, '!');
    has(html, 'href="https://x.com/a.png"');
    assert.match(html, />图片<\/a>/);
  });

  test('![alt](本地路径) 不输出 !，复用 file-ref <code> 预览路径', () => {
    const html = render('![chart](out/chart.png)');
    lacks(html, '!');
    assert.match(html, /<code[^>]*>out\/chart\.png<\/code>/);
  });

  test('感叹号后有空格再接链接时，感叹号保留', () => {
    has(render('太好了! [x](https://x.com/)'), '太好了! <a');
  });

  test('无分隔行的表格降级时逐行 <br>，不用 \\n 塌成一行', () => {
    const html = render('| a | b |\n| c | d |');
    assert.match(html, /\| a \| b \|<br>/);
    assert.doesNotMatch(html, /\|\n\|/);
  });

  test('单列表格（|---|）也能渲染为 table', () => {
    const html = render('| h |\n|---|\n| v |');
    has(html, '<table');
    has(html, '<th>h</th>');
    has(html, '<td>v</td>');
  });

  test('两列表格仍正常渲染', () => {
    const html = render('| h1 | h2 |\n|---|---|\n| v1 | v2 |');
    has(html, '<table');
    has(html, '<td>v2</td>');
  });

  test('2 * 3 * 4 不误斜体', () => {
    const html = render('2 * 3 * 4');
    lacks(html, '<em>');
    has(html, '2 * 3 * 4');
  });

  test('*强调* 仍斜体，**a** 不受影响', () => {
    has(render('*强调*'), '<em>强调</em>');
    const h2 = render('**a**');
    has(h2, '<strong>a</strong>');
    lacks(h2, '<em>');
    const h3 = render('a *b* and **c** and 1 * 2');
    has(h3, '<em>b</em>');
    has(h3, '<strong>c</strong>');
    has(h3, '1 * 2');
  });
});

// parseHtml builds a tree from renderMd output and throws on anything a
// well-formed render cannot contain: a raw `<`, a stray or mismatched close
// tag, an unclosed element. Text nodes hold decoded text.
const VOID = new Set(['br', 'input']);
const ENTITIES = { amp: '&', lt: '<', gt: '>', quot: '"', '#39': "'" };
const decode = (s) => s.replace(/&(amp|lt|gt|quot|#39);/g, (_, e) => ENTITIES[e]);
function parseHtml(html) {
  const root = { tag: '#root', attrs: {}, children: [] };
  const stack = [root];
  const re = /<(\/?)([a-z][a-z0-9]*)((?:\s+[a-z][a-z-]*(?:="[^"]*")?)*)\s*>|([^<]+)|</g;
  for (const m of html.matchAll(re)) {
    const top = stack[stack.length - 1];
    if (m[4] !== undefined) { top.children.push({ text: decode(m[4]) }); continue; }
    if (m[2] === undefined) throw new Error('raw < at ' + m.index + ' in ' + JSON.stringify(html));
    if (m[1]) {
      if (top.tag !== m[2]) throw new Error(`</${m[2]}> closes <${top.tag}> in ${JSON.stringify(html)}`);
      stack.pop();
      continue;
    }
    const attrs = {};
    for (const a of m[3].matchAll(/\s+([a-z][a-z-]*)(?:="([^"]*)")?/g)) attrs[a[1]] = decode(a[2] ?? '');
    const el = { tag: m[2], attrs, children: [] };
    top.children.push(el);
    if (!VOID.has(el.tag)) stack.push(el);
  }
  if (stack.length !== 1) throw new Error(`<${stack[stack.length - 1].tag}> left open in ${JSON.stringify(html)}`);
  return root;
}
const textOf = (n) => n.text ?? n.children.map(textOf).join('');
const escText = (s) => s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
const innerOf = (n) => n.children.map((c) => c.text !== undefined ? escText(c.text) : `<${c.tag}>${innerOf(c)}</${c.tag}>`).join('');
function* walk(n) {
  for (const c of n.children ?? []) {
    if (c.tag) { yield [c, n]; yield* walk(c); }
  }
}
const elements = (root) => [...walk(root)];

// shape renders src and reports the parts of the tree these cases read.
function shape(src) {
  const html = render(src);
  const root = parseHtml(html);
  const els = elements(root);
  const codes = els.filter(([e]) => e.tag === 'code').map(([e]) => e);
  return {
    html,
    text: textOf(root),
    codes: codes.map((c) => ({ cls: c.attrs.class ?? '', text: textOf(c), inner: innerOf(c) })),
    nestedInCode: codes.reduce((n, c) => n + elements(c).length, 0),
    anchors: els.filter(([e]) => e.tag === 'a').map(([e]) => ({ href: e.attrs.href ?? null, cls: e.attrs.class ?? '' })),
    pathlines: els
      .filter(([e, p]) => e.tag === 'code' && (p.attrs.class ?? '').split(' ').includes('md-pathline'))
      .map(([e]) => ({ cls: e.attrs.class ?? '', text: textOf(e) })),
  };
}

describe('renderMd file-ref <code>', () => {
  // A path-shaped markdown link target (safeUrl rejects it) is rescued as
  // inline <code class="md-code"> so the file-ref scanner gives it buttons.
  // Earlier inlineMd passes may have left <strong>/<em> or \x00 sentinels in
  // the target: the link pattern refuses a sentinel, the rescue refuses a
  // tag and slash-shaped non-files. Backtick spans and fenced path-list rows
  // share the <code> helper; each caller escapes once and it does not again.
  test('本地文件链接渲染为 <code class="md-code">，显示路径本身', () => {
    const r = shape('见 [数学/专题/foo.html](数学/专题/foo.html)');
    assert.deepEqual(r.codes, [{ cls: 'md-code', text: '数学/专题/foo.html', inner: '数学/专题/foo.html' }]);
    assert.deepEqual(r.anchors, []);
  });

  test('http 链接仍渲染为 <a class="md-link">，不走救援', () => {
    const r = shape('[site](https://example.com/a.html)');
    assert.deepEqual(r.anchors, [{ href: 'https://example.com/a.html', cls: 'md-link' }]);
    assert.deepEqual(r.codes, []);
  });

  test('目标里被粗体 pass 插入的 <strong> 不会进入 <code>', () => {
    const r = shape('[x](a**b**/c.html)');
    assert.deepEqual(r.codes, []);
    has(r.text, 'x');
  });

  test('目标里的反引号 sentinel 不会在 <code> 里还原成嵌套 <code>', () => {
    const r = shape('[x](`a`/b.html)');
    assert.equal(r.nestedInCode, 0);
    lacks(r.html, '\x00');
    assert.deepEqual(r.codes.filter((c) => c.text.includes('b.html')), []);
  });

  for (const [name, src, label] of [
    ['日期', '[发布日](2024/01/02)', '发布日'],
    ['分数', '[一半](1/2)', '一半'],
    ['无扩展名的 slug', '[文档](docs/guide)', '文档'],
  ]) {
    test(`斜杠形状但不是文件（${name}）：保留作者的标签，不变成 <code>`, () => {
      const r = shape(src);
      assert.deepEqual(r.codes, []);
      has(r.text, label);
    });
  }

  test('反引号路径走同一个 <code class="md-code">', () => {
    const r = shape('打开 `docs/a.md`');
    assert.deepEqual(r.codes, [{ cls: 'md-code', text: 'docs/a.md', inner: 'docs/a.md' }]);
  });

  test('反引号内容只转义一次', () => {
    const r = shape('`a<b>&c.md`');
    assert.deepEqual(r.codes.map((c) => c.text), ['a<b>&c.md']);
    assert.equal(r.codes[0].inner, 'a&lt;b&gt;&amp;c.md');
  });

  test('路径列表 fence 的每行是无 class 的 <code>，路径只转义一次', () => {
    const r = shape('```\nsrc/a.go\nweb/<x>.html\n```');
    assert.deepEqual(r.pathlines, [
      { cls: '', text: 'src/a.go' },
      { cls: '', text: 'web/<x>.html' },
    ]);
  });
});
