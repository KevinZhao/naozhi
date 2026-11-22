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
// The ledger is shown and edited on the machine that keeps it: the route is
// not among a reverse node's, so the group stays hidden while the page shows
// another node's Planner; the gateway takes an edit only straight from
// loopback and says so in `writable`, without which 编辑 and 结束 are not
// offered. With no line the group takes no room.
//
// The lines are read when the page opens and after each Planner turn
// (refreshFronts, with the tasks). An edit or 结束 shows at once, ahead of
// the gateway (held); a failure puts the line back as it was.
import { NZ_CONTRACT } from './contract.js';
import { esc, escAttr, fetchJSON, registerActions, showToast } from './nz_util.js';
import { formatAbsTime } from './utilities.js';
import { isLocalNode, plannerHost } from './planner_host.js';

// IDLE_MS is how long a line goes unchanged before its card says so, the
// morning brief's three days.
const IDLE_MS = 3 * 24 * 3600 * 1000;
// MAX_DECISIONS is how many of a line's decisions an opened card lists.
const MAX_DECISIONS = 3;

let mount = null;
let last = null; // { fronts, writable }
let openId = '';
let editId = '';
let gen = 0;
// held is the user's edits this page shows ahead of the ledger, by line:
// {edit} (the fields saved; close drops the line). While the POST is out
// (upTo Infinity), and for a read started before it answered (gen ≤ upTo),
// a list still showing the line as it was must not undo the edit.
const held = new Map();

const str = (v) => (typeof v === 'string' ? v : '');
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

function cardHtml(f, writable, now) {
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
        (open ? decisionsHtml(f) + '<div class="tk-acts">' + acts + '</div>' : '')) +
    '</div>';
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

// frontsHtml renders a GET /api/planner/fronts answer, "" with no line.
export function frontsHtml(data, now = Date.now()) {
  const fronts = data && Array.isArray(data.fronts) ? data.fronts : [];
  if (!fronts.length) return '';
  return '<h2 class="pl-group-h tk-head">工作线 <span class="tk-n">' + fronts.length + '</span></h2>' +
    '<div class="tk-list" role="list">' + fronts.map((f) => cardHtml(f, !!data.writable, now)).join('') + '</div>';
}

function paint() {
  if (!mount || !mount.isConnected) return;
  const html = last ? frontsHtml(last) : '';
  const group = mount.closest('.pl-group');
  if (group) group.hidden = !html;
  if (mount._frHtml === html) return;
  // An edit in progress keeps what was typed through a repaint.
  const typed = {};
  mount.querySelectorAll('.fr-edit textarea').forEach((t) => { typed[t.dataset.field] = t.value; });
  mount._frHtml = html;
  mount.innerHTML = html;
  mount.querySelectorAll('.fr-edit textarea').forEach((t) => { if (t.dataset.field in typed) t.value = typed[t.dataset.field]; });
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
    last = Object.assign({}, data, { fronts: list });
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
async function save(body, failed) {
  if (!last || !Array.isArray(last.fronts)) return;
  const prior = { fronts: last.fronts, openId, editId };
  const mark = { edit: body, upTo: Infinity };
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
    if (!ok) {
      if (held.get(body.id) === mark) held.delete(body.id);
      const was = prior.fronts.find((f) => f.id === body.id);
      if (was && last) {
        // Put back where it stood, among what the page lists now.
        const now = last.fronts.filter((f) => f.id !== body.id);
        const at = prior.fronts.indexOf(was);
        last = Object.assign({}, last, { fronts: now.slice(0, at).concat([was], now.slice(at)) });
      }
      openId = prior.openId;
      editId = prior.editId;
      paint();
      if (!body.close && editId === body.id) {
        mount?.querySelectorAll('.fr-edit[data-id="' + CSS.escape(body.id) + '"] textarea').forEach((t) => {
          if (typeof body[t.dataset.field] === 'string') t.value = body[t.dataset.field];
        });
      }
      showToast(failed + e.message, 'error');
    }
  }
  if (ok && held.get(body.id) === mark) mark.upTo = gen;
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
});
