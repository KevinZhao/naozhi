// discovery.js — extracted from dashboard.js (#2558 D4).
//
// Verbatim region move: `git diff --color-moved` shows the body as a pure
// move; the import block, the deps table and the export block below are the
// only additions.
//
// Layering (D4-1 rule): a module dashboard imports must NOT import dashboard
// back — that cycle puts dashboard's own top-level consts in TDZ while this
// module evaluates. Dashboard state is read through nz.state; its helpers are
// injected once via configureDiscovery(), called from dashboard's module body.
import { esc, fetchJSON, nzState } from './nz_util.js';

const deps = {
  EVENT_DIVIDER_GAP_MS: null,
  ICONS: null,
  debouncedFetchSessions: null,
  eventHtml: null,
  getToken: null,
  isInternalEvent: null,
  lastDividerTime: null,
  mobileEnterChat: null,
  navRebuild: null,
  navUpdatePill: null,
  processEventsForDisplay: null,
  renderEventsWithDividers: null,
  sessionTypeTag: null,
  setActiveSessionCard: null,
  showAPIError: null,
  showNetworkError: null,
  stickEventsBottom: null,
  stopPreviewPolling: null,
  timeDividerHtml: null,
  wsm: null,
};
export function configureDiscovery(impl) {
  for (const k of Object.keys(deps)) {
    if (typeof impl[k] === 'undefined') throw new Error('discovery dep missing: ' + k);
    deps[k] = impl[k];
  }
}

/* ===== Discovery & Takeover ===== */

// Discovered-card identity is (pid, node): pids repeat across nodes, so every
// key/lookup/removal goes through these helpers (#2431). Key shape is
// '_discovered:<pid>:<node>' — pid stays in slot 1 for parseDiscoveredPid.
function discoveredKey(pid, node) {
  return '_discovered:' + pid + ':' + (node || 'local');
}
function isDiscoveredKey(key) {
  return typeof key === 'string' && key.startsWith('_discovered:');
}
function parseDiscoveredPid(key) {
  return parseInt(key.split(':')[1], 10);
}
function sameDiscovered(d, pid, node) {
  return d.pid === pid && (d.node || 'local') === (node || 'local');
}
function findDiscovered(pid, node) {
  return nzState.discoveredItems.find(d => sameDiscovered(d, pid, node)) || null;
}
function dropDiscovered(pid, node) {
  nzState.discoveredItems = nzState.discoveredItems.filter(d => !sameDiscovered(d, pid, node));
}

async function scanDiscovered() {
  try {
    const headers = {};
    const t = deps.getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    // RNEW-UX-003: 10s timeout — /api/discovered walks the filesystem, so a
    // stalled disk shouldn't wedge the scan button forever.
    const data = await fetchJSON(NZ_CONTRACT.API.discovered, { headers, timeoutMs: 10000 });
    nzState.discoveredItems = data || [];
    // #1770: only force a full sidebar re-render when the discovered set
    // actually changed. Previously every 30s (connected) / 5s (disconnected)
    // scan unconditionally set nzState.lastVersion=0, defeating fetchSessions' version
    // short-circuit and rebuilding the whole sidebar DOM even when nothing
    // changed — wasted CPU/layout on low-end phones. Mirror the
    // nodesHash/historyHash pattern fetchSessions already uses.
    const discoveredHash = JSON.stringify(nzState.discoveredItems);
    if (discoveredHash === nzState.lastDiscoveredJSON) return;
    nzState.lastDiscoveredJSON = discoveredHash;
    // Trigger sidebar re-render to merge discovered into project groups
    nzState.lastVersion = 0;
    deps.debouncedFetchSessions();
  } catch (e) {
    console.warn('scanDiscovered error:', e.message);
  }
}

