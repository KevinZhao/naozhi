// memory_view.js — the settings view's 懂你 section (planner RFC §15.15 ⑦,
// D126): every project's Claude auto-memory on a node, the scope its Planner
// reads, grouped by project; each entry read, edited whole (with the version
// it was opened at) or deleted (5 s 撤销, then the file and its MEMORY.md
// line). Nothing is kept server-side: each open reads the disk again.
//
// Decision B (D126): another node's memories are read through
// /api/remote/{node}/… but never written; a node takes a write only from a
// request that reached its own listener from loopback, so the list says
// `writable` and the section shows 只读 otherwise.
//
// The section is mounted once into #settings-memory, which renderSettingsView
// leaves alone when it repaints the theme, so an open editor and its draft
// survive a theme click; entering the settings view re-reads the list only
// while no editor holds a draft. It picks its node itself (this one always,
// plus the nodes running a Planner), starting from the Planner page's.
//
// Never imports dashboard.js; the view switch is the shell slot.
import { NZ_CONTRACT } from './contract.js';
import { sessionList } from './state.js';
import { esc, escAttr, nzBus, registerActions, showToast } from './nz_util.js';
import { renderMd } from './render_md.js';
import { shell } from './shell.js';
import { isLocalNode, nodeAPI, plannerHost, plannerNodes } from './planner_host.js';
import { formatMonthDayTime, timeAgo } from './utilities.js';

const TYPE_LABEL = { feedback: '偏好', project: '项目', reference: '参考', user: '关于你' };
const RECENT_DAYS = 7;
const RECENT_MAX = 10;
// UNDO_MS matches the session list's 移除 · 撤销 (tuning.js).
const UNDO_MS = 5000;
const PLANNER_SOURCE = 'naozhi-planner';
// The machine itself opened by its LAN address or hostname is not loopback
// either, so the visible line names localhost (a phone has no tooltip).
const READONLY_HINT = '只读：在这台机器本机用 localhost 打开 dashboard 才能改';
const READONLY_HINT_REMOTE = '只读：在那台机器本机用 localhost 打开 dashboard 才能改';
const UNSUPPORTED = '那台机器的 naozhi 版本还不支持懂你，升级后可用';

const mem = {
  root: null,
  // node is the node shown, null until the first open picks one.
  node: null,
  data: null,
  error: '',
  loading: false,
  // rows is per entry key (project/name): {file, editing, draft, note}.
  rows: Object.create(null),
  // focus is the entry to open once the list is in ({workspace, name}).
  focus: null,
  // pending is the delete waiting out UNDO_MS.
  pending: null,
};

const keyOf = (project, name) => project + '/' + name;

// api runs one request on node (the shown one by default) and answers {status, body}; a
// network failure or timeout is status 0.
async function api(path, opts, node) {
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), (opts && opts.timeoutMs) || 10000);
  try {
    const r = await fetch(nodeAPI(node === undefined ? mem.node : node, path), { ...(opts || {}), signal: ctrl.signal });
    const text = await r.text();
    let body = null;
    try { body = text ? JSON.parse(text) : null; } catch { body = null; }
    return { status: r.status, body: body || {} };
  } catch {
    return { status: 0, body: {} };
  } finally {
    clearTimeout(timer);
  }
}

// errorText says why a request failed, for the status line or a toast.
function errorText(res, what) {
  const code = res.body && (res.body.error || res.body.code);
  if (res.status === 404 && code === 'memory_gone') return '这条已经被删了';
  if (res.status === 404) return UNSUPPORTED;
  if (res.status === 403 && code === 'memory_readonly_remote') return readOnlyHint();
  if (res.status === 413) return '太大了（超过 256 KB），请在编辑器里改';
  if (res.status === 501) return '那台机器没有运行 Planner，看不了它的记忆';
  if (res.status === 503 || res.status === 504 || res.status === 0) return what + '（节点离线或没有回应）';
  return what + (res.status ? '（HTTP ' + res.status + '）' : '');
}

