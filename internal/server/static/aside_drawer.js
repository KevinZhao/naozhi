// aside_drawer.js — the 追问 (scratch) drawer. The ↗ button on an AI bubble
// opens it: it creates a scratch session on the server, polls its events,
// sends messages, and can promote it into a sidebar-visible session. Drawer
// DOM lives in dashboard.html; dashboard's module body runs initAsideDrawer().
import { NZ_CONTRACT } from './contract.js';
import { getToken } from './platform.js';
import { selection, sessionList } from './state.js';
import { showToast } from './nz_util.js';
import { eventHtml } from './event_render.js';
import { collapseSidebarForDrawer, restoreSidebarAfterDrawer } from './mobile_nav.js';
import { splitDock } from './split_view.js';
import { regroupAvatars } from './file_refs.js';
import { confirmDialog, showAPIError, showNetworkError, timeDividerHtml } from './utilities.js';
import { fetchSessions } from './session_list.js';

// Self-scheduling poll cadence. A brand-new scratch session has no
// persisted events yet, so /api/sessions/events returns 404 ("session not
// found") until the first turn lands. The old fixed setInterval(…,1000)
// hammered that 404 at 1Hz forever, flooding the browser network log and
// wasting requests. We instead back off 1s→2s→4s→…→POLL_MAX_MS while the
// session is still empty/unreachable, and snap back to POLL_BASE_MS the
// moment a real poll succeeds. R20260605.
const POLL_BASE_MS = 1000;
const POLL_MAX_MS = 8000;

// ad: the drawer's elements (looked up by initAsideDrawer) and the open
// scratch session. state is written only through setScratch.
const ad = {
  drawer: null, elMsgs: null, elEmpty: null, elInput: null, elSend: null, elClose: null, elSave: null,
  elQuoteChip: null, elQuotePreview: null, elQuoteTrunc: null, elQuoteCtx: null, elLoading: null, elAgent: null,
  state: null,            // {scratchId, key, agentId, sourceKey, sourceMsgTime, quote, lastEventTime, pendingUserEchoes}
  pollTimer: null,
  sending: false,
  pollDelayMs: POLL_BASE_MS,
};

// setScratch mirrors the open scratch's router key into selection.scratchKey,
// where ask_card routes an AskUserQuestion answered inside the drawer.
function setScratch(s) {
  ad.state = s;
  selection.scratchKey = s ? s.key : '';
}

function authHeaders(extra) {
  const h = Object.assign({}, extra || {});
  try {
    const t = getToken();
    if (t) h['Authorization'] = 'Bearer ' + t;
  } catch (_) {}
  return h;
}

function clearMessages() {
  if (!ad.elMsgs) return;
  // Preserve the empty placeholder for re-use.
  ad.elMsgs.innerHTML = '';
  ad.elMsgs.appendChild(ad.elEmpty);
}

function showDrawer() {
  ad.drawer.classList.add('visible');
  // Dock as a right-hand split on desktop (no-op on phone overlay).
  splitDock.enter();
  // Opened last → stack on top of the preview pane if both are docked.
  splitDock.bringToFront('scratch');
}
function hideDrawer() {
  ad.drawer.classList.remove('visible');
  splitDock.exit();
  // Re-expand the sidebar if openScratch auto-collapsed it (no-op if the
  // preview drawer is still open or the user collapsed it themselves).
  restoreSidebarAfterDrawer();
}

function stopPolling() {
  if (ad.pollTimer) { clearTimeout(ad.pollTimer); ad.pollTimer = null; }
}

async function closeScratch(silent) {
  stopPolling();
  hideDrawer();
  if (!ad.state) return;
  const id = ad.state.scratchId;
  setScratch(null);
  ad.elSave.classList.remove('visible');
  clearMessages();
  ad.elInput.value = '';
  if (!id) return;
  try {
    await fetch(NZ_CONTRACT.API.scratch_id.replace('{id}', encodeURIComponent(id)), {
      method: 'DELETE', headers: authHeaders(),
    });
  } catch (_) { /* best effort */ }
}

