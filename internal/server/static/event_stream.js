// event_stream.js — the main transcript's event list: the HTTP fetch and poll
// tail, "load earlier" paging with its button, the full render, the live
// append and the DOM cap that bounds it.
import { NZ_CONTRACT } from './contract.js';
import { getToken } from './platform.js';
import { INITIAL_HISTORY_LIMIT } from './session_stream.js';
import { selection, transcript } from './state.js';
import { fetchJSON } from './nz_util.js';
import { eventAlreadyRendered, eventHtml, isInternalEvent, lastDividerTime, leadingTimeDivider, renderEventsWithDividers } from './event_render.js';
import { hydrateAskAnsweredFromHistory, lockRenderedAskCards } from './ask_card.js';
import { runPendingAsync } from './render_md.js';
import { processEventsForDisplay } from './file_refs.js';
import { navRebuild, navSync } from './msg_nav.js';
import { restoreScrollPos, stickEventsBottom } from './running_banner.js';
import { EARLIER_PAGE_LIMIT, EVENT_DIVIDER_GAP_MS, MAX_LIVE_DOM_EVENTS, timeDividerHtml } from './utilities.js';

export async function fetchEvents(full) {
  if (!selection.key) return;
  if (!full && transcript.fetchInFlight) return;
  // Capture session identity at dispatch time so a mid-flight switch doesn't
  // apply stale events to the new session's DOM. `selectedKey` can flip
  // synchronously from `pickSession`/`dismiss` callbacks while `await`
  // suspends us; applying `appendEvents` after that point would graft the
  // prior session's tail into the newly-opened session's scroller.
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
    // The initial (`full`) fetch reads the X-Events-Has-More header so it can
    // mount "load earlier" off the server's truncation decision rather than the
    // brittle len>=INITIAL_HISTORY_LIMIT guess. _eventsHeaders captures the raw
    // Response headers for that one call; incremental polls don't need them.
    let hasMoreHeader = null;
    try {
      const onResp = full ? (resp) => {
        // null when the header is absent (legacy server / remote-node relay) —
        // leave hasMoreHeader null so renderEvents falls back to the length
        // heuristic rather than treating "absent" as an authoritative false.
        const v = resp && resp.headers ? resp.headers.get('X-Events-Has-More') : null;
        if (v != null) hasMoreHeader = (v === '1' || v === 'true');
      } : null;
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

    if (full) {
      // Pass the server's authoritative hasMore when the header was present;
      // null means "fall back to the length heuristic" (legacy / remote node).
      renderEvents(events, hasMoreHeader);
    } else {
      appendEvents(events);
    }

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

// loadEarlierEvents pages backward from the cursor, up to maxPages requests
// (default EARLIER_SKIP_MAX_PAGES), until a page shows a visible bubble.
async function loadEarlierEvents(maxPages) {
  if (transcript.earlierLoading || !selection.key) return;
  const el = document.getElementById('events-scroll');
  if (!el) return;

  // Cursor = oldest FETCHED event, not the oldest rendered bubble: an all-
  // internal page never reaches the DOM, so a DOM cursor re-fetched that same
  // page on every click. The DOM head only covers a missing cursor.
  let oldestTime = transcript.oldestFetchedEventTime;
  const head = oldestTime ? null : el.querySelector(':scope > .event');
  if (head) oldestTime = Number(head.getAttribute('data-time') || 0);
  if (!oldestTime) return;
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
    for (let n = 1; ; n++) {
      // Per page: prependEvents re-mounts the button in its 'ready' state.
      updateEarlierButton('loading');
      let url = NZ_CONTRACT.API.sessions_events + '?key=' + encodeURIComponent(key) +
                '&before=' + oldestTime + '&limit=' + EARLIER_PAGE_LIMIT;
      if (node && node !== 'local') url += '&node=' + encodeURIComponent(node);
      const r = await fetch(url, { headers });
      if (stale()) return;
      if (!r.ok) { updateEarlierButton('error'); return; }
      const body = await r.json();
      if (stale()) return;
      const events = Array.isArray(body) ? body : [];
      const shown = prependEvents(events);
      // A short (or empty) page means the history is exhausted.
      if (events.length < EARLIER_PAGE_LIMIT) { updateEarlierButton('done'); return; }
      const next = transcript.oldestFetchedEventTime;
      // Stop on a visible bubble, on budget, or on a stuck cursor (a server
      // ignoring `before` must not spin this loop).
      if (shown || n >= budget || !next || next >= oldestTime) { updateEarlierButton('ready'); return; }
      oldestTime = next;
    }
  } catch (e) {
    console.error('load earlier events:', e);
    if (!stale()) updateEarlierButton('error');
  } finally {
    // Release the flag unless selectSession has already reset it for a newer
    // session (it bumps _earlierGen) — otherwise a second page-back could run
    // concurrently. Keyed on the generation only, NOT the full stale(): paths
    // that flip selectedKey without selectSession (pending-session create,
    // dismiss / discovered preview → selectedKey=null) never reset the flag,
    // so a stale() check here would leave it stuck true until the next select.
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

  // Advance the pagination cursor before DOM work so a subsequent
  // loadEarlierEvents sees the new floor even if the freshly prepended
  // batch was entirely internal-filtered.
  const firstT = events[0] && events[0].time;
  if (firstT && (transcript.oldestFetchedEventTime === 0 || firstT < transcript.oldestFetchedEventTime)) {
    transcript.oldestFetchedEventTime = firstT;
  }

  // Preserve visual stability: capture distance-from-bottom before ANY mutation
  // (the reader clicked the button, so they sit at the top and removing it
  // would jump the page by its height), then restore after. Bottom-anchored
  // math holds however the content above changes height.
  const prevScrollFromBottom = el.scrollHeight - el.scrollTop - el.clientHeight;

  // Remove "load earlier" button so we can place new events first; it'll be
  // re-added after.
  const btn = document.getElementById('earlier-events-btn');
  if (btn) btn.remove();

  const display = processEventsForDisplay(events);
  const html = renderEventsWithDividers(display, 0);
  // Drop the placeholder only when this page actually brings a visible bubble:
  // leaving it in place would push the prepended real messages below it.
  //
  // When the page is still fully internal (html === '') the placeholder is the
  // only explanation the operator has for an empty transcript. Removing it
  // unconditionally left a blank pane with nothing but a "load earlier" button
  // once maybeAutoPageBack exhausted its AUTO_PAGEBACK_MAX budget — measured in
  // test/e2e/auto_pageback.test.js, and the exact case the retired source
  // anchor claimed to protect by grepping for the placeholder's text (#2547).
  if (html) {
    const placeholder = el.querySelector('.empty-state');
    if (placeholder) placeholder.remove();
  }

  // The DOM's leading divider was emitted for prevTime=0 ("always divide
  // before the first visible bubble"). Once older bubbles sit above it, it is
  // only legitimate when the gap to the newest prepended bubble is a real
  // divider gap — otherwise the pagination seam shows two stacked dividers
  // (#2430).
  const oldLeadDivider = leadingTimeDivider(el);
  const frag = document.createElement('div');
  frag.innerHTML = html;
  const newestPrependedTime = lastDividerTime(frag);
  // Move children one-by-one to preserve DOM structure; innerHTML replace
  // would wipe the existing event bubbles. Anchor on the pre-insert first
  // child once: inserting each child before a moving el.firstChild reversed
  // the prepended page (newest-first) under the seam.
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

  // runPendingAsync only iterates the `pending` dictionaries (new IDs
  // emitted by the freshly-rendered bubbles above), so it is already
  // incremental — no DOM scan is needed.
  runPendingAsync();
  navRebuild();
  return !!html;
}

// ensureEarlierButton injects/refreshes the "load earlier" affordance at the
// top of the scroller. Button state is stored in data-state on the element.
export function ensureEarlierButton() {
  const el = document.getElementById('events-scroll');
  if (!el) return;
  let btn = document.getElementById('earlier-events-btn');
  if (!btn) {
    btn = document.createElement('button');
    btn.id = 'earlier-events-btn';
    btn.type = 'button';
    btn.className = 'earlier-events-btn';
    btn.style.cssText = 'display:block;margin:8px auto;padding:6px 14px;background:var(--nz-bg-2);border:1px solid var(--nz-border);color:var(--nz-text);border-radius:6px;cursor:pointer;font-size:12px';
    btn.textContent = '加载更早的事件';
    btn.onclick = () => loadEarlierEvents();
    el.insertBefore(btn, el.firstChild);
  } else if (el.firstChild !== btn) {
    el.insertBefore(btn, el.firstChild);
  }
  updateEarlierButton('ready');
}

function updateEarlierButton(state) {
  const btn = document.getElementById('earlier-events-btn');
  if (!btn) return;
  btn.dataset.state = state;
  switch (state) {
    case 'loading':
      btn.textContent = '加载中…';
      btn.disabled = true;
      break;
    case 'done':
      btn.textContent = '没有更早的事件';
      btn.disabled = true;
      break;
    case 'error':
      btn.textContent = '加载失败 — 点击重试';
      btn.disabled = false;
      break;
    default:
      btn.textContent = '加载更早的事件';
      btn.disabled = false;
  }
}

// renderEvents replaces the whole events pane on the initial / full-fetch path.
// hasMore (when not null) is the server's authoritative "older history exists"
// signal from the X-Events-Has-More header; null means the header was absent
// (legacy server or remote node) and we fall back to the length heuristic.
export function renderEvents(events, hasMore) {
  const el = document.getElementById('events-scroll');
  if (!el) return;
  // RNEW-UX-007 — innerHTML replace below wipes any live text selection
  // inside the events panel (user was mid-copy of a chat bubble). Events
  // are replayed idempotently each poll/push tick, so skipping one refresh
  // while the user has an active selection inside the events list is safe:
  // the next tick lands with the same data and re-renders then. We check
  // anchorNode lineage so selections elsewhere (sidebar, input, modal) are
  // not affected by this guard.
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
  // Decide whether "load earlier" will mount BEFORE rendering the all-internal
  // placeholder, so its copy never promises a button that won't appear. Mount
  // off the server's hasMore flag when present — it knows the slice was
  // truncated by visible-bubble count, catching the case the old length
  // heuristic missed (more visible bubbles than DefaultVisibleTarget but fewer
  // total events than INITIAL_HISTORY_LIMIT). Fall back to the length heuristic
  // only when the header was absent (hasMore === null).
  const showEarlier = (hasMore === true) ||
    (hasMore == null && events.length >= INITIAL_HISTORY_LIMIT);
  if (html) {
    el.innerHTML = html;
  } else if (events.length === 0) {
    el.innerHTML = '<div class="empty-state">暂无事件</div>';
  } else {
    // The server returned events but every one was filtered out by
    // INTERNAL_EVENT_TYPES — typically a parallel agent team where the
    // visible tail of the log is all tool_use / task_progress. Render a
    // neutral placeholder so the panel isn't a blank void. Only invite the
    // user to "click below" when the button will actually mount; otherwise the
    // whole remembered history is internal activity with nothing older to page
    // to, so promise nothing.
    el.innerHTML = showEarlier
      ? '<div class="empty-state">该会话最近仅有 agent 活动，点击下方加载更早的消息</div>'
      : '<div class="empty-state">该会话仅有 agent 活动，暂无对话消息</div>';
  }
  if (events.length > 0) {
    const last = events[events.length - 1];
    if (last.time) transcript.lastRenderedEventTime = last.time;
    // Wholesale replace: the cursor restarts at this page's head; an older one
    // kept across a WS reconnect / fallback full fetch would skip the gap.
    if (events[0].time) transcript.oldestFetchedEventTime = events[0].time;
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
  // The surviving head, not the last evicted bubble: `before=` is strict.
  const t = Number(el.querySelector(':scope > .event')?.getAttribute('data-time') || 0);
  if (t > transcript.oldestFetchedEventTime) transcript.oldestFetchedEventTime = t;
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