// canWrite is whether the section may offer 编辑 / 删除. Another node is
// read-only whatever its own list says: the primary does not forward a
// write (node.APIRoutes), and a node reached over a loopback URL (a tunnel,
// a second instance on this host) sees a loopback RemoteAddr and answers
// writable:true.
function canWrite() {
  return isLocalNode(mem.node) && !!(mem.data && mem.data.writable);
}

function readOnlyHint() {
  return isLocalNode(mem.node) ? READONLY_HINT : READONLY_HINT_REMOTE;
}

function nodeOptions() {
  const out = ['local'];
  for (const n of plannerNodes()) if (!isLocalNode(n)) out.push(n);
  return out;
}

function nodeLabel(n) {
  if (isLocalNode(n)) return '本机';
  const nd = sessionList.nodesData[n];
  return (nd && nd.display_name) || n;
}

// dirty is whether a draft would be lost by re-reading the list: one in an
// editor, or one shown as saved while its PUT is still out (a failed save
// puts it back in the editor).
function dirty() {
  for (const k in mem.rows) {
    const r = mem.rows[k];
    if (r.saving) return true;
    if (r.editing && r.file && r.draft !== r.file.text) return true;
  }
  return false;
}

// mountMemoryView builds the section once into el.
export function mountMemoryView(el) {
  if (!el || mem.root) return;
  mem.root = el;
  el.innerHTML =
    '<div class="mem-head"><h2>懂你</h2><select class="mem-node" aria-label="看哪台机器的记忆" hidden></select></div>' +
    '<p class="mem-intro"></p>' +
    '<div class="mem-ro" hidden></div>' +
    '<input type="search" class="mem-filter" placeholder="按标题、描述、正文筛选" aria-label="筛选记忆">' +
    '<div class="mem-body" aria-live="polite"></div>';
  el.querySelector('.mem-node').addEventListener('change', (e) => {
    const next = e.target.value;
    if (dirty()) {
      e.target.value = mem.node;
      showToast('先保存或取消正在改的那条', 'info');
      return;
    }
    mem.node = next;
    load();
  });
  el.querySelector('.mem-filter').addEventListener('input', applyFilter);
  el.addEventListener('click', onClick);
  el.addEventListener('input', (e) => {
    const ta = e.target.closest('.mem-edit');
    if (!ta) return;
    const row = mem.rows[ta.dataset.key];
    if (row) row.draft = ta.value;
  });
}

// showMemoryView is the settings view's entry: pick the node and re-read,
// unless an editor holds a draft (the list then stays as it is).
export function showMemoryView() {
  if (!mem.root) return;
  const opts = nodeOptions();
  if (mem.node === null || !opts.includes(mem.node)) {
    mem.node = opts.includes(plannerHost.node) ? plannerHost.node : 'local';
  }
  paintNodes();
  if (dirty()) {
    if (mem.focus) focusEntry();
    return;
  }
  load();
}

function paintNodes() {
  const sel = mem.root.querySelector('.mem-node');
  const opts = nodeOptions();
  if (!opts.includes(mem.node)) opts.push(mem.node);
  sel.innerHTML = opts.map((n) => '<option value="' + escAttr(n) + '"' + (n === mem.node ? ' selected' : '') + '>' + esc(nodeLabel(n)) + '</option>').join('');
  sel.hidden = opts.length < 2;
}

function paintIntro() {
  const intro = mem.root.querySelector('.mem-intro');
  const plannerOff = isLocalNode(mem.node) && plannerHost.localOn === false;
  intro.textContent = (plannerOff ? '各项目会话的 auto-memory。' : 'Planner 和各项目的会话记住的关于你和你项目的事。') +
    '这里能看到它们读的全部内容，改和删都直接写回文件，对之后启动或续接的会话生效（已经在跑的会话在下次压缩或重启前仍按旧的）。' +
    (plannerOff ? '' : '想让它以后都这样，就对 Planner 说"记下来"。');
  const ro = mem.root.querySelector('.mem-ro');
  const readOnly = !!mem.data && !canWrite();
  ro.hidden = !readOnly;
  ro.textContent = readOnly ? readOnlyHint() : '';
  ro.title = readOnly ? '改和删只接受从这台机器自己（localhost）打开的页面' : '';
}

