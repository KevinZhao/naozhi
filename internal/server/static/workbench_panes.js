// workbench_panes.js — the workbench's 产出, 文件 and 详情 tabs, and its
// auto-open: when the selected session makes a new previewable output the
// workbench opens on 预览 and shows it (docs/rfc/planner.md §10, §15.1 W1).
// Each new output opens it once, so a workbench the operator closed stays
// closed until the next one.
//
// Outputs come from GET /api/sessions/outputs?key= (planner/activity), which
// is refetched shortly after a tool event the backend tagged with an
// activity and after a turn ends (the turn-end scan reports files written by
// Bash). The tabs follow the selected local session, and planner:main on
// the Planner page (planner_view.js feeds its events to noteWorkbenchEvent).
import { NZ_CONTRACT } from './contract.js';
import { selection, sessionList, ui } from './state.js';
import { esc, escAttr, fetchJSON, nzBus, registerActions, showToast } from './nz_util.js';
import { isDiscoveredKey, sid } from './session_ident.js';
import { openFilePreview, resolveProjectForAbsPath } from './file_refs.js';
import { crumbsHtml, fileRowsHtml, isPreviewableOutput, mimeForPath, outputId, outputRowsHtml } from './file_rows.js';
import { formatAbsTime, formatTimeShort } from './utilities.js';
import { workbench } from './workbench.js';

// REFRESH_MS is how long after a tagged tool event or a turn end the outputs
// are refetched: the index records a file once its tool call returns, and the
// turn-end scan runs off the read loop.
const REFRESH_MS = 1500;

const st = {
  outputs: [],
  outputsFor: '', // sid st.outputs belongs to
  // seen maps a session's sid to the outputIds it already had; a session
  // first fetched seeds it, so outputs made before it was opened (or before
  // this page loaded) never auto-open.
  seen: new Map(),
  timer: 0,
  files: { root: '', dir: '', label: '' },
};

const PLANNER_KEY = 'planner:main';

// subject is the local session the tabs describe, or null: remote and
// discovered sessions have no outputs index or readable tree here.
function subject() {
  if (ui.activeView === 'planner') return { key: PLANNER_KEY, sid: sid(PLANNER_KEY, 'local'), sd: null };
  const key = selection.key;
  if (!key || isDiscoveredKey(key) || (selection.node && selection.node !== 'local')) return null;
  return { key, sid: sid(key, 'local'), sd: sessionList.sessionsData[sid(key, 'local')] || null };
}

// ---- outputs ----

function refreshOutputs(auto) {
  const s = subject();
  if (!s) { st.outputs = []; st.outputsFor = ''; paintOutputs(); return Promise.resolve(); }
  return fetchJSON(NZ_CONTRACT.API.sessions_outputs + '?key=' + encodeURIComponent(s.key)).then((res) => {
    const cur = subject();
    if (!cur || cur.sid !== s.sid) return;
    const outs = (res && Array.isArray(res.outputs)) ? res.outputs : [];
    const seen = st.seen.get(s.sid);
    const fresh = seen ? outs.filter((o) => !seen.has(outputId(o))) : [];
    st.seen.set(s.sid, new Set(outs.map(outputId)));
    st.outputs = outs;
    st.outputsFor = s.sid;
    paintOutputs();
    // Newest first: the first fresh previewable one is the latest. Only the
    // chat view and the Planner page dock the workbench; elsewhere (or on a
    // phone back on the session list, which keeps the selection) it would
    // cover the view.
    const docks = ui.activeView === 'planner' ||
      (ui.activeView === 'chat' && !document.body.classList.contains('mobile-list-view'));
    const show = auto && docks && fresh.find(isPreviewableOutput);
    if (show) openOutput(show);
  }).catch(() => {});
}

function scheduleRefresh() {
  if (st.timer) return;
  st.timer = setTimeout(() => { st.timer = 0; refreshOutputs(true); }, REFRESH_MS);
}

