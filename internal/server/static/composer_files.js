// composer_files.js — composer attachments: file picker, upload queue,
// thumbnail drag-reorder and the HEIC/orientation pre-pass (#2558 D4-3).
//
// Verbatim move out of dashboard.js; only the import + dep-wiring lines are
// new. Layering (D4-1 rule): never import dashboard back.
import { esc, escAttr, nzState, nzTest, showToast } from './nz_util.js';

const deps = {
  ICONS: null,
  featureForCurrent: null,
  formatFileSize: null,
  getToken: null,
  sendMessage: null,
  showAuthModal: null,
};
export function configureComposerFiles(impl) {
  for (const k of Object.keys(deps)) {
    if (typeof impl[k] === 'undefined') throw new Error('composer_files dep missing: ' + k);
    deps[k] = impl[k];
  }
}

// --- File handling ---
//
// Each selected image is pre-uploaded via POST /api/sessions/upload as soon
// as it's picked. nzState.pendingFiles holds {file, blobUrl, id, status, error}:
//   status: 'uploading' | 'ready' | 'error' — 'ready' means a valid server-side
//   file id is in `id` and can be referenced later via file_ids on send.
// This decouples image transfer from /send, avoids the 105 MB multipart body
// and 15s ReadTimeout, and lets one bad file fail without blocking the rest.

function openFilePicker() {
  // Multi-Backend RFC §8.3 D14: respect feature gate. Toast instead of
  // silently no-op so the user understands why the click did nothing —
  // the .feat-disabled class is the visual cue, this is the audible cue.
  if (!deps.featureForCurrent('image_input')) {
    showToast('当前后端不支持图片上传', 'warn');
    return;
  }
  document.getElementById('file-input').click();
}

// Downscale any image to JPEG with max edge 1600 and quality 0.8.
// Rationale: the CLI writes user messages as one NDJSON line to the shim,
// which is capped at 12 MB per line; base64 inflates bytes by ~1.33×, so a
// 20-image batch must stay under ~9 MB raw to fit. 1600 / q0.8 typically
// yields 150–400 KB per JPEG (vs 500 KB–1.2 MB at 2048 / q0.85) while still
// above the 1568 px knee where Anthropic's vision models stop gaining
// accuracy. HEIC is also handled here — createImageBitmap decodes it on
// Safari 17+ and we re-encode to JPEG.
//
// Orientation: phone cameras tag photos with an EXIF orientation flag
// instead of rotating the pixels. Re-encoding through canvas drops that
// flag, so we must bake the rotation into the pixels at decode time via
// `imageOrientation: 'from-image'` — without it a portrait iPhone shot
// arrives at the backend sideways. With the flag, the returned bitmap's
// width/height are ALREADY the visually-correct (post-rotation) dimensions,
// so the scaling math below needs no orientation branching and we must NOT
// apply any extra ctx.rotate (that would double-correct). The option is the
// modern default on Chrome/Firefox and is honored by Safari/iOS 16+; older
// engines silently ignore the unknown member and fall back to their default
// decode, which is the best we can do client-side.
// Falls back to the original file if decoding fails so the server's
// content-type check still produces a real error message.
async function normalizeImage(file) {
  const MAX_EDGE = 1600;
  try {
    const bmp = await createImageBitmap(file, { imageOrientation: 'from-image' });
    const { width: sw, height: sh } = bmp;
    let dw = sw, dh = sh;
    const m = Math.max(sw, sh);
    if (m > MAX_EDGE) {
      const scale = MAX_EDGE / m;
      dw = Math.max(1, Math.round(sw * scale));
      dh = Math.max(1, Math.round(sh * scale));
    }
    const canvas = document.createElement('canvas');
    canvas.width = dw;
    canvas.height = dh;
    const ctx = canvas.getContext('2d');
    ctx.drawImage(bmp, 0, 0, dw, dh);
    bmp.close();
    const blob = await new Promise(res => canvas.toBlob(res, 'image/jpeg', 0.8));
    if (!blob) return file;
    return new File([blob], (file.name || 'image').replace(/\.[^.]+$/, '') + '.jpg', { type: 'image/jpeg' });
  } catch (_) {
    return file;
  }
}