async function load() {
  // A delete still waiting would come back with the fresh list.
  if (mem.pending) mem.pending.commit();
  mem.loading = true;
  const node = mem.node;
  paintBody();
  const res = await api(NZ_CONTRACT.API.memories, { timeoutMs: 15000 });
  if (node !== mem.node) return;
  mem.loading = false;
  if (res.status === 200 && Array.isArray(res.body.groups)) {
    mem.data = res.body;
    mem.error = '';
    mem.rows = Object.create(null);
  } else {
    mem.data = null;
    mem.error = errorText(res, '读不到记忆');
  }
  paintBody();
  if (mem.focus) focusEntry();
}

// recentEntries are the entries written in the last RECENT_DAYS, newest
// first.
function recentEntries() {
  const since = Date.now() - RECENT_DAYS * 86400000;
  const out = [];
  for (const g of mem.data.groups) {
    for (const e of g.entries) {
      const at = Date.parse(e.modified) || 0;
      if (at >= since) out.push({ g, e, at });
    }
  }
  out.sort((a, b) => b.at - a.at);
  return out;
}

function typeTag(t) {
  if (!t) return '';
  return '<span class="mem-tag">' + esc(TYPE_LABEL[t] || t) + '</span>';
}

function plannerTag(e) {
  if (e.source !== PLANNER_SOURCE) return '';
  const at = Date.parse(e.remembered_at);
  return '<span class="mem-tag mem-tag-pl">Planner 记的' + (at ? ' · ' + esc(formatMonthDayTime(at)) : '') + '</span>';
}

function entryHead(g, e) {
  const at = Date.parse(e.modified) || 0;
  return '<button type="button" class="mem-row-h" data-mem="toggle" aria-expanded="false">' +
    '<span class="mem-title">' + esc(e.title || e.name) + '</span>' + typeTag(e.type) + plannerTag(e) +
    (e.indexed ? '' : '<span class="mem-tag mem-tag-warn" title="MEMORY.md 里没有链接它的行，会话启动时不会看到这条的索引">没进索引</span>') +
    (e.description ? '<span class="mem-desc">' + esc(e.description) + '</span>' : '') +
    '<span class="mem-time" title="' + escAttr(at ? formatMonthDayTime(at) : '') + '">' + esc(at ? timeAgo(at) : '') + '</span>' +
    '</button>';
}

function rowHtml(g, e, recent) {
  const key = keyOf(g.project, e.name);
  const filter = [e.title, e.name, e.description, e.snippet].join('\n').toLowerCase();
  return '<div class="mem-row' + (recent ? ' mem-row-recent' : '') + '" data-key="' + escAttr(key) + '" data-project="' + escAttr(g.project) +
    '" data-name="' + escAttr(e.name) + '" data-filter="' + escAttr(filter) + '"' + (recent ? ' data-recent="1"' : '') + '>' +
    entryHead(g, e) + (recent ? '<span class="mem-where">' + esc(g.label) + '</span>' : '') +
    '<div class="mem-detail" hidden></div></div>';
}

function indexRowHtml(g) {
  const key = keyOf(g.project, 'MEMORY');
  return '<div class="mem-row mem-row-index" data-key="' + escAttr(key) + '" data-project="' + escAttr(g.project) + '" data-name="MEMORY" data-filter="memory.md">' +
    '<button type="button" class="mem-row-h" data-mem="toggle" aria-expanded="false"><span class="mem-title">索引 MEMORY.md</span>' +
    '<span class="mem-desc">每个会话启动时读</span><span class="mem-time">' + esc(g.index.lines + ' 行') + '</span></button>' +
    '<div class="mem-detail" hidden></div></div>';
}