function paintOutputs() {
  const n = st.outputs.length;
  const count = document.getElementById('wb-outputs-count');
  if (count) count.textContent = n ? String(n) : '';
  const box = document.getElementById('wb-outputs');
  if (!box) return;
  box.innerHTML = n
    ? outputRowsHtml(st.outputs)
    : '<div class="files-empty">' + (subject() ? '这个会话还没有产出' : '选择一个会话查看它的产出') + '</div>';
}

// previewRef maps an output to the file API's root + path: its own session
// root while the session still works where it was written, else the project
// holding the directory it was written in (the path is relative to the
// session's tree root, which is that directory unless it sat below a git
// root). null when neither resolves.
export function previewRef(o) {
  if (o.root) return { project: o.root, path: o.path };
  const p = o.dir ? resolveProjectForAbsPath(o.dir.replace(/\/$/, '') + '/' + o.path, 'local') : null;
  return p ? { project: p.name, path: p.relPath } : null;
}

// openOutput shows output o on the 预览 tab.
export function openOutput(o) {
  const ref = previewRef(o);
  if (!ref) { showToast('这个产出所在的目录已不在可浏览范围，无法预览', 'warning'); return; }
  openFilePreview({ dataset: { project: ref.project, node: 'local', path: ref.path, mime: mimeForPath(ref.path), displayPath: o.path } });
}

// ---- files ----

function loadFiles(dir) {
  const s = subject();
  const box = document.getElementById('wb-files-list');
  const crumbs = document.getElementById('wb-files-crumbs');
  if (!box || !crumbs) return;
  if (!s || !s.sd || !s.sd.workspace) {
    crumbs.innerHTML = '';
    box.innerHTML = '<div class="files-empty">' + (s ? '这个会话没有工作目录' : '选择一个会话查看它的工作目录') + '</div>';
    st.files.root = '';
    return;
  }
  const root = 'session:' + s.key;
  if (st.files.root !== root) st.files.label = s.sd.workspace.split('/').filter(Boolean).pop() || s.sd.workspace;
  st.files.root = root;
  st.files.dir = dir || '';
  box.innerHTML = '<div class="files-empty">加载中…</div>';
  const qs = 'root=' + encodeURIComponent(root) + (st.files.dir ? '&dir=' + encodeURIComponent(st.files.dir) : '');
  fetchJSON(NZ_CONTRACT.API.projects_files_list + '?' + qs).then((res) => {
    if (st.files.root !== root || st.files.dir !== (dir || '')) return;
    // The server names the tree the session root resolved to (its git root
    // can sit above the session's cwd).
    if (res && res.root_name) st.files.label = res.root_name;
    crumbs.innerHTML = crumbsHtml('会话 · ' + st.files.label, st.files.dir);
    box.innerHTML = fileRowsHtml(res, st.files.dir);
  }).catch((e) => {
    crumbs.innerHTML = crumbsHtml('会话 · ' + st.files.label, st.files.dir);
    box.innerHTML = '<div class="files-empty">加载失败：' + esc(e.message) + '</div>';
  });
}

function onFilesClick(e) {
  const crumb = e.target.closest('.files-crumb');
  if (crumb) { loadFiles(crumb.dataset.dir || ''); return; }
  const row = e.target.closest('.files-row');
  if (!row || !st.files.root) return;
  const dir = st.files.dir;
  const rel = (name) => (dir ? dir + '/' + name : name);
  if (row.dataset.kind === 'up') { loadFiles(dir.includes('/') ? dir.slice(0, dir.lastIndexOf('/')) : ''); return; }
  if (row.dataset.kind === 'dir') { loadFiles(rel(row.dataset.name)); return; }
  if (row.dataset.kind === 'file') {
    const path = rel(row.dataset.name);
    openFilePreview({ dataset: { project: st.files.root, node: 'local', path, mime: mimeForPath(path) } });
  }
}

// ---- details ----

const HEALTH_LABEL = {
  active: '活跃', idle: '正常空闲', waiting: '待你回复', stalled: '停滞',
  failed: '失败', lost: '失联', unknown: '未知',
};

