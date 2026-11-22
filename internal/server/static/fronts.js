// fronts.js — the "工作线" group of the Planner page's left column (planner
// RFC §15.19, D134): the lines of work the Planner keeps in its ledger
// (planner/fronts.json, §15.17), from GET /api/planner/fronts, the latest
// updated first.
//
// A card is the line's name, where it stands, then one muted line: the next
// step, who it waits on, and how long it has gone unchanged once that is
// three days. "⋯" opens what was decided on it (the latest three) and the
// card's controls: 接着做 puts "继续「名称」" in the Planner's input box, for
// the Planner to find the line's session; 编辑 and 结束 are the user's own
// change to the ledger, which the Planner sees in its next snapshot.
//
// Under a line are its open items (未了事项, §15.23, D143): the things left
// undone on it, the oldest first, five shown and the rest behind "另有 N
// 条". Each says where it came from (a session's report, the patrol, the
// Planner, you), opens its source (a link, or the session) and can be
// struck with "×"; an opened card takes one more typed in. The items under
// no line are listed after the cards as 未归线, by workspace.
//
// The ledger is shown and edited on the machine that keeps it: the route is
// not among a reverse node's, so the group stays hidden while the page shows
// another node's Planner; the gateway takes an edit only straight from
// loopback and says so in `writable`, without which 编辑, 结束, "×" and the
// input are not offered. With no line and no item the group takes no room.
//
// The lines are read when the page opens and after each Planner turn
// (refreshFronts, with the tasks). An edit, 结束, an item struck or added
// shows at once, ahead of the gateway (held, heldItems); a failure puts it
// back as it was.
import { NZ_CONTRACT } from './contract.js';
import { esc, escAttr, fetchJSON, registerActions, showToast } from './nz_util.js';
import { formatAbsTime } from './utilities.js';
import { isLocalNode, plannerHost } from './planner_host.js';
import { sessionTitle, workspaceName } from './planner_cards.js';

// IDLE_MS is how long a line goes unchanged before its card says so, the
// morning brief's three days.
const IDLE_MS = 3 * 24 * 3600 * 1000;
// MAX_DECISIONS is how many of a line's decisions an opened card lists.
const MAX_DECISIONS = 3;
// MAX_ITEMS is how many open items a line (or a workspace's loose ones)
// lists before "另有 N 条".
const MAX_ITEMS = 5;
// FROM names where an item came from.
const FROM = { report: '汇报', patrol: '巡查', planner: 'Planner', user: '你' };

let mount = null;
let last = null; // { fronts, items, writable }
let openId = '';
let editId = '';
let gen = 0;
// held is the user's edits this page shows ahead of the ledger, by line:
// {edit} (the fields saved; close drops the line). While the POST is out
// (upTo Infinity), and for a read started before it answered (gen ≤ upTo),
// a list still showing the line as it was must not undo the edit.
const held = new Map();
// heldItems is the same for the open items, by item: {op: 'done'} keeps a
// struck one off the list, {op: 'add', item} keeps an added one on it.
const heldItems = new Map();
// moreKeys are the item lists unfolded past MAX_ITEMS, by listKey.
const moreKeys = new Set();
let tmpSeq = 0;

const str = (v) => (typeof v === 'string' ? v : '');
// listKey names an item list: a line's, or a workspace's loose ones.
const listKey = (front, ws) => (front ? 'f:' + front : 'w:' + ws);
// waitText drops the verb the Planner often writes itself ("等客户确认").
const waitText = (w) => str(w).replace(/^等\s*/, '').trim();

function idleText(f, now) {
  const t = Date.parse(f.updated || '');
  if (isNaN(t) || now - t < IDLE_MS) return '';
  return Math.floor((now - t) / (24 * 3600 * 1000)) + ' 天没动';
}

function metaHtml(f, now) {
  const parts = [];
  if (str(f.next)) parts.push('→ ' + esc(str(f.next)));
  const w = waitText(f.waiting);
  if (w) parts.push('等 ' + esc(w));
  const idle = idleText(f, now);
  if (idle) parts.push('<span class="fr-idle">' + idle + '</span>');
  return parts.length ? '<div class="fr-meta">' + parts.join(' · ') + '</div>' : '';
}

function decisionsHtml(f) {
  const ds = Array.isArray(f.decisions) ? f.decisions.slice(0, MAX_DECISIONS) : [];
  if (!ds.length) return '';
  return '<ul class="fr-decisions" aria-label="定了的事">' + ds.map((d) => {
    const at = Date.parse(d.at || '');
    return '<li' + (isNaN(at) ? '' : ' title="' + escAttr(formatAbsTime(at)) + '"') + '>定了：' + esc(str(d.text)) + '</li>';
  }).join('') + '</ul>';
}