function groupHeadText(g) {
  const parts = [g.label, g.entries.length + ' 条'];
  if (g.index) parts.push('索引 ' + g.index.lines + ' 行');
  const at = Date.parse(g.modified);
  if (at) parts.push(formatMonthDayTime(at).split(' ')[0].replace('/', '-') + ' 改过');
  return parts.join(' · ');
}

function groupHtml(g) {
  const folded = g.kind === 'temp';
  const hint = g.kind === 'planner' ? '只有 Planner 读' : (g.label !== g.project ? '目录 ' + g.project : '目录名是 Claude 编码过的路径');
  return '<details class="mem-grp" data-project="' + escAttr(g.project) + '"' + (folded ? '' : ' open') + '>' +
    '<summary class="mem-grp-h" title="' + escAttr(hint) + '"><span class="mem-grp-l">' + esc(groupHeadText(g)) + '</span>' +
    (g.kind === 'planner' ? '<span class="mem-tag">只有 Planner 读</span>' : '') +
    (g.dangling ? '<span class="mem-tag mem-tag-warn">索引里 ' + g.dangling + ' 行指向不存在的文件</span>' : '') + '</summary>' +
    (g.index ? indexRowHtml(g) : '') + g.entries.map((e) => rowHtml(g, e, false)).join('') + '</details>';
}

function paintBody() {
  if (!mem.root) return;
  paintIntro();
  const body = mem.root.querySelector('.mem-body');
  if (mem.loading && !mem.data) { body.innerHTML = '<div class="mem-empty">读取中…</div>'; return; }
  if (mem.error) { body.innerHTML = '<div class="mem-empty mem-err">' + esc(mem.error) + '</div>'; return; }
  if (!mem.data) { body.innerHTML = ''; return; }
  const parts = [];
  if (mem.data.claude_md) {
    parts.push('<div class="mem-row mem-row-claudemd" data-key="/CLAUDE" data-project="" data-name="CLAUDE" data-filter="claude.md">' +
      '<button type="button" class="mem-row-h" data-mem="toggle" aria-expanded="false"><span class="mem-title">~/.claude/CLAUDE.md</span>' +
      '<span class="mem-tag">只读</span><span class="mem-desc">Planner 读；naozhi 拉起的会话不读</span></button><div class="mem-detail" hidden></div></div>');
  }
  const recent = recentEntries();
  if (recent.length) {
    parts.push('<div class="mem-recent"><h3>最近 7 天</h3>' + recent.slice(0, RECENT_MAX).map((x) => rowHtml(x.g, x.e, true)).join('') +
      (recent.length > RECENT_MAX ? '<div class="mem-more">还有 ' + (recent.length - RECENT_MAX) + ' 条</div>' : '') + '</div>');
  }
  parts.push('<div class="mem-empty mem-none" hidden>这台机器上还没有记忆</div>');
  parts.push(mem.data.groups.map(groupHtml).join(''));
  body.innerHTML = parts.join('');
  applyFilter();
}

// emptied is whether a group's last entry was removed here (it may still be
// waiting out 撤销): the group then goes with it, while a group the server
// lists empty stays as it came.
function emptied(g) {
  return g.removed > 0 && !g.entries.length && !g.index;
}

