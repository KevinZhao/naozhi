// node --test scripts/render-md.test.mjs
// render_md.js escapes the source before it builds any tag, then swaps
// placeholders (\x00CODE<n>\x00, \x00KTX<n>\x00, \x00MEM<n>\x00, \x00ILM<n>\x00, the table's
// \x00G<n>\x00 / \x00PIPE\x00) back in. These tests run a hostile corpus and a
// seeded fuzz loop through every entry point and check the output against a
// tag and attribute allowlist: an author-supplied tag, an on* attribute, a
// placeholder restored into an attribute value or a leaked \x00 all fail it.
// KaTeX and mermaid are never loaded here, so math and diagrams stay in their
// pending shapes, which are what the renderer itself emits.
import { test } from 'node:test';
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

const { inlineMd, renderMd, renderRich, renderTable } = await import('../internal/server/static/render_md.js');

const TAGS = new Set([
  'a', 'blockquote', 'br', 'button', 'code', 'del', 'div', 'em', 'input', 'li',
  'ol', 'pre', 'span', 'strong', 'table', 'tbody', 'td', 'th', 'thead', 'tr', 'ul',
]);
// Attribute name → value test. Values are checked after the tag regex has
// already refused `"`, `<`, `>` and \x00 inside them.
const ATTRS = {
  'class': v => /^[a-z0-9 _-]+$/.test(v),
  'id': v => /^(?:ktx|mmd)-\d+$/.test(v),
  'href': v => v === '#' || /^https?:\/\//i.test(v),
  'title': () => true,
  'target': v => v === '_blank',
  'rel': v => v === 'noopener noreferrer',
  'data-slug': v => /^[a-zA-Z0-9_-]{1,64}$/.test(v),
  'data-type': v => /^[a-z]+$/.test(v),
  'data-lang': v => /^[\w+#.-]*$/.test(v),
  'data-action': v => v === 'code-copy',
  'tabindex': v => v === '0',
  'role': v => v === 'link',
  'aria-label': () => true,
  'aria-hidden': v => v === 'true',
  'type': v => v === 'checkbox' || v === 'button',
  'start': v => /^\d{1,3}$/.test(v),
  'disabled': v => v === undefined,
  'checked': v => v === undefined,
};
const TAG_RE = /^<(\/?)([a-z][a-z0-9]*)((?:\s+[a-z][a-z-]*(?:="[^"<>\x00]*")?)*)\s*>/;
const ATTR_RE = /\s+([a-z][a-z-]*)(?:="([^"]*)")?/g;

// unsafeReason returns why html breaks the allowlist, or '' when it holds.
function unsafeReason(html) {
  let i = 0;
  while (i < html.length) {
    const lt = html.indexOf('<', i);
    const text = html.slice(i, lt === -1 ? html.length : lt);
    if (text.indexOf('>') !== -1) return 'raw > in text: ' + JSON.stringify(text);
    if (text.indexOf('\x00') !== -1) return 'leaked placeholder in text: ' + JSON.stringify(text);
    if (lt === -1) break;
    const m = TAG_RE.exec(html.slice(lt));
    if (!m) return 'unparseable tag at ' + JSON.stringify(html.slice(lt, lt + 80));
    const [whole, closing, name, attrs] = m;
    if (!TAGS.has(name)) return 'tag <' + name + '> not allowed';
    if (closing && attrs) return 'closing tag with attributes: ' + whole;
    for (const [, attr, value] of attrs.matchAll(ATTR_RE)) {
      const check = ATTRS[attr];
      if (!check) return 'attribute ' + attr + ' not allowed in ' + whole;
      if (!check(value)) return 'attribute ' + attr + '=' + JSON.stringify(value) + ' rejected in ' + whole;
    }
    i = lt + whole.length;
  }
  return '';
}

function assertSafe(html, input) {
  const why = unsafeReason(html);
  assert.equal(why, '', 'input ' + JSON.stringify(input) + '\noutput ' + html);
}

// renderAll runs one input through every production entry point.
function renderAll(s) {
  return [
    renderMd(s),
    renderRich(s),
    renderRich(s, { mode: 'plain' }),
    renderRich(s, { mode: 'tex' }),
  ];
}

test('the checker rejects the shapes it exists to catch', () => {
  assert.notEqual(unsafeReason('<img src=x onerror=alert(1)>'), '');
  assert.notEqual(unsafeReason('<a href="javascript:alert(1)">x</a>'), '');
  assert.notEqual(unsafeReason('<a href="https://e.com/<code class="md-code">x</code>">y</a>'), '');
  assert.notEqual(unsafeReason('<span onclick="x">y</span>'), '');
  assert.notEqual(unsafeReason('a \x00CODE0\x00 b'), '');
  assert.notEqual(unsafeReason('a " onmouseover=x "> b'), '');
  assert.equal(unsafeReason('<a href="https://e.com" class="md-link" target="_blank" rel="noopener noreferrer">y</a><br>'), '');
});

const PAYLOADS = [
  '<script>alert(1)</script>',
  '<img src=x onerror=alert(1)>',
  '<svg onload=alert(1)>',
  '"><img src=x onerror=alert(1)>',
  '\'><svg/onload=alert(1)>',
  '<iframe src="javascript:alert(1)"></iframe>',
  '<a href="javascript:alert(1)">x</a>',
  '<IMG SRC=JaVaScRiPt:alert(1)>',
  '<img src=&#106;&#97;&#118;&#97;&#115;&#99;&#114;&#105;&#112;&#116;&#58;alert(1)>',
  '<div style="background:url(javascript:alert(1))">',
  '&lt;script&gt;alert(1)&lt;/script&gt;',
  '<<script>script>alert(1)<</script>/script>',
  '<style>@import "x"</style>',
];
const LINKS = [
  '[x](javascript:alert(1))',
  '[x](JaVaScRiPt:alert(1))',
  '[x](javascript&#58;alert(1))',
  '[x](&#106;avascript:alert(1))',
  '[x](data:text/html;base64,PHNjcmlwdD4=)',
  '[x](vbscript:msgbox(1))',
  '[x](https://e.com" onmouseover="alert(1))',
  '[x](https://e.com "t\\" onmouseover=\\"x")',
  '[x](https://e.com \'a" onclick="b\')',
  '[<img src=x onerror=1>](https://e.com)',
  '[x](https://e.com/<img>)',
  '![x](https://e.com/x.png" onerror="alert(1))',
  'https://e.com/"onmouseover="alert(1)',
  'https://e.com/\'><svg onload=1>',
];
// Placeholder shapes, forged directly and produced by legitimate markup that
// lands inside a link destination, a title or an autolink.
const PLACEHOLDERS = [
  'see `a"b` and [x](https://e.com/\x00CODE0\x00)',
  '$x+1$ [y](https://e.com/\x00KTX0\x00)',
  '\\(a+1\\) [y](https://e.com/\x00ILM0\x00)',
  '\\(a+1\\) \x00ILM0\x00 \x00ILM9\x00',
  'hi \x00CODE7\x00 there `x` \x00KTX3\x00',
  '| a | b |\n|---|---|\n| `c` | [y](https://e.com/\x00G0\x00) |',
  '| a | b |\n|---|---|\n| \x00G9\x00 | \x00PIPE\x00 |',
  '[y](https://e.com/`a`)',
  '[y](https://e.com/`a" onclick="x`)',
  '[y](https://e.com/$x+1$)',
  '[y](https://e.com/\\(a+1\\))',
  '[y](https://e.com/a "t`c`")',
  '[y](https://e.com/a "t$x+1$")',
  'https://e.com/`a"b`',
  'https://e.com/$x+1$',
  'https://e.com/\\(a+1\\)',
  '[[__x__]]',
  '[[a__b__c]]',
  '[[a]] \x00MEM0\x00 \x00MEM9\x00',
  '[x](https://e.com/[[a]])',
  '[[**x**]] [[_x_]]',
];

test('the hostile corpus renders inside the allowlist in every context', () => {
  const wrap = p => [
    p,
    '**' + p + '**',
    '*' + p + '*',
    '~~' + p + '~~',
    '# ' + p,
    '- ' + p,
    '1. ' + p,
    '- [x] ' + p,
    '> ' + p,
    '| ' + p + ' | b |\n|---|---|\n| c | ' + p + ' |',
    '[' + p + '](https://e.com)',
    '[x](https://e.com "' + p + '")',
    '`' + p + '`',
    '```\n' + p + '\n```',
    '```js\n' + p + '\n```',
    '```' + p + '\n' + p + '\n```',
    '```mermaid\n' + p + '\n```',
    '```math\n' + p + '\n```',
    '```\nsrc/a.go\n' + p + '\n```',
    '$' + p + '$',
    '$$' + p + '$$',
    '\\(' + p + '\\)',
    '\\[' + p + '\\]',
    '\\begin{aligned}' + p + '\\end{aligned}',
    '[[slug_' + p + ']]',
    'text https://e.com/' + p,
  ];
  for (const p of [...PAYLOADS, ...LINKS, ...PLACEHOLDERS]) {
    for (const input of wrap(p)) {
      for (const html of renderAll(input)) assertSafe(html, input);
    }
  }
});

test('U+0000 in the source cannot forge a placeholder', () => {
  const out = renderMd('see `a"b` and [x](https://e.com/\x00CODE0\x00)');
  assert.ok(!out.includes('href="https://e.com/<'), out);
  assert.ok(out.includes('\ufffd'), 'U+0000 becomes U+FFFD: ' + out);
  const ktx = renderMd('$x+1$ [y](https://e.com/\x00KTX0\x00)');
  assert.ok(!ktx.includes('href="https://e.com/<'), ktx);
});

// inlineMd and renderTable take source that renderMd has already cleared of
// NUL, so only a direct call can hand them a forged index.
test('an unknown placeholder index restores to nothing, never "undefined"', () => {
  const outs = [
    inlineMd('hi \x00CODE7\x00 `x`'),
    inlineMd('hi \x00KTX7\x00 $x+1$'),
    inlineMd('hi \x00MEM7\x00 [[a]]'),
    renderTable(['| `a` | \x00G7\x00 |', '|---|---|', '| c | d |']),
    renderMd('\\(a\\) \x00ILM7\x00'),
  ];
  for (const out of outs) {
    assert.ok(!out.includes('undefined'), out);
    assert.ok(!/CODE7|KTX7|MEM7|G7|ILM7/.test(out) || out.includes('\ufffd'), out);
  }
});

// The link passes run while code and math are still placeholders; a link
// whose destination or title holds one is left as text, so the code or math
// renders as an element after it and never inside href or title.
test('code or math inside a link destination or title renders outside the attribute', () => {
  const cases = [
    ['[y](https://e.com/`a`)', '<code class="md-code">a</code>'],
    ['[y](https://e.com/$x+1$)', 'class="katex-pending">x+1</span>'],
    ['[y](https://e.com/\\(a+1\\))', 'class="katex-pending">a+1</span>'],
    ['[y](https://e.com/a "t`c`")', '<code class="md-code">c</code>'],
    ['https://e.com/`a"b`', '<code class="md-code">a"b</code>'],
  ];
  for (const [s, element] of cases) {
    const out = renderMd(s);
    assertSafe(out, s);
    assert.ok(out.includes(element), JSON.stringify(s) + ' => ' + out);
  }
  assert.match(renderMd('https://e.com/`a`'), /^<a href="https:\/\/e\.com\/" [^>]*>https:\/\/e\.com\/<\/a><code class="md-code">a<\/code>/);
});

test('a memory link slug reaches its attributes untouched by the __ pass', () => {
  const out = renderMd('see [[__x__]] and **[[user_a__b]]**');
  assertSafe(out, '[[__x__]]');
  assert.match(out, /<span class="md-memlink" data-slug="__x__" data-type="memory" [^>]*aria-label="memory 引用：\[\[__x__\]\]">/);
  assert.match(out, /<strong><span class="md-memlink" data-slug="user_a__b" data-type="user" /);
});

test('ordinary markdown still renders its links, code and math', () => {
  assert.match(renderMd('[docs](https://e.com/a "T")'), /<a href="https:\/\/e\.com\/a" class="md-link" title="T" /);
  assert.match(renderMd('[`x`](https://e.com)'), /<a href="https:\/\/e\.com" [^>]*><code class="md-code">x<\/code><\/a>/);
  assert.match(renderMd('see https://e.com/a.'), /<a href="https:\/\/e\.com\/a" [^>]*>https:\/\/e\.com\/a<\/a>\./);
  assert.match(renderMd('$x+1$ and `y`'), /<span id="ktx-\d+" class="katex-pending">x\+1<\/span> and <code class="md-code">y<\/code>/);
});

// mulberry32: a fixed seed keeps a failure reproducible from the test output.
function rng(seed) {
  return () => {
    seed = (seed + 0x6D2B79F5) | 0;
    let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

test('seeded fuzz over markdown metacharacters, placeholders and payloads', () => {
  const FRAGMENTS = [
    '`', '``', '```', '```mermaid\n', '```math\n', '$', '$$', '\\(', '\\)', '\\[', '\\]',
    '\\begin{aligned}', '\\end{aligned}', '*', '**', '_', '__', '~~', '[', ']', '(', ')',
    '[[', ']]', '![', '](', '"', '\'', '|', '\\|', '\n', '\n\n', '\r\n', '# ', '- ', '1. ',
    '2024. ', '> ', '- [ ] ', '|---|---|\n', ' ', '\t', 'x+1', 'a_b', 'src/a.go', 'main.py:12',
    'https://e.com/', 'http://e.com/a?b=1&c=2', 'javascript:', '&amp;', '&lt;', '&#58;', '&quot;',
    '<', '>', '<img src=x onerror=1>', '<svg onload=1>', '" onclick="x', '\x00', '\x00CODE0\x00',
    '\x00CODE9\x00', '\x00KTX0\x00', '\x00ILM0\x00', '\x00G0\x00', '\x00PIPE\x00', '\x00MEM0\x00', '[[__x__]]', 'CODE', '中文',
    '\u00a0', '\u2028', '\ufffd',
  ];
  const next = rng(0x3441);
  for (let n = 0; n < 4000; n++) {
    const parts = [];
    const len = 1 + Math.floor(next() * 14);
    for (let k = 0; k < len; k++) parts.push(FRAGMENTS[Math.floor(next() * FRAGMENTS.length)]);
    const input = parts.join('');
    for (const html of renderAll(input)) assertSafe(html, input);
  }
});