// uploadConcurrency caps parallel POST /api/sessions/upload requests so a
// 20-image batch on a mobile connection doesn't fan out 20 simultaneous
// bodies competing for the same uplink. With 15 s server ReadTimeout, too
// many parallel streams starve each other and trigger multipart i/o
// timeouts — pre-R192 the old 10-file ceiling masked this, but at 20 it
// shows up. 3 parallel uploads is the sweet spot: still fast on LTE/WiFi,
// safe on slow cellular.
const uploadConcurrency = 3;
let uploadInFlight = 0;
const uploadQueue = [];

function enqueueUpload(entry) {
  uploadQueue.push(entry);
  drainUploadQueue();
}

function drainUploadQueue() {
  while (uploadInFlight < uploadConcurrency && uploadQueue.length > 0) {
    const entry = uploadQueue.shift();
    uploadInFlight++;
    uploadEntry(entry).finally(() => {
      uploadInFlight--;
      drainUploadQueue();
    });
  }
}

// fileKind maps a browser File's MIME type to the 2 classes naozhi accepts.
// PDF sniffing looks at file.type AND the .pdf extension because some mobile
// Safari builds drop a `content-type: application/octet-stream` on PDFs
// picked from iCloud Drive — the server still sniffs magic bytes, so accepting
// optimistically here just lets the server give the authoritative reject.
function fileKind(f) {
  if (f && f.type === 'application/pdf') return 'pdf';
  if (f && /\.pdf$/i.test(f.name || '')) return 'pdf';
  if (f && f.type && f.type.startsWith('image/')) return 'image';
  return '';
}

function handleFiles(fileList) {
  const toUpload = [];
  // Image source ceiling is kept at 40 MB so iPhone HEIC/JPEG straight from
  // Photos (~6–12 MB) still fits; normalizeImage downscales before upload,
  // so the 10 MB server cap applies to the re-encoded JPEG. PDFs bypass
  // normalization and must stay under the server's 32 MB Anthropic ceiling.
  const MAX_IMAGE_BYTES = 40 * 1024 * 1024;
  const MAX_PDF_BYTES = 32 * 1024 * 1024;
  for (const raw of fileList) {
    const kind = fileKind(raw);
    if (!kind) continue;
    if (kind === 'pdf' && raw.size > MAX_PDF_BYTES) {
      showToast('PDF 过大（上限 32 MB）', 'warning');
      continue;
    }
    if (kind === 'image' && raw.size > MAX_IMAGE_BYTES) {
      showToast('图片过大（上限 40 MB）', 'warning');
      continue;
    }
    if (nzState.pendingFiles.length >= 20) { showToast('最多上传 20 个文件', 'warning'); break; }
    const entry = {
      file: raw,
      kind,
      // blobUrl is still set for images so the existing thumbnail path works
      // unchanged. PDFs render as an icon card (see renderFilePreviews) so no
      // URL.createObjectURL is needed and we skip it to save the tiny
      // revoke-on-remove bookkeeping.
      blobUrl: kind === 'image' ? URL.createObjectURL(raw) : '',
      id: '',
      status: 'uploading',
      error: '',
    };
    nzState.pendingFiles.push(entry);
    toUpload.push(entry);
  }
  const fi = document.getElementById('file-input');
  if (fi) fi.value = '';
  renderFilePreviews();
  toUpload.forEach(enqueueUpload);
}