// applyFilter repaints what the rows imply: which rows and groups the filter
// keeps, each group's head count (from the data, which a removed entry has
// already left), an emptied group, an emptied 最近 7 天, and the empty state.
function applyFilter() {
  if (!mem.root) return;
  const q = (mem.root.querySelector('.mem-filter').value || '').trim().toLowerCase();
  for (const row of mem.root.querySelectorAll('.mem-row')) {
    row.hidden = !!q && !(row.dataset.filter || '').includes(q) || row.classList.contains('mem-gone');
  }
  const groups = mem.data ? mem.data.groups : [];
  for (const el of mem.root.querySelectorAll('.mem-grp')) {
    const g = groups.find((x) => x.project === el.dataset.project);
    const l = el.querySelector('.mem-grp-l');
    if (g && l) l.textContent = groupHeadText(g);
    el.hidden = !!q && !el.querySelector('.mem-row:not([hidden])') || !!g && emptied(g);
    if (q && !el.hidden) el.open = true;
  }
  const recent = mem.root.querySelector('.mem-recent');
  if (recent) recent.hidden = !recent.querySelector('.mem-row:not(.mem-gone)');
  const none = mem.root.querySelector('.mem-none');
  if (none) none.hidden = !groups.every(emptied);
}

function findEntry(project, name) {
  if (!mem.data) return null;
  const g = mem.data.groups.find((x) => x.project === project);
  if (!g) return null;
  return { g, e: g.entries.find((x) => x.name === name) || null };
}

// rowEl is the row of key in its group (not the 最近 7 天 copy).
function rowEl(key) {
  for (const r of mem.root.querySelectorAll('.mem-row')) if (r.dataset.key === key && !r.dataset.recent) return r;
  return null;
}

async function onClick(e) {
  const t = e.target.closest('[data-mem]');
  if (!t || !mem.root.contains(t)) return;
  const rowNode = t.closest('.mem-row');
  const key = rowNode && rowNode.dataset.key;
  switch (t.dataset.mem) {
    case 'toggle': {
      // A 最近 7 天 row opens the entry where it lives.
      const target = rowNode.dataset.recent ? rowEl(key) : rowNode;
      if (!target) return;
      if (rowNode.dataset.recent) {
        const grp = target.closest('.mem-grp');
        if (grp) grp.open = true;
        openRow(target, true);
        target.scrollIntoView({ block: 'center' });
        return;
      }
      openRow(target, target.querySelector('.mem-detail').hidden);
      return;
    }
    case 'edit': startEdit(key); return;
    case 'cancel': cancelEdit(key); return;
    case 'save': save(key); return;
    case 'delete': remove(key); return;
    default:
  }
}

async function openRow(row, open) {
  const detail = row.querySelector('.mem-detail');
  const head = row.querySelector('.mem-row-h');
  detail.hidden = !open;
  head.setAttribute('aria-expanded', String(open));
  if (!open) return;
  const key = row.dataset.key;
  const r = mem.rows[key] || (mem.rows[key] = { file: null, editing: false, draft: '', note: '' });
  if (r.file) { paintDetail(key); return; }
  const found = findEntry(row.dataset.project, row.dataset.name);
  if (found && found.e && found.e.readonly === 'odd_name') {
    detail.innerHTML = '<div class="mem-snip">' + esc(found.e.snippet || '') + '</div><div class="mem-hint">文件名特殊，请在编辑器里看和改</div>';
    return;
  }
  detail.innerHTML = '<div class="mem-empty">读取中…</div>';
  const res = await api(NZ_CONTRACT.API.memories_file + '?project=' + encodeURIComponent(row.dataset.project) + '&name=' + encodeURIComponent(row.dataset.name));
  if (res.status !== 200) {
    detail.innerHTML = '<div class="mem-err">' + esc(errorText(res, '读不到这条')) + '</div>';
    return;
  }
  r.file = res.body;
  paintDetail(key);
}

function readOnlyWhy(f) {
  switch (f.readonly) {
    case 'too_large': return '太大（超过 256 KB），请在编辑器里改';
    case 'not_utf8': return '不是 UTF-8 文本，请在编辑器里改';
    case 'claude_md': return '';
    default: return '';
  }
}

