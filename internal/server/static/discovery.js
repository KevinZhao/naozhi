// discovery.js — extracted from dashboard.js (#2558 D4).
//
// Layering (D4-1 rule): a module dashboard imports must NOT import dashboard
// back — that cycle puts dashboard's own top-level consts in TDZ while this
// module evaluates. Shared state is read from the state.js objects and helpers are
// imported. session_list.js imports this module, so debouncedFetchSessions is a
// shell slot.
import { NZ_CONTRACT } from './contract.js';
import { selection, sessionList, timers, transcript } from './state.js';
import { esc, fetchJSON } from './nz_util.js';
import { sessionStream } from './session_stream.js';
import { discoveredKey, isInternalEvent, sessionTypeTag } from './session_ident.js';
import { EVENT_DIVIDER_GAP_MS, lastDividerTime, mobileEnterChat, setActiveSessionCard, showAPIError, showNetworkError, stickEventsBottom, stopPreviewPolling, timeDividerHtml } from './utilities.js';
import { ICONS } from './icons.js';
import { getToken } from './platform.js';
import { eventHtml, renderEventsWithDividers } from './event_render.js';
import { navRebuild, navSync } from './msg_nav.js';
import { processEventsForDisplay } from './file_refs.js';
import { shell } from './shell.js';

/* ===== Discovery & Takeover ===== */

async function scanDiscovered() {
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    // RNEW-UX-003: 10s timeout — /api/discovered walks the filesystem, so a
    // stalled disk shouldn't wedge the scan button forever.
    const data = await fetchJSON(NZ_CONTRACT.API.discovered, { headers, timeoutMs: 10000 });
    sessionList.discoveredItems = data || [];
    // #1770: only force a full sidebar re-render when the discovered set
    // actually changed (the nodesHash/historyHash pattern fetchSessions uses).
    const discoveredHash = JSON.stringify(sessionList.discoveredItems);
    if (discoveredHash === sessionList.lastDiscoveredJSON) return;
    sessionList.lastDiscoveredJSON = discoveredHash;
    // Trigger sidebar re-render to merge discovered into project groups
    sessionList.lastVersion = 0;
    shell.debouncedFetchSessions();
  } catch (e) {
    console.warn('scanDiscovered error:', e.message);
  }
}

// discoveredPreviewHtml is the read-only panel a discovered (external) CLI
// session previews in: header, empty events pane, nav pill and a composer
// whose first send takes the session over.
function discoveredPreviewHtml(base, typeLabel) {
  return '<div class="main-header">' +
      '<button type="button" class="btn-mobile-back" data-action="mobile-back" title="\u8fd4\u56de\u4f1a\u8bdd\u5217\u8868" aria-label="\u8fd4\u56de\u4f1a\u8bdd\u5217\u8868">' + ICONS.back + '</button>' +
      '<div class="main-header-content">' +
        '<h2>' + esc(base) + '</h2>' +
        '<div class="detail">' +
          sessionTypeTag(typeLabel) +
        '</div>' +
      '</div>' +
    '</div>' +
    '<div class="events" id="events-scroll"><div class="empty-state">加载中…</div></div>' +
    '<div class="nav-pill" id="nav-pill">' +
      '<button type="button" data-action="nav-msg" data-dir="prev" id="nav-prev" title="\u4e0a\u4e00\u6761\u7528\u6237\u6d88\u606f (Alt+\u2191)" aria-label="\u8df3\u5230\u4e0a\u4e00\u6761\u7528\u6237\u6d88\u606f">' + ICONS.navUp + '</button>' +
      '<span class="nav-counter" id="nav-counter" data-action="nav-show-list" title="\u70b9\u51fb\u67e5\u770b\u5168\u90e8\u7528\u6237\u6d88\u606f"></span>' +
      '<button type="button" data-action="nav-msg" data-dir="next" id="nav-next" title="\u4e0b\u4e00\u6761\u7528\u6237\u6d88\u606f (Alt+\u2193)" aria-label="\u8df3\u5230\u4e0b\u4e00\u6761\u7528\u6237\u6d88\u606f">' + ICONS.navDown + '</button>' +
    '</div>' +
    '<div class="input-area" id="input-area">' +
      '<div class="file-preview" id="file-preview"></div>' +
      '<div class="input-row">' +
        '<div id="msg-input" contenteditable="true" role="textbox" aria-label="消息输入框" aria-multiline="true" data-placeholder="send a message to take over..." data-action-keydown="msg-input-key" data-action-compositionend="msg-input-compend"></div>' +
        '<button type="button" class="btn-icon btn-send" id="btn-send" data-action="msg-send" title="发送" aria-label="发送消息">' + ICONS.send + '</button>' +
      '</div>' +
    '</div>';
}

// appendPreviewEvents appends the events a preview poll found past the ones
// already shown, with time dividers, keeping a bottom-anchored pane anchored.
function appendPreviewEvents(el, fresh) {
  const empty = el.querySelector('.empty-state');
  if (empty) empty.remove();
  const wasBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - 30;
  let prevT = lastDividerTime(el);
  fresh.forEach(e => {
    if (isInternalEvent(e)) return;
    const h = eventHtml(e); if (!h) return;
    const t = e.time || 0;
    if (t && (prevT === 0 || t - prevT >= EVENT_DIVIDER_GAP_MS)) {
      el.insertAdjacentHTML('beforeend', timeDividerHtml(t));
    }
    el.insertAdjacentHTML('beforeend', h);
    if (t) prevT = t;
  });
  if (wasBottom) el.scrollTop = el.scrollHeight;
  navSync();
}

