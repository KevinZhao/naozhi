// file_refs.js — extracted from dashboard.js (#2558 D4).
//
// Verbatim region move: `git diff --color-moved` shows the body as a pure
// move; the import block, the deps table and the export block below are the
// only additions.
//
// Layering (D4-1 rule): a module dashboard imports must NOT import dashboard
// back — that cycle puts dashboard's own top-level consts in TDZ while this
// module evaluates. Dashboard state is read through nz.state; its helpers are
// injected once via configureFileRefs(), called from dashboard's module body.
import { esc, fetchJSON, nzState, showToast } from './nz_util.js';

const deps = {
  AVATAR_GROUP_GAP_MS: null,
  ICONS: null,
  collapseSidebarForDrawer: null,
  getToken: null,
  isInternalEvent: null,
  loadKatex: null,
  loadMermaid: null,
  matchProject: null,
  nzSplitBringToFront: null,
  nzSplitEnter: null,
  nzSplitExit: null,
  renderRich: null,
  restoreSidebarAfterDrawer: null,
  runPendingAsync: null,
};
export function configureFileRefs(impl) {
  for (const k of Object.keys(deps)) {
    if (typeof impl[k] === 'undefined') throw new Error('file_refs dep missing: ' + k);
    deps[k] = impl[k];
  }
}

// Late-bound hooks: assigned by the code below, read by other modules at event
// time (never at load time) — the shape they had as dashboard module-scope
// lets before this extraction.
let _activeCardEl = null;
let _pendingSnippet = null;

