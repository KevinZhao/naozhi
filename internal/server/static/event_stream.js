// event_stream.js — the main transcript's event list: the HTTP fetch and poll
// tail, "load earlier" paging, the full render, the live append and its DOM
// cap, and the history / event / send-ack WS frames that feed them.
import { NZ_CONTRACT } from './contract.js';
import { getToken } from './platform.js';
import { INITIAL_HISTORY_LIMIT, sessionStream } from './session_stream.js';
import { wsm } from './ws_manager.js';
import { perSession, selection, sessionList, transcript } from './state.js';
import { fetchJSON, nzViews, showToast } from './nz_util.js';
import { eventAlreadyRendered, eventHtml, leadingTimeDivider, removeOptimisticMsg, renderEventsWithDividers } from './event_render.js';
import { hydrateAskAnsweredFromHistory, lockRenderedAskCards } from './ask_card.js';
import { runPendingAsync } from './render_md.js';
import { processEventsForDisplay } from './file_refs.js';
import { navRebuild, navSync } from './msg_nav.js';
import { applyEventToTurnState, paintTurnElapsed, refreshBanner, resetTurnState, resetTurnStateForUserEcho, restoreScrollPos, scrollSlackPx, turnState, updateSendButton } from './running_banner.js';
import { rollbackOptimisticRunning } from './send_message.js';
import { EARLIER_PAGE_LIMIT, EVENT_DIVIDER_GAP_MS, MAX_LIVE_DOM_EVENTS, lastDividerTime, showAPIError, stickEventsBottom, timeDividerHtml } from './utilities.js';
import { isInternalEvent, sid } from './session_ident.js';
import { isCronSessionFrozen } from './cron_state.js';
import { registerShell } from './shell.js';

export async function fetchEvents(full) {
  if (!selection.key) return;
  if (!full && transcript.fetchInFlight) return;
  // Capture session identity at dispatch time: `pickSession`/`dismiss` can
  // flip selectedKey while `await` suspends us, and appending after that
  // would graft the prior session's tail into the newly-opened scroller.
  const dispatchKey = selection.key;
  const dispatchNode = selection.node;
  if (full) transcript.fetchGen++;
  const gen = transcript.fetchGen;
  const stale = () => selection.key !== dispatchKey || selection.node !== dispatchNode || gen !== transcript.fetchGen;
  transcript.fetchInFlight = true;
  try {
    let url = NZ_CONTRACT.API.sessions_events + '?key=' + encodeURIComponent(dispatchKey);
    if (dispatchNode && dispatchNode !== 'local') url += '&node=' + encodeURIComponent(dispatchNode);
    if (!full && transcript.lastEventTime > 0) {
      url += '&after=' + transcript.lastEventTime;
    } else if (full) {
      // Initial fetch mirrors the WS subscribe: last INITIAL_HISTORY_LIMIT
      // events only. Older pages are loaded on demand by loadEarlierEvents().
      url += '&limit=' + INITIAL_HISTORY_LIMIT;
    }

    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    // RNEW-UX-003: 5s timeout — events poll fallback ticks every 1s, so
    // a hung response must release well before the next tick or the UI
    // falls behind the live stream.
    let events;
    // The initial (`full`) fetch mounts "load earlier" off the server's
    // truncation decision rather than a len>=INITIAL_HISTORY_LIMIT guess.
    let hasMore = null;
    try {
      const onResp = full ? (resp) => { hasMore = hasMoreHeader(resp); } : null;
      events = await fetchJSON(url, { headers, timeoutMs: 5000, onResponse: onResp });
    } catch (err) {
      if (err.status) return; // HTTP non-2xx — mirror legacy !r.ok early-return
      throw err;              // timeout / network — surface via outer catch
    }
    if (!events || events.length === 0) return;
    // Drop stale responses whose selection has since moved, or that a newer
    // `full` fetch has superseded. Clearing `lastEventTime` is the caller's
    // job at switch time, so we don't touch it here.
    if (stale()) return;

    if (full) renderEvents(events, hasMore);
    else appendEvents(events);

    const last = events[events.length - 1];
    if (last && last.time > transcript.lastEventTime) transcript.lastEventTime = last.time;
  } catch (e) {
    console.error('fetch events:', e);
  } finally {
    // Only the newest generation owns the flag (mirrors loadEarlierEvents /
    // _earlierGen): a superseded tail must not free it under the full fetch.
    if (gen === transcript.fetchGen) transcript.fetchInFlight = false;
  }
}

const AUTO_PAGEBACK_MAX = 3;

// maybeAutoPageBack fires one bounded loadEarlierEvents when the events pane
// rendered blank (every event was internal-filtered). Stops once a real bubble
// appears, the cap is reached, or pagination reports it's exhausted. Safe to
// call when no placeholder is showing — it no-ops unless the scroller has zero
// `.event` children.
export function maybeAutoPageBack() {
  const el = document.getElementById('events-scroll');
  if (!el) return;
  // A visible bubble already rendered — nothing to recover.
  if (el.querySelector('.event')) { transcript.autoPageBackCount = 0; return; }
  if (transcript.autoPageBackCount >= AUTO_PAGEBACK_MAX) return;
  if (transcript.earlierLoading) return;
  if (!transcript.oldestFetchedEventTime) return; // no cursor → cannot page back
  transcript.autoPageBackCount++;
  // loadEarlierEvents prepends older events and, when they include a visible
  // bubble, the placeholder is removed by prependEvents. If the new page is
  // still all-internal, chain another attempt (still bounded by the counter).
  Promise.resolve(loadEarlierEvents(1)).then(() => {
    const ev = document.getElementById('events-scroll');
    if (ev && !ev.querySelector('.event')) maybeAutoPageBack();
    else transcript.autoPageBackCount = 0;
  });
}

