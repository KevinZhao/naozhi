// ask_card.js — the AskUserQuestion card: its renderer, the option-toggle and
// submit handlers, and the answered-lock that history and live user events
// apply. Moved verbatim from dashboard.js (S19-3, #3025): event_render.js
// imports the renderer, dashboard.js registers the two data-action handlers.
import { NZ_CONTRACT } from './contract.js';
import { getToken } from './platform.js';
import { hooks, selection, transcript } from './state.js';
import { esc, escAttr } from './nz_util.js';
import { featureForCurrent } from './features.js';
import { formatTimeFull } from './utilities.js';

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
// next turn (#2430). Idempotent: cards onAskSubmit already locked are skipped
// via the existing .ask-status marker.
export function lockRenderedAskCards(scrollEl) {
  if (!scrollEl) return;
  scrollEl.querySelectorAll('.event.ask_question[data-tool-use-id]').forEach(card => {
    const tuid = card.getAttribute('data-tool-use-id') || '';
    if (!tuid) return;
    transcript.askAnswered.add(tuid);
    card.querySelectorAll('button').forEach(b => { b.disabled = true; });
    const content = card.querySelector('.event-content');
    if (!content) return;
    const status = content.querySelector('.ask-status');
    if (!status) {
      const div = document.createElement('div');
      div.className = 'ask-status';
      div.textContent = '已回答';
      content.appendChild(div);
    } else if (status.textContent.indexOf('发送失败') === 0) {
      // onAskSubmit's failure rollback left the card actionable; a user event
      // from another surface has since answered it, so the failure copy is
      // stale — replace it rather than leave a locked card saying "failed".
      status.textContent = '已回答';
    }
  });
}

export function renderAskQuestionCard(e) {
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
        '<span class="ask-opt-label">' + esc(opt.label || '') + '</span>' +
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
    ' data-action="ask-submit">提交全部回答</button>';
  const status = locked
    ? '<div class="ask-status">已回答</div>'
    : '';
  const timeAttr = e.time ? ' data-time="' + e.time + '" title="' + escAttr(formatTimeFull(e.time)) + '"' : '';
  return '<div class="event ask_question"' + timeAttr +
    ' data-tool-use-id="' + escAttr(tuid) + '">' +
    '<span class="event-icon">?</span>' +
    '<div class="event-content ask-card">' +
      '<div class="ask-title">AskUserQuestion · 全部作答后提交</div>' +
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
  card.querySelectorAll('button').forEach(b => { b.disabled = true; });
  const content = card.querySelector('.event-content');
  if (content && !content.querySelector('.ask-status')) {
    const div = document.createElement('div');
    div.className = 'ask-status';
    div.textContent = '已回答：' + answer;
    content.appendChild(div);
  }
  // Route through the regular session send endpoint so queue / passthrough /
  // broadcast semantics all apply; we do NOT call sendMessage() because that
  // path reads from the input box and manages optimistic rendering — the card
  // already shows "已回答", so duplicating would clash.
  sendAskAnswerViaAPI(answer, card).catch(err => {
    transcript.askAnswered.delete(tuid);
    card.querySelectorAll('button').forEach(b => { b.disabled = false; });
    updateAskSubmitState(card);
    const status = card.querySelector('.ask-status');
    if (status) status.textContent = '发送失败：' + (err && err.message || err);
  });
}

// sendAskAnswerViaAPI routes the composed answer text to the session that
// rendered the AskUserQuestion card. The renderer (eventHtml →
// renderAskQuestionCard) is shared between the main transcript and the
// scratch (aside) drawer, so we MUST pick the route from the card's DOM
// ancestry rather than the global selectedKey — otherwise an answer chosen
// inside the drawer would land in the parent session and silently bypass
// the scratch CLI process.
async function sendAskAnswerViaAPI(text, card) {
  let key = selection.key;
  let node = selection.node;
  if (card && card.closest && card.closest('#aside-drawer')) {
    const scratchKey = hooks.getActiveScratchKey
      ? hooks.getActiveScratchKey()
      : '';
    if (!scratchKey) throw new Error('no active scratch session');
    key = scratchKey;
    // Scratch sessions are always local — never forward to a remote node.
    node = 'local';
  }
  if (!key) throw new Error('no active session');
  const headers = { 'Content-Type': 'application/json' };
  const token = getToken();
  if (token) headers['Authorization'] = 'Bearer ' + token;
  const payload = { key: key, text: text };
  if (node && node !== 'local') payload.node = node;
  const r = await fetch(NZ_CONTRACT.API.sessions_send, { method: 'POST', headers, body: JSON.stringify(payload) });
  if (!r.ok) {
    const raw = await r.text().catch(() => '');
    throw new Error('send failed: ' + r.status + ' ' + raw.slice(0, 200));
  }
}