function editHtml(f) {
  const field = (name, label, value) =>
    '<label class="fr-field"><span>' + label + '</span>' +
    '<textarea rows="2" maxlength="200" data-field="' + name + '">' + esc(value) + '</textarea></label>';
  return '<form class="fr-edit" data-id="' + escAttr(f.id) + '">' +
    field('state', '现状', str(f.state)) + field('next', '下一步', str(f.next)) + field('waiting', '等谁', waitText(f.waiting)) +
    '<div class="tk-acts"><button type="submit" class="pl-btn pri tk-act">保存</button>' +
    '<button type="button" class="pl-btn tk-act" data-action="front-edit-cancel">取消</button></div></form>';
}

// sourceHtml opens where an item came from: a web page in a new tab, a
// session in the middle column. A source that is neither (a promise, "11-22
// 用户说", a check) is in the item's tooltip only.
function sourceHtml(src) {
  if (/^https?:\/\//i.test(src)) {
    return '<a class="pl-link fr-src" href="' + escAttr(src) + '" target="_blank" rel="noopener noreferrer" title="' + escAttr(src) + '">出处</a>';
  }
  if (!src.includes(':') || /^(check|patrol):/.test(src) || /\s/.test(src)) return '';
  return '<button type="button" class="pl-link fr-src" data-action="planner-open-session" data-key="' + escAttr(src) + '" title="' + escAttr('打开会话：' + sessionTitle(src)) + '">出处</button>';
}

function itemHtml(it, writable) {
  const id = str(it.id);
  const text = str(it.text);
  const src = str(it.source);
  const from = FROM[it.from] || '';
  // One being added has no ID of the ledger's yet: nothing to strike.
  const strike = writable && !id.startsWith('tmp-')
    ? '<button type="button" class="fr-item-x" data-action="front-item-done" data-id="' + escAttr(id) + '" aria-label="划掉这一条" title="划掉：做完了，或不做了">×</button>'
    : '';
  return '<li class="fr-item" data-id="' + escAttr(id) + '">' +
    '<span class="fr-item-t" title="' + escAttr(text + (src ? '\n出处：' + src : '')) + '">' + esc(text) + '</span>' +
    (from ? '<span class="fr-from">' + from + '</span>' : '') + sourceHtml(src) + strike + '</li>';
}

// itemsHtml lists items, MAX_ITEMS of them until unfolded.
function itemsHtml(items, key, writable) {
  if (!items.length) return '';
  const all = moreKeys.has(key);
  const shown = all ? items : items.slice(0, MAX_ITEMS);
  return '<ul class="fr-items" aria-label="未了事项">' + shown.map((it) => itemHtml(it, writable)).join('') + '</ul>' +
    (items.length > MAX_ITEMS
      ? '<button type="button" class="pl-link fr-items-more" data-action="front-items-more" data-key="' + escAttr(key) + '" aria-expanded="' + all + '">' +
        (all ? '收起' : '另有 ' + (items.length - MAX_ITEMS) + ' 条') + '</button>'
      : '');
}

// addHtml is the one-line input an item is added with: under the line
// front, or among workspace ws's loose ones.
function addHtml(front, ws, label) {
  return '<form class="fr-add" data-key="' + escAttr(listKey(front, ws)) + '" data-front="' + escAttr(front) + '" data-ws="' + escAttr(ws) + '">' +
    '<input type="text" maxlength="100" autocomplete="off" placeholder="加一条未了事项，回车记下" aria-label="' + escAttr(label) + '"></form>';
}

function cardHtml(f, items, writable, now) {
  const id = str(f.id);
  const open = openId === id;
  const editing = writable && editId === id;
  const acts = '<button type="button" class="pl-btn tk-act" data-action="front-continue" data-id="' + escAttr(id) + '" title="在 Planner 的输入框里写上“继续「' + escAttr(str(f.name)) + '」”">接着做</button>' +
    (writable
      ? '<button type="button" class="pl-btn tk-act" data-action="front-edit" data-id="' + escAttr(id) + '">编辑</button>' +
        '<button type="button" class="pl-btn tk-act" data-action="front-close" data-id="' + escAttr(id) + '" title="这条线结束了；以后再提到它会重新打开">结束</button>'
      : '');
  return '<div class="tk-card fr-card' + (open ? ' tk-acts-open' : '') + '" role="listitem" data-id="' + escAttr(id) + '">' +
    '<div class="tk-top"><div class="tk-title"><span class="tk-title-t">' + esc(str(f.name) || id) + '</span></div>' +
      '<button type="button" class="tk-more" data-action="front-more" data-id="' + escAttr(id) + '" aria-expanded="' + open + '" aria-label="' + escAttr((str(f.name) || id) + ' 的更多操作') + '">⋯</button></div>' +
    (editing ? editHtml(f)
      : (str(f.state) ? '<div class="fr-state">' + esc(str(f.state)) + '</div>' : '') + metaHtml(f, now) +
        itemsHtml(items, listKey(id, ''), writable) +
        (open ? decisionsHtml(f) + (writable ? addHtml(id, '', '给「' + (str(f.name) || id) + '」加一条未了事项') : '') + '<div class="tk-acts">' + acts + '</div>' : '')) +
    '</div>';
}

