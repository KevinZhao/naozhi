// render_md.js — markdown / KaTeX / mermaid rendering (#2558 D4).
//
// Moved verbatim out of dashboard.js (`git diff --color-moved` shows this as a
// pure move; only the import/export lines below are new). This is the
// dashboard's XSS-critical surface — every renderer here must keep routing
// untrusted text through esc()/escAttr()/safeUrl(); the sanitiser contract
// tests (static_sanitize_test.go, static_markdown_p3_test.go) pin it.
//
// Layering: imports only nz_util plus the small set of dashboard helpers the
// renderers reach for (file-reference buttons, event grouping). dashboard
// imports this module, so it must not import dashboard back.
import { esc, escAttr } from './nz_util.js';

// deps — the small set of dashboard.js helpers the renderers reach for
// (URL/entity sanitising + the file-reference button builders, which stay
// with the file-ref scanner they belong to). Injected rather than imported:
// dashboard imports THIS module for renderMd, so an import back would form a
// cycle and put dashboard's own const declarations in TDZ during
// render_md's evaluation (measured: "Cannot access 'BLOCK_SPLIT_RE' before
// initialization"). configureRenderMd runs from dashboard's module body, i.e.
// before any render call.
const deps = {
  FILE_REF_HAS_EXT: null,
  decodeEscEntities: null,
  fencedPathList: null,
  fileRefCode: null,
  isFileRefCandidate: null,
  safeUrl: null,
  splitPathLine: null,
};
export function configureRenderMd(impl) {
  for (const k of Object.keys(deps)) {
    if (typeof impl[k] === 'undefined') throw new Error('render_md dep missing: ' + k);
    deps[k] = impl[k];
  }
}

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