// EARLIER_SKIP_MAX_PAGES bounds the pages one click walks while each is all
// internal (an agent team leaves thousands of task_progress in a row); the
// cursor keeps advancing, so an exhausted budget still helps the next click.
const EARLIER_SKIP_MAX_PAGES = 10;
const EARLIER_LIMIT_MAX = 500; // the server's maxEventsPageLimit

// @contract-begin dedupEarlierPage
// eventIdentityKey identifies an entry across overlapping pages: the backend's
// uuid when present, else (time,type,detail) for pre-uuid synthetic entries.
export function eventIdentityKey(e) {
  if (e && e.uuid) return 'u:' + e.uuid;
  return 'k:' + ((e && e.time) || 0) + '|' + ((e && e.type) || '') + '|' + ((e && e.detail) || '');
}

// dedupEarlierPage consumes one page of entries at or before cursorMS (the
// request sent before=cursorMS+1, as `before` is strict): it drops what the
// cursor already holds, i.e. anything newer and the seenKeys at cursorMS.
// Returns the fresh entries, their oldest ms and the keys held there; cursorMS
// 0 seeds the cursor from a full page.
export function dedupEarlierPage(events, cursorMS, seenKeys) {
  const seen = new Set(seenKeys || []);
  const fresh = [];
  let oldest = cursorMS;
  for (const e of events) {
    const t = (e && e.time) || 0;
    if (!e || (cursorMS && t > cursorMS) || (t && t === cursorMS && seen.has(eventIdentityKey(e)))) continue;
    fresh.push(e);
    if (t && (!oldest || t < oldest)) oldest = t;
  }
  const keys = oldest === cursorMS ? [...seen] : [];
  for (const e of fresh) if (oldest && e.time === oldest) keys.push(eventIdentityKey(e));
  return { events: fresh, oldestMS: oldest, seenKeys: keys };
}
// @contract-end dedupEarlierPage

// hasMoreHeader reads X-Events-Has-More; null when absent (an older server
// or relay), which leaves the caller its length heuristic. memoryOnlyHeader:
// that has-more covers only the memory of a remote node too old to page.
export function hasMoreHeader(resp) {
  const v = resp && resp.headers ? resp.headers.get('X-Events-Has-More') : null;
  return v == null ? null : v === '1' || v === 'true';
}
export function memoryOnlyHeader(resp) { return resp?.headers?.get('X-Events-Paging') === 'memory-only'; }

// The load-earlier cursor: a ms plus the keys of the entries held there.
function setEarlierCursor(ms, keys) {
  transcript.oldestFetchedEventTime = ms;
  transcript.oldestFetchedKeys = keys;
}

function seedEarlierCursor(events) {
  const c = dedupEarlierPage(events, 0, []);
  if (c.oldestMS) setEarlierCursor(c.oldestMS, c.seenKeys);
}

// domHeadCursor derives the cursor from the oldest rendered bubbles. A bubble
// without a uuid can't be keyed, so its millisecond is treated as held whole.
function domHeadCursor(el) {
  const t = Number(el.querySelector(':scope > .event')?.getAttribute('data-time') || 0);
  const keys = [];
  for (const n of t ? el.querySelectorAll(':scope > .event[data-time="' + t + '"]') : []) {
    const u = n.getAttribute('data-uuid');
    if (!u) return { ms: t - 1, keys: [] };
    keys.push('u:' + u);
  }
  return { ms: t, keys };
}

// loadEarlierEvents pages backward from the cursor, up to maxPages requests
// (default EARLIER_SKIP_MAX_PAGES), until a page shows a visible bubble.
async function loadEarlierEvents(maxPages) {
  if (transcript.earlierLoading || !selection.key) return;
  const el = document.getElementById('events-scroll');
  if (!el) return;

  // Cursor = oldest FETCHED event, not the oldest rendered bubble: an all-
  // internal page never reaches the DOM, so a DOM cursor re-fetched that same
  // page on every click. The DOM head only covers a missing cursor.
  if (!transcript.oldestFetchedEventTime) { const h = domHeadCursor(el); setEarlierCursor(h.ms, h.keys); }
  if (!transcript.oldestFetchedEventTime) return;
  const budget = maxPages > 0 ? maxPages : EARLIER_SKIP_MAX_PAGES;

  // Capture session identity at dispatch time (mirrors fetchEvents): the
  // operator can switch sessions while we await, and prepending the old
  // session's page into the new session's scroller grafts two histories.
  const key = selection.key;
  const node = selection.node;
  const gen = transcript.earlierGen;
  const stale = () => selection.key !== key || selection.node !== node || gen !== transcript.earlierGen;
  transcript.earlierLoading = true;
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    let steppedPast = false;
    for (let n = 1; ; n++) {
      // Per page: prependEvents re-mounts the button in its 'ready' state.
      updateEarlierButton('loading');
      const c = { ms: transcript.oldestFetchedEventTime, keys: transcript.oldestFetchedKeys };
      // Re-admit the cursor ms so a sibling group split by a page edge keeps
      // its older members; the extra keys.length makes room for the replays.
      const limit = Math.min(EARLIER_PAGE_LIMIT + c.keys.length, EARLIER_LIMIT_MAX);
      let url = NZ_CONTRACT.API.sessions_events + '?key=' + encodeURIComponent(key) +
                '&before=' + (c.ms + 1) + '&limit=' + limit;
      if (node && node !== 'local') url += '&node=' + encodeURIComponent(node);
      const r = await fetch(url, { headers });
      if (stale()) return;
      if (!r.ok) { updateEarlierButton('error'); return; }
      const body = await r.json();
      if (stale()) return;
      const raw = Array.isArray(body) ? body : [];
      const page = dedupEarlierPage(raw, c.ms, c.keys);
      // Advance before DOM work, so the floor holds even for an all-internal page.
      setEarlierCursor(page.oldestMS, page.seenKeys);
      const shown = prependEvents(page.events);
      // The header is authoritative; only without it is a short page the end.
      const hm = hasMoreHeader(r);
      if (hm === false || (hm === null && raw.length < limit)) { updateEarlierButton(memoryOnlyHeader(r) ? 'node-old' : 'done'); return; }
      // More exists yet a short page brought nothing new: the read degraded.
      if (!page.events.length && raw.length < limit) { updateEarlierButton('error'); return; }
      if (!page.events.length && page.oldestMS >= c.ms) {
        // Nothing new in a full page (a same-ms group wider than a page, or a
        // server ignoring `before`): step past the ms strictly, once a click.
        if (steppedPast) { updateEarlierButton('ready'); return; }
        console.warn('load earlier: skipping the rest of ms', c.ms);
        steppedPast = true;
        setEarlierCursor(c.ms - 1, []);
      }
      if (shown || n >= budget) { updateEarlierButton('ready'); return; }
    }
  } catch (e) {
    console.error('load earlier events:', e);
    if (!stale()) updateEarlierButton('error');
  } finally {
    // Keyed on the generation only, not stale(): selectSession resets the flag
    // for a newer session, but paths that null selectedKey without it
    // (pending create, dismiss, discovered preview) never do.
    if (gen === transcript.earlierGen) transcript.earlierLoading = false;
  }
}