// looseHtml lists the items under no line, by workspace in the order their
// first item came.
function looseHtml(items, writable) {
  if (!items.length) return '';
  const by = new Map();
  for (const it of items) {
    const ws = str(it.workspace);
    if (!by.has(ws)) by.set(ws, []);
    by.get(ws).push(it);
  }
  let html = '<h3 class="fr-loose-h">未归线 <span class="tk-n">' + items.length + '</span></h3>';
  for (const [ws, list] of by) {
    const name = ws ? workspaceName(ws) || ws : '没有工作区';
    html += '<div class="fr-loose" data-ws="' + escAttr(ws) + '"><div class="fr-loose-ws" title="' + escAttr(ws) + '">' + esc(name) + '</div>' +
      itemsHtml(list, listKey('', ws), writable) + (writable ? addHtml('', ws, '给「' + name + '」加一条未了事项') : '') + '</div>';
  }
  return html;
}

// applyItems returns items with the held ones applied: a struck one off the
// list, an added one on it.
function applyItems(items) {
  for (const [id, h] of heldItems) {
    if (h.op === 'done') items = items.filter((it) => it.id !== id);
    else if (!items.some((it) => it.id === h.item.id)) items = items.concat([h.item]);
  }
  return items;
}

// applyEdit returns fronts with edit applied as the ledger applies it: the
// fields given, the line then the latest updated (first); close takes it
// off the open list.
function applyEdit(fronts, edit) {
  const i = fronts.findIndex((f) => f.id === edit.id);
  if (i < 0) return fronts;
  const rest = fronts.slice(0, i).concat(fronts.slice(i + 1));
  if (edit.close) return rest;
  const f = Object.assign({}, fronts[i], { updated: new Date().toISOString() });
  for (const k of ['state', 'next', 'waiting']) if (typeof edit[k] === 'string') f[k] = edit[k];
  return [f].concat(rest);
}

// frontsHtml renders a GET /api/planner/fronts answer, "" with no line and
// no open item. An item goes under the line the ledger shows it under; one
// whose line this page no longer lists is loose.
export function frontsHtml(data, now = Date.now()) {
  const fronts = data && Array.isArray(data.fronts) ? data.fronts : [];
  const items = data && Array.isArray(data.items) ? data.items : [];
  if (!fronts.length && !items.length) return '';
  const writable = !!data.writable;
  const by = new Map(fronts.map((f) => [f.id, []]));
  const loose = [];
  for (const it of items) (by.get(it.front) || loose).push(it);
  return '<h2 class="pl-group-h tk-head">工作线' + (fronts.length ? ' <span class="tk-n">' + fronts.length + '</span>' : '') + '</h2>' +
    '<div class="tk-list" role="list">' + fronts.map((f) => cardHtml(f, by.get(f.id), writable, now)).join('') + '</div>' +
    looseHtml(loose, writable);
}

// paintFronts repaints the group from the last read: the workspaces' names
// may come after it.
export function paintFronts() { paint(); }

function paint() {
  if (!mount || !mount.isConnected) return;
  const html = last ? frontsHtml(last) : '';
  const group = mount.closest('.pl-group');
  if (group) group.hidden = !html;
  if (mount._frHtml === html) return;
  // An edit in progress, and an item half typed, keep what was typed
  // through a repaint, the input its focus.
  const typed = {};
  mount.querySelectorAll('.fr-edit textarea').forEach((t) => { typed[t.dataset.field] = t.value; });
  const adding = {};
  mount.querySelectorAll('.fr-add').forEach((f) => { adding[f.dataset.key] = f.querySelector('input').value; });
  const at = document.activeElement?.closest?.('.fr-add');
  const focused = at && mount.contains(at) ? at.dataset.key : null;
  mount._frHtml = html;
  mount.innerHTML = html;
  mount.querySelectorAll('.fr-edit textarea').forEach((t) => { if (t.dataset.field in typed) t.value = typed[t.dataset.field]; });
  mount.querySelectorAll('.fr-add').forEach((f) => {
    const input = f.querySelector('input');
    if (adding[f.dataset.key]) input.value = adding[f.dataset.key];
    if (f.dataset.key === focused) input.focus();
  });
}