function previewText(s) {
  if (!s) return '';
  const one = s.replace(/\s+/g, ' ').trim();
  return one.length > 40 ? one.slice(0, 40) + '…' : one;
}

// De-duplicate echoed user messages: sendInScratch renders the user bubble
// immediately for perceived responsiveness, then the server's event stream
// echoes the same text back as a `user` event. Without this filter the
// user's own message would appear twice. We compare the trimmed detail
// against the pendingUserEchoes set populated by sendInScratch; the set
// is bounded at 10 entries (most users don't queue more than 2-3 sends
// before polling catches up).
function matchesPendingEcho(ev) {
  if (!ad.state || !ad.state.pendingUserEchoes || ev.type !== 'user') return false;
  const body = String(ev.detail || ev.summary || '').trim();
  if (!body) return false;
  for (const pending of ad.state.pendingUserEchoes) {
    if (pending === body) {
      ad.state.pendingUserEchoes.delete(pending);
      return true;
    }
  }
  return false;
}

// isNearBottom mirrors the main transcript's wasBottom check (dashboard.js
// around line 1242 + 4604). 30px slack absorbs sub-pixel layout jitter.
function isNearBottom() {
  if (!ad.elMsgs) return true;
  return ad.elMsgs.scrollTop + ad.elMsgs.clientHeight >= ad.elMsgs.scrollHeight - 30;
}

// stickBottom mirrors the main transcript's stickEventsBottom: two rAFs
// to outlast KaTeX/mermaid layout bumps, plus image-load listeners so a
// late-loading thumbnail doesn't scroll the user away from the bottom.
// Used only when the caller wants a *forced* pin — incremental renders
// go through the isNearBottom path instead, matching the main window.
function stickBottom() {
  if (!ad.elMsgs) return;
  ad.elMsgs.scrollTop = ad.elMsgs.scrollHeight;
  requestAnimationFrame(() => {
    ad.elMsgs.scrollTop = ad.elMsgs.scrollHeight;
    requestAnimationFrame(() => { ad.elMsgs.scrollTop = ad.elMsgs.scrollHeight; });
  });
  ad.elMsgs.querySelectorAll('img').forEach(img => {
    if (img.complete) return;
    const restick = () => {
      if (ad.elMsgs.scrollTop + ad.elMsgs.clientHeight >= ad.elMsgs.scrollHeight - 30) {
        ad.elMsgs.scrollTop = ad.elMsgs.scrollHeight;
      }
    };
    img.addEventListener('load', restick, { once: true });
    img.addEventListener('error', restick, { once: true });
  });
}

// Time-divider helpers mirror renderEventsWithDividers in the main file.
// Keeping this scoped copy lets the aside share the visual grammar
// (mm/dd HH:MM dividers every >15min gap) without exporting internals.
const EVENT_DIVIDER_GAP_MS = 15 * 60 * 1000;
function asideLastTime() {
  // Walk backwards through already-rendered .event nodes to find the
  // newest data-time; used to decide whether a fresh divider is needed.
  for (let i = ad.elMsgs.children.length - 1; i >= 0; i--) {
    const c = ad.elMsgs.children[i];
    if (c.classList && c.classList.contains('event')) {
      return Number(c.getAttribute('data-time') || 0);
    }
  }
  return 0;
}