async function uploadEntry(entry) {
  entry.status = 'uploading';
  entry.error = '';
  renderFilePreviews();
  try {
    // PDFs skip normalizeImage: they travel to the server as-is and end up
    // persisted to the session workspace (see
    // docs/rfc/pdf-attachment.md). Only images go through the downscale
    // step. Track the transmitted byte size on `normalizedSize` for the
    // deps.sendMessage batch-cap check below — for PDFs this equals raw size
    // but is NOT counted against the 9 MB image batch cap (PDFs travel
    // via file_ref, not inline base64).
    const file = entry.kind === 'pdf' ? entry.file : await normalizeImage(entry.file);
    entry.normalizedSize = file.size;
    // Swap the preview thumbnail to the normalized image so what the user
    // sees matches what the backend receives, pixel-for-pixel. The original
    // blobUrl points at the raw picked file, which still carries the EXIF
    // orientation flag — and browsers (notably some WebViews) don't reliably
    // apply it to <img>, so a portrait phone photo previewed sideways. The
    // canvas re-encode in normalizeImage bakes the rotation into the pixels
    // and strips EXIF, so this blob renders upright everywhere. Guard on
    // identity: normalizeImage falls back to the original File on decode
    // failure, in which case there's nothing new to show. PDFs keep their
    // icon card (no blobUrl) untouched.
    if (entry.kind === 'image' && file !== entry.file) {
      const upright = URL.createObjectURL(file);
      if (entry.blobUrl) URL.revokeObjectURL(entry.blobUrl);
      entry.blobUrl = upright;
      renderFilePreviews();
    }
    const fd = new FormData();
    fd.append('file', file);
    const headers = {};
    const token = deps.getToken();
    if (token) headers['Authorization'] = 'Bearer ' + token;
    const r = await fetch(NZ_CONTRACT.API.sessions_upload, { method: 'POST', headers, body: fd });
    if (r.status === 401 || r.status === 403) { deps.showAuthModal(); throw new Error('unauthorized'); }
    if (!r.ok) {
      const txt = await r.text().catch(() => '');
      let msg = 'upload failed: ' + r.status;
      try { const j = JSON.parse(txt); if (j && j.error) msg = j.error; } catch (_) { if (txt) msg = txt; }
      throw new Error(msg);
    }
    const j = await r.json();
    if (!j.id) throw new Error('no id in response');
    entry.id = j.id;
    // Server echoes kind/size/name — trust its view so a client/server
    // sniff disagreement (optimistic PDF accept above, for instance)
    // settles in the server's favour. The UI card uses these.
    if (j.kind) entry.serverKind = j.kind;
    if (j.name) entry.serverName = j.name;
    entry.status = 'ready';
    // Fire-and-forget auto-orientation: for an image with no EXIF flag (a
    // sideways document photo), ask the backend's vision side-call which way
    // is up. We DON'T await it — the upload is already 'ready' and sendable;
    // the rotation, if any, lands silently a few seconds later and refreshes
    // the thumbnail. Never blocks send. Images only; the server no-ops for
    // PDFs and when the feature is disabled.
    if (entry.kind === 'image') maybeAutoOrient(entry);
  } catch (e) {
    entry.status = 'error';
    entry.error = e.message || 'upload failed';
  }
  renderFilePreviews();
}

// ORIENT_MAX_WAIT_MS bounds how long send() will block on an in-flight
// auto-orient. The backend's vision side-call measured ~12s on Haiku; we
// give it a shorter client budget so a slow/hung model never wedges send.
// On timeout we abort the orient fetch, clear the flag, and let send proceed
// with the original (unrotated) bytes — fully consistent with the
// best-effort contract: a missed rotation is acceptable, a blocked send is
// not.
const ORIENT_MAX_WAIT_MS = 8000;

// maybeAutoOrient asks the backend to auto-rotate a just-uploaded image that
// lacks an EXIF orientation flag. Best-effort and silent: any failure leaves
// the image as-is (it's already 'ready' and sendable). On a confirmed
// rotation it refetches the corrected bytes via the attachment endpoint and
// swaps the preview thumbnail so the user sees the upright result that will
// be sent. The entry's `id` is unchanged by rotation (server replaces bytes
// in place), so send still references the same file_id.
//
// Concurrency: the entry is marked `orienting` for the whole call so send()
// can block on it (see the gate in deps.sendMessage). Without this, a user who
// hits send within the ~12s vision window would TakeAll the upload BEFORE the
// rotation lands — the server's in-place Replace then misses the consumed
// entry and the original sideways image goes out. The flag is always cleared
// in finally so a thrown/aborted call can never leave send permanently gated.
async function maybeAutoOrient(entry) {
  if (!entry || !entry.id || entry.kind !== 'image') return;
  entry.orienting = true;
  renderFilePreviews();
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), ORIENT_MAX_WAIT_MS);
  try {
    const headers = { 'Content-Type': 'application/json' };
    const token = deps.getToken();
    if (token) headers['Authorization'] = 'Bearer ' + token;
    const r = await fetch(NZ_CONTRACT.API.sessions_orient, {
      method: 'POST', headers, body: JSON.stringify({ id: entry.id }), signal: ctrl.signal,
    });
    if (!r.ok) return; // 404/expired/etc — nothing to do
    const j = await r.json().catch(() => null);
    if (!j || !j.rotated || !j.image) return; // no rotation applied
    // The server rotated the stored bytes in place (same file_id) and echoed
    // the corrected JPEG inline as a data URL. Swap the preview to it so the
    // thumbnail matches what gets sent. The entry may have been removed while
    // the orient call was in flight — guard before touching it.
    if (!nzState.pendingFiles.includes(entry)) return;
    if (entry.blobUrl) URL.revokeObjectURL(entry.blobUrl);
    entry.blobUrl = j.image; // data: URL, no object URL to revoke later
  } catch (_) {
    // Best-effort: swallow (includes the AbortError on timeout). The image is
    // already sendable as-is.
  } finally {
    clearTimeout(timer);
    entry.orienting = false;
    renderFilePreviews();
  }
}