// prependEvents injects older events at the top of the scroller while keeping
// the user's visual position stable (the bubble they're currently reading
// should not shift). Only runs KaTeX/Mermaid on the freshly-inserted fragment
// so 500-bubble sessions don't re-scan the entire DOM on each page.
// Returns whether the page rendered at least one visible bubble.
function prependEvents(events) {
  const el = document.getElementById('events-scroll');
  if (!el || !events || events.length === 0) return false;

  // Preserve visual stability: capture distance-from-bottom before ANY mutation
  // (the reader clicked the button, so they sit at the top and removing it
  // would jump the page by its height), then restore after. Bottom-anchored
  // math holds however the content above changes height.
  const prevScrollFromBottom = el.scrollHeight - el.scrollTop - el.clientHeight;

  // The button is re-mounted on top after the insert.
  const btn = document.getElementById('earlier-events-btn');
  if (btn) btn.remove();

  const display = processEventsForDisplay(events);
  const html = renderEventsWithDividers(display, 0);
  // Drop the placeholder only when this page brings a visible bubble: for a
  // still all-internal page it is the only explanation of the empty pane.
  if (html) {
    const placeholder = el.querySelector('.empty-state');
    if (placeholder) placeholder.remove();
  }

  // The DOM's leading divider was emitted for prevTime=0; with older bubbles
  // above it, it stays only across a real divider gap (no stacked dividers).
  const oldLeadDivider = leadingTimeDivider(el);
  const frag = document.createElement('div');
  frag.innerHTML = html;
  const newestPrependedTime = lastDividerTime(frag);
  // Move children one by one before the pre-insert first child (a moving
  // el.firstChild anchor would reverse the page).
  const anchor = el.firstChild;
  while (frag.firstChild) {
    el.insertBefore(frag.firstChild, anchor);
  }
  if (oldLeadDivider && newestPrependedTime) {
    const leadT = Number(oldLeadDivider.getAttribute('data-time') || 0);
    if (leadT && leadT - newestPrependedTime < EVENT_DIVIDER_GAP_MS) oldLeadDivider.remove();
  }

  // Re-insert the button at the top.
  ensureEarlierButton();

  // Restore scroll position.
  el.scrollTop = el.scrollHeight - el.clientHeight - prevScrollFromBottom;

  // runPendingAsync only visits the bubbles just rendered; no DOM scan.
  runPendingAsync();
  navRebuild();
  return !!html;
}

// paneButton returns the scroller's button `id`, created on first use and
// kept at the top of the scroller; null without a scroller.
function paneButton(id, onclick) {
  const el = document.getElementById('events-scroll');
  if (!el) return null;
  let btn = document.getElementById(id);
  if (!btn) {
    btn = Object.assign(document.createElement('button'), { id, type: 'button', className: 'earlier-events-btn', onclick });
    btn.style.cssText = 'display:block;margin:8px auto;padding:6px 14px;background:var(--nz-bg-2);border:1px solid var(--nz-border);color:var(--nz-text);border-radius:6px;cursor:pointer;font-size:12px';
  }
  if (el.firstChild !== btn) el.insertBefore(btn, el.firstChild);
  return btn;
}

// ensureEarlierButton injects/refreshes the "load earlier" affordance at the
// top of the scroller. Button state is stored in data-state on the element.
export function ensureEarlierButton() {
  if (paneButton('earlier-events-btn', () => loadEarlierEvents())) updateEarlierButton('ready');
}

// showHistoryRetry answers error 'history unavailable' (a remote node failed
// to serve the history) with a retry where the opening page should be; one
// still owed that page drops the cursor live events may have advanced.
export function showHistoryRetry() {
  const el = document.getElementById('events-scroll');
  if (el && !el.querySelector('.event')) el.replaceChildren();
  const btn = paneButton('history-retry-btn', () => {
    if (sessionStream._initialSubscribe) sessionStream.lastEventTimeWs = 0;
    btn.remove(); sessionStream.subscribe(selection.key, selection.node);
  });
  if (btn) btn.textContent = '历史记录加载失败 — 点击重试';
}

// [label, disabled] per button state; any other state is 'ready'.
const EARLIER_BUTTON_STATES = {
  loading: ['加载中…', true],
  done: ['没有更早的事件', true],
  'node-old': ['已到该节点内存中最早的事件 — 升级该节点可加载更早历史', true],
  error: ['加载失败 — 点击重试', false],
};

