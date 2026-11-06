// ask_card.js — the AskUserQuestion card (提问): its renderer, the
// option-toggle and submit handlers, and the answered-lock that history and
// live user events apply. An answered card folds to one line — 已回答：<选择>,
// or 已按建议选：<选择> · 撤回 when custody answered it by the recommended
// option, whose message then shows no bubble of its own. event_render.js
// imports the renderer; dashboard.js registers the two data-action handlers.
import { NZ_CONTRACT } from './contract.js';
import { selection, transcript } from './state.js';
import { esc, escAttr } from './nz_util.js';
import { featureForCurrent } from './features.js';
import { formatTimeFull } from './utilities.js';
import { custodyMessage, isSuggestedMessage, optionText, suggestedChoice, undoHtml } from './custody.js';

// answers holds what answered each card (tool_use_id → {text, custody, id,
// header: a single question's header, which its line leaves out}),
// folded the custody answers shown on their card instead of a bubble (by
// the bubble id custody.js's 撤回 names).
const answers = new Map();
const folded = new Set();

const bubbleID = (e) => e.uuid || String(e.time || '');

// answerOf is what user event e answered a card with.
function answerOf(e) {
  const custody = e.source === 'custody' && isSuggestedMessage(e);
  const text = custody ? custodyMessage(e).body : (e.detail || e.summary || '');
  return { text, custody, id: bubbleID(e) };
}

// noteAnswer records e as the answer to card tuid; header is the card's
// single question's header ('' for several).
function noteAnswer(tuid, e, header) {
  if (!e) return;
  const a = Object.assign(answerOf(e), { header: header || '' });
  answers.set(tuid, a);
  if (a.custody) folded.add(a.id);
}

// isFoldedAnswer reports whether user event e is custody's answer a card
// shows on its own line: no bubble renders for it.
export function isFoldedAnswer(e) {
  return !!e && e.type === 'user' && e.source === 'custody' && folded.has(bubbleID(e));
}

// answerText is an answer as the card line shows it: the trailing period,
// the recommended markers and a single question's "<header>: " left out.
function answerText(a) {
  const t = suggestedChoice(a.text);
  return a.header && t.startsWith(a.header + ': ') ? t.slice(a.header.length + 2) : t;
}

// singleHeader is the header of a card's only question, '' otherwise.
const singleHeader = (items) => (Array.isArray(items) && items.length === 1 && items[0] && items[0].header) || '';

// answeredHtml is an answered card's line; sk is the card's session.
function answeredHtml(tuid, sk) {
  const a = answers.get(tuid);
  if (!a || !a.text) return '已回答';
  if (!a.custody) return '已回答：<span class="ask-choice">' + esc(answerText(a)) + '</span>';
  return '已按建议选：<span class="ask-choice">' + esc(answerText(a)) + '</span>' +
    '<span class="custody-sep" aria-hidden="true">·</span>' + undoHtml(sk, a.id);
}

// cardSession is the session a card belongs to, from its DOM ancestry: the
// scratch drawer's, the Planner page's opened session, else the selection.
function cardSession(card) {
  if (card && card.closest && card.closest('#aside-drawer')) return { key: selection.scratchKey || '', node: 'local' };
  if (card && card.closest && card.closest('#pl-sess')) return { key: card.closest('#pl-sess').dataset.key || '', node: 'local' };
  return { key: selection.key, node: selection.node };
}

// foldCard folds a card on screen to its answered line.
function foldCard(card, tuid) {
  card.classList.add('ask-done');
  if (answers.get(tuid)?.custody) card.classList.add('custody-suggested-msg');
  const content = card.querySelector('.event-content');
  if (!content) return;
  let status = content.querySelector('.ask-status');
  if (!status) {
    status = document.createElement('div');
    status.className = 'ask-status';
    content.appendChild(status);
  }
  status.innerHTML = answeredHtml(tuid, cardSession(card));
}

