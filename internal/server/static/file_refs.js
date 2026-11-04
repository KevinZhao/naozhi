// file_refs.js — file references in the transcript: existence checks, the
// file preview drawer and avatar regrouping. Layering: never import
// dashboard.js back; the path parsing shared with render_md.js lives in the
// file_ref_parse.js leaf.
import { NZ_CONTRACT } from './contract.js';
import { selection, sessionList } from './state.js';
import { esc, fetchJSON, nzBus, showToast } from './nz_util.js';
import { workbench } from './workbench.js';
import { REMOTE_FILES_UNSUPPORTED, isInternalEvent, matchProject, sid } from './session_ident.js';
import { AVATAR_GROUP_GAP_MS, copyText, formatFileSize } from './utilities.js';
import { ICONS } from './icons.js';
import { isFileRefCandidate, splitPathLine } from './file_ref_parse.js';
import { loadKatex, loadMermaid, renderRich, runPendingAsync } from './render_md.js';

// Late-bound hooks: assigned by the code below, read by other modules at event
// time (never at load time) — the shape they had as dashboard module-scope
// lets before this extraction.
let _pendingSnippet = null;

/* ===== File reference buttons ========================================= */
/* Scan event bubbles for path-shaped strings (inside <code> or literal),
 * verify existence against the active project workspace, and append
 * [preview] [download] buttons inline. Remote-friendly: lazy validation,
 * batched existence checks, only fetches file content when clicked. */

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
  if (!selection.key) return null;
  const sKey = sid(selection.key, selection.node);
  const sd = sessionList.sessionsData[sKey];
  if (!sd) return null;
  const name = sd.project || matchProject(sd.workspace);
  if (!name) return null;
  return { name, node: selection.node || 'local' };
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
  if (!abs || !abs.startsWith('/') || !sessionList.projectsData || sessionList.projectsData.length === 0) return null;
  const wantNode = node || 'local';
  let best = null, bestLen = 0;
  for (const p of sessionList.projectsData) {
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
  // sessionList.projectsData alone as long as we know the node. Fall back to the selected
  // session's node when no project match exists for the session itself.
  const activeNode = activeProj ? activeProj.node :
    (selection.key ? (selection.node || 'local') : null);
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
    // 10s timeout — batch exists-check touches the FS for every
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
  preview.textContent = ICONS.preview; // paired with ICONS.downArrow for symmetric arrow look
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
  download.textContent = ICONS.downArrow;
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

// project may also be a "session:<key>" root (the files view browsing a
// session's working tree), sent as root= instead; project names cannot
// contain ':', so the two never collide.
function fileApiUrl(project, node, path, mode) {
  const param = (project && project.startsWith('session:')) ? 'root=' : 'project=';
  const qs = param + encodeURIComponent(project) +
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

// renderPreviewByMime renders the types that never go through the preview
// JSON (SVG, images, PDF, HTML) straight from the raw / render endpoints, and
// reports whether it did. fresh (a follow refresh) bypasses the image cache.
function renderPreviewByMime(project, node, path, body, mime, fresh) {
  // SVG must be checked BEFORE the generic image/ branch: image/svg+xml starts
  // with "image/" but cannot flow through <img src=...mode=raw>. The server
  // refuses inline SVG via raw (project_files.go: serveRaw rejects svg+xml)
  // because SVG can embed <script> and on* handlers that execute same-origin
  // on top-level navigation. It renders in the sandboxed iframe instead, the
  // same way HTML does.
  if (mime.startsWith('image/svg+xml')) {
    renderSandboxedBlob(project, node, path, body, 'image/svg+xml');
    return true;
  }
  if (mime.startsWith('image/')) {
    body.innerHTML = '';
    const img = document.createElement('img');
    img.src = fileApiUrl(project, node, path, 'raw') + (fresh ? '&t=' + Date.now() : '');
    img.alt = path;
    img.loading = 'lazy';
    body.appendChild(img);
    return true;
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
    return true;
  }
  // HTML / XHTML: render in a sandboxed iframe under its directory's preview
  // link (or the server's inline render form); renderSandboxedBlob below
  // lists the layers. Opened as a top-level page either URL would run
  // workspace HTML same-origin to the dashboard (stored XSS via the CLI's
  // Write tool), which is why the server answers both only inside a frame.
  if (mime.startsWith('text/html') || mime.startsWith('application/xhtml')) {
    renderSandboxedBlob(project, node, path, body, 'text/html');
    return true;
  }
  return false;
}

// renderPreviewText fills the drawer from the preview endpoint's JSON: a
// placeholder for binary content, rich rendering for markdown / tex, and a
// line-numbered listing for everything else.
function renderPreviewText(project, node, path, body, data, line) {
  if (data.binary) {
    const binMime = String(data.mime || '');
    // HTML / XHTML / SVG land in `binary:true` by design (R176-SEC-H3:
    // active-content bytes never flow through the preview JSON content
    // field). The server detects the MIME from the bytes, so the iframe
    // parses the right document type.
    if (binMime.startsWith('text/html') || binMime.startsWith('application/xhtml') || binMime.startsWith('image/svg+xml')) {
      renderPreviewByMime(project, node, path, body, binMime);
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
  // Route through renderRich — same renderer chat bubbles use so behaviour
  // (math, mermaid, tables, lists, file-refs) stays consistent across
  // surfaces. Source-code files keep the line-number gutter layout.
  if (lang === 'markdown' || lang === 'tex') {
    const mode = lang === 'tex' ? 'tex' : 'markdown';
    parts.push('<div class="fv-rich">' + renderRich(data.content || '', { mode: mode }) + '</div>');
  } else {
    const raw = data.content || '';
    const lines = raw.split('\n');
    const gutter = lines.map((_, i) => String(i + 1)).join('\n');
    parts.push('<pre class="fv-lined"><span class="fv-gutter" aria-hidden="true">' + gutter + '</span><code class="fv-code">' + esc(raw) + '</code></pre>');
  }
  body.innerHTML = parts.join('');
  // Flush renderRich's KaTeX/Mermaid slots, or a first .md open shows katex-pending.
  runPendingAsync();
  // Mirror chat-side file-ref chip injection so paths inside the preview
  // body also get [preview]/[download] affordances.
  body.querySelectorAll('.fv-rich').forEach(scanEventForFileRefs);
  if (line) scrollToPreviewLine(body, parseInt(line, 10));
}

// openFilePreview shows wrapEl's file on the workbench's 预览 tab and follows
// it; follow (a follow refresh) re-renders in place: no tab switch, no
// loading placeholder, the scroll kept or, for a growing log, at its end.
async function openFilePreview(wrapEl, follow) {
  const drawer = document.getElementById('fv-drawer');
  const body = document.getElementById('fv-body');
  const title = document.getElementById('fv-title');
  const meta = document.getElementById('fv-meta');
  if (!drawer || !body || !title || !meta) return;
  // Warm-start async renderers the moment the drawer opens (idempotent once
  // ready), in parallel with the preview fetch.
  loadKatex();
  loadMermaid();
  const project = wrapEl.dataset.project;
  const node = wrapEl.dataset.node;
  const path = wrapEl.dataset.path;
  const line = wrapEl.dataset.line || '';
  const mime = wrapEl.dataset.mime || '';
  const size = +wrapEl.dataset.size || 0;
  if (follow) {
    if (drawer.dataset.path !== path || drawer.dataset.project !== project || drawer.dataset.node !== node) return;
    if (renderPreviewByMime(project, node, path, body, mime, true)) return;
    try {
      const r = await fetch(fileApiUrl(project, node, path, 'preview'), { cache: 'no-cache' });
      if (!r.ok || drawer.dataset.path !== path) return;
      const data = await r.json();
      const keep = followScrollState(body, data.size);
      renderPreviewText(project, node, path, body, data, '');
      keep(path);
    } catch (_) { /* keep what is shown */ }
    return;
  }

  // The preview is the workbench's 预览 tab.
  workbench.open('preview');
  drawer.dataset.project = project;
  drawer.dataset.node = node;
  drawer.dataset.path = path;
  // Show the original string (may be abs) in the header so the user can
  // still match the preview to the bubble they clicked; server calls below
  // use the workspace-relative `path`.
  const headerPath = wrapEl.dataset.displayPath || path;
  title.textContent = headerPath + (line ? ':' + line : '');
  meta.textContent = [mime, formatFileSize(size)].filter(Boolean).join(' \u00b7 ');
  body.innerHTML = '<div class="fv-loading">loading\u2026</div>';
  resetFollowScroll(body);
  const again = { dataset: { project, node, path, mime } };
  followPreview('workbench', { root: project, node, path }, () => openFilePreview(again, true));
  if (renderPreviewByMime(project, node, path, body, mime)) return;

  // Text / unknown: go through preview endpoint which returns structured JSON.
  try {
    const headers = {};
    const r = await fetch(fileApiUrl(project, node, path, 'preview'), { headers });
    if (!r.ok) {
      body.innerHTML = '<div class="fv-error">' + (r.status === 501 ? REMOTE_FILES_UNSUPPORTED : 'preview failed (' + r.status + ')') + '</div>';
      return;
    }
    const data = await r.json();
    followScrollState(body, data.size);
    renderPreviewText(project, node, path, body, data, line);
  } catch (e) {
    body.innerHTML = '<div class="fv-error">' + esc(String(e && e.message || e)) + '</div>';
  }
}

// renderSandboxedBlob points a sandboxed iframe at the server. HTML on this
// node is framed under its directory's preview link (POST /api/preview →
// /api/preview/<token>/<name>), so its relative CSS, JS and images load; a
// remote node's HTML, an SVG, or a failed mint use the single-file inline
// render (mode=render&inline=1). The iframe document takes its CSP from the
// response, not from the dashboard page, whose script-src has no
// 'unsafe-inline'. A document built client-side (a blob: URL or an inline
// document) inherits the parent policy in current engines
// (docs/rfc/csp-data-action.md §4), so workspace HTML that uses MathJax /
// KaTeX / Mermaid would not run in one.
//
// Defense layers:
//   (1) Both forms answer only frame and subresource requests (Sec-Fetch-Dest),
//       so a direct URL hit gets 403 (Firefox ignores a CSP sandbox on
//       top-level navigation); the plain render form is octet-stream +
//       attachment.
//   (2) The response carries `Content-Security-Policy: sandbox
//       allow-scripts …`, an opaque origin however it is embedded; a
//       preview link's policy admits scripts, styles and fetches from its
//       own prefix only and lets only the dashboard frame it.
//   (3) sandbox='allow-scripts' on the iframe withholds the same-origin
//       token, so the document cannot read dashboard cookies, storage or
//       DOM, and its subresource requests carry no dashboard credentials
//       (the preview link's token is theirs). Scripts stay on so math and
//       diagram libraries render.
// The body param is the container element; blobType is the document type
// the call site expects (the server's detected MIME drives parsing).
function renderSandboxedBlob(project, node, path, body, blobType) {
  body.innerHTML = '';
  const frame = document.createElement('iframe');
  frame.title = path;
  frame.setAttribute('sandbox', 'allow-scripts');
  frame.referrerPolicy = 'no-referrer';
  body.appendChild(frame);
  const inline = fileApiUrl(project, node, path, 'render') + '&inline=1';
  if (blobType !== 'text/html' || (node && node !== 'local')) { frame.src = inline; return; }
  const req = (project && project.startsWith('session:')) ? { root: project, path } : { project, path };
  fetchJSON(NZ_CONTRACT.API.preview, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(req) })
    .then((res) => { frame.src = (res && res.url) || inline; })
    .catch(() => { frame.src = inline; });
}

/* ===== Follow refresh (跟随刷新) ======================================= */
/* A preview showing a file the session is writing re-renders off the
 * session's live events, never on a timer (docs/rfc/planner.md §10.2 ⑤): a
 * Write / Edit event names its target (ev.path, relative to the session's
 * file root). A Bash command writing a log names no path, so a log-like
 * text (.log / .txt) under the session's tree is also refreshed on its other
 * tool calls and at turn end. One refresh FOLLOW_MS after the first matching
 * event, so a burst coalesces. Events are only read, never appended, so a
 * refresh cannot pass for a new turn. */

const FOLLOW_MS = 1000;
// surface ('workbench' | 'files') → { ref: {root, node, path}, refresh, timer }
const _follows = new Map();

// followPreview makes surface follow ref, re-rendering through refresh;
// a null ref stops it.
function followPreview(surface, ref, refresh) {
  const cur = _follows.get(surface);
  if (cur && cur.timer) clearTimeout(cur.timer);
  if (!ref || !ref.path) { _follows.delete(surface); return; }
  _follows.set(surface, { ref, refresh, timer: 0 });
}

function isLogLike(path) { return /\.(log|txt)$/i.test(path || ''); }

// projectFileAbs is the absolute path of a project-rooted ref, or ''.
function projectFileAbs(ref) {
  const p = (sessionList.projectsData || []).find((x) => x.name === ref.root && (x.node || 'local') === (ref.node || 'local'));
  return p && p.path ? p.path.replace(/\/$/, '') + '/' + ref.path : '';
}

// previewHit reports whether ev, a live event of session key on node, may
// have written the file ref shows. ev.path is relative to the session's file
// root: its git tree root, which holds its workspace.
function previewHit(key, node, ev, ref) {
  if ((ref.node || 'local') !== (node || 'local')) return false;
  const own = ref.root === 'session:' + key;
  const sd = sessionList.sessionsData[sid(key, node || 'local')];
  const ws = sd && sd.workspace ? sd.workspace.replace(/\/$/, '') : '';
  const abs = own ? '' : projectFileAbs(ref);
  if (ev.type === 'tool_use' && ev.path) {
    if (own) return ev.path === ref.path;
    if (!abs || !ws || !abs.endsWith('/' + ev.path)) return false;
    const tree = abs.slice(0, abs.length - ev.path.length - 1);
    return ws === tree || ws.startsWith(tree + '/');
  }
  if (!isLogLike(ref.path) || (ev.type !== 'tool_use' && ev.type !== 'result')) return false;
  return own || (!!abs && !!ws && abs.startsWith(ws + '/'));
}

// notePreviewEvent takes a live event of session key on node.
function notePreviewEvent(key, node, ev) {
  if (!key || !ev) return;
  for (const [surface, f] of _follows) {
    if (f.timer || !previewHit(key, node, ev, f.ref)) continue;
    f.timer = setTimeout(() => {
      f.timer = 0;
      if (_follows.get(surface) === f) f.refresh();
    }, FOLLOW_MS);
  }
}

// Scroll of a followed preview: body._nzMoved marks a user scroll away from
// the end since the last render, body._nzTop where the last render left it
// (so its own scroll events are not taken for the user's), body._nzSize the
// size it rendered (-1 before the first).
function trackFollowScroll(body) {
  if (body._nzTrack) return;
  body._nzTrack = true;
  body.addEventListener('scroll', () => {
    if (Math.abs(body.scrollTop - (body._nzTop || 0)) <= 2) return;
    body._nzMoved = body.scrollTop + body.clientHeight < body.scrollHeight - 24;
  }, { passive: true });
}

function resetFollowScroll(body) {
  trackFollowScroll(body);
  body._nzMoved = false;
  body._nzTop = 0;
  body._nzSize = -1;
}

// followScrollState notes a render of a file now size bytes long and returns
// what to call after it: a log-like file that grew lands at its end unless
// the user scrolled up from there; anything else keeps its place.
function followScrollState(body, size) {
  trackFollowScroll(body);
  const top = body.scrollTop;
  const grew = body._nzSize >= 0 && size > body._nzSize;
  body._nzSize = size;
  return (path) => {
    body.scrollTop = (isLogLike(path) && grew && !body._nzMoved) ? body.scrollHeight : top;
    body._nzTop = body.scrollTop;
  };
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

function inferLang(path, mime) {
  const ext = (path.split('.').pop() || '').toLowerCase();
  if (ext === 'md' || ext === 'markdown') return 'markdown';
  if (ext === 'tex' || ext === 'latex') return 'tex';
  if (mime === 'text/markdown') return 'markdown';
  if (mime === 'text/x-tex' || mime === 'application/x-tex') return 'tex';
  return '';
}

// closeFilePreview closes the workbench, whose close clears the preview.
function closeFilePreview() {
  workbench.close();
}

// Closing the workbench, from any surface, drops what the 预览 tab showed.
nzBus.addEventListener('workbench:close', () => {
  const drawer = document.getElementById('fv-drawer');
  if (!drawer) return;
  delete drawer.dataset.snippetMode;
  delete drawer.dataset.snippetName;
  delete drawer.dataset.path;
  _pendingSnippet = null;
  followPreview('workbench', null);
  const body = document.getElementById('fv-body');
  if (body) body.innerHTML = '';
  const title = document.getElementById('fv-title');
  if (title) title.textContent = '预览';
  const meta = document.getElementById('fv-meta');
  if (meta) meta.textContent = '';
});

// Wire drawer buttons once on load.
document.addEventListener('DOMContentLoaded', function () {
  const copy = document.getElementById('fv-btn-copy');
  if (copy) copy.addEventListener('click', () => {
    const drawer = document.getElementById('fv-drawer');
    if (!drawer) return;
    const isSnippet = drawer.dataset.snippetMode === '1';
    const text = isSnippet ? _pendingSnippet : drawer.dataset.path;
    if (!text) return;
    const label = isSnippet ? '片段已复制' : '路径已复制';
    copyText(text).then((ok) => showToast(ok ? label : '复制失败', ok ? 'success' : 'warning', 1000));
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
// AVATAR_GROUP_GAP_MS — the CSS then hides the repeated avatar. Only "user"
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
      (t - prevTime) < AVATAR_GROUP_GAP_MS;
    node.classList.toggle('nz-grouped', grouped);
    prevSender = sender;
    prevTime = t; // 0 when undated → next bubble can't group against it
  }
}



function processEventsForDisplay(events) {
  return events.filter(e => !isInternalEvent(e));
}

export {
  closeFilePreview,
  fileApiUrl,
  followPreview,
  followScrollState,
  notePreviewEvent,
  openFilePreview,
  processEventsForDisplay,
  regroupAvatars,
  renderSandboxedBlob,
  resetFollowScroll,
  resolveProjectForAbsPath,
  startFileRefObserver,
};