function detailRows(rows) {
  return rows.filter((r) => r[1]).map((r) =>
    '<dt>' + esc(r[0]) + '</dt><dd' + (r[2] ? ' title="' + escAttr(r[2]) + '"' : '') + '>' + esc(r[1]) + '</dd>').join('');
}

function loadDetails() {
  const box = document.getElementById('wb-details');
  if (!box) return;
  const s = subject();
  if (!s) { box.innerHTML = '<div class="files-empty">选择一个会话查看详情</div>'; return; }
  const sd = s.sd || {};
  const base = [
    ['会话', s.key],
    ['模型', sd.model || ''],
    ['工作目录', sd.workspace || ''],
    ['最后活动', formatTimeShort(sd.last_active), formatAbsTime(sd.last_active)],
  ];
  box.innerHTML = detailRows(base);
  fetchJSON(NZ_CONTRACT.API.sessions_detail + '?key=' + encodeURIComponent(s.key)).then((d) => {
    const cur = subject();
    if (!cur || cur.sid !== s.sid || !d) return;
    const h = d.health;
    const since = h && h.since ? Date.parse(h.since) : 0;
    const healthText = h ? (HEALTH_LABEL[h.state] || h.state) + (since ? ' · 自 ' + formatTimeShort(since) + ' 起' : '') : '';
    box.innerHTML = detailRows([
      base[0],
      ['跨会话名字', d.name || ''],
      ['工作区', d.workspace ? d.workspace.name : ''],
      ['健康状态', healthText, h ? (h.evidence || '') : ''],
      ...base.slice(1),
    ]);
  }).catch(() => {});
}

// ---- tab wiring ----

function paintTab(tab) {
  const body = document.getElementById('fv-body');
  if (tab === 'preview' && body && !body.childElementCount) {
    body.innerHTML = '<div class="fv-loading">还没有打开的文件：在「产出」或「文件」里点一个</div>';
  } else if (tab === 'outputs') { paintOutputs(); refreshOutputs(false); }
  else if (tab === 'files') loadFiles(st.files.root === 'session:' + (subject()?.key || '') ? st.files.dir : '');
  else if (tab === 'details') loadDetails();
}

nzBus.addEventListener('workbench:tab', (e) => paintTab(e.detail.tab));

// noteWorkbenchEvent takes a live event of the subject.
export function noteWorkbenchEvent(ev) {
  if (ev.activity || ev.type === 'result') scheduleRefresh();
}

// session:event is a live event of the selected session (event_stream.js);
// on the Planner page the selection is not the subject.
nzBus.addEventListener('session:event', (e) => {
  if (ui.activeView !== 'planner') noteWorkbenchEvent(e.detail.ev);
});

// followWorkbenchSubject re-points the tabs after the subject changed: the
// session's outputs seed its seen set, and an open tab repaints for it.
export function followWorkbenchSubject() {
  st.files.root = '';
  refreshOutputs(false);
  if (workbench.isOpen() && workbench.tab() !== 'preview') paintTab(workbench.tab());
}

// session:select follows the selection (selectSession, and the new-session
// paths in auth_modal.js).
nzBus.addEventListener('session:select', followWorkbenchSubject);

// workbench-toggle is the session header's 工作台 button; with nothing
// previewed it opens on 产出.
registerActions({
  'workbench-toggle': () => {
    if (workbench.isOpen()) { workbench.close(); return; }
    const body = document.getElementById('fv-body');
    workbench.open(body && body.querySelector(':not(.fv-loading)') ? undefined : 'outputs');
  },
});

document.addEventListener('DOMContentLoaded', () => {
  document.getElementById('wb-outputs')?.addEventListener('click', (e) => {
    const row = e.target.closest('.files-row[data-kind="output"]');
    if (!row) return;
    const o = st.outputs[+row.dataset.i];
    if (o && isPreviewableOutput(o)) openOutput(o);
  });
  const files = document.querySelector('#fv-drawer [data-wb-pane="files"]');
  if (files) files.addEventListener('click', onFilesClick);
});