// hydrateAskAnsweredFromHistory walks a time-sorted event list and marks
// every ask_question whose tool_use_id is followed by at least one user
// event as already-answered. Called from onHistory before rendering.
export function hydrateAskAnsweredFromHistory(events) {
  if (!Array.isArray(events)) return;
  for (let i = 0; i < events.length; i++) {
    const e = events[i];
    if (!e || e.type !== 'ask_question') continue;
    const tuid = (e.ask_question && e.ask_question.tool_use_id) || e.tool_use_id || '';
    if (!tuid) continue;
    // Any later user event → this question was answered by some surface.
    for (let j = i + 1; j < events.length; j++) {
      if (events[j] && events[j].type === 'user') {
        transcript.askAnswered.add(tuid);
        noteAnswer(tuid, events[j], singleHeader(e.ask_question.items));
        break;
      }
    }
  }
}

// lockRenderedAskCards applies hydrateAskAnsweredFromHistory's rule to the
// live DOM: a `user` event landing incrementally (WS onEvent, onHistory
// backfill, poll appendEvents) means every AskUserQuestion card already on
// screen was answered on some surface (Feishu, the input box, another tab), so
// it must lock now — not only after a reload replays history. Without this the
// stale card stayed submittable and pushed an out-of-date answer into the
// next turn. Idempotent: cards onAskSubmit already locked are skipped
// via the existing .ask-status marker. ev is the user event answering them,
// when the caller has it: a card it answers folds to its line.
export function lockRenderedAskCards(scrollEl, ev) {
  if (!scrollEl) return;
  scrollEl.querySelectorAll('.event.ask_question[data-tool-use-id]').forEach(card => {
    const tuid = card.getAttribute('data-tool-use-id') || '';
    if (!tuid) return;
    const fresh = !card.classList.contains('ask-done');
    transcript.askAnswered.add(tuid);
    const heads = card.querySelectorAll('.ask-q-group');
    if (fresh && !answers.has(tuid)) noteAnswer(tuid, ev, heads.length === 1 ? (heads[0].querySelector('.ask-q-header')?.textContent || '') : '');
    card.querySelectorAll('.ask-opt, .ask-submit').forEach(b => { b.disabled = true; });
    // onAskSubmit's failure rollback left a card actionable; a user event
    // from another surface has since answered it, so its failure copy goes.
    if (fresh) foldCard(card, tuid);
  });
}