// awaitPendingOrients resolves once no pending image is still auto-orienting,
// or after ORIENT_MAX_WAIT_MS as a hard ceiling. send() awaits this so an
// in-flight rotation lands (server Replace) BEFORE TakeAll consumes the
// upload. Polling (rather than tracking promises) keeps it robust to entries
// added/removed mid-wait — it simply re-reads the live nzState.pendingFiles each tick.
function awaitPendingOrients() {
  if (!nzState.pendingFiles.some(f => f.orienting)) return Promise.resolve();
  return new Promise((resolve) => {
    const deadline = Date.now() + ORIENT_MAX_WAIT_MS;
    const tick = () => {
      if (!nzState.pendingFiles.some(f => f.orienting) || Date.now() >= deadline) { resolve(); return; }
      setTimeout(tick, 120);
    };
    setTimeout(tick, 120);
  });
}

function retryUpload(idx) {
  const entry = nzState.pendingFiles[idx];
  if (entry && entry.status === 'error') enqueueUpload(entry);
}

function removeFile(idx) {
  const [removed] = nzState.pendingFiles.splice(idx, 1);
  if (removed && removed.blobUrl) URL.revokeObjectURL(removed.blobUrl);
  renderFilePreviews();
}

// reorderPendingFile moves nzState.pendingFiles[from] to position `to`. Pure array
// operation extracted so the drag-drop handler and a keyboard a11y fallback
// can share one code path and so contract tests can assert the move semantics
// without touching the DOM. Returns true when the array actually changed.
function reorderPendingFile(from, to) {
  if (!Number.isInteger(from) || !Number.isInteger(to)) return false;
  if (from < 0 || from >= nzState.pendingFiles.length) return false;
  if (to < 0) to = 0;
  if (to > nzState.pendingFiles.length - 1) to = nzState.pendingFiles.length - 1;
  if (from === to) return false;
  const [moved] = nzState.pendingFiles.splice(from, 1);
  nzState.pendingFiles.splice(to, 0, moved);
  return true;
}

// Drag source index for the thumbnail-reorder gesture. A module-level slot is
// safer than dataTransfer because the latter is sometimes empty on drop in
// Safari when the drag never left the origin element.
let _dragReorderFrom = -1;

function onThumbDragStart(ev, idx) {
  // Only 'ready' files are reorderable; uploading/error thumbs are pinned to
  // their current slot because their index may still be referenced by the
  // in-flight upload completion path.
  const entry = nzState.pendingFiles[idx];
  if (!entry || entry.status !== 'ready') { ev.preventDefault(); return; }
  _dragReorderFrom = idx;
  try {
    ev.dataTransfer.effectAllowed = 'move';
    // Firefox requires some data to be set or dragstart is cancelled.
    ev.dataTransfer.setData('text/plain', String(idx));
  } catch (_) {}
  ev.currentTarget.classList.add('dragging');
}

function onThumbDragOver(ev) {
  if (_dragReorderFrom < 0) return;
  ev.preventDefault();
  ev.dataTransfer.dropEffect = 'move';
  ev.currentTarget.classList.add('drop-target');
}

function onThumbDragLeave(ev) {
  ev.currentTarget.classList.remove('drop-target');
}

function onThumbDrop(ev, idx) {
  ev.preventDefault();
  ev.currentTarget.classList.remove('drop-target');
  const from = _dragReorderFrom;
  _dragReorderFrom = -1;
  if (from < 0 || from === idx) { renderFilePreviews(); return; }
  reorderPendingFile(from, idx);
  renderFilePreviews();
}

function onThumbDragEnd() {
  _dragReorderFrom = -1;
  const el = document.getElementById('file-preview');
  if (!el) return;
  el.querySelectorAll('.file-thumb.dragging, .file-thumb.drop-target').forEach(n => {
    n.classList.remove('dragging');
    n.classList.remove('drop-target');
  });
}