async function previewDiscovered(sessionId, cwd, pid, procStartTime, node, cliName, entrypoint) {
  // Generation guard: two rapid clicks on different discovered cards both
  // pass the synchronous prologue, then the first call's awaited fetch used to
  // resolve into the SECOND card's #events-scroll and arm a second
  // setInterval without clearing the first (nzState.previewTimer was simply
  // overwritten → leaked interval appending the wrong session's events).
  // deps.stopPreviewPolling() bumps nzState._previewGen, so capture AFTER calling it.
  deps.stopPreviewPolling();
  const gen = nzState._previewGen;
  // Deselect any managed session. We null `nzState.selectedKey` but deliberately
  // leave `selectedNode` intact — it now doubles as the sidebar filter and
  // nulling it would strand the user on an empty list until their next
  // refresh. The "no managed session selected" state is fully represented
  // by `nzState.selectedKey === null`; other call sites check it that way.
  nzState.selectedKey = null;
  if (deps.wsm.subscribedKey) deps.wsm.unsubscribe();
  if (nzState.eventTimer) { clearInterval(nzState.eventTimer); nzState.eventTimer = null; }
  deps.mobileEnterChat();

  // Highlight the discovered card
  deps.setActiveSessionCard(discoveredKey(pid, node), node || 'local');

  const base = cwd.split('/').pop() || cwd;
  const main = document.getElementById('main');
  main.innerHTML =
    '<div class="main-header">' +
      '<button type="button" class="btn-mobile-back" data-action="mobile-back" title="\u8fd4\u56de\u4f1a\u8bdd\u5217\u8868" aria-label="\u8fd4\u56de\u4f1a\u8bdd\u5217\u8868">' + deps.ICONS.back + '</button>' +
      '<div class="main-header-content">' +
        '<h2>' + esc(base) + '</h2>' +
        '<div class="detail">' +
          deps.sessionTypeTag(cliName || 'cli', entrypoint || '') +
        '</div>' +
      '</div>' +
    '</div>' +
    '<div class="events" id="events-scroll"><div class="empty-state">加载中…</div></div>' +
    '<div class="nav-pill" id="nav-pill">' +
      '<button type="button" data-action="nav-msg" data-dir="prev" id="nav-prev" title="\u4e0a\u4e00\u6761\u7528\u6237\u6d88\u606f (Alt+\u2191)" aria-label="\u8df3\u5230\u4e0a\u4e00\u6761\u7528\u6237\u6d88\u606f">' + deps.ICONS.navUp + '</button>' +
      '<span class="nav-counter" id="nav-counter" data-action="nav-show-list" title="\u70b9\u51fb\u67e5\u770b\u5168\u90e8\u7528\u6237\u6d88\u606f"></span>' +
      '<button type="button" data-action="nav-msg" data-dir="next" id="nav-next" title="\u4e0b\u4e00\u6761\u7528\u6237\u6d88\u606f (Alt+\u2193)" aria-label="\u8df3\u5230\u4e0b\u4e00\u6761\u7528\u6237\u6d88\u606f">' + deps.ICONS.navDown + '</button>' +
    '</div>' +
    '<div class="input-area" id="input-area">' +
      '<div class="file-preview" id="file-preview"></div>' +
      '<div class="input-row">' +
        '<div id="msg-input" contenteditable="true" role="textbox" aria-label="消息输入框" aria-multiline="true" data-placeholder="send a message to take over..." data-action-keydown="msg-input-key" data-action-compositionend="msg-input-compend"></div>' +
        '<button type="button" class="btn-icon btn-send" id="btn-send" data-action="msg-send" title="发送" aria-label="发送消息">' + deps.ICONS.send + '</button>' +
      '</div>' +
    '</div>';
  deps.navRebuild(); // clear stale nav state before async preview fetch
  nzState.pendingDiscovered = {pid: pid, sessionId: sessionId, cwd: cwd, procStartTime: procStartTime, node: node};

  try {
    const nodeParam = node ? '&node=' + encodeURIComponent(node) : '';
    // Pass cwd so the backend resolves the JSONL via an O(1) os.Stat on the
    // CWD-derived path instead of the fallback scan + its 60s negative cache.
    // Without this hint a single transient miss (card shown before the JSONL
    // flushed, or while claude renamed it during compaction) poisons preview
    // for the full TTL, leaving a blank splash that only "fixes itself" once
    // the cache expires.
    const cwdParam = cwd ? '&cwd=' + encodeURIComponent(cwd) : '';
    const headers = {};
    const t = deps.getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    // RNEW-UX-003: 10s timeout — discovered preview loads a ~200-event tail
    // from a JSONL transcript; a hung read shouldn't trap the user on a
    // "加载中..." splash indefinitely.
    let events;
    try {
      events = await fetchJSON(NZ_CONTRACT.API.discovered_preview + '?session_id=' + encodeURIComponent(sessionId) + nodeParam + cwdParam, { headers, timeoutMs: 10000 });
    } catch (err) {
      if (gen !== nzState._previewGen) return;
      const errText = err.message || '';
      const el0 = document.getElementById('events-scroll');
      if (el0) el0.innerHTML = '<div class="empty-state">' + esc(errText || '预览失败') + '</div>';
      if (err.status) deps.showAPIError('预览会话', err.status, errText);
      return;
    }
    // A newer previewDiscovered(), selectSession() or createSession() (all of
    // which run deps.stopPreviewPolling → nzState._previewGen++) may have superseded this
    // call while the fetch was in flight. The managed-session panel reuses the
    // #events-scroll id, so an element check alone is not enough — never
    // paint into someone else's panel.
    if (gen !== nzState._previewGen) return;
    const el = document.getElementById('events-scroll');
    if (!el) return;
    const display = deps.processEventsForDisplay(events);
    if (events.length === 0) {
      el.innerHTML = '<div class="empty-state">暂无会话历史</div>';
    } else {
      el.innerHTML = deps.renderEventsWithDividers(display, 0);
      deps.stickEventsBottom();
    }
    deps.navRebuild();
    // nzState.previewTimer is provably null here: it is only ever armed below, after
    // this generation check, and any older generation's interval was cleared
    // by the deps.stopPreviewPolling() in our own prologue. Do NOT call
    // deps.stopPreviewPolling() at this point — it would bump nzState._previewGen and
    // invalidate this very call.
    nzState.previewEventCount = events.length;
    const capturedSid = sessionId;
    // #1770: guard against overlapping ticks. Each tick re-fetches the full
    // preview event list; on a slow link a fetch can outlast the 2s interval,
    // so without this flag consecutive ticks pile up concurrent requests.
    // Mirrors _fetchEventsInFlight on the main events poll.
    let previewInFlight = false;
    nzState.previewTimer = setInterval(async () => {
      // A newer previewDiscovered() already cleared this interval in its
      // prologue; the check is defence-in-depth against a tick that was
      // queued before clearInterval landed.
      if (gen !== nzState._previewGen) return;
      if (previewInFlight) return;
      previewInFlight = true;
      try {
        const headers2 = {};
        const t2 = deps.getToken();
        if (t2) headers2['Authorization'] = 'Bearer ' + t2;
        const r2 = await fetch(NZ_CONTRACT.API.discovered_preview + '?session_id=' + encodeURIComponent(capturedSid) + nodeParam + cwdParam, { headers: headers2 });
        if (!r2.ok) return;
        const all = await r2.json();
        if (gen !== nzState._previewGen) return;
        if (all.length <= nzState.previewEventCount) return;
        const fresh = all.slice(nzState.previewEventCount);
        nzState.previewEventCount = all.length;
        const el2 = document.getElementById('events-scroll');
        if (!el2) { deps.stopPreviewPolling(); return; }
        const empty = el2.querySelector('.empty-state');
        if (empty) empty.remove();
        const wasBottom = el2.scrollTop + el2.clientHeight >= el2.scrollHeight - 30;
        let prevT2 = deps.lastDividerTime(el2);
        fresh.forEach(e => {
          if (deps.isInternalEvent(e)) return;
          const h = deps.eventHtml(e); if (!h) return;
          const t = e.time || 0;
          if (t && (prevT2 === 0 || t - prevT2 >= deps.EVENT_DIVIDER_GAP_MS)) {
            el2.insertAdjacentHTML('beforeend', deps.timeDividerHtml(t));
          }
          el2.insertAdjacentHTML('beforeend', h);
          if (t) prevT2 = t;
        });
        if (wasBottom) el2.scrollTop = el2.scrollHeight;
        nzState.navUserEls = [...document.querySelectorAll('#events-scroll .event.user')];
        deps.navUpdatePill();
      } catch (_) {
      } finally {
        previewInFlight = false;
      }
    }, 2000);
  } catch (e) {
    deps.showNetworkError('预览会话', e);
  }
}


export {
  discoveredKey,
  dropDiscovered,
  findDiscovered,
  isDiscoveredKey,
  parseDiscoveredPid,
  previewDiscovered,
  sameDiscovered,
  scanDiscovered,
};
