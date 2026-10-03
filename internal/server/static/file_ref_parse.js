// file_ref_parse.js — the path-shape tests shared by the file-ref scanner
// (file_refs.js) and the markdown renderer (render_md.js): the candidate
// regexes, path / line / note splitting, the fenced path-list classifier and
// the <code> wrapper both emit. A leaf (caps.leaves) with no imports: it sits
// below both, so render_md need not import file_refs, which imports render_md.

// Path candidate regex: accepts two shapes —
//   (a) path with at least one `/` (with optional :line / :line-line suffix).
//       e.g. `src/foo.go`, `./a/b.ts:42`, `manifests/ec2nodeclass.yaml:9`.
//   (b) bare filename that MUST carry a :line suffix to disambiguate from
//       prose. e.g. `option_install_gpu_nodegroups.sh:1838-1883`. Review
//       output often references a single-file path without any `/` prefix;
//       the line suffix is a strong signal it is in fact a file reference
//       rather than an English word that happens to contain a dot.
// Segments accept any non-whitespace, non-colon char so Unicode filenames
// (Chinese, Japanese, …) are not silently dropped. Absolute paths are
// resolved to project-relative form by resolveProjectForAbsPath before the
// server call — server still rejects absolute paths for defence in depth.
// Rejects spaces (breaks on prose) and leading URL schemes.
// Line suffix accepts `:L`, `:L-L2` (range) and `:L:C` (go build / eslint
// `file:line:col`); splitPathLine keeps only the line for the preview jump.
const FILE_REF_WITH_SLASH = /^(?:\.\.?\/|\/)?(?!https?:)[^\s:]+(?:\/[^\s:]+)+(?::\d+(?:-\d+|:\d+)?)?$/;
const FILE_REF_BARE_WITH_LINE = /^(?!https?:)[^\s:\/]+\.[A-Za-z0-9_]+:\d+(?:-\d+|:\d+)?$/;
export function isFileRefCandidate(text) {
  return FILE_REF_WITH_SLASH.test(text) || FILE_REF_BARE_WITH_LINE.test(text);
}

// Every path-list line's basename must carry a file extension (a `.ext` tail).
// isFileRefCandidate alone is too loose for whole-block classification:
// dependency lists (`@angular/core`), module paths (`github.com/gin-gonic/gin`),
// REST routes (`/api/v1/users`), and fractions/dates (`1/2`, `2024/01/02`) all
// match the slash-shaped path regex line-for-line and would hijack a legit
// no-language code block. Real file paths — including every case this fix
// targets — end in an extension, so this is a cheap high-signal gate that drops
// those false positives without losing the screenshot scenario (`.html` lists).
// Trailing `:line` suffixes are stripped by splitPathLine before this runs.
export const FILE_REF_HAS_EXT = /\.[A-Za-z0-9]+$/;

// splitPathNote separates a fenced path line into its path candidate and a
// trailing human annotation. AI replies commonly tag path lines with inline
// notes — `语文/...诊断.md   ← 待生成`, `bar.html  # 答案`, `foo.go (new)` —
// where the note is set off from the path by whitespace. A real file path never
// contains whitespace (isFileRefCandidate rejects spaces), so the first
// whitespace-delimited token IS the path candidate and everything after the gap
// is a note we preserve for display but exclude from the <code> body (so copy +
// the file-ref scanner see the bare path). Returns {path, note}; note is '' when
// the line is a lone path.
function splitPathNote(line) {
  const m = line.match(/^(\S+)(?:\s+(.*\S))?\s*$/);
  if (!m) return { path: line, note: '' };
  return { path: m[1], note: m[2] || '' };
}

// fencedPathList decides whether a language-less fenced code block is in fact
// a plain list of file paths (one per line). AI replies frequently dump
// generated/affected files inside a ``` fence — those paths are invisible to
// the inline file-ref scanner because it skips <pre> content. When EVERY
// non-empty line is a path candidate whose basename has an extension we return
// the parsed rows ({path, note}) so the caller can render them as clickable
// rows. Returns null otherwise, leaving the verbatim-code path untouched.
// Requiring all lines to match keeps real code blocks out: any block with one
// prose/code/extension-less line fails the test.
//
// A single path line is accepted (one generated file inside a ``` fence is a
// very common AI shape). The isFileRefCandidate + FILE_REF_HAS_EXT double gate
// plus the server-side existence check (a non-file that slips through resolves
// to {exists:false} and silently gets no button) make a lone-line list safe;
// the old "≥2 lines" guard left every single-file fence button-less.
//
// Trailing annotations (`foo.md   ← 待生成`) are stripped via splitPathNote so
// the note no longer breaks isFileRefCandidate (which rejects whitespace). The
// note is carried through for display but kept out of the path.
//
// Known non-goal: lines with trailing punctuation glued to the path
// (`foo.md。`) or inline backtick wrapping are not normalized here — they'd
// resolve to a non-existent path and silently get no button.
export function fencedPathList(code) {
  const lines = code.split('\n');
  const paths = [];
  for (const raw of lines) {
    const line = raw.trim();
    if (line === '') continue;        // blank lines are tolerated as spacing
    if (line.length > 512) return null;
    const { path, note } = splitPathNote(line);
    if (!isFileRefCandidate(path)) return null;
    const { path: bare } = splitPathLine(path);
    const base = bare.slice(bare.lastIndexOf('/') + 1);
    if (!FILE_REF_HAS_EXT.test(base)) return null; // no extension → not a file list
    paths.push({ path, note });
  }
  if (paths.length < 1) return null;  // empty fence: nothing to render
  return paths;
}

// Split a candidate like "src/foo.go:42" into {path, line}. Line is optional.
export function splitPathLine(cand) {
  // Optional trailing `:col` is dropped: the preview only scrolls to a line.
  const m = cand.match(/^(.+?):(\d+(?:-\d+)?)(?::\d+)?$/);
  if (m) return { path: m[1], line: m[2] };
  return { path: cand, line: '' };
}

// fileRefCode produces the inline <code> element that the file-ref scanner
// (scanEventForFileRefs, which walks `code, .md-code`) recognises as a path so
// it can attach [↗ preview][↓ download] buttons. Centralising the markup here
// keeps the three callsites (markdown-link rescue, CODE-token restore,
// fencedPathList row) in lockstep: a future change to the tag/class/attrs (e.g.
// adding data-file-ref) lands in one place instead of three divergent string
// literals where missing one would silently drop that path shape's buttons.
//
// Escaping contract: this helper does NOT escape `inner` — every callsite is
// responsible for its own escaping/guarding (esc(), tokenizer guards, or the
// `<`/`\x00` rejection in the link rescue). The helper only owns the wrapper.
//
// className: defaults to "md-code" (the inline-code pill used by backtick spans
// and the link rescue). fencedPathList passes "" to keep a bare <code>: the
// `.md-pathline code` CSS deliberately omits the .md-code pill background/
// padding/border-radius, so tagging those rows with .md-code would visibly turn
// each clean path row into a pill. Both shapes are still caught by the scanner's
// `code, .md-code` selector, so the buttons attach either way.
export function fileRefCode(inner, className) {
  const cls = className === undefined ? 'md-code' : className;
  return cls ? '<code class="' + cls + '">' + inner + '</code>'
             : '<code>' + inner + '</code>';
}
