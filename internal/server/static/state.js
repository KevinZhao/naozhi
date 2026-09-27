// state.js — the dashboard's shared state, one object per concern.
//
// Every object here is exported as a const and changed field by field, never
// replaced, so a module that imports it always sees the current values: no
// copy taken at startup can go stale, and no write can land on a property
// nobody reads (#2550). A module owns the fields it writes; others read them.
//
// This module imports nothing, so any module can import it without a cycle.

// ui: which activity view and popover are showing.
export const ui = {
  // activeView is the root view-router state: which top-level view owns the
  // viewport. 'chat' is the default (session sidebar + chat main). 'assets' /
  // 'cron' / 'settings' are full-screen peers driven by setActivityView() and
  // the matching body.nz-view-* CSS classes. Cron rendering is gated on
  // activeView==='cron' (was selectedKey===null) so async cron repaints never
  // clobber the chat DOM and vice-versa.
  activeView: 'chat',
  activePopover: null,
  activePopoverBackdrop: null,
};

// selection: the session the main pane shows.
export const selection = {
  key: null,
  // Initialized in dashboard.js at load.
  node: null,
  // #2431: last (session, state) pushed through updateSendButton — the main-area
  // state that is actually on screen, whichever path (WS push, optimistic flip,
  // renderMainShell, REST reconcile) applied it. The fetchSessions reconcile
  // compares against this so a 5 s fallback poll only re-applies a state that
  // has actually changed. Cleared on session switch.
  lastAppliedMainState: null,
  // {pid, sessionId, cwd, procStartTime, node} when previewing a discovered session
  pendingDiscovered: null,
  pendingRestored: false,
};

// composer: the message being written.
export const composer = {
  lastCompositionEnd: 0,
  // {file, id, status: 'uploading'|'ready'|'error'}
  pendingFiles: [],
  sending: false,
};

// transcript: the events pane: time cursors, paging and fetch generations.
export const transcript = {
  lastEventTime: 0,
  lastRenderedEventTime: 0,
  // oldestFetchedEventTime tracks the earliest server event we've already
  // requested, independent of what's currently rendered in the DOM. The
  // "load earlier" pagination originally took its cursor from the first
  // `.event` child in the scroller — but when a page of 100 events is
  // entirely internal-only (tool_use / agent / task_start / task_progress /
  // task_done / result, filtered out by INTERNAL_EVENT_TYPES), no `.event`
  // is rendered and the pagination silently bails with no cursor. That
  // happens in practice whenever a parallel agent team runs long enough to
  // fill the ring buffer with tool activity; the operator sees a blank
  // events panel and a dead "加载更早的事件" button. Keep this cursor so
  // pagination works regardless of what got filtered out.
  oldestFetchedEventTime: 0,
  previewEventCount: 0,
  // _previewGen is bumped on every previewDiscovered() entry. The awaited
  // preview fetch and the 2s poll tick compare their captured generation
  // against it so a stale call can't render into (or start a second interval
  // for) a card the operator has since clicked away from.
  previewGen: 0,
  // _fetchEventsInFlight gates concurrent HTTP polls of `/api/sessions/events`.
  // The 1 s `setInterval` driver and the on-demand `full` fetch (session
  // switch / WS fallback) can otherwise pile up when the network lags or the
  // server is slow: the second request completes first, `appendEvents`
  // re-orders events, and the first response is then applied on top. The
  // simpler in-flight flag (mirroring `_earlierLoading` on
  // `loadEarlierEvents`) skips overlapping polls — a missed tick is cheap
  // because the next tick will pick up any accumulated events via `after=`
  // anyway.
  //
  // A `full` fetch must NOT be coalesced (#2430): it is the session-switch
  // render. Dropping it left the new session to the next tick, which ran with
  // lastEventTime=0 and no `limit` → the server's legacy default branch handed
  // back the whole ring, appendEvents grafted ≤500 bubbles in one shot, and
  // neither "load earlier" nor the saved scroll position was restored. Instead
  // a full fetch bumps _fetchEventsGen so the tail still in flight becomes
  // stale: it can neither append into the new render nor release the in-flight
  // flag the full fetch now owns.
  fetchInFlight: false,
  fetchGen: 0,
  // loadEarlierEvents fetches up to EARLIER_PAGE_LIMIT events older than the
  // currently-oldest rendered bubble. Prepends the rendered output to the top
  // of the events pane and preserves scroll position so the user's view doesn't
  // jump when new content is injected above.
  //
  // Idempotent: calls bail out while a prior fetch is in flight.
  earlierLoading: false,
  // _earlierGen is bumped by selectSession so a stale loadEarlierEvents (still
  // awaiting the previous session's page) can neither prepend into the new
  // session's scroller nor clear the new session's in-flight flag.
  earlierGen: 0,
  // _autoPageBackCount bounds the frontend safety net for the "parallel agent
  // team ate my history" bug. The server's visible-aware initial read
  // (EventLastNVisibleCtx) already keeps the first page non-blank for local
  // sessions, but a few paths still can't guarantee it — remote nodes (their
  // reverse-RPC fetch predates the visible-aware read), disk-exhausted sessions,
  // or a precision gap where a visible-typed entry still renders to empty HTML.
  // When the rendered page is blank despite events existing, maybeAutoPageBack
  // transparently pages backward (reusing loadEarlierEvents + the
  // oldestFetchedEventTime cursor) up to AUTO_PAGEBACK_MAX times so the operator
  // sees real messages instead of the "该会话最近仅有 agent 活动" placeholder.
  // The counter resets on every session switch (selectSession).
  autoPageBackCount: 0,
  exportInFlight: false,
};