function updateEarlierButton(state) {
  const btn = document.getElementById('earlier-events-btn');
  if (!btn) return;
  btn.dataset.state = state;
  [btn.textContent, btn.disabled] = EARLIER_BUTTON_STATES[state] || ['加载更早的事件', false];
}

// renderEvents replaces the whole events pane on the initial / full-fetch path.
// hasMore (when not null) is the server's authoritative "older history exists"
// signal from the X-Events-Has-More header; null means the header was absent
// (legacy server or node) and we fall back to the length heuristic.
export function renderEvents(events, hasMore) {
  const el = document.getElementById('events-scroll');
  if (!el) return;
  // The innerHTML replace would wipe a text selection inside the pane (a
  // mid-copy); skip this refresh, the next tick replays the same data.
  try {
    const sel = window.getSelection && window.getSelection();
    if (sel && !sel.isCollapsed && sel.anchorNode && el.contains(sel.anchorNode)) {
      return;
    }
  } catch (_) { /* getSelection unavailable — proceed with refresh */ }
  // Poll-fallback twin of onHistory's pre-render hydrate: rebuild the
  // answered-set so replayed AskUserQuestion cards render locked (#2430).
  hydrateAskAnsweredFromHistory(events);
  const display = processEventsForDisplay(events);
  const html = renderEventsWithDividers(display, 0);
  // Decide "load earlier" BEFORE the all-internal placeholder so its copy never
  // promises a missing button. The server's hasMore knows the slice was cut by
  // visible-bubble count; the length heuristic is only for an absent header.
  const showEarlier = (hasMore === true) ||
    (hasMore == null && events.length >= INITIAL_HISTORY_LIMIT);
  if (html) {
    el.innerHTML = html;
  } else if (events.length === 0) {
    el.innerHTML = '<div class="empty-state">暂无事件</div>';
  } else {
    // Every event was internal (an agent team's tail). Invite "click below"
    // only when the button will mount; otherwise there is nothing older.
    el.innerHTML = showEarlier
      ? '<div class="empty-state">该会话最近仅有 agent 活动，点击下方加载更早的消息</div>'
      : '<div class="empty-state">该会话仅有 agent 活动，暂无对话消息</div>';
  }
  if (events.length > 0) {
    const last = events[events.length - 1];
    if (last.time) transcript.lastRenderedEventTime = last.time;
    // Wholesale replace: the cursor restarts at this page's head; an older one
    // kept across a WS reconnect / fallback full fetch would skip the gap.
    seedEarlierCursor(events);
  }
  if (showEarlier) {
    ensureEarlierButton();
  }
  runPendingAsync();
  navRebuild();
  if (!restoreScrollPos(selection.key, selection.node)) {
    stickEventsBottom();
  }
  // Safety net: if the page rendered to the all-internal placeholder (no
  // visible bubble) but events exist, transparently page back to real
  // messages. Bounded by AUTO_PAGEBACK_MAX. Covers the paths the server-side
  // visible-aware read can't (remote nodes, disk-exhausted sessions).
  if (!html && events.length > 0) maybeAutoPageBack();
}

// trimEventsScroll bounds the live DOM (#398): drop oldest top children once the
// scroller exceeds MAX_LIVE_DOM_EVENTS. Preserves a pinned "load earlier" button
// (it always lives at the top) and moves oldestFetchedEventTime to the new head
// so a later loadEarlierEvents re-fetches whatever we just evicted, gap-free.
export function trimEventsScroll(el) {
  if (!el) return;
  // Count rendered event bubbles only; dividers/buttons are cheap and ride along.
  let bubbles = el.querySelectorAll(':scope > .event').length;
  if (bubbles <= MAX_LIVE_DOM_EVENTS) return;
  const btn = document.getElementById('earlier-events-btn');
  let node = el.firstChild;
  while (node && bubbles > MAX_LIVE_DOM_EVENTS) {
    const next = node.nextSibling;
    if (node === btn) { node = next; continue; }
    if (node.nodeType === 1 && node.classList && node.classList.contains('event')) bubbles--;
    el.removeChild(node);
    node = next;
  }
  // The surviving head, keyed by the bubbles left at its ms, so the next page
  // re-fetches the evicted ones, same-ms siblings included.
  const c = domHeadCursor(el);
  if (c.ms > 0 && c.ms >= transcript.oldestFetchedEventTime) setEarlierCursor(c.ms, c.keys);
  // The tail no longer starts at the true session head, so make "load earlier"
  // available even if the initial page was short.
  ensureEarlierButton();
}