/* ===== File reference buttons ========================================= */
/* Scan event bubbles for path-shaped strings (inside <code> or literal),
 * verify existence against the active project workspace, and append
 * [preview] [download] buttons inline. Remote-friendly: lazy validation,
 * batched existence checks, only fetches file content when clicked. */

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
function isFileRefCandidate(text) {
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
const FILE_REF_HAS_EXT = /\.[A-Za-z0-9]+$/;

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
function fencedPathList(code) {
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

// expandBraces expands a single `{a,b,c}` group in a path candidate into its
// concrete variants so AI output like `foo-{x86,graviton}.yaml:9` resolves to
// `foo-x86.yaml:9` / `foo-graviton.yaml:9`. Only the first group is expanded
// — nested / multi-group patterns are uncommon in review output and
// exploding them would blow past the server's 100-path stat budget. Returns
// a single-element array with tag:'' when no expansion applies. Bail on
// empty alternatives or whitespace inside the group so we don't silently
// match prose like `{ foo }`. Each variant carries a `tag` (the branch
// alternative, e.g. `x86`) used to label the variant's button group.
function expandBraces(text) {
  const m = text.match(/^(.*?)\{([^{}\s]+)\}(.*)$/);
  if (!m) return [{ path: text, tag: '' }];
  const [, pre, inner, post] = m;
  if (!inner.includes(',')) return [{ path: text, tag: '' }];
  const parts = inner.split(',');
  const out = [];
  for (const p of parts) {
    if (p === '') return [{ path: text, tag: '' }]; // `{a,,b}` → not a valid expansion
    out.push({ path: pre + p + post, tag: p });
  }
  return out;
}

// resolveVariant maps a single concrete path to the owning project + the
// workspace-relative form the server accepts. Shared between single-path
// and brace-expanded scans so the project-resolution rules stay identical.
function resolveVariant(p, activeNode, activeProj) {
  if (p.startsWith('/')) {
    const hit = resolveProjectForAbsPath(p, activeNode);
    if (!hit) return null;
    return { projName: hit.name, projNode: hit.node, serverPath: hit.relPath };
  }
  if (!activeProj) return null;
  return {
    projName: activeProj.name,
    projNode: activeProj.node,
    serverPath: p.replace(/^\.\//, ''),
  };
}

// Per-project path validation cache: key = "<project>|<path>" → entry.
// TTL 60s so mtime changes re-verify eventually without the user needing
// to refresh; short enough that server-side edits propagate within one
// round of scrolling back.
const _filePathCache = new Map();
const _FILE_PATH_CACHE_MAX = 2000;
const _FILE_PATH_CACHE_TTL = 60 * 1000;

// Pending batch of path candidates waiting for /api/projects/files/exists.
let _fileRefPendingBatch = null; // { project, node, paths: Map<string, HTMLElement[]> }
let _fileRefBatchTimer = null;
const _FILE_REF_BATCH_DELAY = 120; // ms
const _FILE_REF_BATCH_MAX = 80; // paths per request (server caps at 100)

// resolveActiveProject infers which project owns the currently selected
// session, so inline path chips query the right workspace. Falls back to
// longest-prefix match on the session's workspace dir; returns null if
// we cannot determine a project.
function resolveActiveProject() {
  if (!nzState.selectedKey) return null;
  const sKey = sid(nzState.selectedKey, nzState.selectedNode);
  const sd = nzState.sessionsData[sKey];
  if (!sd) return null;
  const name = sd.project || deps.matchProject(sd.workspace);
  if (!name) return null;
  return { name, node: nzState.selectedNode || 'local' };
}

// Split a candidate like "src/foo.go:42" into {path, line}. Line is optional.
function splitPathLine(cand) {
  // Optional trailing `:col` is dropped: the preview only scrolls to a line.
  const m = cand.match(/^(.+?):(\d+(?:-\d+)?)(?::\d+)?$/);
  if (m) return { path: m[1], line: m[2] };
  return { path: cand, line: '' };
}

// resolveProjectForAbsPath maps an absolute path (e.g. `/home/.../gaokao/x.md`)
// to the owning project on the given node. Returns { name, node, relPath }
// on match, or null. The server rejects absolute paths by contract (see
// resolveProjectFileWithRoot); doing the conversion here keeps that boundary
// intact while letting AI output that quotes absolute paths — which claude CLI
// routinely does — still produce preview buttons.
//
// Scoping rules:
//   - node must match (cross-node abs paths get no preview).
//   - longest-prefix wins when a project contains nested projects.
//   - path must be strictly inside the project dir (no prefix-only match like
//     `/foo/barfoo` matching project `/foo/bar`).
function resolveProjectForAbsPath(abs, node) {
  if (!abs || !abs.startsWith('/') || !nzState.projectsData || nzState.projectsData.length === 0) return null;
  const wantNode = node || 'local';
  let best = null, bestLen = 0;
  for (const p of nzState.projectsData) {
    if ((p.node || 'local') !== wantNode) continue;
    if (!p.path) continue;
    const prefix = p.path.endsWith('/') ? p.path : p.path + '/';
    if (abs === p.path || abs.startsWith(prefix)) {
      if (p.path.length > bestLen) {
        best = p; bestLen = p.path.length;
      }
    }
  }
  if (!best) return null;
  let rel = abs === best.path ? '' : abs.slice(best.path.length);
  if (rel.startsWith('/')) rel = rel.slice(1);
  if (!rel) return null; // pointing at project root itself is not a file
  return { name: best.name, node: best.node || 'local', relPath: rel };
}

function _fileRefCacheGet(key) {
  const hit = _filePathCache.get(key);
  if (!hit) return null;
  if (Date.now() - hit.t > _FILE_PATH_CACHE_TTL) {
    _filePathCache.delete(key);
    return null;
  }
  return hit.v;
}

function _fileRefCacheSet(key, value) {
  if (_filePathCache.size >= _FILE_PATH_CACHE_MAX) {
    const firstKey = _filePathCache.keys().next().value;
    _filePathCache.delete(firstKey);
  }
  _filePathCache.set(key, { v: value, t: Date.now() });
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
function fileRefCode(inner, className) {
  const cls = className === undefined ? 'md-code' : className;
  return cls ? '<code class="' + cls + '">' + inner + '</code>'
             : '<code>' + inner + '</code>';
}

// scanEventForFileRefs walks .event-content <code> descendants of a freshly-
// inserted event bubble and wraps any path-shaped literals in a .file-ref
// span with data-* attrs so verification + button injection can run async.
//
// Absolute paths (e.g. AI output that says `/home/.../gaokao/化学/foo.md`) are
// mapped to the owning project on the active session's node via
// resolveProjectForAbsPath — they may belong to a project other than the one
// matched by the session's workspace (think: a session rooted at repo A that
// references a file in repo B on the same host). dataset.displayPath keeps
// the original string for the button's aria-label/title/preview header so the
// user sees what they expect, while dataset.path holds the project-relative
// form the server accepts.
function scanEventForFileRefs(eventEl) {
  const activeProj = resolveActiveProject();
  // activeProj is optional now: we can still resolve absolute paths via
  // nzState.projectsData alone as long as we know the node. Fall back to the selected
  // session's node when no project match exists for the session itself.
  const activeNode = activeProj ? activeProj.node :
    (nzState.selectedKey ? (nzState.selectedNode || 'local') : null);
  if (!activeNode) return;
  // Selector covers both shapes of container:
  //   - chat bubbles: `.event > .event-content > code/.md-code`
  //   - preview drawer: `.fv-rich > code/.md-code`
  //   - future drawers (scratch/aside): same contract — we only care about
  //     code-shaped inline elements inside the passed root, regardless of
  //     the intermediate wrapper class.
  const codeEls = eventEl.querySelectorAll('code, .md-code');
  codeEls.forEach(code => {
    if (code.dataset.frScanned === '1') return;
    code.dataset.frScanned = '1';
    const text = (code.textContent || '').trim();
    if (!text || text.length > 512) return; // absurdly long paths skip
    if (!isFileRefCandidate(text)) return;
    // Skip when nested inside <a> (authored link target).
    if (code.closest('a')) return;
    // Skip fenced code blocks (<pre><code>): those are content, not refs.
    if (code.closest('pre')) return;
    const { path, line } = splitPathLine(text);
    const variants = expandBraces(path);

    // Shared wrap hosts the original <code> element plus one per-variant
    // button pair. Without brace expansion there is exactly one variant so
    // the DOM shape matches the pre-expansion code path. With expansion,
    // each variant adds its own [↗][↓] group labelled with the alternative
    // so clicking the x86 arrows opens ec2nodeclass-x86.yaml rather than
    // guessing which branch the user meant.
    const wrap = document.createElement('span');
    wrap.className = 'file-ref';
    code.parentNode.insertBefore(wrap, code);
    wrap.appendChild(code);

    for (const v of variants) {
      const resolved = resolveVariant(v.path, activeNode, activeProj);
      if (!resolved) continue;
      const slot = document.createElement('span');
      slot.className = 'fr-slot fr-candidate';
      slot.dataset.path = resolved.serverPath;       // what we send to the server
      slot.dataset.displayPath = v.path;             // what the user typed / saw
      slot.dataset.line = line;
      slot.dataset.project = resolved.projName;
      slot.dataset.node = resolved.projNode;
      if (v.tag) slot.dataset.variantTag = v.tag;
      wrap.appendChild(slot);
      queueFileRefCheck(slot);
    }
    // No resolvable variants — remove the empty wrap so the original <code>
    // is left in place for the user to copy.
    if (!wrap.querySelector('.fr-slot')) {
      wrap.parentNode.insertBefore(code, wrap);
      wrap.remove();
    }
  });
}

function queueFileRefCheck(wrapEl) {
  const proj = wrapEl.dataset.project;
  const node = wrapEl.dataset.node || 'local';
  const path = wrapEl.dataset.path;
  const cacheKey = proj + '|' + node + '|' + path;
  const cached = _fileRefCacheGet(cacheKey);
  if (cached) {
    applyFileRefResult(wrapEl, cached);
    return;
  }
  if (!_fileRefPendingBatch || _fileRefPendingBatch.project !== proj || _fileRefPendingBatch.node !== node) {
    flushFileRefBatch();
    _fileRefPendingBatch = { project: proj, node, paths: new Map() };
  }
  if (!_fileRefPendingBatch.paths.has(path)) _fileRefPendingBatch.paths.set(path, []);
  _fileRefPendingBatch.paths.get(path).push(wrapEl);
  // Flush if we hit per-request cap.
  if (_fileRefPendingBatch.paths.size >= _FILE_REF_BATCH_MAX) {
    flushFileRefBatch();
    return;
  }
  if (_fileRefBatchTimer) return;
  _fileRefBatchTimer = setTimeout(flushFileRefBatch, _FILE_REF_BATCH_DELAY);
}

async function flushFileRefBatch() {
  if (_fileRefBatchTimer) { clearTimeout(_fileRefBatchTimer); _fileRefBatchTimer = null; }
  const batch = _fileRefPendingBatch;
  _fileRefPendingBatch = null;
  if (!batch || batch.paths.size === 0) return;

  const paths = Array.from(batch.paths.keys());
  try {
    const headers = { 'Content-Type': 'application/json' };
    const t = deps.getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    // RNEW-UX-003: 10s timeout — batch exists-check touches the FS for every
    // path; a stalled disk shouldn't leak pending renders forever.
    let data;
    try {
      data = await fetchJSON(NZ_CONTRACT.API.projects_files_exists, {
        method: 'POST', headers,
        body: JSON.stringify({ project: batch.project, node: batch.node, paths }),
        timeoutMs: 10000,
      });
    } catch (err) {
      if (err.status) return;
      throw err;
    }
    const results = (data && data.results) || {};
    for (const p of paths) {
      const entry = results[p] || { exists: false };
      const cacheKey = batch.project + '|' + batch.node + '|' + p;
      _fileRefCacheSet(cacheKey, entry);
      const els = batch.paths.get(p) || [];
      els.forEach(wrap => applyFileRefResult(wrap, entry));
    }
  } catch (_) { /* network failure: leave candidates as-is */ }
}

function applyFileRefResult(wrapEl, entry) {
  if (!entry || !entry.exists || entry.is_dir) {
    wrapEl.classList.remove('fr-candidate');
    wrapEl.classList.add('fr-missing');
    return;
  }
  if (wrapEl.querySelector('.fr-btn')) return; // already injected
  wrapEl.classList.remove('fr-candidate');
  wrapEl.classList.add('fr-verified');
  wrapEl.dataset.size = entry.size || 0;
  wrapEl.dataset.mime = entry.mime || '';
  // Preview and download share the same visual size \u2014 single-glyph icons
  // with an aria-label for accessibility so assistive tech still announces
  // "Preview" / "Download" clearly. Labels use displayPath (original string
  // the AI output, e.g. an absolute path) so the tooltip matches what the
  // user sees in the bubble \u2014 wrapEl.dataset.path may be the rewritten
  // project-relative form.
  const label = wrapEl.dataset.displayPath || wrapEl.dataset.path;
  // Brace-expanded variants carry a human-visible tag (e.g. "x86" /
  // "graviton") so the user can tell paired button groups apart when the
  // same line mentions foo-{x86,graviton}.yaml.
  if (wrapEl.dataset.variantTag) {
    const tag = document.createElement('span');
    tag.className = 'fr-tag';
    tag.textContent = wrapEl.dataset.variantTag;
    tag.title = label;
    wrapEl.appendChild(tag);
  }
  const preview = document.createElement('button');
  preview.type = 'button';
  preview.className = 'fr-btn fr-btn-preview';
  preview.textContent = deps.ICONS.preview; // paired with deps.ICONS.downArrow for symmetric arrow look
  preview.setAttribute('aria-label', 'Preview ' + label);
  preview.title = 'Preview ' + label;
  preview.addEventListener('click', evt => {
    evt.preventDefault();
    evt.stopPropagation();
    openFilePreview(wrapEl);
  });
  const download = document.createElement('button');
  download.type = 'button';
  download.className = 'fr-btn fr-btn-download';
  download.textContent = deps.ICONS.downArrow;
  download.setAttribute('aria-label', 'Download ' + label);
  download.title = 'Download ' + label;
  download.addEventListener('click', evt => {
    evt.preventDefault();
    evt.stopPropagation();
    triggerFileDownload(wrapEl);
  });
  wrapEl.appendChild(preview);
  wrapEl.appendChild(download);
}

function fileApiUrl(project, node, path, mode) {
  const qs = 'project=' + encodeURIComponent(project) +
    '&path=' + encodeURIComponent(path) +
    '&mode=' + encodeURIComponent(mode) +
    (node && node !== 'local' ? '&node=' + encodeURIComponent(node) : '');
  return NZ_CONTRACT.API.projects_file + '?' + qs;
}

function triggerFileDownload(wrapEl) {
  const url = fileApiUrl(wrapEl.dataset.project, wrapEl.dataset.node, wrapEl.dataset.path, 'download');
  // Use a transient anchor so the token-auth cookie is sent with the GET.
  const a = document.createElement('a');
  a.href = url;
  a.download = (wrapEl.dataset.path.split('/').pop() || 'file');
  a.rel = 'noopener';
  document.body.appendChild(a);
  a.click();
  a.remove();
}

async function openFilePreview(wrapEl) {
  const drawer = document.getElementById('fv-drawer');
  const body = document.getElementById('fv-body');
  const title = document.getElementById('fv-title');
  const meta = document.getElementById('fv-meta');
  if (!drawer || !body || !title || !meta) return;
  // Warm-start async renderers the moment the drawer opens. deps.loadKatex /
  // deps.loadMermaid are idempotent no-ops once ready; kicking them off in
  // parallel with the preview fetch eliminates first-open pending flicker
  // on .md / .tex files that contain math or diagrams.
  deps.loadKatex();
  deps.loadMermaid();
  const project = wrapEl.dataset.project;
  const node = wrapEl.dataset.node;
  const path = wrapEl.dataset.path;
  const line = wrapEl.dataset.line || '';
  const mime = wrapEl.dataset.mime || '';
  const size = +wrapEl.dataset.size || 0;

  drawer.classList.remove('hidden');
  drawer.classList.add('fv-open');
  // Dock as a right-hand split on desktop so the transcript stays visible
  // beside the preview (no-op on phone — falls back to the overlay).
  if (deps.nzSplitEnter) deps.nzSplitEnter();
  // Opened last → stack on top of the 追问 pane if both are docked.
  if (deps.nzSplitBringToFront) deps.nzSplitBringToFront('preview');
  deps.collapseSidebarForDrawer();
  drawer.dataset.project = project;
  drawer.dataset.node = node;
  drawer.dataset.path = path;
  // Show the original string (may be abs) in the header so the user can
  // still match the preview to the bubble they clicked; server calls below
  // use the workspace-relative `path`.
  const headerPath = wrapEl.dataset.displayPath || path;
  title.textContent = headerPath + (line ? ':' + line : '');
  meta.textContent = (mime ? mime + ' \u00b7 ' : '') + formatFileSize(size);
  body.innerHTML = '<div class="fv-loading">loading\u2026</div>';

  // SVG must be checked BEFORE the generic image/ branch: image/svg+xml starts
  // with "image/" but cannot flow through <img src=...mode=raw>. The server
  // refuses inline SVG via raw (project_files.go: serveRaw rejects svg+xml)
  // because SVG can embed <script> and on* handlers that execute same-origin
  // on top-level navigation. Route through the sandboxed-blob path instead,
  // which serves attachment/octet-stream from the server and wraps the bytes
  // in a Blob with type=image/svg+xml client-side.
  if (mime.startsWith('image/svg+xml')) {
    renderSandboxedBlob(project, node, path, body, 'image/svg+xml');
    return;
  }
  // Image / PDF: use raw endpoint directly, no JSON round trip.
  if (mime.startsWith('image/')) {
    body.innerHTML = '';
    const img = document.createElement('img');
    img.src = fileApiUrl(project, node, path, 'raw');
    img.alt = path;
    img.loading = 'lazy';
    body.appendChild(img);
    return;
  }
  if (mime === 'application/pdf') {
    body.innerHTML = '';
    const frame = document.createElement('iframe');
    // Defense-in-depth: serveRaw forces application/pdf to an attachment
    // download so this never inline-renders today. sandbox="" guards the
    // case where Content-Disposition is stripped (proxy) or serveRaw later
    // renders PDF inline — zero capabilities, no plugin/script execution,
    // while the native PDF viewer still works.
    frame.setAttribute('sandbox', '');
    frame.src = fileApiUrl(project, node, path, 'raw');
    frame.title = path;
    body.appendChild(frame);
    return;
  }
  // HTML / XHTML: render via blob URL inside a sandboxed iframe.
  //
  // Why blob + sandbox instead of `iframe.src = fileApiUrl(...render)`:
  // Firefox ignores the HTTP `Content-Security-Policy: sandbox` directive
  // on top-level navigation, so a direct-URL open would run workspace HTML
  // same-origin to the dashboard → stored-XSS via the Claude CLI Write tool.
  // The server returns the bytes as `application/octet-stream + attachment`
  // specifically so that a direct URL hit DOWNLOADS instead of renders.
  // Client-side we fetch, wrap bytes in a Blob({type:'text/html'}), and
  // feed the blob: URL into the iframe — blob origins are opaque, so even
  // if sandbox is stripped the document cannot read dashboard cookies.
  if (mime.startsWith('text/html') || mime.startsWith('application/xhtml')) {
    renderSandboxedBlob(project, node, path, body, 'text/html');
    return;
  }

  // Text / unknown: go through preview endpoint which returns structured JSON.
  try {
    const headers = {};
    const t = deps.getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const r = await fetch(fileApiUrl(project, node, path, 'preview'), { headers });
    if (!r.ok) {
      body.innerHTML = '<div class="fv-error">preview failed (' + r.status + ')</div>';
      return;
    }
    const data = await r.json();
    if (data.binary) {
      const binMime = String(data.mime || '');
      // HTML / XHTML / SVG land in `binary:true` by design (R176-SEC-H3:
      // active-content bytes never flow through the preview JSON content
      // field). Upgrade to the sandboxed blob render instead of showing a
      // "please download" placeholder — that's the whole point of render
      // mode. Blob type matches the source MIME so the iframe parses bytes
      // as the right document type.
      if (binMime.startsWith('text/html') || binMime.startsWith('application/xhtml')) {
        renderSandboxedBlob(project, node, path, body, 'text/html');
        return;
      }
      if (binMime.startsWith('image/svg+xml')) {
        renderSandboxedBlob(project, node, path, body, 'image/svg+xml');
        return;
      }
      body.innerHTML = '<div class="fv-binary">Binary file — click <strong>download</strong> to save.<span class="fv-mime">' + esc(binMime) + '</span></div>';
      return;
    }
    const parts = [];
    if (data.truncated) {
      parts.push('<div class="fv-truncated">file truncated at ' + formatFileSize(1024 * 1024) + ' (total ' + formatFileSize(data.size || 0) + ') — download for full content</div>');
    }
    const lang = inferLang(path, data.mime || '');
    // Route through deps.renderRich — same renderer chat bubbles use so behaviour
    // (math, mermaid, tables, lists, file-refs) stays consistent across
    // surfaces. Source-code files keep the line-number gutter layout.
    if (lang === 'markdown' || lang === 'tex') {
      const mode = lang === 'tex' ? 'tex' : 'markdown';
      parts.push('<div class="fv-rich">' + deps.renderRich(data.content || '', { mode: mode }) + '</div>');
    } else {
      const raw = data.content || '';
      const lines = raw.split('\n');
      const gutter = lines.map((_, i) => String(i + 1)).join('\n');
      parts.push('<pre class="fv-lined"><span class="fv-gutter" aria-hidden="true">' + gutter + '</span><code class="fv-code">' + esc(raw) + '</code></pre>');
    }
    body.innerHTML = parts.join('');
    // Flush KaTeX / Mermaid pending slots produced by deps.renderRich above.
    // Without this call, first-open of a .md file with math would leave
    // `<span class="katex-pending">` placeholders on screen until a chat
    // render happened to fire from another code path.
    deps.runPendingAsync();
    // Mirror chat-side file-ref chip injection so paths inside the preview
    // body also get [preview]/[download] affordances.
    {
      body.querySelectorAll('.fv-rich').forEach(scanEventForFileRefs);
    }
    if (line) scrollToPreviewLine(body, parseInt(line, 10));
  } catch (e) {
    body.innerHTML = '<div class="fv-error">' + esc(String(e && e.message || e)) + '</div>';
  }
}

// renderSandboxedBlob points a sandboxed iframe at the server's inline
// render endpoint (mode=render&inline=1). The iframe document's CSP comes
// from that response — NOT inherited from the dashboard page — which is what
// keeps workspace HTML (MathJax / KaTeX / Mermaid) rendering after #1980
// dropped script-src 'unsafe-inline' from the dashboard CSP: blob: and inline-doc
// documents inherit the parent policy in current engines (measured in
// docs/rfc/csp-data-action.md §4), so the old fetch→Blob desktop path and
// the mobile inline-doc fallback would both have gone dead. Pointing src at the
// endpoint also retires the WebKit blob-frame bug workaround and the old
// fallback UTF-8/SVG parsing trade-offs — one path for every platform.
//
// Defense layers (unchanged in spirit from the blob era):
//   (1) The endpoint's inline form answers only requests stamped
//       Sec-Fetch-Dest: iframe, so a direct URL hit gets 403 (covers
//       Firefox's CSP-sandbox top-level-navigation gap); the legacy
//       fetch form stays octet-stream + attachment.
//   (2) The response carries `Content-Security-Policy: sandbox
//       allow-scripts …` — an opaque origin regardless of embedding.
//   (3) sandbox='allow-scripts' on the iframe withholds the same-origin
//       token — the document cannot read dashboard cookies, storage, or
//       DOM. Scripts are required so workspace HTML using MathJax / KaTeX /
//       Mermaid / chart libs renders. The contract test
//       (TestDashboardJS_SandboxedBlobRender) substring-matches this helper
//       body for forbidden tokens, so comments must NEVER spell them out.
// The name keeps its historic "Blob" for the window-bridge/API stability;
// the body param is the container element, kept as-is; blobType is unused
// since the server's detected MIME now drives parsing, kept for call-site
// compatibility until #2557 PR-E trims the bridge.
function renderSandboxedBlob(project, node, path, body, blobType) {
  void blobType;
  body.innerHTML = '';
  const frame = document.createElement('iframe');
  frame.title = path;
  frame.setAttribute('sandbox', 'allow-scripts');
  frame.referrerPolicy = 'no-referrer';
  frame.src = fileApiUrl(project, node, path, 'render') + '&inline=1';
  body.appendChild(frame);
}

function scrollToPreviewLine(body, line) {
  if (!line || line < 1) return;
  const pre = body.querySelector('pre');
  if (!pre) return;
  // Approximate scroll: average line height in our monospace pre is ~18px.
  // Good enough for remote-dashboard purposes; precise highlighting would
  // require splitting every line into a <span> and costs too much for
  // the marginal "scroll near line 42" benefit.
  pre.parentElement.scrollTop = Math.max(0, (line - 3) * 18);
}

// formatFileSize renders a byte count as a short human label (e.g. "1.2 MB").
// Single declaration on purpose: a second hoisted `function formatFileSize`
// used to shadow this one silently. Promotion checks the *rounded* value so
// 1048575 B renders "1.0 MB" rather than "1024.0 KB".
function formatFileSize(bytes) {
  if (!bytes || bytes <= 0) return '';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let v = bytes, i = 0;
  while (i < units.length - 1 && (i === 0 ? v >= 1024 : Number(v.toFixed(1)) >= 1024)) {
    v /= 1024;
    i++;
  }
  return i === 0 ? v + ' B' : v.toFixed(1) + ' ' + units[i];
}

function inferLang(path, mime) {
  const ext = (path.split('.').pop() || '').toLowerCase();
  if (ext === 'md' || ext === 'markdown') return 'markdown';
  if (ext === 'tex' || ext === 'latex') return 'tex';
  if (mime === 'text/markdown') return 'markdown';
  if (mime === 'text/x-tex' || mime === 'application/x-tex') return 'tex';
  return '';
}

function closeFilePreview() {
  const drawer = document.getElementById('fv-drawer');
  if (!drawer) return;
  drawer.classList.remove('fv-open');
  drawer.classList.add('hidden');
  // Undock the split (no-op if the 追问 drawer is still open).
  if (deps.nzSplitExit) deps.nzSplitExit();
  // Re-expand the sidebar if the open path auto-collapsed it (no-op if the
  // 追问 drawer is still open or the user collapsed it themselves).
  deps.restoreSidebarAfterDrawer();
  delete drawer.dataset.snippetMode;
  delete drawer.dataset.snippetName;
  _pendingSnippet = null;
  const body = document.getElementById('fv-body');
  if (body) body.innerHTML = '';
}

// Wire drawer buttons once on load.
document.addEventListener('DOMContentLoaded', function () {
  const close = document.getElementById('fv-btn-close');
  if (close) close.addEventListener('click', closeFilePreview);
  const copy = document.getElementById('fv-btn-copy');
  if (copy) copy.addEventListener('click', () => {
    const drawer = document.getElementById('fv-drawer');
    if (!drawer) return;
    const isSnippet = drawer.dataset.snippetMode === '1';
    const text = isSnippet ? _pendingSnippet : drawer.dataset.path;
    if (!text) return;
    const label = isSnippet ? '片段已复制' : '路径已复制';
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(
        () => showToast(label, 'success', 1000),
        () => showToast('复制失败', 'warning', 1000)
      );
    }
  });
  const download = document.getElementById('fv-btn-download');
  if (download) download.addEventListener('click', () => {
    const drawer = document.getElementById('fv-drawer');
    if (!drawer) return;
    // Snippet mode: download the inline code via a blob URL.
    if (drawer.dataset.snippetMode === '1' && _pendingSnippet) {
      const blob = new Blob([_pendingSnippet], { type: 'text/plain;charset=utf-8' });
      const url = URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = url;
      a.download = drawer.dataset.snippetName || 'snippet.txt';
      a.rel = 'noopener';
      document.body.appendChild(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
      return;
    }
    if (!drawer.dataset.path) return;
    triggerFileDownload({ dataset: drawer.dataset });
  });
  // Esc closes drawer (but only when nothing else is handling Esc).
  document.addEventListener('keydown', evt => {
    if (evt.key !== 'Escape') return;
    const drawer = document.getElementById('fv-drawer');
    if (drawer && !drawer.classList.contains('hidden')) {
      evt.stopPropagation();
      closeFilePreview();
    }
  }, true);
});

// Observe #events-scroll so every newly-inserted event bubble gets scanned
// for file-ref candidates. Using a MutationObserver lets us stay out of the
// existing render pipelines (eventHtml / WS handlers) — they keep producing
// the same HTML, we just enhance it post-insertion.
//
// renderMainShell rebuilds the #events-scroll DOM on every session switch,
// so we track the active observer and disconnect it whenever the target
// element is replaced. Without this, stale observers pile up in memory
// across rapid session switches (one per switch), and the old observer
// would silently re-trigger if the DOM node was ever reparented.
let _fileRefObserver = null;
let _fileRefObserverTarget = null;

function startFileRefObserver() {
  const target = document.getElementById('events-scroll');
  if (!target) return;
  if (_fileRefObserverTarget === target) return; // already wired to this DOM
  if (_fileRefObserver) {
    _fileRefObserver.disconnect();
    _fileRefObserver = null;
  }
  _fileRefObserverTarget = target;
  const mo = new MutationObserver(mutations => {
    for (const m of mutations) {
      m.addedNodes.forEach(node => {
        if (!(node instanceof HTMLElement)) return;
        if (node.classList && node.classList.contains('event')) {
          scanEventForFileRefs(node);
        } else if (node.querySelectorAll) {
          node.querySelectorAll('.event').forEach(scanEventForFileRefs);
        }
      });
    }
    // Re-evaluate avatar grouping whenever bubbles are inserted/removed. The
    // decision is relative to the PREVIOUS visible same-sender bubble, which
    // an incremental append can't know without looking back — so we rescan
    // the (DOM-bounded, ≤MAX_LIVE_DOM_EVENTS) container rather than diffing.
    regroupAvatars(target);
  });
  mo.observe(target, { childList: true, subtree: false });
  _fileRefObserver = mo;
  // Initial scan for bubbles rendered synchronously before the observer
  // attached (e.g. the full-history render on session select).
  target.querySelectorAll('.event').forEach(scanEventForFileRefs);
  regroupAvatars(target);
}

// regroupAvatars walks the rendered transcript and tags each .event bubble
// with .nz-grouped when it continues a same-sender run within
// deps.AVATAR_GROUP_GAP_MS — the CSS then hides the repeated avatar. Only "user"
// and "text" (assistant) bubbles carry avatars and participate; any other
// visible bubble type (system/init/todo/result/…) or a sender switch or a
// time gap ≥ threshold resets the run so the next same-sender bubble shows
// its avatar again. Bubbles without a data-time are treated as run-breakers
// (conservatively keep their avatar). Idempotent: safe to call on every
// mutation; it toggles the class to match current DOM order.
function regroupAvatars(container) {
  const el = container || document.getElementById('events-scroll');
  if (!el) return;
  let prevSender = '';
  let prevTime = 0;
  for (const node of el.children) {
    if (!node.classList || !node.classList.contains('event')) continue;
    const isUser = node.classList.contains('user');
    const isText = node.classList.contains('text');
    if (!isUser && !isText) {
      // A non-avatar bubble (system/divider handled separately) breaks the run.
      prevSender = '';
      prevTime = 0;
      continue;
    }
    const sender = isUser ? 'user' : 'text';
    const t = Number(node.getAttribute('data-time') || 0);
    const grouped = !!t && sender === prevSender && prevTime > 0 &&
      (t - prevTime) < deps.AVATAR_GROUP_GAP_MS;
    node.classList.toggle('nz-grouped', grouped);
    prevSender = sender;
    prevTime = t; // 0 when undated → next bubble can't group against it
  }
}



function processEventsForDisplay(events) {
  return events.filter(e => !deps.isInternalEvent(e));
}

function sid(key, node) { return key + '\t' + (node || 'local'); }

// setActiveSessionCard flips the .active class on at most one session card.
// Replaces the old O(N) querySelectorAll('.session-card').forEach pattern
// with a cached reference (_activeCardEl). key===null drops selection
// altogether (used by openCronPanel / previewDiscovered clear paths). Node
// defaults to 'local' to match data-node attribute emission. A subsequent
// card with the same key but a different node counts as "different" — the
// data-key + data-node pair is the identity.
function setActiveSessionCard(key, node) {
  const n = node || 'local';
  // Drop stale cached ref if the previous card was detached by a sidebar
  // rebuild (renderSidebar replaces list.innerHTML wholesale).
  if (_activeCardEl && !_activeCardEl.isConnected) _activeCardEl = null;
  if (_activeCardEl) _activeCardEl.classList.remove('active');
  _activeCardEl = null;
  if (key === null || key === undefined) return null;
  const next = document.querySelector(
    '.session-card[data-key="' + (window.CSS && CSS.escape ? CSS.escape(key) : key) + '"]'
    + '[data-node="' + (window.CSS && CSS.escape ? CSS.escape(n) : n) + '"]'
  );
  if (next) {
    next.classList.add('active');
    _activeCardEl = next;
  }
  return next;
}

function isMultiNode() {
  const keys = Object.keys(nzState.nodesData);
  return keys.length > 1 || (keys.length === 1 && keys[0] !== 'local');
}

const NODE_BADGE_COLORS = ['#1f6feb','#0550ae','#1a7f37','#6e40c9','#9a6700','#cf222e'];
function nodeColor(id) {
  let h = 0;
  for (let i = 0; i < id.length; i++) h = (h * 31 + id.charCodeAt(i)) >>> 0;
  return NODE_BADGE_COLORS[h % NODE_BADGE_COLORS.length];
}


export {
  FILE_REF_HAS_EXT,
  _activeCardEl,
  _pendingSnippet,
  closeFilePreview,
  fencedPathList,
  fileApiUrl,
  fileRefCode,
  formatFileSize,
  isFileRefCandidate,
  isMultiNode,
  nodeColor,
  processEventsForDisplay,
  regroupAvatars,
  renderSandboxedBlob,
  setActiveSessionCard,
  sid,
  splitPathLine,
  startFileRefObserver,
};