// renderAskQuestionCard renders card e of session sk (for 撤回 on its line).
export function renderAskQuestionCard(e, sk) {
  const aq = e.ask_question;
  if (!aq || !Array.isArray(aq.items) || aq.items.length === 0) {
    // Defensive: if payload missing, fall back to a plain status bubble.
    return '<div class="event ask_question"><span class="event-icon">?</span>' +
      '<div class="event-content">' + esc(e.summary || 'AskUserQuestion') + '</div></div>';
  }
  // Multi-Backend RFC §8.3 D12 — when the active session's backend doesn't
  // declare askuser, render a degraded card that lists the questions/options
  // as plain text and tells the operator to type the answer manually. Stops
  // the interactive submit-handler from sending an answer the backend can't
  // route (kiro 2.3.0 has no AskUserQuestion equivalent — V13 validation).
  if (!featureForCurrent('askuser')) {
    const lines = aq.items.map(it => {
      const header = it && it.header ? '<strong>' + esc(it.header) + '</strong>: ' : '';
      const q = it && it.question ? esc(it.question) : '';
      const opts = (it && Array.isArray(it.options))
        ? it.options.map(o => '· ' + esc((o && o.label) || '')).join('<br>')
        : '';
      return '<div class="ask-degraded-q">' + header + q +
        (opts ? '<div class="ask-degraded-opts">' + opts + '</div>' : '') + '</div>';
    }).join('');
    return '<div class="event ask_question ask-degraded"><span class="event-icon">?</span>' +
      '<div class="event-content">' +
        '<div class="ask-degraded-hint">' +
          '当前后端不支持 AskUserQuestion，请直接回复你的选择：' +
        '</div>' + lines +
      '</div></div>';
  }
  // A question with zero options would deadlock the submit button
  // (updateAskSubmitState requires every group to have a .selected option,
  // and a group with no .ask-opt can never satisfy that). Rather than
  // render a broken card, fall back to a simple label and log at debug so
  // the malformed payload surfaces in dev tools.
  const hasDegenerateItem = aq.items.some(it => !it || !Array.isArray(it.options) || it.options.length === 0);
  if (hasDegenerateItem) {
    return '<div class="event ask_question"><span class="event-icon">?</span>' +
      '<div class="event-content">' + esc(e.summary || 'AskUserQuestion (malformed: empty options)') + '</div></div>';
  }
  const tuid = aq.tool_use_id || '';
  const locked = transcript.askAnswered.has(tuid);
  const groups = aq.items.map((item, qi) => {
    const header = item.header ? '<div class="ask-q-header">' + esc(item.header) + '</div>' : '';
    const question = '<div class="ask-q-text">' + esc(item.question || '') + '</div>';
    const multi = !!item.multi_select;
    const opts = (item.options || []).map((opt, oi) => {
      // Buttons toggle a .selected class only; nothing is sent until the
      // card-level submit. data-* attrs carry the minimal info the compose
      // step needs so the handler doesn't have to walk the aq tree.
      return '<button class="ask-opt" type="button"' +
        ' data-tuid="' + escAttr(tuid) + '"' +
        ' data-qi="' + qi + '"' +
        ' data-oi="' + oi + '"' +
        ' data-multi="' + (multi ? '1' : '0') + '"' +
        ' data-header="' + escAttr(item.header || '') + '"' +
        ' data-label="' + escAttr(opt.label || '') + '"' +
        (locked ? ' disabled' : '') +
        ' data-action="ask-option-toggle">' +
        '<span class="ask-opt-label">' + esc(optionText(opt.label)) + '</span>' +
        (opt.description ? '<span class="ask-opt-desc">' + esc(opt.description) + '</span>' : '') +
        '</button>';
    }).join('');
    const hint = multi
      ? '<div class="ask-q-hint">可多选</div>'
      : '';
    return '<div class="ask-q-group" data-qi="' + qi + '" data-multi="' + (multi ? '1' : '0') + '">' +
      header + question + hint +
      '<div class="ask-opts">' + opts + '</div>' +
      '</div>';
  }).join('');
  // Single bottom submit: always starts disabled (no selection yet); either
  // unlocked dynamically by updateAskSubmitState when every group has ≥1
  // selected option, or permanently disabled if the card is locked
  // (replayed after a prior answer).
  const submitBtn =
    '<button class="ask-submit" type="button"' +
    ' data-tuid="' + escAttr(tuid) + '"' +
    ' disabled' +
    ' data-action="ask-submit">' + (aq.items.length > 1 ? '提交全部回答' : '提交') + '</button>';
  const status = locked
    ? '<div class="ask-status">' + answeredHtml(tuid, sk || { key: selection.key, node: selection.node }) + '</div>'
    : '';
  const done = locked ? ' ask-done' + (answers.get(tuid)?.custody ? ' custody-suggested-msg' : '') : '';
  const questions = aq.items.map((it) => (it && it.question) || '').filter(Boolean).join('\n');
  const timeAttr = e.time ? ' data-time="' + e.time + '" title="' + escAttr([formatTimeFull(e.time), questions].filter(Boolean).join('\n')) + '"' : '';
  return '<div class="event ask_question' + done + '"' + timeAttr +
    ' data-tool-use-id="' + escAttr(tuid) + '">' +
    '<span class="event-icon">?</span>' +
    '<div class="event-content ask-card">' +
      '<div class="ask-title">' + (aq.items.length > 1 ? '提问 · 全部作答后提交' : '提问') + '</div>' +
      groups +
      '<div class="ask-submit-row">' + submitBtn + '</div>' +
      status +
    '</div></div>';
}

// Compose the final reply text from every question's chosen labels.
// Format: "Header1: Label1. Header2: A, B. Label-only question: Label."
// The final "." is added per group so grouping is unambiguous to CC.
// AQ4 verified this format is sufficient context for CC to continue.
function composeAskAnswerFromGroups(groups) {
  const parts = [];
  groups.forEach(g => {
    if (!g.labels.length) return;
    const h = (g.header || '').trim();
    const l = g.labels.map(s => s.trim()).filter(Boolean).join(', ');
    if (!l) return;
    parts.push(h ? (h + ': ' + l) : l);
  });
  if (parts.length === 0) return '';
  return parts.join('. ') + '.';
}