export function appendEvents(events) {
  const el = document.getElementById('events-scroll');
  if (!el) return;
  const empty = el.querySelector('.empty-state');
  if (empty) empty.remove();
  const wasBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - 30;
  let prevT = lastDividerTime(el);
  // An ask_question followed by a user event inside this same batch must
  // render already locked (mirrors onHistory's pre-render hydrate).
  hydrateAskAnsweredFromHistory(events);
  // Force-bottom when a "user" event arrives: either the local operator just
  // hit send, or a teammate posted through the IM channel — in both cases the
  // message must be visible, even if the viewport was scrolled up.
  let sawUser = false;
  events.forEach(e => {
    if (isInternalEvent(e)) return;
    // Deduplicate: drop strictly-older events. Same-ms events are legitimate
    // siblings (thinking + text from one frame, two text blocks) and are only
    // dropped when their uuid is already on screen — same rule as onHistory.
    if (e.time && e.time < transcript.lastRenderedEventTime) return;
    if (e.time && e.time === transcript.lastRenderedEventTime && eventAlreadyRendered(el, e.uuid)) return;
    if (e.type === 'user') {
      // Same rules as the WS paths (onEvent / onHistory): a user bubble whose
      // uuid is already on screen is a replay, and the first arrival of the
      // real user event replaces the optimistic bubble the send rendered.
      // Without this a send that left over WS and was echoed by the poll
      // (socket dropped in between) painted the message twice (#2430).
      if (eventAlreadyRendered(el, e.uuid)) {
        if (e.time && e.time > transcript.lastRenderedEventTime) transcript.lastRenderedEventTime = e.time;
        return;
      }
      const opt = el.querySelector('.optimistic-msg');
      if (opt) opt.remove();
      // Lock cards already on screen before this user bubble is appended.
      lockRenderedAskCards(el);
    }
    const h = eventHtml(e); if (!h) return;
    const t = e.time || 0;
    if (t && (prevT === 0 || t - prevT >= EVENT_DIVIDER_GAP_MS)) {
      el.insertAdjacentHTML('beforeend', timeDividerHtml(t));
    }
    el.insertAdjacentHTML('beforeend', h);
    if (t) prevT = t;
    if (e.time && e.time > transcript.lastRenderedEventTime) transcript.lastRenderedEventTime = e.time;
    if (e.type === 'user') sawUser = true;
  });
  // Bound the live DOM before scroll/scan so a long streaming session can't
  // grow #events-scroll without limit and OOM the tab (#398).
  trimEventsScroll(el);
  if (sawUser) stickEventsBottom();
  else if (wasBottom) el.scrollTop = el.scrollHeight;
  runPendingAsync();
  navSync();
}

/* ===== Session-stream frames: history, event and the send / interrupt acks ===== */

// renderInitialHistory paints a subscribe's opening frame over the whole pane:
// the placeholder for an empty or all-internal page, the paging cursor and
// "load earlier", then the scroll position.
function renderInitialHistory(el, msg, events, display) {
  // Full render replaces everything — remove any optimistic messages
  const html = renderEventsWithDividers(display, 0);
  // Decide "load earlier" BEFORE the all-internal placeholder so its copy never
  // invites a click on a button that won't appear. The server's has_more knows
  // the frame was cut by visible-bubble count (DefaultVisibleTarget); the length
  // heuristic is only for servers / relayed nodes that omit it.
  const wsHasMore = (typeof msg.has_more === 'boolean') ? msg.has_more : null;
  const showEarlier = (wsHasMore === true) ||
    (wsHasMore == null && events.length >= INITIAL_HISTORY_LIMIT);
  // Only show "no events yet" when the server returned zero events and the session
  // is idle. For running sessions, show "loading events..." since eventPushLoop will
  // deliver events shortly (fixes blank-then-"no events yet" flash on click).
  if (html) {
    el.innerHTML = html;
  } else if (events.length === 0) {
    const sd = sessionList.sessionsData[sid(selection.key, selection.node)];
    el.innerHTML = (sd && sd.state === 'running')
      ? '<div class="empty-state loading-indicator">\u6b63\u5728\u52a0\u8f7d\u4e8b\u4ef6\u2026</div>'
      : '<div class="empty-state">\u6682\u65e0\u4e8b\u4ef6</div>';
  } else {
    // Every event was internal: same placeholder rule as renderEvents.
    el.innerHTML = showEarlier
      ? '<div class="empty-state">\u8be5\u4f1a\u8bdd\u6700\u8fd1\u4ec5\u6709 agent \u6d3b\u52a8\uff0c\u70b9\u51fb\u4e0b\u65b9\u52a0\u8f7d\u66f4\u65e9\u7684\u6d88\u606f</div>'
      : '<div class="empty-state">\u8be5\u4f1a\u8bdd\u4ec5\u6709 agent \u6d3b\u52a8\uff0c\u6682\u65e0\u5bf9\u8bdd\u6d88\u606f</div>';
  }
  // 水位无条件重置：整页替换后 lastRenderedEventTime 只能描述"这一页渲染了
  // 什么"。空 Initial 帧（running 会话刚起进程，completeSubscribe 的空帧臂）
  // 也必须把水位归零 —— 否则被顶替订阅的 stale 增量帧先到把水位推高、空
  // Initial 帧把面板重置成加载占位符但水位没动，新 pushLoop 推同批事件时
  // 全部撞上 `e.time <= lastRenderedEventTime` 被整批丢弃。
  transcript.lastRenderedEventTime = events.length ? (events[events.length - 1].time || 0) : 0;
  // Full replace: the cursor restarts at this frame's head (see renderEvents).
  seedEarlierCursor(events);
  if (showEarlier) {
    ensureEarlierButton();
  }
  runPendingAsync();
  navRebuild();
  // 若有上次切走时保存的滚动位置且不在底部，恢复它；否则照旧贴底。
  if (!restoreScrollPos(selection.key, selection.node)) {
    stickEventsBottom();
  }
  // Safety net: blank page despite events existing → page back to real
  // messages (bounded). Twin of renderEvents' maybeAutoPageBack call;
  // covers remote nodes whose subscribe predates the visible-aware read.
  if (!html && events.length > 0) maybeAutoPageBack();
}

