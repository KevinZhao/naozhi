// render_md.js — markdown / KaTeX / mermaid rendering (#2558 D4).
//
// The dashboard's XSS-critical surface: every renderer here routes untrusted
// text through esc()/escAttr()/safeUrl(); static_sanitize_test.go,
// static_markdown_p3_test.go and scripts/render-md.test.mjs pin it.
//
// Layering: imports nz_util, the URL / entity sanitisers from utilities.js
// and the path-shape tests from the file_ref_parse.js leaf (file-reference
// <code> rows and link rescue). dashboard and file_refs import this module, so
// it must import neither back.
import { esc, escAttr } from './nz_util.js';
import { decodeEscEntities, safeUrl } from './utilities.js';
import { FILE_REF_HAS_EXT, fencedPathList, fileRefCode, isFileRefCandidate, splitPathLine } from './file_ref_parse.js';

// KaTeX environment names we split out as block-level math. Whitelisted —
// feeding KaTeX an environment it doesn't support just emits an error span
// and pollutes the block flow.
const KATEX_ENVS = 'equation|align|aligned|gather|multline|cases|array|pmatrix|bmatrix|vmatrix|Vmatrix|matrix|split|alignat|CD';
const BLOCK_SPLIT_RE = new RegExp(
  '(```[\\s\\S]*?```' +
  '|\\$\\$[\\s\\S]*?\\$\\$' +
  '|\\\\\\[[\\s\\S]*?\\\\\\]' +
  '|\\\\begin\\{(?:' + KATEX_ENVS + ')\\*?\\}[\\s\\S]*?\\\\end\\{(?:' + KATEX_ENVS + ')\\*?\\})',
  'g'
);

// 2-column step (LLM/CJK convention). MAX_LIST_DEPTH caps adversarial input.
const LIST_DEPTH_STEP = 2;
const MAX_LIST_DEPTH = 6;
// Trailing \r? handles CRLF source. The list pass runs after split('\n')
// which preserves the \r on Windows-pasted text; without explicit handling
// (.*)$ would not match (`.` excludes \r, $ does not anchor before \r
// without the m flag), and the line silently falls out of the list path.
// Leading whitespace is restricted to ASCII space + tab so leadingColumns
// stays in sync with the captured prefix; a Unicode-`\s*` would consume
// NBSP/U+2028/etc. while leadingColumns counted only space+tab, producing
// cols=0 for a visually-indented bullet.
const LIST_ITEM_RE = /^([ \t]*)(?:([-*+])|(\d+)\.)[ \t]+(.*?)\r?$/;
// Cheap shape check used by the lazy-continuation guard. Treats both digit
// shapes (any length) — the digit-cap reject path in parseListItem returns
// null, but for lazy-continuation purposes "this looks like a list bullet"
// is exactly what we want: refuse to fold such lines into the previous <li>.
const LIST_SHAPE_RE = /^[ \t]*(?:[-*+]|\d+\.)[ \t]+/;
// Reject anything > 3 digits as a list start: real ordinals max out around
// dozens; 4+ digit prefixes are year/version/issue tokens ("2024. 关于...",
// "1234. xxx") that the user does not want rendered as <ol start="2024">.
// The cap is enforced on numeric value, not string length, so "00100." (5
// chars but value 100) is accepted while "2024." (4 chars, value 2024) is
// rejected — symmetric vs. the documented intent.
const OL_START_MAX = 999;
// `> quote` line: marker at column 0 (up to 3 leading spaces per GFM), one
// optional space after it is eaten.
const BLOCKQUOTE_RE = /^ {0,3}>[ \t]?(.*)$/;

function leadingColumns(s) {
  let cols = 0;
  for (let i = 0; i < s.length; i++) {
    const ch = s.charCodeAt(i);
    if (ch === 32) cols++;
    else if (ch === 9) cols += 4 - (cols % 4);
    else break;
  }
  return cols;
}

// GFM task-list marker at the head of a bullet's content. Rendered as a
// disabled checkbox (display only — the dashboard has no write-back path).
// The rest of the content still goes through inlineMd (esc + inline passes).
const TASK_ITEM_RE = /^\[([ xX])\][ \t]+(.*)$/;
function listItemHtml(content) {
  const tm = TASK_ITEM_RE.exec(content);
  if (!tm) return inlineMd(content);
  const checked = tm[1] === ' ' ? '' : ' checked';
  return '<input type="checkbox" class="md-task" disabled' + checked + '> ' + inlineMd(tm[2]);
}

function parseListItem(line, baselineCols) {
  const m = LIST_ITEM_RE.exec(line);
  if (!m) return null;
  const cols = leadingColumns(m[1]);
  const base = baselineCols < 0 ? cols : baselineCols;
  let depth = Math.floor(Math.max(0, cols - base) / LIST_DEPTH_STEP);
  if (depth > MAX_LIST_DEPTH) depth = MAX_LIST_DEPTH;
  if (m[2]) return { kind: 'ul', depth, cols, content: m[4] };
  // Reject year/version tokens as list starts. parseInt cap (not string
  // length) so "00100." (value 100) is accepted, "2024." rejected. Returning
  // null pushes the line into the plain-text/lazy-continuation branch.
  const startNum = parseInt(m[3], 10);
  if (startNum > OL_START_MAX) return null;
  return { kind: 'ol', depth, cols, startNum, content: m[4] };
}

// mergeProseDollarBlocks — post-process `s.split(BLOCK_SPLIT_RE)`. The split
// with a capturing group alternates text / block / text / ...; odd indexes
// are matched blocks. A `$$...$$` block whose content fails isMathDisplay
// (shell `echo $$ then kill $$`, #2428) is folded back into the surrounding
// text so the line keeps flowing as one prose part. Rendering it as its own
// part would inject a spurious <br> mid-sentence and re-run the block
// heuristics on the fragment. Fenced code / \[ \] / \begin{} blocks are
// untouched.
function mergeProseDollarBlocks(parts) {
  if (parts.length < 3) return parts;
  const out = [parts[0]];
  for (let i = 1; i < parts.length; i += 2) {
    const block = parts[i];
    const text = parts[i + 1] || '';
    if (block.startsWith('$$') && !isMathDisplay(block.slice(2, -2))) {
      out[out.length - 1] += block + text;
    } else {
      out.push(block, text);
    }
  }
  return out;
}

// replaceNul turns U+0000 into U+FFFD (CommonMark 2.3): the inline passes
// delimit placeholders with \x00, and a source NUL could forge one.
function replaceNul(s) {
  return s.indexOf('\x00') === -1 ? s : s.replace(/\x00/g, '\uFFFD');
}

