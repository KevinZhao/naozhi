// workbench_panes.js — the workbench's 产出, 文件 and 详情 tabs, and its
// auto-open: when the selected session makes a new previewable output the
// workbench opens on 预览 and shows it (docs/rfc/planner.md §10, §15.1 W1).
// Each new output opens it once, so a workbench the operator closed stays
// closed until the next one.
//
// Outputs come from GET /api/sessions/outputs?key= (planner/activity), which
// is refetched shortly after a tool event the backend tagged with an
// activity and after a turn ends (the turn-end scan reports files written by
// Bash). The tabs follow the selected session — a remote one's outputs and
// tree are read from its node, when the node serves its files — and
// planner:main on the Planner page (planner_view.js feeds its events to
// noteWorkbenchEvent).
import { NZ_CONTRACT } from './contract.js';
import { selection, sessionList, ui } from './state.js';
import { esc, escAttr, fetchJSON, nzBus, registerActions, showToast } from './nz_util.js';
import { REMOTE_FILES_UNSUPPORTED, isDiscoveredKey, nodeServesFiles, sid } from './session_ident.js';
import { notePreviewEvent, openFilePreview, resolveProjectForAbsPath } from './file_refs.js';
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
  files: { root: '', node: 'local', dir: '', label: '' },
};

const PLANNER_KEY = 'planner:main';
// plannerSubject is what the tabs describe on the Planner page: planner:main,
// or the session opened in its middle column (planner_chat.js).
let plannerSubject = PLANNER_KEY;

// subject is the session the tabs describe and the node it runs on, or
// null: discovered sessions have no outputs index or readable tree.
function subject() {
  if (ui.activeView === 'planner') {
    const key = plannerSubject;
    return { key, node: 'local', sid: sid(key, 'local'), sd: key === PLANNER_KEY ? null : sessionList.sessionsData[sid(key, 'local')] || null };
  }
  const key = selection.key;
  if (!key || isDiscoveredKey(key)) return null;
  const node = selection.node || 'local';
  return { key, node, sid: sid(key, node), sd: sessionList.sessionsData[sid(key, node)] || null };
}

// nodeQuery is the &node= of a file API call about s.
function nodeQuery(s) { return s.node !== 'local' ? '&node=' + encodeURIComponent(s.node) : ''; }

// ---- outputs ----

function refreshOutputs(auto) {
  const s = subject();
  if (!s || !nodeServesFiles(s.node)) { st.outputs = []; st.outputsFor = ''; paintOutputs(); return Promise.resolve(); }
  return fetchJSON(NZ_CONTRACT.API.sessions_outputs + '?key=' + encodeURIComponent(s.key) + nodeQuery(s)).then((res) => {
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
    // cover the view. Nor over the Planner drawer: opening it would put the
    // drawer away mid-sentence.
    const docks = !document.body.classList.contains('nz-pld-open') && (ui.activeView === 'planner' ||
      (ui.activeView === 'chat' && !document.body.classList.contains('mobile-list-view')));
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
  const s = subject();
  box.innerHTML = n
    ? outputRowsHtml(st.outputs)
    : '<div class="files-empty">' + (!s ? '选择一个会话查看它的产出' : nodeServesFiles(s.node) ? '这个会话还没有产出' : REMOTE_FILES_UNSUPPORTED) + '</div>';
}

// previewRef maps an output to the file API's root + path: its own session
// root while the session still works where it was written, else the project
// holding the directory it was written in (the path is relative to the
// session's tree root, which is that directory unless it sat below a git
// root). null when neither resolves. node is where the output was made.
export function previewRef(o, node = 'local') {
  if (o.root) return { project: o.root, path: o.path };
  const p = o.dir ? resolveProjectForAbsPath(o.dir.replace(/\/$/, '') + '/' + o.path, node) : null;
  return p ? { project: p.name, path: p.relPath } : null;
}

// openOutput shows output o of the subject on the 预览 tab.
export function openOutput(o) {
  const node = subject()?.node || 'local';
  const ref = previewRef(o, node);
  if (!ref) { showToast('这个产出所在的目录已不在可浏览范围，无法预览', 'warning'); return; }
  openFilePreview({ dataset: { project: ref.project, node, path: ref.path, mime: mimeForPath(ref.path), displayPath: o.path } });
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
  if (!nodeServesFiles(s.node)) {
    crumbs.innerHTML = '';
    box.innerHTML = '<div class="files-empty">' + REMOTE_FILES_UNSUPPORTED + '</div>';
    st.files.root = '';
    return;
  }
  const root = 'session:' + s.key;
  if (st.files.root !== root || st.files.node !== s.node) st.files.label = s.sd.workspace.split('/').filter(Boolean).pop() || s.sd.workspace;
  st.files.root = root;
  st.files.node = s.node;
  st.files.dir = dir || '';
  box.innerHTML = '<div class="files-empty">加载中…</div>';
  const qs = 'root=' + encodeURIComponent(root) + (st.files.dir ? '&dir=' + encodeURIComponent(st.files.dir) : '') + nodeQuery(s);
  fetchJSON(NZ_CONTRACT.API.projects_files_list + '?' + qs).then((res) => {
    if (st.files.root !== root || st.files.node !== s.node || st.files.dir !== (dir || '')) return;
    // The server names the tree the session root resolved to (its git root
    // can sit above the session's cwd).
    if (res && res.root_name) st.files.label = res.root_name;
    crumbs.innerHTML = crumbsHtml('会话 · ' + st.files.label, st.files.dir);
    box.innerHTML = fileRowsHtml(res, st.files.dir);
  }).catch((e) => {
    crumbs.innerHTML = crumbsHtml('会话 · ' + st.files.label, st.files.dir);
    box.innerHTML = '<div class="files-empty">' + (e.status === 501 ? REMOTE_FILES_UNSUPPORTED : '加载失败：' + esc(e.message)) + '</div>';
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
    openFilePreview({ dataset: { project: st.files.root, node: st.files.node, path, mime: mimeForPath(path) } });
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
  // The detail endpoint describes local sessions only.
  if (s.node !== 'local') return;
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
  else if (tab === 'files') {
    const s = subject();
    loadFiles(s && st.files.root === 'session:' + s.key && st.files.node === s.node ? st.files.dir : '');
  }
  else if (tab === 'details') loadDetails();
}

nzBus.addEventListener('workbench:tab', (e) => paintTab(e.detail.tab));

// noteWorkbenchEvent takes a live event of the subject; the previews follow
// the files it writes (file_refs.js).
export function noteWorkbenchEvent(ev) {
  if (ev.activity || ev.type === 'result') scheduleRefresh();
  const s = subject();
  if (s) notePreviewEvent(s.key, s.node, ev);
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

// followPlannerSubject points the tabs on the Planner page at key.
export function followPlannerSubject(key) {
  plannerSubject = key || PLANNER_KEY;
  if (ui.activeView === 'planner') followWorkbenchSubject();
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
