// file_rows.js — the row markup the files view (files_view.js) and the
// workbench (workbench_panes.js) share: directory entries, breadcrumbs and
// the outputs of GET /api/sessions/outputs (docs/rfc/planner.md §10.2).
// A leaf: callers own the containers and the click delegation.
import { esc, escAttr } from './nz_util.js';
import { formatFileSize, historyDayLabel } from './utilities.js';

// fileRowsHtml renders a /api/projects/files/list answer. Rows carry
// data-kind (up / dir / file / symlink) and data-name; a folder row
// navigates, a file row previews, a symlink row is inert.
export function fileRowsHtml(res, dir) {
  const entries = (res && res.entries) || [];
  let html = dir
    ? '<div class="files-row files-up" role="listitem" data-kind="up"><span class="files-ic">↑</span><span class="files-name">上级目录</span></div>'
    : '';
  // Keep the ↑ 上级目录 row (when present) so an empty subdirectory is
  // still navigable out of.
  if (!entries.length) return html + '<div class="files-empty">空目录</div>';
  for (const e of entries) {
    const icon = e.is_dir ? '📁' : (e.symlink ? '🔗' : '📄');
    const meta = e.is_dir ? '' : (e.symlink ? '链接' : formatFileSize(e.size));
    const cls = 'files-row' + (e.is_dir ? ' files-dir' : '') + (e.symlink ? ' files-link' : '');
    const kind = e.symlink ? 'symlink' : (e.is_dir ? 'dir' : 'file');
    html += '<div class="' + cls + '" role="listitem" data-kind="' + kind + '" data-name="' + escAttr(e.name) + '">' +
      '<span class="files-ic">' + icon + '</span>' +
      '<span class="files-name">' + esc(e.name) + '</span>' +
      '<span class="files-meta">' + esc(meta) + '</span></div>';
  }
  if (res && res.truncated) {
    html += '<div class="files-empty">条目过多，仅显示前若干项，请进入子目录细化。</div>';
  }
  return html;
}

// crumbsHtml renders the breadcrumb for dir under a root labelled head.
export function crumbsHtml(head, dir) {
  let html = '<button type="button" class="files-crumb" data-dir="">' + esc(head) + '</button>';
  let acc = '';
  for (const seg of (dir ? dir.split('/') : [])) {
    if (!seg) continue;
    acc = acc ? acc + '/' + seg : seg;
    html += '<span class="files-crumb-sep">/</span>' +
      '<button type="button" class="files-crumb" data-dir="' + escAttr(acc) + '">' + esc(seg) + '</button>';
  }
  return html;
}

const IMG_EXT = { png: 'image/png', jpg: 'image/jpeg', jpeg: 'image/jpeg', gif: 'image/gif', webp: 'image/webp' };

// mimeForPath is the MIME openFilePreview renders by for the types it shows
// straight from the raw / render endpoints ('' sends the rest through the
// preview JSON, which re-detects HTML and SVG from the bytes).
export function mimeForPath(p) {
  const ext = (String(p).split('.').pop() || '').toLowerCase();
  if (IMG_EXT[ext]) return IMG_EXT[ext];
  if (ext === 'svg') return 'image/svg+xml';
  if (ext === 'pdf') return 'application/pdf';
  if (ext === 'html' || ext === 'htm') return 'text/html';
  return '';
}

// Office files (pptx / xlsx / docx) are outputs the preview cannot show.
const UNPREVIEWABLE = { pptx: 1, xlsx: 1, docx: 1 };

// isPreviewableOutput reports whether the 预览 tab can show output o: a file,
// not an Office document.
export function isPreviewableOutput(o) {
  if (!o || !o.path) return false;
  return !UNPREVIEWABLE[(o.path.split('.').pop() || '').toLowerCase()];
}

// outputRowsHtml renders outputs newest first, grouped by calendar day
// (historyDayLabel). A file row is data-kind="output" with data-i its index
// in outputs; a PR row is a link that opens GitHub. sessionLabel(o), when
// given, names the session in the row's meta (the outputs library).
export function outputRowsHtml(outputs, sessionLabel) {
  if (!outputs.length) return '';
  let html = '';
  let group = '';
  outputs.forEach((o, i) => {
    const t = new Date(o.time);
    const day = historyDayLabel(t);
    if (day !== group) {
      group = day;
      html += '<div class="wb-group" role="presentation">' + esc(day) + '</div>';
    }
    const hm = String(t.getHours()).padStart(2, '0') + ':' + String(t.getMinutes()).padStart(2, '0');
    const who = sessionLabel ? sessionLabel(o) : '';
    const meta = esc((who ? who + ' · ' : '') + hm);
    if (!o.path) {
      const label = o.state === 'merged' || o.type === 'pr_merge' ? 'PR 已合并' : 'PR';
      html += '<a class="files-row wb-out wb-out-pr" role="listitem" href="' + escAttr(o.url) +
        '" target="_blank" rel="noopener noreferrer" title="' + escAttr(o.url) + '">' +
        '<span class="files-ic">🔗</span><span class="files-name">' + esc(label + ' · ' + prName(o.url)) + '</span>' +
        '<span class="files-meta">' + meta + '</span></a>';
      return;
    }
    const name = o.path.split('/').pop() || o.path;
    // The directory only: the full path is the row's title already.
    const dir = o.path.slice(0, o.path.length - name.length).replace(/\/$/, '');
    const ok = isPreviewableOutput(o);
    html += '<div class="files-row wb-out' + (ok ? '' : ' wb-out-off') + '" role="listitem" data-kind="output" data-i="' + i +
      '" title="' + escAttr(o.path + (ok ? '' : '（不支持预览，可在文件里下载）')) + '">' +
      '<span class="files-ic">' + (IMG_EXT[(name.split('.').pop() || '').toLowerCase()] ? '🖼' : '📄') + '</span>' +
      '<span class="files-name">' + esc(name) + (dir ? '<span class="wb-out-path">' + esc(dir) + '</span>' : '') + '</span>' +
      '<span class="files-meta">' + meta + '</span></div>';
  });
  return html;
}

// prName shortens a GitHub PR URL to "repo#N".
function prName(url) {
  const m = /github\.com\/[^/]+\/([^/]+)\/pull\/(\d+)/.exec(url || '');
  return m ? m[1] + '#' + m[2] : (url || '');
}