// refreshFronts reads the ledger and repaints the group. Another node's
// Planner has no ledger this page can read: the group stays hidden.
export async function refreshFronts() {
  if (!mount) return;
  const g = ++gen;
  if (!isLocalNode(plannerHost.node)) {
    last = null;
    paint();
    return;
  }
  try {
    const data = await fetchJSON(NZ_CONTRACT.API.planner_fronts, { timeoutMs: 8000 });
    if (g !== gen) return;
    let list = Array.isArray(data.fronts) ? data.fronts : [];
    for (const [id, h] of held) {
      if (g > h.upTo) held.delete(id);
      else list = applyEdit(list, h.edit);
    }
    for (const [id, h] of heldItems) if (g > h.upTo) heldItems.delete(id);
    last = Object.assign({}, data, { fronts: list, items: applyItems(Array.isArray(data.items) ? data.items : []) });
    if (openId && !(data.fronts || []).some((f) => f.id === openId)) openId = '';
    if (editId && !(data.fronts || []).some((f) => f.id === editId)) editId = '';
  } catch {
    // The last read stays: a blip must not empty the column.
    if (g !== gen) return;
  }
  paint();
}

// mountFronts renders the group into el and keeps it current.
export function mountFronts(el) {
  if (!el) return;
  if (mount !== el) {
    mount = el;
    el.classList.add('tk-group');
    el.addEventListener('submit', (ev) => {
      const add = ev.target.closest('.fr-add');
      if (add) {
        ev.preventDefault();
        addItem(add);
        return;
      }
      const form = ev.target.closest('.fr-edit');
      if (!form) return;
      ev.preventDefault();
      const body = { id: form.dataset.id };
      form.querySelectorAll('textarea').forEach((t) => { body[t.dataset.field] = t.value.trim(); });
      save(body, '没有保存：');
    });
  }
  return refreshFronts();
}

// save shows the edit (or 结束) at once and leaves the editor, then sends
// it. A failure puts the line back as it was, the editor reopened on what
// was typed (or its card's controls, for 结束), and says why; a line the
// ledger no longer has is gone already, which is what 结束 asked.
//
// The card stays open after an edit, so 结束 may come while the edit is
// out: what shows is then only the edit's prediction. The later save takes
// over the line as the ledger last had it (was) and holds the edit with its
// own; the earlier one, failing, leaves the page to it (its own failure or
// the read after it puts back what it must).
async function save(body, failed) {
  if (!last || !Array.isArray(last.fronts)) return;
  const prior = { openId, editId };
  const prev = held.get(body.id);
  const chained = prev && prev.upTo === Infinity ? prev : null;
  const i = last.fronts.findIndex((f) => f.id === body.id);
  const was = chained ? chained.was : { line: i < 0 ? null : last.fronts[i], at: i };
  const mark = { edit: chained ? Object.assign({}, chained.edit, body) : body, upTo: Infinity, was };
  held.set(body.id, mark);
  last = Object.assign({}, last, { fronts: applyEdit(last.fronts, body) });
  editId = '';
  if (body.close && openId === body.id) openId = '';
  paint();
  let ok = true;
  try {
    const res = await fetchJSON(NZ_CONTRACT.API.planner_fronts, {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body), timeoutMs: 8000,
    });
    // The ledger's line is what it holds now.
    if (!body.close && res && res.id === body.id && held.get(body.id) === mark && last) {
      last = Object.assign({}, last, { fronts: last.fronts.map((f) => (f.id === body.id ? Object.assign({}, f, res) : f)) });
      paint();
    }
  } catch (e) {
    ok = body.close && e.status === 404;
    if (!ok && held.get(body.id) === mark) {
      held.delete(body.id);
      if (was.line && last) {
        // Put back where it stood, among what the page lists now.
        const now = last.fronts.filter((f) => f.id !== body.id);
        last = Object.assign({}, last, { fronts: now.slice(0, was.at).concat([was.line], now.slice(was.at)) });
      }
      openId = prior.openId;
      editId = prior.editId;
      paint();
      if (!body.close && editId === body.id) {
        mount?.querySelectorAll('.fr-edit[data-id="' + CSS.escape(body.id) + '"] textarea').forEach((t) => {
          if (typeof body[t.dataset.field] === 'string') t.value = body[t.dataset.field];
        });
      }
    }
    if (!ok) showToast(failed + e.message, 'error');
  }
  if (ok && held.get(body.id) === mark) mark.upTo = gen;
  await refreshFronts();
}