function paintDetail(key) {
  const row = rowEl(key);
  const r = mem.rows[key];
  if (!row || !r || !r.file) return;
  const f = r.file;
  const detail = row.querySelector('.mem-detail');
  const writable = canWrite() && !f.readonly;
  const isIndex = f.name === 'MEMORY';
  const note = r.note ? '<div class="mem-note" role="status">' + esc(r.note) + '</div>' : '';
  if (r.editing) {
    const planner = f.source === PLANNER_SOURCE;
    detail.innerHTML = note +
      (r.conflictText != null ? '<div class="mem-latest-l">现在的内容</div><pre class="mem-latest">' + esc(r.conflictText) + '</pre>' : '') +
      '<textarea class="mem-edit" data-key="' + escAttr(key) + '" spellcheck="false" aria-label="编辑这条记忆"></textarea>' +
      (planner ? '<div class="mem-hint">这条是 Planner 记的：它再记同名的一条会整条替换，包括你改的部分；去掉 source 行后，这条只能在这里删。</div>' : '') +
      '<div class="mem-acts"><button type="button" class="pl-btn pri" data-mem="save">保存</button>' +
      '<button type="button" class="pl-btn" data-mem="cancel">取消</button></div>';
    const ta = detail.querySelector('.mem-edit');
    ta.value = r.draft;
    return;
  }
  const why = readOnlyWhy(f);
  detail.innerHTML = note + '<div class="mem-md">' + (isIndex || f.project === '' ? '<pre class="mem-raw">' + esc(f.body) + '</pre>' : renderMd(f.body || '')) + '</div>' +
    (f.truncated ? '<div class="mem-hint">（已截断）</div>' : '') +
    (why ? '<div class="mem-hint">' + esc(why) + '</div>' : '') +
    (writable && !r.saving ? '<div class="mem-acts"><button type="button" class="pl-btn" data-mem="edit">编辑</button>' +
      (isIndex ? '' : '<button type="button" class="pl-btn mem-del" data-mem="delete">删除</button>') + '</div>' : '');
}

function startEdit(key) {
  const r = mem.rows[key];
  if (!r || !r.file) return;
  r.editing = true;
  r.draft = r.file.text;
  r.note = '';
  r.conflictText = null;
  paintDetail(key);
  const row = rowEl(key);
  const ta = row && row.querySelector('.mem-edit');
  if (ta) ta.focus();
}

function cancelEdit(key) {
  const r = mem.rows[key];
  if (!r) return;
  r.editing = false;
  r.note = '';
  r.conflictText = null;
  paintDetail(key);
}

// draftBody is what the rendered view shows of text before the server has
// read it back: the part after the frontmatter, or all of it for the index
// and CLAUDE.md (as the server's body).
function draftBody(f, text) {
  if (f.name === 'MEMORY' || f.project === '') return text;
  const m = /^\uFEFF?---\r?\n[\s\S]*?\n---[^\n]*(?:\n|$)/.exec(text);
  return (m ? text.slice(m[0].length) : text.replace(/^\uFEFF/, '')).replace(/^[\r\n]+/, '');
}