// appendHistoryBackfill appends a frame that is not the opening one, skipping
// what an earlier frame already painted.
function appendHistoryBackfill(el, display) {
  const wasBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - 30;
  // Remove stale "no events yet" before processing incremental events
  const emptyEl = el.querySelector('.empty-state');
  if (emptyEl) emptyEl.remove();
  let prevT = lastDividerTime(el);
  // Force-bottom when a "user" event arrives: either the local operator
  // just hit send, or a teammate posted through the IM channel — in both
  // cases the message must be visible even if the viewport was scrolled up.
  let sawUser = false;
  display.forEach(e => {
    // Strictly-older events were rendered by an earlier frame. Same-ms
    // events must NOT be dropped on time alone: one CLI frame's blocks
    // (thinking + text, process_event_format.go) and ACP's trailing
    // thinking/text/result share one millisecond, and eventHtml(thinking)
    // renders nothing while the cursor below still advances — so `<=`
    // swallowed the text bubble that followed. Same-ms replays (the backend
    // re-admits the watermark ms, #2402) are dropped by uuid instead: a
    // same-time same-uuid pair is always the same entry (RFC dashboard-
    // event-uuid-idempotent-render §3). Newer-time events keep their
    // append behaviour untouched, so streaming text is never frozen.
    if (e.time && e.time < transcript.lastRenderedEventTime) return;
    if (e.time && e.time === transcript.lastRenderedEventTime && eventAlreadyRendered(el, e.uuid)) return;
    if (e.type === 'user') {
      // uuid idempotency for user bubbles: the time-cursor guard above
      // misses the bug case (onEvent rendered the real user event but a
      // restart re-subscribe replays it before the cursor advanced), so
      // dedup on the authoritative uuid as the backstop. User-only by
      // design — streaming text re-emits the same uuid (RFC §3).
      if (eventAlreadyRendered(el, e.uuid)) {
        if (e.time && e.time > transcript.lastRenderedEventTime) transcript.lastRenderedEventTime = e.time;
        return;
      }
      const opt = el.querySelector('.optimistic-msg');
      if (opt) opt.remove();
      sawUser = true;
      lockRenderedAskCards(el);
    }
    const h = eventHtml(e);
    if (h) {
      const t = e.time || 0;
      if (t && (prevT === 0 || t - prevT >= EVENT_DIVIDER_GAP_MS)) {
        el.insertAdjacentHTML('beforeend', timeDividerHtml(t));
      }
      el.insertAdjacentHTML('beforeend', h);
      if (t) prevT = t;
    }
    if (e.time && e.time > transcript.lastRenderedEventTime) transcript.lastRenderedEventTime = e.time;
  });
  // Bound the live DOM on the incremental WS history path too (#398);
  // mirror appendEvents — trim before the scrollHeight reads below.
  trimEventsScroll(el);
  if (sawUser) stickEventsBottom();
  else if (wasBottom) el.scrollTop = el.scrollHeight;
  runPendingAsync();
  navSync();
}

// rebuildTurnFromHistory replays the opening frame's last turn into turnState.
function rebuildTurnFromHistory(events) {
  // Full rebuild: scan backward to find the last turn boundary
  resetTurnState();
  let turnStart = events.length;
  for (let i = events.length - 1; i >= 0; i--) {
    if (events[i].type === 'user' || events[i].type === 'result') { turnStart = i + 1; break; }
    if (i === 0) turnStart = 0;
  }
  // Anchor timer to the actual turn start time, not Date.now()
  if (turnStart < events.length && events[turnStart].time) {
    turnState.turnStartTime = events[turnStart].time;
    paintTurnElapsed();
    turnState.timerId = setInterval(paintTurnElapsed, 1000);
  }
  for (let i = turnStart; i < events.length; i++) {
    applyEventToTurnState(events[i]);
  }
}

// applyBackfillToTurn feeds a backfill frame into turnState additively; user
// and result events are the turn boundaries.
function applyBackfillToTurn(events) {
  for (let i = 0; i < events.length; i++) {
    const ev = events[i];
    if (ev.type === 'user') {
      resetTurnStateForUserEcho();
      const text = ev.detail || ev.summary || '';
      if (text) {
        const h2 = document.querySelector('.main-header h2');
        if (h2) h2.textContent = text;
      }
      continue;
    }
    if (ev.type === 'result') {
      applyTurnResult(ev);
      continue;
    }
    applyEventToTurnState(ev);
  }
}

// applyTurnResult ends the turn a result event closes, on either frame path.
function applyTurnResult(ev) {
  if (ev.cost) {
    const sKey = sid(selection.key, selection.node);
    // ev.cost is the CLI's per-incarnation cumulative total, which RESETS on
    // resume. The authoritative session total is the monotonic delta-sum the
    // server ships as total_cost on the next snapshot poll; never let this
    // optimistic bump regress below it (post-resume ev.cost is lower than the
    // carried-over total).
    if (sessionList.sessionsData[sKey] && ev.cost > (sessionList.sessionsData[sKey].total_cost || 0)) {
      sessionList.sessionsData[sKey].total_cost = ev.cost;
    }
  }
  // Optimistic: result means the turn is done. Update state to "ready"
  // immediately so the banner hides without waiting for session_state WS msg.
  const rsKey = sid(selection.key, selection.node);
  if (sessionList.sessionsData[rsKey] && sessionList.sessionsData[rsKey].state === 'running') {
    sessionList.sessionsData[rsKey].state = 'ready';
    updateSendButton('ready');
  } else {
    resetTurnState();
  }
}