// Keyboard a11y: when a .file-thumb is focused, Left/Right arrow keys move it
// left/right by one slot. Mirrors the drag gesture for keyboard-only users.
function onThumbKeyDown(ev, idx) {
  if (ev.key !== 'ArrowLeft' && ev.key !== 'ArrowRight') return;
  const entry = nzState.pendingFiles[idx];
  if (!entry || entry.status !== 'ready') return;
  const to = idx + (ev.key === 'ArrowLeft' ? -1 : 1);
  if (!reorderPendingFile(idx, to)) return;
  ev.preventDefault();
  renderFilePreviews();
  // After re-render, restore focus to the moved thumb's new slot so rapid
  // arrow presses keep working.
  const el = document.getElementById('file-preview');
  if (!el) return;
  const next = el.querySelector('.file-thumb[data-idx="' + to + '"]');
  if (next) next.focus();
}

function renderFilePreviews() {
  const el = document.getElementById('file-preview');
  if (!el) return;
  el.innerHTML = nzState.pendingFiles.map((entry, i) => {
    const overlay =
      entry.status === 'uploading' ? '<div class="upload-status uploading"></div>' :
      entry.status === 'error' ? '<div class="upload-status error" title="' + escAttr(entry.error || 'upload failed') + '" data-action="upload-retry">\u21bb</div>' :
      '';
    // Only 'ready' files are draggable so an in-flight upload's index stays
    // stable for the uploadEntry completion handler. tabindex=0 makes the
    // thumb keyboard-focusable; ArrowLeft/Right then reorder via onThumbKeyDown.
    const draggable = entry.status === 'ready';
    const isPDF = entry.kind === 'pdf';
    // PDF card: fixed-size chip with the .pdf icon + filename + size.
    // Image thumb: the existing <img> preview. Both share the remove button
    // and the upload-status overlay.
    const body = isPDF
      ? ('<div class="pdf-chip" aria-hidden="true">' +
           '<div class="pdf-icon">PDF</div>' +
           '<div class="pdf-meta">' +
             '<div class="pdf-name" title="' + escAttr(entry.file.name || 'document.pdf') + '">' +
               esc((entry.file.name || 'document.pdf')) +
             '</div>' +
             '<div class="pdf-size">' + esc(deps.formatFileSize(entry.file.size || 0)) + '</div>' +
           '</div>' +
         '</div>')
      : '<img src="' + entry.blobUrl + '" draggable="false">';
    return '<div class="file-thumb ' + entry.status + (isPDF ? ' pdf' : '') + '"' +
      ' data-idx="' + i + '"' +
      (draggable ? ' draggable="true" tabindex="0" role="button" aria-label="' + (isPDF ? 'PDF' : '\u56fe\u7247') + ' ' + (i + 1) + '\uff0c\u62d6\u52a8\u6216\u7528\u5de6\u53f3\u65b9\u5411\u952e\u6392\u5e8f"' : '') +
      (draggable ? ' data-action-dragstart="thumb-dragstart"' : '') +
      (draggable ? ' data-action-dragover="thumb-dragover"' : '') +
      (draggable ? ' data-action-dragleave="thumb-dragleave"' : '') +
      (draggable ? ' data-action-drop="thumb-drop"' : '') +
      (draggable ? ' data-action-dragend="thumb-dragend"' : '') +
      (draggable ? ' data-action-keydown="thumb-key"' : '') +
      '>' +
      body +
      overlay +
      '<button class="remove" type="button" data-action="file-remove" title="\u79fb\u9664" aria-label="\u79fb\u9664">' + deps.ICONS.close + '</button>' +
      '</div>';
  }).join('');
}


export {
  ORIENT_MAX_WAIT_MS,
  awaitPendingOrients,
  handleFiles,
  maybeAutoOrient,
  onThumbDragEnd,
  onThumbDragLeave,
  onThumbDragOver,
  onThumbDragStart,
  onThumbDrop,
  onThumbKeyDown,
  openFilePicker,
  removeFile,
  renderFilePreviews,
  retryUpload,
};

// nz.test surface for the Playwright specs (#2557 PR-E3 pattern).
Object.assign(nzTest, { ORIENT_MAX_WAIT_MS, awaitPendingOrients, maybeAutoOrient, renderFilePreviews, handleFiles, openFilePicker, removeFile, retryUpload });