// save leaves the editor at once, showing the draft as the saved text, then
// PUTs it with the version it was opened at. Until the PUT answers the row
// offers no 编辑 / 删除 (either would act on the version this write
// replaces) and the draft counts as unsaved (dirty), so nothing re-reads
// over it. A refused save goes back to the editor exactly as it was, the
// draft in it.
async function save(key) {
  const r = mem.rows[key];
  if (!r || !r.file || r.saving || mem.pending && mem.pending.key === key) return;
  const opened = r.file;
  const draft = r.draft;
  const conflictText = r.conflictText;
  const node = mem.node;
  r.saving = true;
  r.editing = false;
  r.conflictText = null;
  r.file = { ...opened, text: draft, body: draftBody(opened, draft) };
  r.note = '保存中…';
  paintDetail(key);
  const res = await api(NZ_CONTRACT.API.memories_file, {
    method: 'PUT', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ project: opened.project, name: opened.name, text: draft, version: opened.version }),
  }, node);
  r.saving = false;
  if (res.status === 200) {
    r.file = { ...r.file, version: res.body.version, modified: res.body.modified, size: res.body.size };
    r.note = '已保存';
    paintDetail(key);
    // The rendered view and the title then come from a fresh read, unless
    // the row moved on meanwhile (another edit, a re-read list).
    const version = r.file.version;
    api(NZ_CONTRACT.API.memories_file + '?project=' + encodeURIComponent(opened.project) + '&name=' + encodeURIComponent(opened.name), undefined, node).then((fresh) => {
      if (fresh.status !== 200 || mem.rows[key] !== r || r.editing || r.saving || !r.file || r.file.version !== version) return;
      r.file = fresh.body;
      paintDetail(key);
    });
    return;
  }
  r.file = opened;
  r.editing = true;
  r.draft = draft;
  r.conflictText = conflictText;
  if (res.status === 409 && res.body && res.body.error === 'memory_changed') {
    // Someone else wrote it: take their version, keep the draft below.
    const latest = res.body;
    const at = Date.parse(latest.modified);
    r.file = { ...r.file, ...latest };
    r.conflictText = latest.text || latest.body || '';
    r.note = '这条在你打开后被改过' + (at ? '（' + formatMonthDayTime(at).split(' ')[1] + '）' : '') + '，已载入最新内容；你的改动留在下面的框里，复制后再保存';
    paintDetail(key);
    return;
  }
  if (res.status === 404 && res.body && res.body.error === 'memory_gone') {
    r.note = '这条已经被删了';
    paintDetail(key);
    return;
  }
  r.note = errorText(res, '没保存成');
  paintDetail(key);
}

// remove hides the entry at once and offers 撤销 for UNDO_MS; only then
// does the DELETE go out. A failed delete puts the row back and says why.
function remove(key) {
  const r = mem.rows[key];
  if (!r || !r.file || r.saving) return;
  if (mem.pending) mem.pending.commit();
  const rows = [...mem.root.querySelectorAll('.mem-row')].filter((x) => x.dataset.key === key);
  for (const x of rows) x.classList.add('mem-gone');
  // The entry leaves the data with its row, so the group's count and an
  // emptied group follow at once; restore puts it back where it was.
  const found = findEntry(r.file.project, r.file.name);
  const g = found && found.e ? found.g : null;
  const at = g ? g.entries.indexOf(found.e) : -1;
  if (g) {
    g.entries.splice(at, 1);
    g.removed = (g.removed || 0) + 1;
  }
  applyFilter();
  const title = r.file.title || r.file.name;
  const node = mem.node;
  const p = { key };
  const settle = () => {
    clearTimeout(p.timer);
    if (mem.pending === p) { mem.pending = null; hideUndo(); }
  };
  const restore = (msg) => {
    for (const x of rows) x.classList.remove('mem-gone');
    // A list re-read meanwhile has its own copy of the entry.
    if (g && mem.data && mem.data.groups.includes(g) && !g.entries.includes(found.e)) {
      g.entries.splice(Math.min(at, g.entries.length), 0, found.e);
      g.removed--;
    }
    applyFilter();
    if (msg) showToast(msg, 'error');
  };
  p.undo = () => { settle(); restore(''); };
  p.commit = async (leaving) => {
    settle();
    const path = NZ_CONTRACT.API.memories_file + '?project=' + encodeURIComponent(r.file.project) + '&name=' + encodeURIComponent(r.file.name) +
      '&version=' + encodeURIComponent(r.file.version);
    const res = await api(path, { method: 'DELETE', keepalive: leaving === true }, node);
    if (res.status === 200 || (res.status === 404 && res.body.error === 'memory_gone')) {
      dropEntry(r.file.project, r.file.name);
      return;
    }
    if (res.status === 409) {
      // The 409 carries the file as it is now: without it the row would
      // keep showing the old text and every further 删除 would send the
      // same stale version.
      const latest = { ...(res.body || {}) };
      delete latest.error;
      if (latest.version) {
        r.file = { ...r.file, ...latest };
        r.editing = false;
        r.conflictText = null;
        r.note = '这条在你打开后被改过，下面是最新内容';
        if (mem.rows[key] === r && mem.node === node) paintDetail(key);
      }
      restore('这条刚被改过，没有删');
    }
    else if (res.status === 503 || res.status === 504 || res.status === 0) restore('没删成（节点离线）');
    else restore(errorText(res, '没删成'));
  };
  mem.pending = p;
  p.timer = setTimeout(p.commit, UNDO_MS);
  showUndo('已删除「' + title + '」');
}