function renderMdUncached(s) {
  // Normalize CRLF/CR to LF up front. Source can be Windows-pasted text or
  // IM payloads carrying \r\n. Without this every per-line regex below
  // (LIST_ITEM_RE, heading, table) would silently miss-match on the trailing
  // \r and demote rich blocks to plain <br> spans.
  if (s.indexOf('\r') !== -1) s = s.replace(/\r\n?/g, '\n');
  // Split by fenced code blocks and display math blocks (including LaTeX
  // environments like \begin{aligned}...\end{aligned}).
  const parts = mergeProseDollarBlocks(s.split(BLOCK_SPLIT_RE));
  return parts.map(part => {
    if (part.startsWith('```')) {
      // Single-line fence (```ls -la```) carries no info string — everything
      // between the backticks is code. Skip the info-string match for it,
      // otherwise the greedy `[^\n]*` below would swallow the body as "lang".
      const oneLine = part.indexOf('\n') === -1;
      const m = oneLine ? null : part.match(/^```([^\n]*)\n?([\s\S]*?)```$/);
      // Info string → lang: first word, cut at whitespace / `:` / `{` so
      // `python:main.py` and `js {1,3}` yield `python` / `js`; restricted to a
      // safe charset so `c++` / `c#` / `objective-c` survive intact while stray
      // punctuation never reaches data-lang. The old `(\w*)` stopped at the
      // first non-word char and left the remainder (`++`) in the code body.
      const lang = m ? (m[1].trim().split(/[\s:{]/)[0] || '').replace(/[^\w+#.\-]/g, '') : '';
      // Unclosed fence (streaming tail: BLOCK_SPLIT_RE needs a closing ```, so
      // the remainder arrives as a plain part that still starts with ```):
      // strip only the opening ```lang line. The old slice(3, -3) assumed a
      // closing fence and ate the last 3 characters of live output.
      const code = oneLine
        ? part.replace(/^```/, '').replace(/```$/, '')
        : m ? m[2].replace(/\n$/, '') : part.replace(/^```[^\n]*\n?/, '');
      if (lang === 'mermaid') {
        const id = 'mmd-' + (++mermaidCounter);
        mermaidPending[id] = code;
        return '<div class="mermaid-wrap"><pre id="' + id + '" class="mermaid-pending"></pre></div>';
      }
      // Opt-in math fence: ```math / ```latex / ```tex hand the entire block
      // to KaTeX in displayMode. Mirrors the mermaid convention — authors
      // explicitly mark intent so legitimate $-bearing source code (shell
      // $VAR, Make $@, Perl $_, Python f-strings) keeps its existing
      // verbatim rendering. KaTeX renderToString errors fall through to
      // an error span with throwOnError:false so a malformed expression
      // still surfaces the source instead of crashing the bubble.
      if (lang === 'math' || lang === 'latex' || lang === 'tex') {
        return '<div class="md-math-display">' + renderKatex(code, true) + '</div>';
      }
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
      const pathLines = lang === '' ? deps.fencedPathList(code) : null;
      if (pathLines) {
        // Each row is {path, note}. The path goes in <code> so the file-ref
        // scanner + copy see only the bare path; the optional note renders as
        // a dimmed sibling span outside the <code> so it is visible but never
        // folded into the path the exists-check queries.
        const rows = pathLines.map(p => {
          const noteHtml = p.note
            ? '<span class="md-pathnote">' + esc(p.note) + '</span>'
            : '';
          return '<div class="md-pathline">' + deps.fileRefCode(esc(p.path), '') + noteHtml + '</div>';
        }).join('');
        return '<div class="md-code-wrap md-pathlist">' + rows +
          '<div class="md-code-actions">' +
            '<button type="button" class="md-code-btn md-copy-btn" data-action="code-copy" aria-label="Copy file paths">copy</button>' +
          '</div>' +
          '</div>';
      }
      const langAttr = lang ? ' data-lang="' + escAttr(lang) + '"' : '';
      return '<div class="md-code-wrap"><pre class="md-pre"><code' + langAttr + '>' + esc(code) + '</code></pre>' +
        '<div class="md-code-actions">' +
          '<button type="button" class="md-code-btn md-copy-btn" data-action="code-copy" aria-label="Copy code snippet">copy</button>' +
        '</div>' +
        '</div>';
    }
    // isMathDisplay re-checked here: mergeProseDollarBlocks folds a rejected
    // `$$...$$` back into its neighbouring text, and when both neighbours are
    // empty the merged prose part itself still starts/ends with `$$`.
    if (part.startsWith('$$') && part.endsWith('$$') && isMathDisplay(part.slice(2, -2))) {
      return '<div class="md-math-display">' + renderKatex(part.slice(2, -2).trim(), true) + '</div>';
    }
    if (part.startsWith('\\[') && part.endsWith('\\]')) {
      return '<div class="md-math-display">' + renderKatex(part.slice(2, -2).trim(), true) + '</div>';
    }
    if (part.startsWith('\\begin{')) {
      // Hand the whole environment to KaTeX in displayMode. KaTeX accepts
      // `\begin{aligned}...\end{aligned}` etc. directly without outer `\[ \]`.
      return '<div class="md-math-display">' + renderKatex(part, true) + '</div>';
    }
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
    // Process line by line for block elements. Accumulate into a chunks array
    // + single join() at the end rather than `html +=` per line: V8 reallocates
    // the underlying string on every concat past the small-string threshold,
    // which is O(n^2) over line count. A 200-line response rendered ~50 times
    // per history replay was the dominant cost in the text-event path.
    const lines = part.split('\n');
    const chunks = [];
    // List state: stack of { kind: 'ol'|'ul', depth }, outermost at index 0.
    // Replaces a single 'inList' string so we can render nested + mixed-type
    // lists without each unordered run cleaving an enclosing ordered list.
    // baselineCols anchors depth=0 to the first list item's column so an
    // entire indented block does not start at depth>0.
    const listStack = [];
    let baselineCols = -1;
    const closeTo = (targetTopDepth) => {
      while (listStack.length > 0 &&
             listStack[listStack.length - 1].depth > targetTopDepth) {
        chunks.push('</li>');
        chunks.push('</' + listStack.pop().kind + '>');
      }
      if (listStack.length === 0) baselineCols = -1;
    };
    const closeAll = () => closeTo(-1);
    for (let i = 0; i < lines.length; i++) {
      let line = lines[i];
      // Headings
      const hm = line.match(/^(#{1,4})\s+(.+)$/);
      if (hm) {
        closeAll();
        const level = hm[1].length;
        chunks.push('<strong class="md-h' + level + '">' + inlineMd(hm[2]) + '</strong>\n');
        continue;
      }
      const li = parseListItem(line, baselineCols);
      if (li) {
        if (listStack.length === 0) baselineCols = li.cols;
        // Step 1: when the new bullet matches an existing frame in the stack
        // by *source column AND kind*, that frame owns the bullet. Pop down
        // to it and re-use it as a sibling. This is the key correctness
        // fix-up over the original lenient-promotion design: without it, a
        // promoted-up sibling chain like "1. a\n- b\n- c\n" causes each `-`
        // to first close the just-promoted <ul> (because parseListItem
        // computes li.depth=0 from cols=0 while top.depth was promoted to 1)
        // and then reopen a brand-new <ul> — every bullet ends up in its
        // own one-item list. Walking the stack first lets us recognise that
        // `- c` belongs to the `- b` frame and emit a sibling <li>.
        for (let k = listStack.length - 1; k >= 0; k--) {
          const f = listStack[k];
          if (f.cols === li.cols && f.kind === li.kind) {
            // Close everything strictly above this frame, then sibling-emit.
            closeTo(f.depth);
            chunks.push('</li><li>' + listItemHtml(li.content));
            // We did NOT mutate the frame's depth, so no extra book-keeping.
            // Skip the rest of the dispatch.
            li.handled = true;
            break;
          }
          // Another frame at strictly shallower cols means we cannot match
          // anything further down — treat the new bullet as belonging to
          // a position deeper than that frame.
          if (f.cols < li.cols) break;
        }
        if (li.handled) continue;

        // Step 2: standard depth-based dispatch (unchanged from R1).
        const top = listStack[listStack.length - 1];
        if (top && top.depth > li.depth) {
          closeTo(li.depth);
        }
        let top2 = listStack[listStack.length - 1];
        // Lenient nesting: an unindented bullet of the opposite kind at the
        // same visual depth as the current frame becomes a nested child
        // rather than slicing the parent list. LLM output routinely writes
        // "1. parent\n- detail\n2. next" without indenting the bullets —
        // strict CommonMark would render three separate lists (the
        // dashboard screenshot's "全是 1." root cause). MAX_LIST_DEPTH cap
        // keeps the stack bounded under adversarial deep-promote sequences.
        if (top2 && top2.depth === li.depth && top2.kind !== li.kind) {
          if (li.depth < MAX_LIST_DEPTH) {
            li.depth = li.depth + 1;
          } else {
            // already at the cap → fall back to the strict same-depth swap
            chunks.push('</li>');
            chunks.push('</' + listStack.pop().kind + '>');
          }
          top2 = listStack[listStack.length - 1];
        }
        if (top2 && top2.depth === li.depth) {
          if (top2.kind === li.kind) {
            chunks.push('</li><li>' + listItemHtml(li.content));
            continue;
          }
          chunks.push('</li>');
          chunks.push('</' + listStack.pop().kind + '>');
        }
        // startNum=0 / negative / NaN never produce a start attribute —
        // <ol start="0"> renders "0. 1. ..." which is jarring; let the
        // browser fall back to default "1. 2. ..." instead.
        const startAttr = (li.kind === 'ol' && li.startNum >= 2)
            ? ' start="' + li.startNum + '"' : '';
        const cls = li.kind === 'ol' ? 'md-ol' : 'md-ul';
        chunks.push('<' + li.kind + ' class="' + cls + '"' + startAttr + '>');
        // Frame stores li.cols too so lazy-continuation can use the original
        // source column rather than the (possibly promoted) depth — promotion
        // mutates depth but not the user's actual indent.
        listStack.push({ kind: li.kind, depth: li.depth, cols: li.cols });
        chunks.push('<li>' + listItemHtml(li.content));
        continue;
      }
      // Treat all-whitespace lines as blanks: LLM/IM pipelines occasionally
      // emit a single space on otherwise-empty lines. Without this they fall
      // through to lazy continuation (or closeAll) and visibly fracture lists.
      if (line === '' || /^\s+$/.test(line)) {
        if (listStack.length > 0) {
          // Look ahead: only keep list state when the next non-blank line is
          // a list item OF THE SAME KIND as the active top frame. Cross-kind
          // continuation across a blank line is exactly the case the user
          // means as "two separate lists with paragraph break between them"
          // — keeping state would force the next list into a nested child.
          let peek = i + 1;
          while (peek < lines.length && (lines[peek] === '' || /^\s+$/.test(lines[peek]))) peek++;
          if (peek < lines.length) {
            const pli = parseListItem(lines[peek], baselineCols);
            const top = listStack[listStack.length - 1];
            if (pli && pli.kind === top.kind) {
              continue;
            }
          }
          closeAll();
        }
        chunks.push('<div class="md-blank"></div>');
        continue;
      }
      // Lazy continuation: a non-list line indented at least one step beyond
      // the active top frame's source column folds into the open <li>. Using
      // top.cols (raw source column) instead of top.depth keeps the threshold
      // honest after lenient promotion bumped depth without bumping cols.
      // Guard rails: never fold lines that *look* like a list bullet shape
      // (ordinal capped out — see OL_START_MAX), a markdown table row, or
      // a heading. Without these guards an indented "2024. ..." paragraph,
      // an indented "| h | v |" table, or an indented "## sub" heading
      // disappears into the previous <li> as silent inline text.
      if (listStack.length > 0) {
        const top = listStack[listStack.length - 1];
        const cols = leadingColumns(line);
        if (cols - top.cols >= LIST_DEPTH_STEP) {
          const trimmed = line.trim();
          const looksLikeBlock =
            LIST_SHAPE_RE.test(line) ||
            /^\|.+\|$/.test(trimmed) ||
            /^#{1,4}\s/.test(trimmed);
          if (!looksLikeBlock) {
            chunks.push(' ' + inlineMd(trimmed));
            continue;
          }
        }
      }
      closeAll();
      // Blockquote: consecutive `>` lines merge into one <blockquote>; the
      // marker is stripped BEFORE inlineMd so the remaining text still goes
      // through esc(). Nested `> >` is not unwrapped (renders as literal &gt;).
      const qm = BLOCKQUOTE_RE.exec(line);
      if (qm) {
        const q = [qm[1]];
        while (i + 1 < lines.length && BLOCKQUOTE_RE.test(lines[i + 1])) {
          q.push(BLOCKQUOTE_RE.exec(lines[++i])[1]);
        }
        chunks.push('<blockquote class="md-quote">' + q.map(inlineMd).join('<br>') + '</blockquote>');
        continue;
      }
      if (/^\|.+\|$/.test(line.trim())) {
        let tbl = [line];
        while (i + 1 < lines.length && /^\|.+\|$/.test(lines[i + 1].trim())) { tbl.push(lines[++i]); }
        chunks.push(renderTable(tbl));
        continue;
      }
      chunks.push(inlineMd(line) + '<br>');
    }
    closeAll();
    let rendered = chunks.join('');
    // Restore the cross-line `\(...\)` tokens captured before the per-line
    // loop. inlineMd tokens (`\x00KTX*\x00`) were already restored inside
    // inlineMd itself; these ILM tokens sit at the block level.
    if (inlineMathTokens.length > 0) {
      rendered = rendered.replace(/\x00ILM(\d+)\x00/g, function(_, idx) {
        return inlineMathTokens[+idx];
      });
    }
    return rendered;
  }).join('');
}

/* Inline markdown: bold, italic, code, links, math */
// `[text]( dest "title" )` — dest is a run of non-space/non-paren chars with
// at most one nested `(...)` group; the title group is optional; CommonMark
// permits whitespace padding on both sides of the body.
const MD_LINK_RE = /\[([^\]]+)\]\(\s*((?:[^()\s]|\([^()\s]*\))+)(?:\s+(?:"([^"]*)"|'([^']*)'))?\s*\)/g;
// Bare-URL autolink. Applied only to text OUTSIDE already-emitted <a>…</a>
// so a URL inside a link's label never becomes a nested anchor.
const MD_AUTOLINK_RE = /(^|[^"'>])(https?:\/\/(?:(?!&lt;|&gt;)[^\s<)}\]\u3001-\u3003\u3008-\u3011\u3014-\u301f\uff01-\uff0f\uff1a-\uff20\uff3b-\uff40\uff5b-\uff65])+)/g;
const MD_ANCHOR_SPLIT_RE = /(<a [^>]*>[\s\S]*?<\/a>)/;
function inlineMd(s) {
  // Extract `code` spans FIRST — before math/bold/italic — so KaTeX does not
  // peek inside them. Previously `$NVIDIA_DEVICE_PLUGIN_IMAGE$` written inside
  // backticks was grabbed by the `$...$` math pass and rendered as italicised
  // subscripts, mangling legitimate shell/env-var snippets. Code content is
  // esc()'d immediately so the final token restore emits literal text safely.
  const codeTokens = [];
  if (s.indexOf('`') !== -1) {
    s = s.replace(/`([^`]+)`/g, function(_, c) {
      const idx = codeTokens.length;
      codeTokens.push(esc(c));
      return '\x00CODE' + idx + '\x00';
    });
  }
  // Extract inline math before HTML escaping. Use \x00 delimiters to avoid
  // collisions with user content. Fast path: the overwhelming majority of
  // lines in tool output / assistant text have no math markers, so we
  // short-circuit the two regex scans + mathTokens allocation when neither
  // `$` nor `\(` appears. This is called once per line in renderMdUncached
  // — on a 200-line response the savings are measurable in V8 profiler.
  const mathTokens = [];
  if (s.indexOf('$') !== -1 || s.indexOf('\\(') !== -1) {
    // `$...$`: require non-alphanumeric outside + math-like content inside.
    // The outer guard (non-alphanumeric on both sides) handles the "每月$650$USD"
    // prose case. The inner guard (isMathInline) decides whether the captured
    // span looks like a formula — accepting plain algebra like `$x=1$` /
    // `$2x$` / `$a+b$` which the previous LaTeX-only heuristic rejected.
    s = s.replace(/(?<![A-Za-z0-9])\$([^\s\$][^\$\n]*?[^\s\$]|[^\s\$])\$(?![A-Za-z0-9])/g, function(match, tex) {
      if (!isMathInline(tex)) return match;
      const idx = mathTokens.length;
      mathTokens.push(renderKatex(tex, false));
      return '\x00KTX' + idx + '\x00';
    });
    s = s.replace(/\\\((.+?)\\\)/g, function(_, tex) {
      const idx = mathTokens.length;
      mathTokens.push(renderKatex(tex, false));
      return '\x00KTX' + idx + '\x00';
    });
  }
  s = esc(s);
  // Memory wiki-link `[[slug]]`: Claude's auto-memory cross-reference syntax
  // occasionally leaks into chat output. Substitute before `[link](url)` runs
  // so the [[…]] form cannot collide with the markdown link grammar
  // (`[label](url)`); slug charset is locked to [a-zA-Z0-9_-]{1,64} which
  // also makes path-traversal in the data-slug attribute impossible by
  // construction. The popover handler below attaches lazily on hover/click.
  // See docs/rfc/memory-link-rendering.md.
  // Chip render: type prefix → color + emoji icon; tail of slug → short label.
  // Type heuristic uses Claude's auto-memory naming convention (feedback_*,
  // project_*, user_*, reference_*) — unknown prefixes fall back to a neutral
  // 🧠 chip. Hover popover (below) still shows full slug + body.
  // a11y contract: aria-label exposes the full [[slug]] to screen readers and
  // role=link + tabindex=0 + Enter/Space handler (see popover IIFE) make the
  // chip keyboard-activable. Copy fallback: a document-level `copy` listener
  // rewrites clipboardData to [[slug]] when selection touches a chip, so the
  // wiki-link survives copy/paste even though the visible glyphs are shorter.
  s = s.replace(/\[\[([a-zA-Z0-9_\-]{1,64})\]\]/g, function(_, slug) {
    var m = slug.match(/^(feedback|project|user|reference)_(.+)$/);
    var type = m ? m[1] : 'memory';
    var tail = m ? m[2] : slug;
    var segs = tail.split('_');
    var label = segs.slice(-3).join('_');
    var icon = ({
      feedback:  '💡',
      project:   '📌',
      user:      '👤',
      reference: '🔗',
      memory:    '🧠',
    })[type];
    var ariaLabel = 'memory 引用：[[' + slug + ']]';
    return '<span class="md-memlink" data-slug="' + escAttr(slug) +
      '" data-type="' + type + '" tabindex="0" role="link"' +
      ' aria-label="' + escAttr(ariaLabel) + '">' +
      '<span class="md-memlink-icon" aria-hidden="true">' + icon + '</span>' +
      '<span class="md-memlink-label">' + esc(label) + '</span></span>';
  });
  // Use function-form replacements to prevent JS's special $-sequences
  // ($&, $', $`, $n) from expanding inside the replacement string. Those
  // sequences survive esc() (they aren't HTML entities) and would let an
  // attacker-controlled LLM snippet splice unescaped characters into the
  // emitted HTML by embedding `$&` inside a backtick/bold region.
  //
  // SECURITY CONTRACT: bold/italic regex must run AFTER esc(s) (line ~8193)
  // AND AFTER code/wiki-link injection passes. The bold .+? capture can
  // span injected <span>/<code> HTML; this is safe ONLY because the inner
  // text was already esc()'d. Do NOT reorder these passes without first
  // adding a unit test asserting the bold output never contains a raw
  // '<' character.
  s = s.replace(/\*\*(.+?)\*\*/g, (_, c) => '<strong>' + c + '</strong>');
  // Italic requires the opening `*` to be followed by, and the closing `*` to
  // be preceded by, non-whitespace — so arithmetic like `2 * 3 * 4` no longer
  // turns into `2 <em> 3 </em> 4`. Lookbehind is already relied upon by the
  // inline-math pass above.
  s = s.replace(/\*(?!\s)(.+?)(?<!\s)\*/g, (_, c) => '<em>' + c + '</em>');
  // `__bold__`: both delimiters must sit at a word boundary (not touching
  // [A-Za-z0-9_]) so `snake_case_name` / `foo__bar__baz` stay literal —
  // mirrors GFM's flanking rule for `_`. The opener additionally rejects a
  // preceding `/` or `.` because these passes run BEFORE the link/autolink
  // passes: `https://x/pkg/__init__.py`, `pkg/__init__.py` (local-file
  // rescue) and `foo.__init__()` must survive verbatim, while `a __bold__ b`
  // and `__all__ = []` still bold. `~~del~~` gets the same opener guard so
  // `https://x.com/a~~b~~c` is not sliced; `~/.config` and a lone ` ~~ ` are
  // untouched by construction. Body is capped at 300 chars: an unbounded
  // `.+?` rescans to end-of-line from every opener (quadratic on inputs like
  // `' __a'.repeat(10000)`). Same post-esc() contract as the passes above.
  s = s.replace(/(?<![A-Za-z0-9_\/.])__(?!\s)(.{1,300}?)(?<!\s)__(?![A-Za-z0-9_])/g, (_, c) => '<strong>' + c + '</strong>');
  s = s.replace(/(?<![A-Za-z0-9_\/.])~~(?!\s)(.{1,300}?)(?<!\s)~~/g, (_, c) => '<del>' + c + '</del>');
  // `![alt](url)` image syntax. The dashboard CSP (img-src 'self' data: blob:)
  // blocks remote images, so an <img> would only ever render broken. Drop the
  // `!` and let the link pass below handle the target: remote → clickable
  // md-link labelled with the alt text; local path → the deps.fileRefCode rescue
  // below, which is the existing image-preview path (preview/download buttons).
  s = s.replace(/!(?=\[[^\]]+\]\([^)]+\))/g, '');
  // Destination grammar: one level of balanced parens is allowed inside the
  // URL (`https://x/a_(b)`) and an optional `"title"` / `'title'` may follow
  // after whitespace (GFM link title). The title never reaches the href; it
  // is emitted as a title attribute through escAttr. `"` survives esc() (only
  // & < > are encoded) so the quote delimiters match literally.
  s = s.replace(MD_LINK_RE, function(_, text, urlEsc, titleDq, titleSq) {
    const title = titleDq !== undefined ? titleDq : titleSq;
    // `urlEsc` is the esc()'d capture (`&` → `&amp;`). Decode esc's entities
    // before the scheme check + escAttr so the href is encoded exactly once —
    // previously `?a=1&b=2` shipped as `?a=1&amp;amp;b=2`.
    const url = deps.decodeEscEntities(urlEsc);
    const safe = deps.safeUrl(url);
    const titleAttr = title ? ' title="' + escAttr(deps.decodeEscEntities(title)) + '"' : '';
    // `text` is the already-esc()'d+partially-transformed capture — it may
    // legitimately contain <strong>/<em>/<code> spans from prior passes.
    // When the URL is rejected we still want to render the label, but
    // returning `text` as-is lets those inline tags survive in the output
    // stream unattached to an anchor. This is accepted (matches GitHub's
    // behaviour) because the substituted tags are naozhi-controlled and
    // cannot contain unescaped attacker content (each bold/italic/code
    // substitution already used `esc()`'d capture groups).
    if (safe === '#') {
      // Local-file link: claude CLI routinely emits generated files as
      // markdown links `[数学/专题/foo.html](数学/专题/foo.html)` rather than
      // backtick code. deps.safeUrl rejects the non-http target (→ '#'), so the
      // anchor branch is skipped and the link would collapse to plain text —
      // invisible to scanEventForFileRefs (which only walks <code>/.md-code).
      // Re-render a path-shaped target as inline <code> so the file-ref
      // scanner attaches the same [↗ preview][↓ download] buttons it gives
      // backtick paths. `urlEsc` is already esc()'d (esc ran above), so embed it
      // directly; scanEventForFileRefs reads code.textContent (browser-decoded
      // back to the real path) when calling the exists API. Display uses the
      // path itself rather than `text` so the user sees which file resolves —
      // and so the scanner's textContent is the path, not a friendly label.
      //
      // The `urlEsc` capture is NOT raw text — earlier inlineMd passes have already
      // tokenized it. Two classes of contamination must be rejected before the
      // target can be embedded in <code>:
      //   1. naozhi-injected markup: the bold/italic passes run before this and
      //      splice <strong>/<em> spans into the capture when the target itself
      //      contains `**`/`*` (e.g. `[a](**x**/y.html)`). A real file path
      //      never contains `<`, so reject any `<`-bearing target.
      //   2. tokenizer placeholders: the backtick-code and inline-math passes
      //      replace `` `x` ``/`$x$` with \x00CODE<n>\x00 / \x00KTX<n>\x00
      //      sentinels. \x00 is non-whitespace/non-colon so it slips through
      //      deps.isFileRefCandidate, and the restore passes that run AFTER this one
      //      would rewrite the sentinel into a nested <code>/<span> inside our
      //      new <code> — malformed HTML plus a corrupted path for the scanner.
      //      Reject any \x00-bearing target.
      // Finally require a real extension (the same deps.FILE_REF_HAS_EXT gate
      // deps.fencedPathList applies) so slash-shaped non-files — dates `2024/01/02`,
      // fractions `1/2`, doc slugs without an extension — don't hijack the link
      // into a bogus file ref with the author's label discarded.
      const target = urlEsc.trim();
      if (target.indexOf('<') === -1 && target.indexOf('\x00') === -1 && deps.isFileRefCandidate(target)) {
        const { path: bare } = deps.splitPathLine(target);
        const base = bare.slice(bare.lastIndexOf('/') + 1);
        if (deps.FILE_REF_HAS_EXT.test(base)) {
          return deps.fileRefCode(target);
        }
      }
      return text;
    }
    return '<a href="' + escAttr(safe) + '" class="md-link"' + titleAttr + ' target="_blank" rel="noopener noreferrer">' + text + '</a>';
  });
  // Auto-link bare URLs not already inside an <a> tag.
  // R243-SEC-11 (#797): strip a wider set of trailing punctuation —
  // including `>`, `]`, `"` and `'` — before forming the anchor. escAttr
  // already neutralises these inside the href attribute, but stripping
  // here also keeps them out of the link's visible text where they would
  // otherwise dangle past sentences like `see <https://x.y/z>` or
  // `[link](https://x.y/z)`. Defence-in-depth, not the only barrier.
  // The URL charset additionally stops at CJK / fullwidth *punctuation* only
  // (U+3001–3003, 3008–3011, 3014–301F, FF01–FF0F, FF1A–FF20, FF3B–FF40,
  // FF5B–FF65) so `https://x.com/a。然后` no longer swallows the rest of the
  // sentence, while 々〆〇 and halfwidth katakana (U+FF61–FF9F) stay legal so
  // Japanese paths survive. It also stops at the `&lt;`/`&gt;` entities esc()
  // emitted for `<https://…>` so the closing bracket stays out of the href.
  // Odd segments of the split are the <a>…</a> runs emitted by the link pass
  // above (esc() turned every authored `<` into &lt;, so `<a ` can only be
  // ours); they are passed through untouched.
  const autolinkSeg = function(seg) {
    return seg.replace(MD_AUTOLINK_RE, function(_, prefix, url) {
      // `shown` is still esc()'d — safe to emit as the anchor's text. `clean` is
      // the decoded URL for the href (escAttr re-encodes it exactly once).
      var shown = url.replace(/[.,;:!?)>\]"'。，、；：！？）》」』】〉]+$/, '');
      var trail = url.slice(shown.length);
      var clean = deps.decodeEscEntities(shown);
      return prefix + '<a href="' + escAttr(clean) + '" class="md-link" target="_blank" rel="noopener noreferrer">' + shown + '</a>' + trail;
    });
  };
  if (s.indexOf('https://') !== -1 || s.indexOf('http://') !== -1) {
    s = s.indexOf('<a ') === -1
      ? autolinkSeg(s)
      : s.split(MD_ANCHOR_SPLIT_RE).map((seg, k) => (k % 2 ? seg : autolinkSeg(seg))).join('');
  }
  // Restore math tokens after escaping
  if (mathTokens.length > 0) {
    s = s.replace(/\x00KTX(\d+)\x00/g, function(_, idx) { return mathTokens[+idx]; });
  }
  // Restore code tokens last — their contents were esc()'d at capture time.
  if (codeTokens.length > 0) {
    s = s.replace(/\x00CODE(\d+)\x00/g, function(_, idx) {
      return deps.fileRefCode(codeTokens[+idx]);
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
  // LLM output frequently embeds unescaped `|` inside `$...$`, `\(...\)`,
  // or backtick code spans (e.g. `$|AB|=2$`, `$2^a - 2$ | < | ...`).
  // Protect those regions BEFORE splitting on `|`, otherwise a single math
  // formula would get sliced into many spurious columns.
  //
  // CAVEAT (currency vs math): a row like `| Pro | $20 | 1,000 | $0.04 |`
  // contains four currency-style `$N` tokens, NOT two math spans. A naive
  // `\$[^$]+\$` pass would greedily pair `$20 ... $0.04`, swallow the two
  // pipes between them, and collapse the row from 4 cells to 2. To avoid
  // that, only stash a `$...$` pair when its inner content unambiguously
  // looks like LaTeX — either it carries a math-only character (\ ^ _ { })
  // OR it sits entirely on one side of a pipe (no `|` inside). Pure-numeric
  // tokens like `$20` / `$0.04/credit` then split as ordinary cells.
  // Pipe-bearing math like `$|AB|=2$` should be authored with `\(...\)` or
  // backticks inside tables — accepted limitation.
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
        .replace(/\x00G(\d+)\x00/g, (_, i) => guards[+i])
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

let mermaidLoading = false;
let mermaidReady = false;

function loadMermaid() {
  if (mermaidReady || mermaidLoading) return;
  mermaidLoading = true;
  const s = document.createElement('script');
  s.src = 'https://cdn.jsdelivr.net/npm/mermaid@11.14.0/dist/mermaid.min.js';
  s.integrity = 'sha384-1CMXl090wj8Dd6YfnzSQUOgWbE6suWCaenYG7pox5AX7apTpY3PmJMeS2oPql4Gk';
  s.crossOrigin = 'anonymous';
  s.onload = () => {
    window.mermaid.initialize(mermaidConfig());
    mermaidReady = true;
    mermaidLoading = false;
    runMermaid();
  };
  s.onerror = () => { mermaidLoading = false; };
  document.head.appendChild(s);
}

function runMermaid() {
  if (Object.keys(mermaidPending).length === 0) return;
  if (!mermaidReady) { loadMermaid(); return; }
  let hasNew = false;
  Object.entries(mermaidPending).forEach(([id, code]) => {
    const el = document.getElementById(id);
    if (!el) { delete mermaidPending[id]; return; }
    el.textContent = code;
    el.className = 'mermaid';
    delete mermaidPending[id];
    hasNew = true;
  });
  if (hasNew) {
    // Re-initialise per run so diagrams rendered after a theme switch pick
    // up the current theme (already-rendered SVGs are left as-is).
    window.mermaid.initialize(mermaidConfig());
    window.mermaid.run({ nodes: document.querySelectorAll('.mermaid') });
  }
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

let katexLoading = false;
let katexReady = false;
let katexCounter = 0;
const katexPending = {};

function loadKatex() {
  if (katexReady || katexLoading) return;
  katexLoading = true;
  // Inject stylesheet on demand (moved out of <head> to unblock first paint).
  // R219-SEC-4: KaTeX CDN link + script must carry SRI integrity hashes;
  // contract pinned by TestDashboardJS_CDNScriptsHaveSRI.
  if (!document.querySelector('link[data-nz-katex]')) {
    const link = document.createElement('link');
    link.rel = 'stylesheet';
    link.href = 'https://cdn.jsdelivr.net/npm/katex@0.16.21/dist/katex.min.css';
    link.integrity = 'sha384-zh0CIslj+VczCZtlzBcjt5ppRcsAmDnRem7ESsYwWwg3m/OaJ2l4x7YBZl9Kxxib';
    link.crossOrigin = 'anonymous';
    link.setAttribute('data-nz-katex', '1');
    document.head.appendChild(link);
  }
  const s = document.createElement('script');
  s.src = 'https://cdn.jsdelivr.net/npm/katex@0.16.21/dist/katex.min.js';
  s.integrity = 'sha384-Rma6DA2IPUwhNxmrB/7S3Tno0YY7sFu9WSYMCuulLhIqYSGZ2gKCJWIqhBWqMQfh';
  s.crossOrigin = 'anonymous';
  s.onload = () => {
    katexReady = true;
    katexLoading = false;
    runKatex();
  };
  s.onerror = () => { katexLoading = false; };
  document.head.appendChild(s);
}

function runKatex() {
  if (Object.keys(katexPending).length === 0) return;
  if (!katexReady) { loadKatex(); return; }
  Object.entries(katexPending).forEach(([id, info]) => {
    const el = document.getElementById(id);
    if (!el) { delete katexPending[id]; return; }
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
//      `)letter`. The function-call clause accepts `$h(x)$` / `$f(x)$` /
//      `$g(t)$` which the previous "must contain digit/operator" rule
//      mistakenly rejected (function references in prose carry no operator
//      character themselves). Pure prose tokens like `$(test)$` still
//      reject because they lack both a math hint and a function-call shape.
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
  if (katexReady) {
    try { return window.katex.renderToString(tex, { displayMode: displayMode, throwOnError: false }); }
    catch(_) { return esc(tex); }
  }
  const id = 'ktx-' + (++katexCounter);
  katexPending[id] = { tex: tex, display: displayMode };
  loadKatex();
  return '<span id="' + id + '" class="katex-pending">' + esc(tex) + '</span>';
}

// runPendingAsync — single post-render glue point for every async pipeline
// triggered by renderMd/renderRich output. Call sites that attach rendered
// HTML to the live DOM invoke this once; never call runKatex / runMermaid
// directly from feature code. Keeps chat bubbles, preview drawer, scratch
// drawer, aside drawer on one flush contract so future pipelines (syntax
// highlight etc.) plug in here without scattering across call sites.
function runPendingAsync() {
  runMermaid();
  runKatex();
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
// RNEW-PERF-003 (#454): cap cacheable input length at 2000 chars. The
// previous 20000-char cap caused two pathologies on streaming text events:
//
//  1. Cache MISS on every render — streaming `text` events grow by chunks,
//     so the cache key (full string) is unique per WS push. The Map.get
//     was always undefined, the work was always done from scratch.
//  2. Cache WRITE on every render evicted long-lived plain-text bubbles
//     (welcome banner, system prompts, short replies) that would otherwise
//     have been cheap repeat-hits as the user navigated views. Net cache
//     hit rate fell off a cliff once a long streaming reply landed.
//
// Plain replies under 2000 chars ARE the cache's intended audience —
// they're the ones that re-render on nav rebuild + preview poll without
// changing. Above that threshold the input is either a wall-of-text final
// reply (rendered once, never again — cache is a write-only bloat) or a
// still-streaming `running` event (key changes every push — cache never
// hits). Skip the cache write in both cases. The 2000-char threshold
// covers >95% of stable IM-style replies on naozhi without paying
// hash-the-string cost on streaming-storm responses.
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
  katexReady,
  loadKatex,
  loadMermaid,
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