function renderMdUncached(s) {
  // Normalize CRLF/CR to LF up front. Source can be Windows-pasted text or
  // IM payloads carrying \r\n. Without this every per-line regex below
  // (LIST_ITEM_RE, heading, table) would silently miss-match on the trailing
  // \r and demote rich blocks to plain <br> spans.
  if (s.indexOf('\r') !== -1) s = s.replace(/\r\n?/g, '\n');
  s = replaceNul(s);
  // Split by fenced code blocks and display math blocks (including LaTeX
  // environments like \begin{aligned}...\end{aligned}).
  const parts = mergeProseDollarBlocks(s.split(BLOCK_SPLIT_RE));
  return parts.map(renderBlockPart).join('');
}

// BLOCK_RENDERERS — ordered dispatch for one `BLOCK_SPLIT_RE` part: first
// matching `test` wins. `prose` is a catch-all (test always true) and MUST
// stay last — fence/math parts never reach it, and nothing after it would
// ever run. isMathDisplay is re-checked for `$$`: mergeProseDollarBlocks
// folds a rejected `$$...$$` back into its neighbouring text, and when both
// neighbours are empty the merged prose part itself still starts/ends with
// `$$`.
const BLOCK_RENDERERS = [
  { test: (part) => part.startsWith('```'), render: renderFence },
  {
    test: (part) => part.startsWith('$$') && part.endsWith('$$') && isMathDisplay(part.slice(2, -2)),
    render: (part) => '<div class="md-math-display">' + renderKatex(part.slice(2, -2).trim(), true) + '</div>',
  },
  {
    test: (part) => part.startsWith('\\[') && part.endsWith('\\]'),
    render: (part) => '<div class="md-math-display">' + renderKatex(part.slice(2, -2).trim(), true) + '</div>',
  },
  {
    // Hand the whole environment to KaTeX in displayMode. KaTeX accepts
    // `\begin{aligned}...\end{aligned}` etc. directly without outer `\[ \]`.
    test: (part) => part.startsWith('\\begin{'),
    render: (part) => '<div class="md-math-display">' + renderKatex(part, true) + '</div>',
  },
  { test: () => true, render: renderProse },
];

function renderBlockPart(part) {
  for (const { test, render } of BLOCK_RENDERERS) {
    if (test(part)) return render(part);
  }
  return renderProse(part);
}