// @contract-begin scratchAdmitEvent
// scratchAdmitEvent is the same-ms replay gate for the drawer's HTTP poll.
// HandleEvents ?after= re-admits the watermark millisecond (#2456, so a
// same-ms sibling is never lost), which means every idle tick replays the
// entries AT st.lastEventTime. st.seenAtWM holds the uuids already
// processed (rendered OR echo-dropped) at that ms: the local optimistic
// user bubble carries no data-uuid and matchesPendingEcho consumes its
// entry on first sight, so a DOM lookup alone would re-render the echoed
// user event on the next tick. Returns false when e must be skipped;
// otherwise records it and advances the watermark. uuid-less (pre-uuid)
// events at the watermark are admitted — never swallow what we can't
// identify (losing history is worse than a duplicate bubble).
function scratchAdmitEvent(st, e) {
  const t = (e && typeof e.time === 'number') ? e.time : 0;
  if (t && t < st.lastEventTime) return false;
  if (!st.seenAtWM) st.seenAtWM = new Set();
  if (t && t === st.lastEventTime && e.uuid && st.seenAtWM.has(e.uuid)) return false;
  if (t > st.lastEventTime) {
    st.lastEventTime = t;
    st.seenAtWM.clear();
  }
  if (t && t === st.lastEventTime && e.uuid) st.seenAtWM.add(e.uuid);
  return true;
}
// @contract-end scratchAdmitEvent

function renderNewEvents(events) {
  if (!Array.isArray(events) || events.length === 0) return;
  // Remember whether the user was reading the latest message BEFORE we
  // mutate the DOM. Mirrors the main transcript's policy: only auto-pin
  // to the bottom if the user is already there, never drag them away
  // from content they're reading. The visible symptom on mobile — the
  // drawer snapping to the newest message every poll tick — was the
  // earlier "always scrollTop=scrollHeight" behaviour.
  const wasBottom = isNearBottom();
  // Clear placeholder on first real content.
  if (ad.elEmpty && ad.elEmpty.parentNode === ad.elMsgs) {
    ad.elMsgs.removeChild(ad.elEmpty);
  }
  let sawUser = false;
  let prevT = asideLastTime();
  for (const e of events) {
    // Same-ms replay / strictly-older guard; also owns the watermark.
    if (!scratchAdmitEvent(ad.state, e)) continue;
    // Drop server-echoed user messages that we already rendered locally.
    if (matchesPendingEcho(e)) continue;
    // Reuse the main event renderer so aside bubbles match the transcript
    // style (markdown, code blocks, etc.) without duplicating logic.
    const h = eventHtml(e);
    if (!h) continue;
    const t = e.time || 0;
    // Insert a divider when the gap between adjacent visible bubbles
    // exceeds EVENT_DIVIDER_GAP_MS — matches the main-window grammar.
    if (t && (prevT === 0 || t - prevT >= EVENT_DIVIDER_GAP_MS)
    ) {
      ad.elMsgs.insertAdjacentHTML('beforeend', timeDividerHtml(t));
    }
    const tmp = document.createElement('div');
    tmp.innerHTML = h;
    while (tmp.firstChild) ad.elMsgs.appendChild(tmp.firstChild);
    if (t) prevT = t;
    if (e.type === 'user') sawUser = true;
  }
  // Hide any "↗ 追问" buttons inside the aside itself — stacking is disabled.
  for (const btn of ad.elMsgs.querySelectorAll('.event-ask-btn')) btn.remove();
  // Apply WeChat-style avatar grouping in the aside too (it reuses eventHtml
  // and the same .nz-grouped CSS, but lives outside the #events-scroll
  // observer, so tag it explicitly).
  regroupAvatars(ad.elMsgs);
  // Scroll policy, aligned with main window:
  //  - the user just sent (sawUser on a local-render call): force-pin.
  //  - otherwise: only stick if they were already at the bottom.
  if (sawUser) stickBottom();
  else if (wasBottom) ad.elMsgs.scrollTop = ad.elMsgs.scrollHeight;
  // Save button appears once there's at least one AI reply.
  if (events.some(e => e.type === 'text' || e.type === 'result')) {
    ad.elSave.classList.add('visible');
  }
}