const postItem = (body) => fetchJSON(NZ_CONTRACT.API.planner_fronts, {
  method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body), timeoutMs: 8000,
});

// errorText is why the ledger refused, as it said it ("开着的未了事项已有
// 300 条"), else the failure as fetchJSON words it.
function errorText(e) {
  const m = /^HTTP \d+: ([\s\S]*)$/.exec((e && e.message) || '');
  try { const j = m && JSON.parse(m[1]); if (j && j.error) return j.error; } catch { /* not JSON */ }
  return (e && e.message) || '';
}

// setItems replaces the items this page lists.
function setItems(fn) {
  if (!last) return;
  last = Object.assign({}, last, { items: fn(Array.isArray(last.items) ? last.items : []) });
  paint();
}

// strikeItem takes the item off the list at once, then tells the ledger. A
// failure puts it back where it stood and says why; one the ledger no
// longer has is struck already.
async function strikeItem(id) {
  const items = last && Array.isArray(last.items) ? last.items : [];
  const i = items.findIndex((it) => it.id === id);
  if (i < 0 || heldItems.has(id)) return;
  const was = items[i];
  const mark = { op: 'done', upTo: Infinity };
  heldItems.set(id, mark);
  setItems((list) => list.filter((it) => it.id !== id));
  try {
    await postItem({ item_done: id });
  } catch (e) {
    if (e.status !== 404) {
      heldItems.delete(id);
      setItems((list) => (list.some((it) => it.id === id) ? list : list.slice(0, i).concat([was], list.slice(i))));
      showToast('没有划掉：' + errorText(e), 'error');
      return;
    }
  }
  mark.upTo = gen;
  await refreshFronts();
}

// addItem lists what was typed at once, as the user's, then files it. A
// failure takes it off again, puts the words back in the input and says
// why.
async function addItem(form) {
  const input = form.querySelector('input');
  const text = input.value.trim().replace(/\s+/g, ' ');
  if (!text || !last) return;
  const { key, front, ws } = form.dataset;
  const tmp = { id: 'tmp-' + (++tmpSeq), text, front, workspace: ws, from: 'user', created: new Date().toISOString() };
  const mark = { op: 'add', upTo: Infinity, item: tmp };
  heldItems.set(tmp.id, mark);
  input.value = '';
  setItems((list) => list.concat([tmp]));
  const body = { item: { text } };
  if (front) body.item.front = front;
  if (ws) body.item.workspace = ws;
  try {
    const res = await postItem(body);
    // The ledger's item takes the place of the one typed.
    mark.item = Object.assign({}, tmp, res);
    setItems((list) => list.filter((it) => it.id !== mark.item.id).map((it) => (it.id === tmp.id ? mark.item : it)));
  } catch (e) {
    heldItems.delete(tmp.id);
    setItems((list) => list.filter((it) => it.id !== tmp.id));
    const again = mount?.querySelector('.fr-add[data-key="' + CSS.escape(key) + '"] input');
    if (again && !again.value) again.value = text;
    showToast('没有记下：' + errorText(e), 'error');
    return;
  }
  mark.upTo = gen;
  await refreshFronts();
}

const byId = (id) => (last && Array.isArray(last.fronts) ? last.fronts.find((f) => f.id === id) : null);

registerActions({
  'front-more': (el) => {
    const id = el.dataset.id || '';
    openId = openId === id ? '' : id;
    if (!openId || editId !== openId) editId = '';
    paint();
    if (openId) mount?.querySelector('.fr-card[data-id="' + CSS.escape(openId) + '"] .tk-act')?.focus();
  },
  // 接着做: the words go into the Planner's input box, sent by the user.
  'front-continue': (el) => {
    const f = byId(el.dataset.id || '');
    const input = document.getElementById('pl-input');
    if (!f || !input) return;
    input.value = '继续「' + (str(f.name) || f.id) + '」';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    input.focus();
  },
  'front-edit': (el) => {
    editId = el.dataset.id || '';
    paint();
    mount?.querySelector('.fr-edit textarea')?.focus();
  },
  'front-edit-cancel': () => {
    editId = '';
    paint();
  },
  'front-close': (el) => {
    const id = el.dataset.id || '';
    if (id) save({ id, close: true }, '没有结束：');
  },
  'front-item-done': (el) => strikeItem(el.dataset.id || ''),
  'front-items-more': (el) => {
    const key = el.dataset.key || '';
    if (!moreKeys.delete(key)) moreKeys.add(key);
    paint();
  },
});