// sessionList: the sidebar and its sources: sessions, nodes, projects, discovered and history sessions.
export const sessionList = {
  sessionsData: {},
  allSessionsCache: [],
  // costSummaryCache is the ledger's last-30-day unit-bucketed total from
  // /api/cost/summary (null until the first fetch lands); the 服务概览 花费 card
  // prefers it over the live-session sum, which forgets deleted sessions and cron.
  // Keys (sid(key,node)) optimistically removed by dismissSession before the
  // DELETE round-trips. fetchSessions/renderSidebar skip these so an in-flight
  // poll or sessions_update WS event that still lists the session cannot
  // resurrect a card the operator already dismissed. Cleared when DELETE
  // confirms (success/404) or fails — see dismissSession's normal-session branch.
  optimisticDeleteKeys: new Set(),
  // Initialized in dashboard.js at load.
  collapsedProjects: null,
  nodesData: {},
  lastVersion: 0,
  lastNodesJSON: '',
  lastHistoryJSON: '',
  // _lastSidebarData caches the most recent /api/sessions payload so the
  // sidebar can re-render locally without re-hitting the server. Set by
  // fetchSessions after a successful render.
  lastSidebarData: null,
  // _lastSidebarHtml caches the last fully-built sidebar HTML string so
  // renderSidebar can skip the (expensive) `list.innerHTML = html` write
  // when the produced markup is byte-identical to what is already mounted.
  // 20 sessions × 1 Hz polling rebuilds the same string every tick when
  // nothing actually changed — comparing the produced string to the cache
  // is O(n) but fast (string equality short-circuits on length and runs in
  // native code), and skipping the assignment avoids a full sidebar reflow
  // + active-card detachment / re-resolve cycle. The cache is the only
  // consumer of the *output* — input fingerprinting is intentionally
  // avoided because the card HTML embeds many fields (selectedKey/Node,
  // unread counts, last_active text, project flags …) and any missed
  // field would cause stale-DOM bugs. Comparing the final string is
  // inherently correct: if it differs by a byte we re-render, if it
  // doesn't there is no observable change to apply. R33-UX1.
  lastSidebarHtml: null,
  // discovered sessions, merged into sidebar
  discoveredItems: [],
  // #1770: last /api/discovered payload, to skip forced re-render when unchanged
  lastDiscoveredJSON: '',
  sessionCounter: 0,
  // [{name, path, node}] from API
  projectsData: [],
  // from API history_sessions (all filesystem sessions)
  historySessionsData: [],
};

// serverInfo: server-side facts cached by the dashboard.
export const serverInfo = {
  defaultWorkspace: '',
  defaultCLIName: '',
  defaultCLIVersion: '',
  // R110-P1 Home panel health strip (Round 148) — cached snapshot of the
  // /api/sessions `stats` object so renderRecentSessionsPanel can surface
  // service health (active / running / ready / uptime / watchdog kills / cli
  // version) without an extra fetch. Refreshed by fetchSessions on every
  // successful poll. Nil-safe consumer: absence = show nothing, never throw.
  lastStatsSnapshot: null,
  // cached LOCAL /api/cli/backends response: {backends, default, detected}
  cliBackends: null,
  cliBackendsFetchedAt: 0,
  // cached /api/access-profiles response: {profiles, default}
  accessProfiles: null,
  accessProfilesFetchedAt: 0,
};

// timers: the dashboard's polling and debounce timer handles.
export const timers = {
  // _activeCardEl caches the currently-.active session card element so the
  // selector switch doesn't have to O(N) scan every card each time. Stays in
  // sync via setActiveSessionCard(); after renderSidebar rebuilds the list the
  // cached node becomes detached — the helper's isConnected guard recovers.
  events: null,
  sessionPoll: null,
  discoveredPoll: null,
  preview: null,
  // Debounced variant: coalesces multiple calls within 300ms into a single fetch.
  // Returns a Promise that resolves after the actual fetch completes.
  fetchDebounce: null,
  fetchDebounceResolvers: [],
};

// hooks: functions assigned at load by the drawers and lightbox that own them, for callers that load earlier.
export const hooks = {
  // Late-bound intra-module hooks (#2557 PR-E3): these used to be IIFE
  // self-exports on window; they are module-scope lets now, assigned when the
  // owning IIFE runs and read at event time (never at load time).
  openLightboxGroup: null,
  openLightboxFromThumb: null,
  getActiveScratchKey: null,
  closeScratchDrawer: null,
  askAside: null,
};