async function pollOnce() {
  if (!ad.state) return;
  try {
    let url = NZ_CONTRACT.API.sessions_events + '?key=' + encodeURIComponent(ad.state.key);
    if (ad.state.lastEventTime > 0) url += '&after=' + ad.state.lastEventTime;
    else url += '&limit=50';
    const r = await fetch(url, { headers: authHeaders() });
    if (!r.ok) {
      // 404 = the scratch session has no persisted events yet (brand-new,
      // first turn not landed). That is an expected empty state, not an
      // error: back off so we stop hammering it at 1Hz. Other non-OK
      // statuses get the same treatment — a transient server hiccup
      // shouldn't busy-loop either.
      ad.pollDelayMs = Math.min(ad.pollDelayMs * 2, POLL_MAX_MS);
      return;
    }
    // A successful poll means the session is reachable; snap cadence back
    // to the responsive base so newly-arriving events render promptly.
    ad.pollDelayMs = POLL_BASE_MS;
    const evs = await r.json();
    if (Array.isArray(evs) && evs.length > 0) {
      renderNewEvents(evs);
      // Hide the "thinking…" indicator once the first bubble arrives.
      if (evs.some(e => e.type === 'text' || e.type === 'result')) {
        ad.elLoading.classList.remove('visible');
      }
    }
  } catch (_) {
    // Network error: back off too, same rationale as a non-OK response.
    ad.pollDelayMs = Math.min(ad.pollDelayMs * 2, POLL_MAX_MS);
  }
}

function startPolling() {
  stopPolling();
  ad.pollDelayMs = POLL_BASE_MS;
  // Self-scheduling loop (not setInterval) so each tick's delay can grow
  // with the backoff set inside pollOnce. stopPolling()'s clearTimeout
  // cancels the next scheduled tick.
  const tick = async () => {
    await pollOnce();
    // stopPolling() nulls pollTimer; if that happened during the await we
    // must not reschedule (the drawer closed mid-flight).
    if (ad.pollTimer === null) return;
    ad.pollTimer = setTimeout(tick, ad.pollDelayMs);
  };
  ad.pollTimer = setTimeout(tick, ad.pollDelayMs);
}

async function openScratch(quote, agentId, sourceKey, sourceMsgTime) {
  // Confirm replacement if an aside is already open. Replacement is
  // non-destructive (the previous scratch is still reachable via history)
  // so we use 'primary' variant instead of 'danger'.
  if (ad.state) {
    const ok = await confirmDialog({
      title: '替换当前追问窗口？',
      message: '当前未保存为正式会话的追问内容将被关闭。',
      confirmText: '替换',
      variant: 'primary',
    });
    if (!ok) return;
    await closeScratch(true);
  }
  try {
    const r = await fetch(NZ_CONTRACT.API.scratch_open, {
      method: 'POST',
      headers: authHeaders({'Content-Type': 'application/json'}),
      body: JSON.stringify({
        source_key: sourceKey,
        source_message_id: String(sourceMsgTime || ''),
        // Time hint lets the server fetch 5 turns on each side of the
        // quoted message. Omitted (0) → server falls back to a tail-only
        // window which still seeds the aside with some context.
        source_message_time: Number(sourceMsgTime) || 0,
        quote,
      }),
    });
    if (!r.ok) {
      const txt = await r.text().catch(() => '');
      showAPIError('打开追问', r.status, txt);
      return;
    }
    const data = await r.json();
    setScratch({
      scratchId: data.scratch_id,
      key: data.key,
      agentId: data.agent_id || agentId || 'general',
      sourceKey,
      sourceMsgTime: sourceMsgTime || 0,
      quote,
      lastEventTime: 0,
      seenAtWM: new Set(), // uuids processed AT lastEventTime (scratchAdmitEvent)
      // Bounded Set of user-message bodies that sendInScratch rendered
      // locally. Consumed by matchesPendingEcho when the server event
      // stream replays the same text as a `user` event. Set over array
      // for O(1) lookup; bounded at ~10 entries by sendInScratch.
      pendingUserEchoes: new Set(),
    });
    ad.elAgent.textContent = ad.state.agentId && ad.state.agentId !== 'general' ? '· ' + ad.state.agentId : '';
    ad.elQuotePreview.textContent = previewText(quote);
    ad.elQuoteTrunc.style.display = data.quote_truncated ? 'inline' : 'none';
    // Context badge states (all three visible to the user):
    //   turns > 0                    → "(上下文 N 轮[+])"  — injected; "+" = byte-budget trimmed
    //   turns = 0 && truncated=true  → "(上下文已抑制)"    — quote filled the budget, nothing else fit
    //   turns = 0 && truncated=false → hidden              — no eligible surrounding turns
    // The third case is common for brand-new sessions so we hide the
    // badge rather than claim "(上下文 0 轮)".
    if (ad.elQuoteCtx) {
      const turns = Number(data.context_turns) || 0;
      const truncated = !!data.context_truncated;
      if (turns > 0) {
        ad.elQuoteCtx.textContent = '(上下文 ' + turns + ' 轮' + (truncated ? '+' : '') + ')';
        ad.elQuoteCtx.style.display = 'inline';
      } else if (truncated) {
        ad.elQuoteCtx.textContent = '(上下文已抑制)';
        ad.elQuoteCtx.style.display = 'inline';
      } else {
        ad.elQuoteCtx.textContent = '';
        ad.elQuoteCtx.style.display = 'none';
      }
    }
    ad.elQuoteChip.classList.remove('expanded');
    ad.elQuoteChip.dataset.full = quote;
    clearMessages();
    ad.elSave.classList.remove('visible');
    showDrawer();
    collapseSidebarForDrawer();
    setTimeout(() => ad.elInput.focus(), 60);
    startPolling();
  } catch (e) {
    console.error('open scratch', e);
    showNetworkError('打开追问', e);
  }
}