// Toggle the clicked option. Single-select: clear siblings in the same
// question group, mark the clicked one. Multi-select: just toggle.
// Then re-evaluate the submit button's disabled state.
export function onAskOptionToggle(btn) {
  const tuid = btn.dataset.tuid || '';
  if (!tuid || transcript.askAnswered.has(tuid)) return;
  const group = btn.closest('.ask-q-group');
  if (!group) return;
  const multi = group.dataset.multi === '1';
  if (multi) {
    btn.classList.toggle('selected');
  } else {
    group.querySelectorAll('.ask-opt').forEach(b => b.classList.remove('selected'));
    btn.classList.add('selected');
  }
  updateAskSubmitState(btn.closest('.event.ask_question'));
}

// Enable submit only when every question has at least one selected option.
function updateAskSubmitState(card) {
  if (!card) return;
  const groups = card.querySelectorAll('.ask-q-group');
  let allAnswered = groups.length > 0;
  groups.forEach(g => {
    if (!g.querySelector('.ask-opt.selected')) allAnswered = false;
  });
  const submit = card.querySelector('.ask-submit');
  if (!submit) return;
  submit.disabled = !allAnswered;
}

export function onAskSubmit(btn) {
  const tuid = btn.dataset.tuid || '';
  if (!tuid || transcript.askAnswered.has(tuid)) return;
  const card = btn.closest('.event.ask_question');
  if (!card) return;
  // Gather selections per question group.
  const groups = [];
  card.querySelectorAll('.ask-q-group').forEach(g => {
    const header = (g.querySelector('.ask-q-header') || {}).textContent || '';
    const labels = [];
    g.querySelectorAll('.ask-opt.selected').forEach(b => {
      const l = b.dataset.label || '';
      if (l) labels.push(l);
    });
    groups.push({ header: header, labels: labels });
  });
  const answer = composeAskAnswerFromGroups(groups);
  if (!answer) return;
  // Lock the card so re-clicks or slow network can't duplicate the send.
  transcript.askAnswered.add(tuid);
  answers.set(tuid, { text: answer, custody: false, id: '', header: groups.length === 1 ? groups[0].header : '' });
  card.querySelectorAll('button').forEach(b => { b.disabled = true; });
  foldCard(card, tuid);
  // Route through the regular session send endpoint so queue / passthrough /
  // broadcast semantics all apply; we do NOT call sendMessage() because that
  // path reads from the input box and manages optimistic rendering — the card
  // already shows "已回答", so duplicating would clash.
  sendAskAnswerViaAPI(answer, card).catch(err => {
    transcript.askAnswered.delete(tuid);
    answers.delete(tuid);
    card.classList.remove('ask-done');
    card.querySelectorAll('button').forEach(b => { b.disabled = false; });
    updateAskSubmitState(card);
    const status = card.querySelector('.ask-status');
    if (status) status.textContent = '发送失败：' + (err && err.message || err);
  });
}

// sendAskAnswerViaAPI routes the composed answer text to the session that
// rendered the AskUserQuestion card. The renderer (eventHtml →
// renderAskQuestionCard) is shared between the main transcript and the
// scratch (aside) drawer and the Planner page's opened session, so we MUST
// pick the route from the card's DOM
// ancestry rather than the global selectedKey — otherwise an answer chosen
// inside the drawer would land in the parent session and silently bypass
// the scratch CLI process.
async function sendAskAnswerViaAPI(text, card) {
  let key = selection.key;
  let node = selection.node;
  if (card && card.closest && card.closest('#aside-drawer')) {
    const scratchKey = selection.scratchKey;
    if (!scratchKey) throw new Error('no active scratch session');
    key = scratchKey;
    // Scratch sessions are always local — never forward to a remote node.
    node = 'local';
  } else if (card && card.closest && card.closest('#pl-sess')) {
    // The Planner page's opened session card (planner_chat.js), local only.
    key = card.closest('#pl-sess').dataset.key || '';
    node = 'local';
  }
  if (!key) throw new Error('no active session');
  const headers = { 'Content-Type': 'application/json' };
  const payload = { key: key, text: text };
  if (node && node !== 'local') payload.node = node;
  const r = await fetch(NZ_CONTRACT.API.sessions_send, { method: 'POST', headers, body: JSON.stringify(payload) });
  if (!r.ok) {
    const raw = await r.text().catch(() => '');
    throw new Error('send failed: ' + r.status + ' ' + raw.slice(0, 200));
  }
}