// startPreviewPolling re-fetches the preview every 2s and appends what is new.
// timers.preview is provably null when it is called: only previewDiscovered
// arms it, after its generation check, and any older generation's interval
// was cleared by the stopPreviewPolling() in its prologue.
function startPreviewPolling(gen, url) {
  // #1770: a fetch can outlast the 2s interval on a slow link; without this
  // flag consecutive ticks pile up concurrent requests.
  let previewInFlight = false;
  timers.preview = setInterval(async () => {
    // Defence-in-depth against a tick queued before a newer
    // previewDiscovered()'s clearInterval landed.
    if (gen !== transcript.previewGen) return;
    if (previewInFlight) return;
    previewInFlight = true;
    try {
      const headers = {};
      const t = getToken();
      if (t) headers['Authorization'] = 'Bearer ' + t;
      const r = await fetch(url, { headers });
      if (!r.ok) return;
      const all = await r.json();
      if (gen !== transcript.previewGen) return;
      if (all.length <= transcript.previewEventCount) return;
      const fresh = all.slice(transcript.previewEventCount);
      transcript.previewEventCount = all.length;
      const el = document.getElementById('events-scroll');
      if (!el) { stopPreviewPolling(); return; }
      appendPreviewEvents(el, fresh);
    } catch (_) {
    } finally {
      previewInFlight = false;
    }
  }, 2000);
}

async function previewDiscovered(sessionId, cwd, pid, procStartTime, node, typeLabel) {
  // Generation guard: two rapid clicks on different discovered cards both
  // pass the synchronous prologue; the first call's fetch must not resolve
  // into the second card's #events-scroll or arm a second interval.
  // stopPreviewPolling() bumps transcript.previewGen, so capture AFTER calling it.
  stopPreviewPolling();
  const gen = transcript.previewGen;
  // Deselect any managed session. We null `selection.key` but deliberately
  // leave `selectedNode` intact — it doubles as the sidebar filter and
  // nulling it would strand the user on an empty list until their next
  // refresh.
  selection.key = null;
  if (sessionStream.subscribedKey) sessionStream.unsubscribe();
  if (timers.events) { clearInterval(timers.events); timers.events = null; }
  mobileEnterChat();

  // Highlight the discovered card
  setActiveSessionCard(discoveredKey(pid, node), node || 'local');

  const base = cwd.split('/').pop() || cwd;
  const main = document.getElementById('main');
  main.innerHTML = discoveredPreviewHtml(base, typeLabel);
  navRebuild(); // clear stale nav state before async preview fetch
  selection.pendingDiscovered = {pid: pid, sessionId: sessionId, cwd: cwd, procStartTime: procStartTime, node: node};

  try {
    const nodeParam = node ? '&node=' + encodeURIComponent(node) : '';
    // Pass cwd so the backend resolves the JSONL via an O(1) os.Stat on the
    // CWD-derived path instead of the fallback scan + its 60s negative cache,
    // where a single transient miss poisons preview for the full TTL.
    const cwdParam = cwd ? '&cwd=' + encodeURIComponent(cwd) : '';
    const url = NZ_CONTRACT.API.discovered_preview + '?session_id=' + encodeURIComponent(sessionId) + nodeParam + cwdParam;
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    // RNEW-UX-003: 10s timeout — a hung JSONL read shouldn't trap the user
    // on a "加载中..." splash indefinitely.
    let events;
    try {
      events = await fetchJSON(url, { headers, timeoutMs: 10000 });
    } catch (err) {
      if (gen !== transcript.previewGen) return;
      const errText = err.message || '';
      const el0 = document.getElementById('events-scroll');
      if (el0) el0.innerHTML = '<div class="empty-state">' + esc(errText || '预览失败') + '</div>';
      if (err.status) showAPIError('预览会话', err.status, errText);
      return;
    }
    // A newer previewDiscovered(), selectSession() or createSession() (all of
    // which run stopPreviewPolling → transcript.previewGen++) may have superseded this
    // call while the fetch was in flight. The managed-session panel reuses the
    // #events-scroll id, so an element check alone is not enough — never
    // paint into someone else's panel.
    if (gen !== transcript.previewGen) return;
    const el = document.getElementById('events-scroll');
    if (!el) return;
    const display = processEventsForDisplay(events);
    if (events.length === 0) {
      el.innerHTML = '<div class="empty-state">暂无会话历史</div>';
    } else {
      el.innerHTML = renderEventsWithDividers(display, 0);
      stickEventsBottom();
    }
    navRebuild();
    // Do NOT call stopPreviewPolling() here — it would bump
    // transcript.previewGen and invalidate this very call.
    transcript.previewEventCount = events.length;
    startPreviewPolling(gen, url);
  } catch (e) {
    showNetworkError('预览会话', e);
  }
}

export {
  previewDiscovered,
  scanDiscovered,
};