// dropEntry forgets a deleted entry: remove took it out of the data already,
// a list re-read since may still have it.
function dropEntry(project, name) {
  const found = findEntry(project, name);
  if (found && found.e) {
    found.g.entries = found.g.entries.filter((x) => x !== found.e);
    found.g.removed = (found.g.removed || 0) + 1;
  }
  delete mem.rows[keyOf(project, name)];
  for (const x of [...mem.root.querySelectorAll('.mem-row')]) if (x.dataset.key === keyOf(project, name)) x.remove();
  applyFilter();
}

function showUndo(text) {
  let el = document.getElementById('mem-undo-toast');
  if (!el) {
    el = document.createElement('div');
    el.id = 'mem-undo-toast';
    el.className = 'undo-toast';
    el.setAttribute('role', 'status');
    el.setAttribute('aria-live', 'polite');
    el.innerHTML = '<span class="undo-toast-text"></span><button type="button" class="undo-toast-btn">撤销</button>';
    el.querySelector('button').addEventListener('click', () => { if (mem.pending) mem.pending.undo(); });
    document.body.appendChild(el);
  }
  el.querySelector('.undo-toast-text').textContent = text;
  el.hidden = false;
}

function hideUndo() {
  const el = document.getElementById('mem-undo-toast');
  if (el) el.hidden = true;
}

// A page left within UNDO_MS still deletes what it was told to.
window.addEventListener('pagehide', () => { if (mem.pending) mem.pending.commit(true); });

// focusEntry opens the entry asked for (the 记下了 card's workspace and
// name) once the list is in.
function focusEntry() {
  const want = mem.focus;
  mem.focus = null;
  if (!want || !mem.data || !want.name) return;
  const ws = String(want.workspace || '').toLowerCase();
  const g = mem.data.groups.find((x) => (ws === 'global' ? x.kind === 'planner'
    : (x.workspace_id && x.workspace_id.toLowerCase() === ws) || (x.workspace_name && x.workspace_name.toLowerCase() === ws)) &&
    x.entries.some((e) => e.name === want.name));
  const row = g && rowEl(keyOf(g.project, want.name));
  if (!row) { showToast('没找到这一条（可能已经撤回或删了）', 'info'); return; }
  const grp = row.closest('.mem-grp');
  if (grp) grp.open = true;
  row.classList.add('mem-row-focus');
  openRow(row, true);
  row.scrollIntoView({ block: 'center' });
}

// openMemory switches to the settings view's 懂你 section on node, opening
// the entry {workspace, name} when given.
export function openMemory(detail) {
  const d = detail || {};
  const node = d.node || plannerHost.node || 'local';
  if (!dirty()) mem.node = node;
  mem.focus = d.name ? { workspace: d.workspace || '', name: d.name } : null;
  if (shell.setActivityView) shell.setActivityView('settings');
  if (mem.root) mem.root.scrollIntoView({ block: 'start' });
}

// The asset browser's 记忆 kind links here (it imports nothing of ours).
nzBus.addEventListener('memory:open', (e) => openMemory(e.detail));

// 懂你（记忆）› in the Planner page's "⋯" and 在懂你里看 › on a 记下了 card.
registerActions({
  'memory-open': (el) => {
    const more = el.closest('.pl-more');
    if (more) more.open = false;
    openMemory({ node: el.dataset.node || plannerHost.node, workspace: el.dataset.ws || '', name: el.dataset.name || '' });
  },
});