// appendLiveEvent appends a pushed event's bubble: a user replay is dropped by
// uuid, then the time divider, the cursor, the DOM cap and the scroll stick.
function appendLiveEvent(ev) {
  const html = eventHtml(ev);
  if (!html) return;
  const el = document.getElementById('events-scroll');
  if (!el) return;
  const empty = el.querySelector('.empty-state');
  if (empty) empty.remove();
  const isUser = ev.type === 'user';
  if (isUser) {
    // uuid idempotency (user bubbles only): a duplicate push or a re-subscribe
    // replay must not paint the same user message twice; advance the cursor so
    // the time-gated onHistory path stays consistent. User-only by design:
    // streaming text re-emits the same uuid many times (RFC §3).
    if (eventAlreadyRendered(el, ev.uuid)) {
      const t = ev.time || 0;
      if (t && t > transcript.lastRenderedEventTime) transcript.lastRenderedEventTime = t;
      return;
    }
    // First arrival of the real user event: drop the optimistic placeholder.
    const opt = el.querySelector('.optimistic-msg');
    if (opt) opt.remove();
  }
  // An 80px slack band so a small natural scroll doesn't leave auto-stick.
  // User events always pin; AI chunks / result events only stick when the
  // user is in the band, so reading earlier history keeps its position.
  const wasBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - scrollSlackPx;
  const prevT = lastDividerTime(el);
  const evT = ev.time || 0;
  if (evT && (prevT === 0 || evT - prevT >= EVENT_DIVIDER_GAP_MS)) {
    el.insertAdjacentHTML('beforeend', timeDividerHtml(evT));
  }
  el.insertAdjacentHTML('beforeend', html);
  // Advance the cursor on first render too, like appendEvents and onHistory: a
  // uuid-less entry this push painted is otherwise admitted again (#2063).
  if (evT && evT > transcript.lastRenderedEventTime) transcript.lastRenderedEventTime = evT;
  // Bound the live DOM so a long streaming session over the WS push path
  // can't grow #events-scroll without limit and OOM the tab (#398). Must run
  // before the scrollHeight reads below, matching appendEvents.
  trimEventsScroll(el);
  // User events always force-bottom; AI output only sticks when already at bottom.
  if (isUser) stickEventsBottom();
  else if (wasBottom) el.scrollTop = el.scrollHeight;
  runPendingAsync();
  if (ev.type === 'user') navSync();
}