async function sendInScratch() {
  if (ad.sending || !ad.state) return;
  const text = ad.elInput.value.trim();
  if (!text) return;
  ad.sending = true;
  ad.elSend.disabled = true;
  ad.elLoading.classList.add('visible');
  // Cap the pending echo set at 10 to bound memory under rapid repeated
  // sends; old entries are dropped FIFO-ish (Set iteration order =
  // insertion order).
  if (ad.state.pendingUserEchoes.size >= 10) {
    const first = ad.state.pendingUserEchoes.values().next().value;
    if (first !== undefined) ad.state.pendingUserEchoes.delete(first);
  }
  ad.state.pendingUserEchoes.add(text);
  // Render the user message immediately via renderNewEvents so scroll
  // policy, divider insertion, and ↗-button stripping all match the
  // poll path. The time stamp is just above Date.now() so it sorts
  // after whatever was already rendered; the subsequent server replay
  // will be consumed by matchesPendingEcho.
  renderNewEvents([{type: 'user', detail: text, time: Date.now()}]);
  ad.elInput.value = '';
  try {
    const r = await fetch(NZ_CONTRACT.API.sessions_send, {
      method: 'POST',
      headers: authHeaders({'Content-Type': 'application/json'}),
      body: JSON.stringify({key: ad.state.key, text}),
    });
    if (!r.ok) {
      const txt = await r.text().catch(() => '');
      showAPIError('发送消息', r.status, txt);
      ad.elLoading.classList.remove('visible');
    } else {
      // The user just sent a turn, so the session is now live and events
      // are imminent. Restart polling at the responsive base so the reply
      // renders fast — without this, a tick already scheduled at the
      // backed-off delay (up to POLL_MAX_MS from the pre-first-turn 404
      // phase) could stall the first reply by several seconds.
      if (ad.pollTimer !== null) startPolling();
    }
  } catch (e) {
    console.error('scratch send', e);
    showNetworkError('发送消息', e);
    ad.elLoading.classList.remove('visible');
  } finally {
    ad.sending = false;
    ad.elSend.disabled = false;
    ad.elInput.focus();
  }
}