// renderFence — fenced code block (```lang\n...\n```). Dispatches on the
// info-string language via FENCE_RENDERERS; falls back to the path-list
// shape, then verbatim code.
function renderFence(part) {
  // Single-line fence (```ls -la```) carries no info string — everything
  // between the backticks is code. Skip the info-string match for it,
  // otherwise the greedy `[^\n]*` below would swallow the body as "lang".
  const oneLine = part.indexOf('\n') === -1;
  const m = oneLine ? null : part.match(/^```([^\n]*)\n?([\s\S]*?)```$/);
  // Info string → lang: first word, cut at whitespace / `:` / `{` so
  // `python:main.py` and `js {1,3}` yield `python` / `js`; restricted to a
  // safe charset so `c++` / `c#` / `objective-c` survive intact while stray
  // punctuation never reaches data-lang.
  const lang = m ? (m[1].trim().split(/[\s:{]/)[0] || '').replace(/[^\w+#.\-]/g, '') : '';
  // Unclosed fence (streaming tail: BLOCK_SPLIT_RE needs a closing ```, so
  // the remainder arrives as a plain part that still starts with ```):
  // strip only the opening ```lang line, never the tail of live output.
  const code = oneLine
    ? part.replace(/^```/, '').replace(/```$/, '')
    : m ? m[2].replace(/\n$/, '') : part.replace(/^```[^\n]*\n?/, '');
  // FENCE_RENDERERS is a Map, so a fence lang such as `constructor` or
  // `__proto__` cannot resolve to an inherited Object.prototype member.
  const fenceRenderer = FENCE_RENDERERS.get(lang);
  if (fenceRenderer) return fenceRenderer(code);
  // Path-list fence: a language-less block whose every non-empty line is a
  // file-path literal (the shape AI emits when it lists generated files,
  // e.g. a "here are the files I created" reply). Inside <pre><code> these
  // paths are invisible to scanEventForFileRefs (which deliberately skips
  // fenced blocks — see its `code.closest('pre')` guard), so they never get
  // preview/download buttons. Render each line as its own non-<pre> <code>
  // inside the wrap so the file-ref scanner can attach buttons, while the
  // whole block keeps a single copy button. Requiring EVERY line to be a
  // path candidate keeps real code blocks (which always carry at least one
  // non-path line) on the verbatim path.
  const pathLines = lang === '' ? fencedPathList(code) : null;
  if (pathLines) return renderPathListFence(pathLines);
  return renderCodeFence(code, lang);
}

function renderMermaidFence(code) {
  const id = 'mmd-' + (++mermaidCounter);
  mermaidPending[id] = code;
  return '<div class="mermaid-wrap"><pre id="' + id + '" class="mermaid-pending"></pre></div>';
}

// renderMathFence — opt-in math fence: ```math / ```latex / ```tex hand the
// entire block to KaTeX in displayMode. Mirrors the mermaid convention —
// authors explicitly mark intent so legitimate $-bearing source code (shell
// $VAR, Make $@, Perl $_, Python f-strings) keeps its existing verbatim
// rendering. KaTeX renderToString errors fall through to an error span with
// throwOnError:false so a malformed expression still surfaces the source
// instead of crashing the bubble.
function renderMathFence(code) {
  return '<div class="md-math-display">' + renderKatex(code, true) + '</div>';
}

const FENCE_RENDERERS = new Map([
  ['mermaid', renderMermaidFence],
  ['math', renderMathFence],
  ['latex', renderMathFence],
  ['tex', renderMathFence],
]);

function renderPathListFence(pathLines) {
  // Each row is {path, note}. The path goes in <code> so the file-ref
  // scanner + copy see only the bare path; the optional note renders as
  // a dimmed sibling span outside the <code> so it is visible but never
  // folded into the path the exists-check queries.
  const rows = pathLines.map(p => {
    const noteHtml = p.note
      ? '<span class="md-pathnote">' + esc(p.note) + '</span>'
      : '';
    return '<div class="md-pathline">' + fileRefCode(esc(p.path), '') + noteHtml + '</div>';
  }).join('');
  return '<div class="md-code-wrap md-pathlist">' + rows +
    '<div class="md-code-actions">' +
      '<button type="button" class="md-code-btn md-copy-btn" data-action="code-copy" aria-label="Copy file paths">copy</button>' +
    '</div>' +
    '</div>';
}

function renderCodeFence(code, lang) {
  const langAttr = lang ? ' data-lang="' + escAttr(lang) + '"' : '';
  return '<div class="md-code-wrap"><pre class="md-pre"><code' + langAttr + '>' + esc(code) + '</code></pre>' +
    '<div class="md-code-actions">' +
      '<button type="button" class="md-code-btn md-copy-btn" data-action="code-copy" aria-label="Copy code snippet">copy</button>' +
    '</div>' +
    '</div>';
}

// renderProse — the non-fence, non-display-math branch: block elements
// (headings, lists, blockquotes, tables) built line by line, with inline
// formatting (bold/italic/code/links/inline math) handled per line by
// inlineMd. Line-level state lives in `ctx` so LINE_HANDLERS can be plain
// functions rather than closures.
function renderProse(part) {
  // Pre-extract cross-line `\(...\)` before the per-line loop runs. inlineMd
  // processes one line at a time, which would otherwise truncate multi-line
  // inline math. Tokens survive esc() (NUL byte is not an HTML special) and
  // get swapped back in after list/heading/table rendering completes.
  // Alternation puts single-line `code` spans first so a `\(...\)` written
  // inside backticks (e.g. a regex like `\(\d+\)`) is skipped here and
  // reaches inlineMd intact, where the code-span pass claims it before the
  // math pass (#2428). Code spans are returned verbatim — no placeholder —
  // so nothing new can leak into later markdown stages. `[^`\n]` mirrors
  // inlineMd's per-line code-span scope; a stray backtick pair spanning
  // lines is not a code span there either.
  const inlineMathTokens = [];
  if (part.indexOf('\\(') !== -1) {
    part = part.replace(/`[^`\n]+`|\\\(([\s\S]+?)\\\)/g, function(m, tex) {
      if (tex === undefined) return m;
      inlineMathTokens.push(renderKatex(tex.trim(), false));
      return '\x00ILM' + (inlineMathTokens.length - 1) + '\x00';
    });
  }
  // ctx collects block-level output in chunks joined once at the end: `html +=`
  // per line reallocates the string each time, O(n^2) over line count on a
  // path history replay re-renders many times. listStack/baselineCols are the
  // list-nesting state LINE_HANDLERS read and mutate; `i` lets a handler
  // consume extra lines (blockquote/table lookahead) by advancing it.
  const ctx = { chunks: [], listStack: [], baselineCols: -1, lines: part.split('\n'), i: 0 };
  for (ctx.i = 0; ctx.i < ctx.lines.length; ctx.i++) {
    for (const handler of LINE_HANDLERS) {
      if (handler(ctx)) break;
    }
  }
  closeAll(ctx);
  let rendered = ctx.chunks.join('');
  // Restore the cross-line `\(...\)` tokens captured before the per-line
  // loop. inlineMd tokens (`\x00KTX*\x00`) were already restored inside
  // inlineMd itself; these ILM tokens sit at the block level.
  if (inlineMathTokens.length > 0) {
    rendered = rendered.replace(/\x00ILM(\d+)\x00/g, function(_, idx) {
      return inlineMathTokens[+idx];
    });
  }
  return rendered;
}

// closeTo/closeAll pop the open <li>/<ol>/<ul> tags down to (and not
// including) a target depth. baselineCols resets to "unset" once the stack
// is fully closed so the NEXT list anchors depth=0 to its own first item's
// column instead of inheriting a prior list's indent.
function closeTo(ctx, targetTopDepth) {
  const listStack = ctx.listStack;
  while (listStack.length > 0 && listStack[listStack.length - 1].depth > targetTopDepth) {
    ctx.chunks.push('</li>');
    ctx.chunks.push('</' + listStack.pop().kind + '>');
  }
  if (listStack.length === 0) ctx.baselineCols = -1;
}
function closeAll(ctx) { closeTo(ctx, -1); }

function headingHandler(ctx) {
  const line = ctx.lines[ctx.i];
  const hm = line.match(/^(#{1,4})\s+(.+)$/);
  if (!hm) return false;
  closeAll(ctx);
  const level = hm[1].length;
  ctx.chunks.push('<strong class="md-h' + level + '">' + inlineMd(hm[2]) + '</strong>\n');
  return true;
}

// listItemHandler — list-item dispatch (R1 depth logic + the sibling-frame
// fix-up below). Step 1: when the new bullet matches an existing frame in
// the stack by *source column AND kind*, that frame owns the bullet. Pop
// down to it and re-use it as a sibling — without this, a promoted-up
// sibling chain like "1. a\n- b\n- c\n" causes each `-` to first close the
// just-promoted <ul> (because parseListItem computes li.depth=0 from cols=0
// while top.depth was promoted to 1) and then reopen a brand-new <ul> —
// every bullet ends up in its own one-item list. Walking the stack first
// lets us recognise that `- c` belongs to the `- b` frame and emit a
// sibling <li>. Step 2 is the standard depth-based dispatch, including
// lenient nesting: an unindented bullet of the opposite kind at the same
// visual depth as the current frame becomes a nested child rather than
// slicing the parent list (LLM output routinely writes "1. parent\n-
// detail\n2. next" without indenting the bullets), capped by
// MAX_LIST_DEPTH against adversarial deep-promote sequences.
function listItemHandler(ctx) {
  const line = ctx.lines[ctx.i];
  const li = parseListItem(line, ctx.baselineCols);
  if (!li) return false;
  const listStack = ctx.listStack;
  if (listStack.length === 0) ctx.baselineCols = li.cols;
  for (let k = listStack.length - 1; k >= 0; k--) {
    const f = listStack[k];
    if (f.cols === li.cols && f.kind === li.kind) {
      // Close everything strictly above this frame, then sibling-emit. We
      // did NOT mutate the frame's depth, so no extra book-keeping.
      closeTo(ctx, f.depth);
      ctx.chunks.push('</li><li>' + listItemHtml(li.content));
      li.handled = true;
      break;
    }
    // Another frame at strictly shallower cols means we cannot match
    // anything further down — treat the new bullet as belonging to a
    // position deeper than that frame.
    if (f.cols < li.cols) break;
  }
  if (li.handled) return true;

  const top = listStack[listStack.length - 1];
  if (top && top.depth > li.depth) closeTo(ctx, li.depth);
  let top2 = listStack[listStack.length - 1];
  if (top2 && top2.depth === li.depth && top2.kind !== li.kind) {
    if (li.depth < MAX_LIST_DEPTH) {
      li.depth = li.depth + 1;
    } else {
      // already at the cap → fall back to the strict same-depth swap
      ctx.chunks.push('</li>');
      ctx.chunks.push('</' + listStack.pop().kind + '>');
    }
    top2 = listStack[listStack.length - 1];
  }
  if (top2 && top2.depth === li.depth) {
    if (top2.kind === li.kind) {
      ctx.chunks.push('</li><li>' + listItemHtml(li.content));
      return true;
    }
    ctx.chunks.push('</li>');
    ctx.chunks.push('</' + listStack.pop().kind + '>');
  }
  // startNum=0 / negative / NaN never produce a start attribute — <ol
  // start="0"> renders "0. 1. ..." which is jarring; let the browser fall
  // back to default "1. 2. ..." instead.
  const startAttr = (li.kind === 'ol' && li.startNum >= 2) ? ' start="' + li.startNum + '"' : '';
  const cls = li.kind === 'ol' ? 'md-ol' : 'md-ul';
  ctx.chunks.push('<' + li.kind + ' class="' + cls + '"' + startAttr + '>');
  // Frame stores li.cols too so lazy-continuation can use the original
  // source column rather than the (possibly promoted) depth — promotion
  // mutates depth but not the user's actual indent.
  listStack.push({ kind: li.kind, depth: li.depth, cols: li.cols });
  ctx.chunks.push('<li>' + listItemHtml(li.content));
  return true;
}

// blankHandler — treat all-whitespace lines as blanks too: LLM/IM pipelines
// occasionally emit a single space on otherwise-empty lines. Look ahead:
// only keep list state when the next non-blank line is a list item OF THE
// SAME KIND as the active top frame. Cross-kind continuation across a blank
// line is exactly the case the user means as "two separate lists with
// paragraph break between them" — keeping state would force the next list
// into a nested child.
function blankHandler(ctx) {
  const line = ctx.lines[ctx.i];
  if (line !== '' && !/^\s+$/.test(line)) return false;
  if (ctx.listStack.length > 0) {
    let peek = ctx.i + 1;
    while (peek < ctx.lines.length && (ctx.lines[peek] === '' || /^\s+$/.test(ctx.lines[peek]))) peek++;
    if (peek < ctx.lines.length) {
      const pli = parseListItem(ctx.lines[peek], ctx.baselineCols);
      const top = ctx.listStack[ctx.listStack.length - 1];
      if (pli && pli.kind === top.kind) return true;
    }
    closeAll(ctx);
  }
  ctx.chunks.push('<div class="md-blank"></div>');
  return true;
}

// listContinuationHandler — lazy continuation: a non-list line indented at
// least one step beyond the active top frame's source column folds into the
// open <li>. Using top.cols (raw source column) instead of top.depth keeps
// the threshold honest after lenient promotion bumped depth without bumping
// cols. Guard rails: never fold lines that *look* like a list bullet shape
// (ordinal capped out — see OL_START_MAX), a markdown table row, or a
// heading. Without these guards an indented "2024. ..." paragraph, an
// indented "| h | v |" table, or an indented "## sub" heading disappears
// into the previous <li> as silent inline text.
function listContinuationHandler(ctx) {
  if (ctx.listStack.length === 0) return false;
  const line = ctx.lines[ctx.i];
  const top = ctx.listStack[ctx.listStack.length - 1];
  const cols = leadingColumns(line);
  if (cols - top.cols < LIST_DEPTH_STEP) return false;
  const trimmed = line.trim();
  const looksLikeBlock =
    LIST_SHAPE_RE.test(line) ||
    /^\|.+\|$/.test(trimmed) ||
    /^#{1,4}\s/.test(trimmed);
  if (looksLikeBlock) return false;
  ctx.chunks.push(' ' + inlineMd(trimmed));
  return true;
}

// blockquoteHandler — consecutive `>` lines merge into one <blockquote>; the
// marker is stripped BEFORE inlineMd so the remaining text still goes
// through esc(). Nested `> >` is not unwrapped (renders as literal &gt;).
// closeAll runs here unconditionally (ahead of the table/paragraph
// fallbacks too — a no-op once the stack is already empty) because none of
// blockquote/table/paragraph continue an open list.
function blockquoteHandler(ctx) {
  closeAll(ctx);
  const line = ctx.lines[ctx.i];
  const qm = BLOCKQUOTE_RE.exec(line);
  if (!qm) return false;
  const q = [qm[1]];
  while (ctx.i + 1 < ctx.lines.length && BLOCKQUOTE_RE.test(ctx.lines[ctx.i + 1])) {
    q.push(BLOCKQUOTE_RE.exec(ctx.lines[++ctx.i])[1]);
  }
  ctx.chunks.push('<blockquote class="md-quote">' + q.map(inlineMd).join('<br>') + '</blockquote>');
  return true;
}

function tableHandler(ctx) {
  const line = ctx.lines[ctx.i];
  if (!/^\|.+\|$/.test(line.trim())) return false;
  const tbl = [line];
  while (ctx.i + 1 < ctx.lines.length && /^\|.+\|$/.test(ctx.lines[ctx.i + 1].trim())) {
    tbl.push(ctx.lines[++ctx.i]);
  }
  ctx.chunks.push(renderTable(tbl));
  return true;
}

function paragraphHandler(ctx) {
  ctx.chunks.push(inlineMd(ctx.lines[ctx.i]) + '<br>');
  return true;
}

// LINE_HANDLERS — tried in order per prose line; the first to return true
// claims the line. paragraphHandler is the catch-all and MUST stay last.
const LINE_HANDLERS = [
  headingHandler,
  listItemHandler,
  blankHandler,
  listContinuationHandler,
  blockquoteHandler,
  tableHandler,
  paragraphHandler,
];

/* Inline markdown: bold, italic, code, links, math */
// `[text]( dest "title" )` — dest is a run of non-space/non-paren chars with
// at most one nested `(...)` group; the title group is optional; CommonMark
// permits whitespace padding on both sides of the body. Dest and title
// exclude \x00 so no placeholder is restored inside an attribute value.
const MD_LINK_RE = /\[([^\]]+)\]\(\s*((?:[^()\s\x00]|\([^()\s\x00]*\))+)(?:\s+(?:"([^"\x00]*)"|'([^'\x00]*)'))?\s*\)/g;
// Bare-URL autolink. Applied only to text OUTSIDE already-emitted <a>…</a>
// so a URL inside a link's label never becomes a nested anchor; it stops at
// a \x00 placeholder for the same reason as MD_LINK_RE.
const MD_AUTOLINK_RE = /(^|[^"'>])(https?:\/\/(?:(?!&lt;|&gt;)[^\s<)}\]\x00\u3001-\u3003\u3008-\u3011\u3014-\u301f\uff01-\uff0f\uff1a-\uff20\uff3b-\uff40\uff5b-\uff65])+)/g;
const MD_ANCHOR_SPLIT_RE = /(<a [^>]*>[\s\S]*?<\/a>)/;
// Memory wiki-link `[[slug]]`. See docs/rfc/memory-link-rendering.md.
function memlinkHtml(slug) {
  var m = slug.match(/^(feedback|project|user|reference)_(.+)$/);
  var type = m ? m[1] : 'memory';
  var label = (m ? m[2] : slug).split('_').slice(-3).join('_');
  var icon = ({ feedback: '💡', project: '📌', user: '👤', reference: '🔗', memory: '🧠' })[type];
  var ariaLabel = 'memory 引用：[[' + slug + ']]';
  return '<span class="md-memlink" data-slug="' + escAttr(slug) +
    '" data-type="' + type + '" tabindex="0" role="link"' +
    ' aria-label="' + escAttr(ariaLabel) + '">' +
    '<span class="md-memlink-icon" aria-hidden="true">' + icon + '</span>' +
    '<span class="md-memlink-label">' + esc(label) + '</span></span>';
}
function inlineMd(s) {
  // `code` spans extracted FIRST so later passes never peek inside them.
  const codeTokens = [];
  if (s.indexOf('`') !== -1) {
    s = s.replace(/`([^`]+)`/g, function(_, c) {
      const idx = codeTokens.length;
      codeTokens.push(esc(c));
      return '\x00CODE' + idx + '\x00';
    });
  }
  // Inline math extracted before HTML escaping, via \x00 delimiters. Math
  // wrapping a code placeholder stays text: KaTeX copies its source into an
  // error span's title, where the code restore would land inside it.
  const mathTokens = [];
  if (s.indexOf('$') !== -1 || s.indexOf('\\(') !== -1) {
    const stash = (match, tex) => (tex.indexOf('\x00') !== -1 ? match
      : '\x00KTX' + (mathTokens.push(renderKatex(tex, false)) - 1) + '\x00');
    // `$...$`: non-alphanumeric outside + isMathInline on the inside.
    s = s.replace(/(?<![A-Za-z0-9])\$([^\s\$][^\$\n]*?[^\s\$]|[^\s\$])\$(?![A-Za-z0-9])/g,
      (match, tex) => (isMathInline(tex) ? stash(match, tex) : match));
    s = s.replace(/\\\((.+?)\\\)/g, stash);
  }
  s = esc(s);
  // Wiki-links run before `[link](url)` so the grammars cannot collide, and
  // are stashed like code spans so `__` cannot rewrite a slug's attributes.
  const memTokens = [];
  s = s.replace(/\[\[([a-zA-Z0-9_\-]{1,64})\]\]/g, function(_, slug) {
    memTokens.push(memlinkHtml(slug));
    return '\x00MEM' + (memTokens.length - 1) + '\x00';
  });
  // SECURITY CONTRACT: bold/italic regex must run AFTER esc(s) and the
  // code/wiki-link passes above — same for strike/link below (`.+?`
  // captures may span already-esc()'d injected HTML) — do not reorder
  // without a test asserting the output never contains a raw '<'.
  s = s.replace(/\*\*(.+?)\*\*/g, (_, c) => '<strong>' + c + '</strong>');
  s = s.replace(/\*(?!\s)(.+?)(?<!\s)\*/g, (_, c) => '<em>' + c + '</em>');
  // `__bold__`/`~~del~~` require a word boundary and no preceding `/`/`.`
  // (so `pkg/__init__.py` survives); body capped at 300 chars.
  s = s.replace(/(?<![A-Za-z0-9_\/.])__(?!\s)(.{1,300}?)(?<!\s)__(?![A-Za-z0-9_])/g, (_, c) => '<strong>' + c + '</strong>');
  s = s.replace(/(?<![A-Za-z0-9_\/.])~~(?!\s)(.{1,300}?)(?<!\s)~~/g, (_, c) => '<del>' + c + '</del>');
  // `![alt](url)`: CSP blocks remote images; drop `!`, let the link pass
  // below render the target (remote link or local file-ref rescue).
  s = s.replace(/!(?=\[[^\]]+\]\([^)]+\))/g, '');
  s = s.replace(MD_LINK_RE, function(_, text, urlEsc, titleDq, titleSq) {
    const title = titleDq !== undefined ? titleDq : titleSq;
    const url = decodeEscEntities(urlEsc);
    const safe = safeUrl(url);
    const titleAttr = title ? ' title="' + escAttr(decodeEscEntities(title)) + '"' : '';
    if (safe === '#') {
      // Local-file link rescue: `urlEsc` is tokenized, not raw text —
      // reject a `<`-bearing target before it reaches fileRefCode, and
      // require a real extension.
      const target = urlEsc.trim();
      if (target.indexOf('<') === -1 && isFileRefCandidate(target)) {
        const { path: bare } = splitPathLine(target);
        const base = bare.slice(bare.lastIndexOf('/') + 1);
        if (FILE_REF_HAS_EXT.test(base)) return fileRefCode(target);
      }
      return text;
    }
    return '<a href="' + escAttr(safe) + '" class="md-link"' + titleAttr + ' target="_blank" rel="noopener noreferrer">' + text + '</a>';
  });
  // Auto-link bare URLs not already inside an <a> tag.
  const autolinkSeg = function(seg) {
    return seg.replace(MD_AUTOLINK_RE, function(_, prefix, url) {
      var shown = url.replace(/[.,;:!?)>\]"'。，、；：！？）》」』】〉]+$/, '');
      var trail = url.slice(shown.length);
      var clean = decodeEscEntities(shown);
      return prefix + '<a href="' + escAttr(clean) + '" class="md-link" target="_blank" rel="noopener noreferrer">' + shown + '</a>' + trail;
    });
  };
  if (s.indexOf('https://') !== -1 || s.indexOf('http://') !== -1) {
    s = s.indexOf('<a ') === -1
      ? autolinkSeg(s)
      : s.split(MD_ANCHOR_SPLIT_RE).map((seg, k) => (k % 2 ? seg : autolinkSeg(seg))).join('');
  }
  if (memTokens.length > 0) {
    s = s.replace(/\x00MEM(\d+)\x00/g, function(_, idx) { return memTokens[+idx] || ''; });
  }
  if (mathTokens.length > 0) {
    s = s.replace(/\x00KTX(\d+)\x00/g, function(_, idx) { return mathTokens[+idx] || ''; });
  }
  if (codeTokens.length > 0) {
    s = s.replace(/\x00CODE(\d+)\x00/g, function(_, idx) {
      return codeTokens[+idx] ? fileRefCode(codeTokens[+idx]) : '';
    });
  }
  return s;
}

function renderTable(lines) {
  // Fallback (not a real table): emit each row as its own line. The block
  // renderer joins ordinary lines with <br>, so do the same here — a bare
  // '\n' collapses inside white-space:normal and the rows ran together.
  const fallback = () => lines.map(l => inlineMd(l) + '<br>').join('');
  if (lines.length < 2) return fallback();
  // Separator row: `|---|` (single column) is valid GFM too.
  if (!/^\|[\s\-:]+(\|[\s\-:]+)*\|$/.test(lines[1].trim())) return fallback();
  // Honour the GFM `\|` escape for a literal pipe inside a cell (common when
  // authors quote shell snippets like `cmd \| true`). Without this the cell
  // splits mid-snippet and the trailing fragment spills into an extra column.
  // Strategy: encode `\|` → sentinel, split on `|`, decode sentinel → `|`.
  const PIPE = '\x00PIPE\x00';
  // `|` inside `$...$`, `\(...\)` or a code span (`$2^a - 2$ | < | ...`) is
  // protected before the row splits on `|`, or one formula becomes many cells.
  // A `$...$` pair is stashed only when it carries a math-only char (\ ^ _ { })
  // or holds no `|`: in `| Pro | $20 | 1,000 | $0.04 |` a greedy pairing of
  // `$20 ... $0.04` would swallow two pipes. Pipe-bearing math like `$|AB|=2$`
  // has to use `\(...\)` or backticks inside a table.
  const isTableMathSpan = inner => {
    if (/[\\^_{}]/.test(inner)) return true;
    if (inner.indexOf('|') !== -1) return false;
    return isMathInline(inner);
  };
  const cells = l => {
    let s = l.trim().replace(/\\\|/g, PIPE);
    const guards = [];
    const stash = (re, predicate) => {
      s = s.replace(re, (m, inner) => {
        if (predicate && !predicate(inner == null ? m : inner)) return m;
        guards.push(m);
        return '\x00G' + (guards.length - 1) + '\x00';
      });
    };
    stash(/`[^`]+`/g);
    stash(/\\\(([^)]+?)\\\)/g);
    stash(/\$([^$\n]+?)\$/g, isTableMathSpan);
    return s.replace(/^\||\|$/g, '')
      .split('|')
      .map(c => c.trim()
        .replace(/\x00G(\d+)\x00/g, (_, i) => guards[+i] || '')
        .split(PIPE).join('|'));
  };
  const header = cells(lines[0]);
  const ncol = header.length;
  // Overflow guard: when an LLM emits a row with more cells than the header
  // (unbalanced pipes it refused to escape), merge the tail into the last
  // cell instead of letting empty columns spill off to the right.
  const clamp = row => {
    if (row.length <= ncol) return row;
    const head = row.slice(0, ncol - 1);
    const tail = row.slice(ncol - 1).join(' | ');
    return head.concat([tail]);
  };
  let h = '<table class="md-table"><thead><tr>' + header.map(c => '<th>' + inlineMd(c) + '</th>').join('') + '</tr></thead><tbody>';
  for (let i = 2; i < lines.length; i++) h += '<tr>' + clamp(cells(lines[i])).map(c => '<td>' + inlineMd(c) + '</td>').join('') + '</tr>';
  return '<div class="md-table-wrap">' + h + '</tbody></table></div>';
}

const LOAD_RETRY_MS = 60000;
const mermaidLoad = { ready: false, busy: false, failures: 0, loaded: new Set() };
const katexLoad = { ready: false, busy: false, failures: 0, loaded: new Set() };

// lazyLoad appends each lazily loaded asset ([url, sha384]) under its SRI
// pin; st is ready once all of them have loaded. A failed attempt is retried
// once, LOAD_RETRY_MS later, re-appending only the assets that did not load,
// then not until the page reloads. rerun runs when an attempt settles and when
// the retry falls due; meanwhile runMermaid / runKatex leave the source showing.
function lazyLoad(st, assets, rerun) {
  if (st.ready || st.busy) return;
  st.busy = true;
  const todo = assets.filter(([url]) => !st.loaded.has(url));
  let left = todo.length, failed = false;
  const settle = () => {
    if (--left > 0) return;
    if (!failed) st.ready = true;
    else if (++st.failures === 1) setTimeout(() => { st.busy = false; rerun(); }, LOAD_RETRY_MS);
    rerun();
  };
  todo.forEach(([url, integrity]) => {
    const css = url.endsWith('.css');
    document.head.appendChild(Object.assign(document.createElement(css ? 'link' : 'script'),
      css ? { rel: 'stylesheet', href: url } : { src: url }, {
        integrity, crossOrigin: 'anonymous',
        onload: () => { st.loaded.add(url); settle(); },
        onerror: () => { failed = true; settle(); },
      }));
  });
}

// awaitingLoad reports whether pending[id] still waits for st, marking an
// attached el while st is failing. The entry is dropped once st has loaded or
// failed for good, so an offline page does not keep one per detached bubble.
function awaitingLoad(pending, id, el, st) {
  if (el) {
    const failed = !st.ready && st.failures > 0;
    el.classList.toggle('md-render-unavailable', failed);
    el.title = failed ? '渲染器未加载（离线？），显示源码' : '';
    if (st.ready) return false;
  }
  if (st.ready || st.failures > 1) delete pending[id];
  return true;
}

function loadMermaid() {
  lazyLoad(mermaidLoad, [['/static/vendor/mermaid-11.14.0/mermaid.min.js',
    'sha384-1CMXl090wj8Dd6YfnzSQUOgWbE6suWCaenYG7pox5AX7apTpY3PmJMeS2oPql4Gk']], () => {
    if (mermaidLoad.ready) window.mermaid.initialize(mermaidConfig());
    runMermaid();
  });
}

function runMermaid() {
  if (Object.keys(mermaidPending).length === 0) return;
  if (!mermaidLoad.ready) loadMermaid();
  let hasNew = false;
  Object.entries(mermaidPending).forEach(([id, code]) => {
    const el = document.getElementById(id);
    if (el) el.textContent = code;
    if (awaitingLoad(mermaidPending, id, el, mermaidLoad)) return;
    el.className = 'mermaid';
    delete mermaidPending[id];
    hasNew = true;
  });
  if (hasNew) {
    // Re-initialise per run so diagrams rendered after a theme switch pick
    // up the current theme (already-rendered SVGs are left as-is).
    window.mermaid.initialize(mermaidConfig());
    Promise.resolve(window.mermaid.run({ nodes: document.querySelectorAll('.mermaid') }))
      .then(adoptMermaidStyles, adoptMermaidStyles);
  }
}

// The dashboard CSP has no style-src 'unsafe-inline', so the browser refuses
// style attributes and <style> elements that arrive as markup — which is how
// KaTeX's renderToString and mermaid's SVG carry their layout and theme. CSSOM
// writes are not markup and are not refused, so the same declarations are
// applied again through el.style and constructed style sheets. Only KaTeX and
// mermaid output goes through here; renderMd escapes every author-supplied tag.

// applyMarkupStyles re-applies the style attribute of root and its descendants.
function applyMarkupStyles(root) {
  const els = root.matches('[style]') ? [root] : [];
  root.querySelectorAll('[style]').forEach(el => els.push(el));
  els.forEach(el => { el.style.cssText = el.getAttribute('style'); });
}

// applyKatexStyles styles every KaTeX tree not yet handled. renderToString
// output reaches the DOM as markup; katex.render (the pending path) builds
// nodes through CSSOM already and needs nothing.
function applyKatexStyles() {
  document.querySelectorAll('.katex:not([data-nz-styled])').forEach(k => {
    applyMarkupStyles(k);
    k.setAttribute('data-nz-styled', '');
  });
}

// mermaidSheets holds the constructed sheet of each rendered diagram, keyed by
// its svg id. Mermaid scopes every rule under that id, so adopting the sheet at
// document level styles only its own diagram. mermaidAdopted is the set last
// written to document.adoptedStyleSheets, so sheets other code adopts survive.
const mermaidSheets = new Map();
const mermaidAdopted = new Set();

function adoptMermaidStyles() {
  document.querySelectorAll('.mermaid svg[id]').forEach(svg => {
    if (mermaidSheets.has(svg.id)) return;
    let css = '';
    svg.querySelectorAll('style').forEach(st => { css += st.textContent; });
    const sheet = new CSSStyleSheet();
    sheet.replaceSync(css);
    mermaidSheets.set(svg.id, sheet);
    applyMarkupStyles(svg);
  });
  // A diagram that left the DOM takes its sheet with it.
  for (const id of mermaidSheets.keys()) {
    if (!document.getElementById(id)) mermaidSheets.delete(id);
  }
  const live = [...mermaidSheets.values()];
  document.adoptedStyleSheets = document.adoptedStyleSheets
    .filter(sh => !mermaidAdopted.has(sh))
    .concat(live);
  mermaidAdopted.clear();
  live.forEach(sh => mermaidAdopted.add(sh));
}

// mermaidThemeName maps the resolved dashboard theme (data-theme, with
// 'auto' following prefers-color-scheme) to a mermaid theme name (#2429).
function mermaidThemeName() {
  const t = document.documentElement.dataset.theme;
  if (t === 'light') return 'default';
  if (t === 'dark') return 'dark';
  try {
    if (window.matchMedia && window.matchMedia('(prefers-color-scheme: light)').matches) return 'default';
  } catch (_) {}
  return 'dark';
}

function mermaidConfig() {
  return { startOnLoad: false, theme: mermaidThemeName(), securityLevel: 'strict' };
}

let mermaidCounter = 0;
const mermaidPending = {};

let katexCounter = 0;
const katexPending = {};

// Formulas wait for the stylesheet as well as the script: without it KaTeX
// markup shows its MathML and HTML copies side by side. Both are vendored, and
// TestVendorAssets_SRIMatchesEmbedded checks each SRI pin (R219-SEC-4).
function loadKatex() {
  lazyLoad(katexLoad, [
    ['/static/vendor/katex-0.16.21/katex.min.css',
      'sha384-zh0CIslj+VczCZtlzBcjt5ppRcsAmDnRem7ESsYwWwg3m/OaJ2l4x7YBZl9Kxxib'],
    ['/static/vendor/katex-0.16.21/katex.min.js',
      'sha384-Rma6DA2IPUwhNxmrB/7S3Tno0YY7sFu9WSYMCuulLhIqYSGZ2gKCJWIqhBWqMQfh'],
  ], runKatex);
}

function runKatex() {
  if (Object.keys(katexPending).length === 0) return;
  if (!katexLoad.ready) loadKatex();
  Object.entries(katexPending).forEach(([id, info]) => {
    const el = document.getElementById(id);
    if (awaitingLoad(katexPending, id, el, katexLoad)) return;
    try {
      window.katex.render(info.tex, el, { displayMode: info.display, throwOnError: false });
    } catch(_) {
      el.textContent = (info.display ? '$$' : '$') + info.tex + (info.display ? '$$' : '$');
    }
    delete katexPending[id];
  });
}

// isMathInline — decide whether the content captured between `$...$` pair
// looks like a math expression rather than prose. Called after the outer
// guard (non-alphanumeric on both sides of the `$`) has already rejected
// obvious prose like "每月$650$USD". Three-tier check, any-match passes:
//   1) contains an unambiguous LaTeX char (\ ^ _ { })
//   2) otherwise must be built from "math alphabet" chars only (digits,
//      single letters, operators, parens, punctuation) AND contain no two
//      consecutive 3+ letter English words AND contain at least one math
//      hint — digit, operator, OR a function-call pattern `letter(` /
//      `)letter`, so `$f(x)$` passes while prose like `$(test)$` does not.
function isMathInline(tex) {
  if (/[\\^_{}]/.test(tex)) return true;
  // Bare 1-2 letter variable / segment name (`$x$`, `$AB$`): no digit,
  // operator, or call shape to hint on, but the outer `$` guard already
  // rejected the alphanumeric-adjacent prose case, so accept it (#2428).
  // 3+ letters (`$USD$`) still fall through to the hint check below.
  if (/^[a-zA-Z]{1,2}$/.test(tex)) return true;
  if (!/^[\s\d+\-*/=<>≤≥≠±·×÷!().,;\[\]|a-zA-Z]+$/.test(tex)) return false;
  if (/[a-zA-Z]{3,}\s+[a-zA-Z]{3,}/.test(tex)) return false;
  if (!/[\d+\-*/=<>]|[a-zA-Z]\(|\)[a-zA-Z]/.test(tex)) return false;
  return true;
}

// isMathDisplay — content sanity gate for a `$$...$$` block captured by
// BLOCK_SPLIT_RE (`tex` is the inner text, `$$` already stripped). Shell
// prose uses `$$` for the current PID, so two of them on one line
// (`echo $$ then kill $$`) form a syntactically valid display-math pair
// whose "formula" is the prose in between (#2428). Accept when:
//   1) an unambiguous LaTeX / equation char is present (\ ^ _ { } =)
//   2) the block spans multiple lines — a deliberately fenced formula block
//   3) otherwise the same heuristic as inline `$...$` (isMathInline)
// Trade-off: a bare 3+ letter word (`$$ area $$`) has no hint and renders
// literally, same as inline `$USD$`.
function isMathDisplay(tex) {
  const t = tex.trim();
  if (t === '') return false;
  if (/[\\^_{}=]/.test(t)) return true;
  if (t.indexOf('\n') !== -1) return true;
  return isMathInline(t);
}

function renderKatex(tex, displayMode) {
  if (katexLoad.ready) {
    try { return window.katex.renderToString(tex, { displayMode: displayMode, throwOnError: false }); }
    catch(_) { return esc(tex); }
  }
  const id = 'ktx-' + (++katexCounter);
  katexPending[id] = { tex: tex, display: displayMode };
  loadKatex();
  return '<span id="' + id + '" class="katex-pending">' + esc(tex) + '</span>';
}

// runPendingAsync is the one post-render flush for every async pipeline that
// renderMd/renderRich output starts: call sites invoke it once after attaching
// the HTML, never runKatex / runMermaid directly.
function runPendingAsync() {
  runMermaid();
  runKatex();
  applyKatexStyles();
}

// renderRich — unified rich-text entrypoint. Single source of truth for
// chat bubbles, file-preview drawer, scratch drawer, aside drawer. Pure
// HTML producer (does NOT touch DOM); caller must runPendingAsync() after
// attaching the result so KaTeX / Mermaid pending slots get flushed.
//
// opts.mode:
//   'markdown' (default) — full md renderer (fenced code, math, mermaid,
//                          tables, lists, links)
//   'tex'                — .tex / .latex file: extract math blocks,
//                          everything else kept as preformatted text
//   'plain'              — no rendering, esc + <pre>
function renderRich(src, opts) {
  if (!src) return '';
  src = replaceNul(src);
  const mode = (opts && opts.mode) || 'markdown';
  if (mode === 'plain') return '<pre class="rich-plain">' + esc(src) + '</pre>';
  if (mode === 'tex')   return renderTexDoc(src);
  return renderMd(src);
}

// renderTexDoc — light .tex/.latex renderer. Not a LaTeX compiler; extracts
// delimiters KaTeX supports and leaves the rest as preformatted text so
// authors can see their source comments / section headers intact.
function renderTexDoc(src) {
  const RE = /(\$\$[\s\S]+?\$\$|\\\[[\s\S]+?\\\]|\\begin\{(equation|align|aligned|gather|multline|cases|array|pmatrix|bmatrix|vmatrix|Vmatrix|matrix)\*?\}[\s\S]+?\\end\{\2\*?\}|\$[^\$\n]+?\$|\\\([\s\S]+?\\\))/g;
  const out = [];
  let last = 0, m;
  while ((m = RE.exec(src)) !== null) {
    if (m.index > last) {
      out.push('<pre class="rich-plain">' + esc(src.slice(last, m.index)) + '</pre>');
    }
    const b = m[0];
    if (b.startsWith('$$')) {
      out.push('<div class="md-math-display">' + renderKatex(b.slice(2, -2).trim(), true) + '</div>');
    } else if (b.startsWith('\\[')) {
      out.push('<div class="md-math-display">' + renderKatex(b.slice(2, -2).trim(), true) + '</div>');
    } else if (b.startsWith('\\begin')) {
      out.push('<div class="md-math-display">' + renderKatex(b, true) + '</div>');
    } else if (b.startsWith('\\(')) {
      out.push(renderKatex(b.slice(2, -2).trim(), false));
    } else {
      out.push(renderKatex(b.slice(1, -1), false));
    }
    last = m.index + b.length;
  }
  if (last < src.length) {
    out.push('<pre class="rich-plain">' + esc(src.slice(last)) + '</pre>');
  }
  return out.join('');
}

/* Lightweight Markdown renderer for text/result events.
   Plain messages (no fenced code, math, or mermaid) are memoized since event
   renders run repeatedly — every WS push triggers a full-list re-render for
   the initial history, plus nav rebuilds, plus preview polls. */
const _mdCache = new Map();
const _MD_CACHE_MAX = 500;
// Cacheable inputs are under 2000 chars (#454): short stable replies are what
// re-render unchanged on nav rebuild and preview poll. A longer input is a
// final reply rendered once or a streaming event whose key changes on every
// push; caching it never hits and evicts the short bubbles that would.
const _MD_CACHE_INPUT_MAX = 2000;
// Any construct that can mint a unique DOM id (mmd-N via ```mermaid, ktx-N
// via $ / \[ / \( / \begin{env}) must bypass the cache: a cached pending
// span keeps a `ktx-N` id whose katexPending entry is deleted on first
// flush, so later cache hits would show raw TeX forever (#2428). Every
// alternative in BLOCK_SPLIT_RE plus inlineMd's inline math triggers is
// mirrored here; keep them in sync.
const _MD_UNCACHEABLE_RE = /```|\$|\\\[|\\\(|\\begin\{/;

function renderMd(s) {
  if (!s) return '';
  const cacheable = s.length < _MD_CACHE_INPUT_MAX && !_MD_UNCACHEABLE_RE.test(s);
  if (cacheable) {
    const hit = _mdCache.get(s);
    if (hit !== undefined) return hit;
  }
  const out = renderMdUncached(s);
  if (cacheable) {
    if (_mdCache.size >= _MD_CACHE_MAX) {
      const firstKey = _mdCache.keys().next().value;
      _mdCache.delete(firstKey);
    }
    _mdCache.set(s, out);
  }
  return out;
}

// ─── exports (#2558 D4) ─────────────────────────────────────────────────────
export {
  BLOCK_SPLIT_RE,
  LIST_ITEM_RE,
  LIST_SHAPE_RE,
  MAX_LIST_DEPTH,
  _mdCache,
  inlineMd,
  isMathDisplay,
  isMathInline,
  katexPending,
  loadKatex,
  loadMermaid,
  mermaidPending,
  parseListItem,
  renderKatex,
  renderMd,
  renderMdUncached,
  renderRich,
  renderTable,
  renderTexDoc,
  runMermaid,
  runPendingAsync,
};