// Session-stream frame handlers; the subscription bookkeeping is sessionStream.
const sessionFrames = {
  onHistory(msg) {
    if (msg.key !== selection.key || (msg.node || 'local') !== selection.node) return;
    const el = document.getElementById('events-scroll');
    if (!el) return;
    const events = msg.events || [];
    // 初始帧判别以服务端的 initial 标记为准（ServerMsg.Initial），不看到达
    // 顺序，也不看 'subscribed' ack 是否已回来 —— 两者都不可靠：
    //   * reverseconn 首订阅路径的 history 由本地 FetchEvents goroutine 发出，
    //     ack 却要经远端 readLoop 绕回来，两者顺序不定；
    //   * 被顶替订阅的 eventPushLoop 是独立 goroutine，unsub() 不排空在途的
    //     backfill，它的帧可以落在新 ack 的前面或后面。
    // backfill 帧不带 initial 标记，无论何时落地都只走增量 append；标记只在
    // 真正消费了初始帧时才清，留给真正的初始帧。
    const isInitial = sessionStream._initialSubscribe && msg.initial === true;
    if (isInitial) sessionStream._initialSubscribe = false;

    // Rebuild the answered-set from history BEFORE rendering so card
    // re-renders show the correct locked state. The Set is in-memory so
    // a page reload or session switch would otherwise make an already-
    // answered card re-actionable and invite duplicate answers to CC.
    hydrateAskAnsweredFromHistory(events);

    const display = processEventsForDisplay(events);

    if (isInitial) renderInitialHistory(el, msg, events, display);
    else appendHistoryBackfill(el, display);

    if (events.length > 0) {
      const last = events[events.length - 1];
      if (last.time > sessionStream.lastEventTimeWs) sessionStream.lastEventTimeWs = last.time;
    }
    // Build turnState from events
    if (isInitial) rebuildTurnFromHistory(events);
    else applyBackfillToTurn(events);
    refreshBanner();
  },

  onEvent(msg) {
    if (msg.key !== selection.key || (msg.node || 'local') !== selection.node) return;
    // Cron timed_out / failed 终态后丢弃后续 ghost 事件（CLI 子进程
    // 在 deadline 命中后还会再吐 result，但 cron run 已记录为终态，
    // 继续追加只会让用户看到"超时但还在工作"的分裂视觉）。
    if (isCronSessionFrozen(msg.key)) return;
    const ev = msg.event;
    if (!ev) return;
    if (ev.time > sessionStream.lastEventTimeWs) sessionStream.lastEventTimeWs = ev.time;
    // Turn boundaries: reset state, don't feed into applyEventToTurnState
    if (ev.type === 'user') {
      const text = ev.detail || ev.summary || '';
      if (text) {
        const h2 = document.querySelector('.main-header h2');
        if (h2) h2.textContent = text;
      }
      resetTurnStateForUserEcho();
      // A user message after an AskUserQuestion means it was answered on some
      // surface — lock the card before the bubble lands (#2430).
      lockRenderedAskCards(document.getElementById('events-scroll'));
    } else if (ev.type === 'result') {
      applyTurnResult(ev);
    } else {
      applyEventToTurnState(ev);
      refreshBanner();
    }
    if (isInternalEvent(ev)) return;
    // RFC v4 agent-team-ui §3.6.2 — when the user has drilled into an
    // agent, the events-scroll pane belongs to that agent; parent events
    // still feed into turnState / banner (handled above) but must not
    // land in the DOM until the user returns.
    if (nzViews.agent && nzViews.agent.activeTaskID()) return;
    appendLiveEvent(ev);
  },

  onSendAck(msg) {
    // "reset" = /clear or /new — the send was consumed by the router to reset
    // the session, not handed to the CLI, so roll back the optimistic running
    // flip. No banner, no turn.
    if (msg.status === 'reset') {
      rollbackOptimisticRunning(msg.key || selection.key, msg.node || selection.node);
      delete perSession.lastSent[sid(msg.key || selection.key, msg.node || selection.node)];
      return;
    }
    // "accepted" = owner of a new turn, "queued" = appended to an active turn.
    // Both are success cases; the dashboard should behave the same way.
    if (msg.status === 'accepted' || msg.status === 'queued') {
      flashSendBtn();
      if (msg.status === 'queued') {
        // Attach an inline chip to the optimistic user bubble instead of a
        // top-of-screen toast. The chip is bound to the bubble, so when the
        // real "user" event replaces it (see the .optimistic-msg removal
        // path in onEvent) the chip disappears along with the bubble — no
        // separate lifecycle to manage.
        const lastOpt = document.querySelector('#events-scroll .event.user.optimistic-msg:last-of-type .event-content');
        if (lastOpt && !lastOpt.querySelector('.msg-queued-chip')) {
          const chip = document.createElement('div');
          chip.className = 'msg-queued-chip';
          chip.textContent = '排队中…';
          lastOpt.appendChild(chip);
        }
      }
      // Subscribe to the session we just sent to, unless we're already
      // subscribed or a subscribe is already pending for this exact key.
      const ackKey = msg.key || selection.key;
      if (ackKey && sessionStream.subscribedKey !== ackKey && sessionStream._pendingSubscribeKey !== ackKey) {
        sessionStream.lastEventTimeWs = 0;
        sessionStream.subscribe(ackKey, selection.node);
      }
      // Re-subscribe is NOT needed here for already-subscribed sessions.
      // The existing eventPushLoop is still connected to the process's event
      // log and will deliver new events (including the user message we just
      // sent). Re-subscribing would cause a history replay that overlaps with
      // events already pushed by the running eventPushLoop, resulting in
      // duplicate user messages in the UI.
      // For process restarts (dead → running), onSessionState
      // handles re-subscription exclusively.
    } else if (msg.status === 'busy') {
      // Queue is disabled (MaxDepth<=0) and the session is currently
      // processing another message, so our send was dropped rather than
      // enqueued. Roll back the optimistic bubble and tell the operator
      // to retry — otherwise the UI silently eats the message.
      showToast('会话正忙，消息未送达，请稍后重试', 'error');
      removeOptimisticMsg(msg.id);
      rollbackOptimisticRunning(msg.key || selection.key, msg.node || selection.node);
      // send 从未真正进入 turn，别把它当成「当前 turn 的输入」残留 —— 否则
      // 下次中断会把这条从未送达的文本回填上来。
      delete perSession.lastSent[sid(msg.key || selection.key, msg.node || selection.node)];
    } else if (msg.status === 'error') {
      // The WS send_ack error is an in-band message, not an HTTP status,
      // but treat the server-supplied `error` string the same way as an
      // HTTP 500 body: truncate + prefix with "发送消息失败：".
      showAPIError('发送消息', 500, msg.error || '');
      // Remove this send's optimistic message on send failure
      removeOptimisticMsg(msg.id);
      rollbackOptimisticRunning(msg.key || selection.key, msg.node || selection.node);
      delete perSession.lastSent[sid(msg.key || selection.key, msg.node || selection.node)];
    }
  },

  // onInterruptAck surfaces a failed / no-op interrupt. interruptSession()
  // toasts "已发送中断" optimistically the moment the frame leaves the socket,
  // so a status:"error" ack (unknown node / server shutting down / remote RPC
  // failure / internal error) or "not_running" (no live process to interrupt)
  // must be reported or the operator believes the interrupt landed.
  onInterruptAck(msg) {
    if (!msg || msg.status === 'ok') return;
    if (msg.status === 'not_running') {
      showToast('会话未在运行，无需中断', 'warning');
      return;
    }
    if (msg.status === 'error') {
      showAPIError('中断会话', 500, msg.error || '');
    }
  },

  // send_error: the HTTP send path (every file-bearing send, plus the WS-down
  // fallback) has no per-request back-channel after its 202, so the server
  // fans asynchronous failures (spawn error, passthrough send failure, remote
  // node send failure) out to every subscriber of the key as this frame.
  //
  // Gate on httpSendPending / sessionLastSent: the frame reaches every tab
  // watching the key, but only the tab that actually sent the failed message
  // owns an optimistic bubble / running flip for it. A second operator's tab
  // (or this tab after it sent nothing) must ignore the frame entirely —
  // otherwise it would tear down its own legitimate optimistic state and
  // toast about a message it never sent. httpSendPending is the primary gate
  // (set for every HTTP send, image-only included); sessionLastSent is the
  // text-only secondary. When we did send: reuse the send_ack error recovery
  // (toast, drop the optimistic bubble if any, roll back running) for the
  // on-screen key, or just undo the running flip for a key we sent to and
  // then navigated away from.
  onSendError(msg) {
    if (!msg || !msg.key) return;
    const node = msg.node || 'local';
    const sKey = sid(msg.key, node);
    if (!perSession.lastSent[sKey] && !perSession.httpSendPending.has(sKey)) return;
    perSession.httpSendPending.delete(sKey);
    if (msg.key === selection.key && node === (selection.node || 'local')) {
      sessionFrames.onSendAck({ status: 'error', key: msg.key, node: msg.node, error: msg.error });
      return;
    }
    rollbackOptimisticRunning(msg.key, node);
    delete perSession.lastSent[sKey];
  },
};

wsm.on(NZ_CONTRACT.WS.history, (msg) => sessionFrames.onHistory(msg));
wsm.on(NZ_CONTRACT.WS.event, (msg) => sessionFrames.onEvent(msg));
wsm.on(NZ_CONTRACT.WS.send_ack, (msg) => sessionFrames.onSendAck(msg));
wsm.on(NZ_CONTRACT.WS.send_error, (msg) => sessionFrames.onSendError(msg));
wsm.on(NZ_CONTRACT.WS.interrupt_ack, (msg) => sessionFrames.onInterruptAck(msg));
// send_message (imported above) polls the transcript through the shell.
registerShell({ fetchEvents });

function flashSendBtn() {
  const btn = document.getElementById('btn-send');
  const stop = document.getElementById('btn-stop');
  const target = (btn && btn.style.display !== 'none') ? btn : stop;
  if (!target) return;
  target.style.boxShadow = '0 0 8px #3fb950';
  setTimeout(() => { target.style.boxShadow = ''; }, 600);
}