// promoteScratch turns the open scratch into a sidebar session (the
// 'scratch-promote' action on #ad-save) and resolves to the new session's key
// once the sidebar has it, for dashboard to select; otherwise to undefined.
export async function promoteScratch() {
  if (!ad.state) {
    showToast('追问会话已关闭，无法保存');
    return;
  }
  const id = ad.state.scratchId;
  try {
    const r = await fetch(NZ_CONTRACT.API.scratch_id_promote.replace('{id}', encodeURIComponent(id)), {
      method: 'POST', headers: authHeaders(),
    });
    if (!r.ok) {
      const txt = await r.text().catch(() => '');
      showAPIError('保存为正式会话', r.status, txt);
      return;
    }
    const data = await r.json();
    setScratch(null);   // scratch was detached server-side; skip the DELETE in closeScratch
    stopPolling();
    hideDrawer();
    clearMessages();
    ad.elSave.classList.remove('visible');
    ad.elInput.value = '';
    showToast('已保存为正式会话');
    // Refresh sidebar so the caller can select the new key.
    try {
      if (typeof sessionList.lastVersion !== 'undefined') sessionList.lastVersion = 0;
      await fetchSessions();
      return data.key;
    } catch (_) {}
  } catch (e) {
    console.error('promote scratch', e);
    showNetworkError('保存为正式会话', e);
  }
}

// closeScratchDrawer lets the view-router (setActivityView) tear the 追问
// drawer down when leaving the chat view — the drawer is position:fixed and
// would otherwise float over assets/cron/settings. closeScratch handles the
// no-op-when-closed case internally.
export function closeScratchDrawer() {
  if (ad.drawer) closeScratch(true);
}

// askAside opens the drawer on an AI bubble's text (the ↗ 'ask-aside' action).
export function askAside(btn) {
  if (!btn || !ad.drawer) return;
  const raw = btn.getAttribute('data-raw') || '';
  const msgTime = Number(btn.getAttribute('data-msg-time') || 0);
  if (!raw || raw.length < 1) return;
  if (!selection.key) {
    showToast('请先选择会话');
    return;
  }
  // Derive agentId from the current session key (4th segment) so the
  // server can inherit the matching agent registration.
  const parts = String(selection.key).split(':');
  const agentId = parts.length >= 4 ? parts[3] : 'general';
  openScratch(raw, agentId, selection.key, msgTime);
}

// initAsideDrawer looks up the drawer's elements and wires its buttons
// (#ad-save goes through the 'scratch-promote' action). Without
// #aside-drawer the drawer stays off and the exports above do nothing.
export function initAsideDrawer() {
  const drawer = document.getElementById('aside-drawer');
  if (!drawer) return;
  const $ = (id) => document.getElementById(id);
  ad.drawer = drawer;
  ad.elMsgs = $('ad-messages');
  ad.elEmpty = $('ad-empty');
  ad.elInput = $('ad-input');
  ad.elSend = $('ad-send');
  ad.elClose = $('ad-close');
  ad.elSave = $('ad-save');
  ad.elQuoteChip = $('ad-quote-chip');
  ad.elQuotePreview = $('ad-quote-preview');
  ad.elQuoteTrunc = $('ad-quote-trunc');
  ad.elQuoteCtx = $('ad-quote-ctx');
  ad.elLoading = $('ad-loading');
  ad.elAgent = $('ad-agent');

  // Wire drawer buttons.
  ad.elClose.addEventListener('click', () => { closeScratch(true); });
  ad.elSend.addEventListener('click', sendInScratch);
  ad.elInput.addEventListener('keydown', (e) => {
    // Enter sends; Shift+Enter inserts newline.
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      sendInScratch();
    }
  });
  ad.elQuoteChip.addEventListener('click', () => {
    const expanded = ad.elQuoteChip.classList.toggle('expanded');
    ad.elQuotePreview.textContent = expanded ? (ad.elQuoteChip.dataset.full || '') : previewText(ad.elQuoteChip.dataset.full || '');
    // Clicking the already-expanded chip scrolls the main transcript to the source.
    if (!expanded && ad.state && ad.state.sourceMsgTime) {
      const el = document.querySelector('.event[data-time="' + ad.state.sourceMsgTime + '"]');
      if (el && typeof el.scrollIntoView === 'function') {
        el.scrollIntoView({behavior: 'smooth', block: 'center'});
      }
    }
  });

  // ESC closes when drawer has focus.
  drawer.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') { e.preventDefault(); closeScratch(true); }
  });
}
