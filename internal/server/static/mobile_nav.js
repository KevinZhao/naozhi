// mobile_nav.js — mobile shell navigation: viewport tracking, list/chat
// switching, swipe-back, long-press session actions, and the desktop
// sidebar fully-collapse helpers that share its drawer bookkeeping
// (#2558 D4-3).
//
// Verbatim move out of dashboard.js; only the import + dep-wiring lines are
// new. Layering (D4-1 rule): never import dashboard back.
import { esc, nzState, nzTest, showToast } from './nz_util.js';

const deps = {
  ICONS: null,
  confirmDialog: null,
  dismissSession: null,
  lsGet: null,
  lsSet: null,
  nzAnyDrawerOpen: null,
  nzSplitExit: null,
  renameSession: null,
  renderMainHeader: null,
  selectSession: null,
};
export function configureMobileNav(impl) {
  for (const k of Object.keys(deps)) {
    if (typeof impl[k] === 'undefined') throw new Error('mobile_nav dep missing: ' + k);
    deps[k] = impl[k];
  }
  // Deps are live now — run the load-time bootstrap that needs them.
  initSidebarCollapsed();
}

/* ===== Mobile Navigation ===== */

const mobileQuery = window.matchMedia('(max-width:768px)');
function isMobile() { return mobileQuery.matches; }

// Re-initialise when crossing the 768px breakpoint (e.g. orientation change)
mobileQuery.addEventListener('change', e => {
  if (!e.matches) {
    document.body.classList.remove('mobile-list-view', 'mobile-chat-view');
  } else {
    initMobile();
  }
});

function mobileEnterChat() {
  if (!isMobile()) return;
  // #2431: switching sessions while already in chat view must not stack
  // another entry — replace ours so a single back press leaves chat.
  if (history.state && history.state.view === 'chat') history.replaceState({ view: 'chat' }, '');
  else history.pushState({ view: 'chat' }, '');
  document.body.classList.remove('mobile-list-view');
  document.body.classList.add('mobile-chat-view');
}

function mobileShowList() {
  document.body.classList.remove('mobile-chat-view');
  document.body.classList.add('mobile-list-view');
  if (document.activeElement) document.activeElement.blur();
}

// In-app back button / swipe-back. Flips the view synchronously, then pops
// the history entry mobileEnterChat pushed so the browser stack stays in
// step (#2431); the popstate below sees list view already and no-ops.
function mobileBack() {
  const ownsEntry = isMobile() && history.state && history.state.view === 'chat';
  mobileShowList();
  if (ownsEntry) history.back();
}

// Handle Android back button and iOS swipe-back gesture
window.addEventListener('popstate', () => {
  if (isMobile() && document.body.classList.contains('mobile-chat-view')) {
    mobileShowList();
  }
});

function initMobile() {
  if (!isMobile()) return;
  const hasSession = !!nzState.selectedKey;
  document.body.classList.toggle('mobile-chat-view', hasSession);
  document.body.classList.toggle('mobile-list-view', !hasSession);
}

/* Track iOS visual viewport so the main-header stays visible when the keyboard opens.
   Without this, position:fixed elements get scrolled above the viewport when the
   soft keyboard pushes the page up. */
function initViewportTracking() {
  const vv = window.visualViewport;
  if (!vv) return;
  const root = document.documentElement;
  let raf = 0;
  const apply = () => {
    raf = 0;
    root.style.setProperty('--vv-top', vv.offsetTop + 'px');
    root.style.setProperty('--vv-height', vv.height + 'px');
    // Soft keyboard detection: visualViewport shrinks by >150px when the
    // on-screen keyboard opens on iOS / Android. Toggle body.kbd-open so
    // CSS can collapse space-hogging elements (running banner / nav pill)
    // and keep the input within thumb reach.
    const layoutH = window.innerHeight || vv.height;
    const kbdOpen = layoutH - vv.height > 150;
    document.body.classList.toggle('kbd-open', kbdOpen);
  };
  const schedule = () => { if (!raf) raf = requestAnimationFrame(apply); };
  vv.addEventListener('resize', schedule);
  vv.addEventListener('scroll', schedule);
  apply();
}

// R110-P1 long-press context menu state + constants. LONG_PRESS_MS matches
// the Android / iOS WebKit default for "long-press" detection; shorter feels
// trigger-happy (users misfire while scrolling) and longer reads as a hang.
// MOVE_CANCEL_PX below the 5px swipe threshold so "small jitter" does not
// cancel long-press before swipe starts tracking, but any directional intent
// above 8px unambiguously means the user wants to swipe (or scroll).
const LONG_PRESS_MS = 500;
const LONG_PRESS_MOVE_CANCEL_PX = 8;
let _longPressTimer = null;
let _longPressFired = false;

// closeContextMenu tears down any open .ctx-menu + its overlay. Safe to call
// when nothing is open (no-op). Exposed at module scope so both the menu
// actions and the global touch/click handlers can call it.
function closeContextMenu() {
  const m = document.getElementById('session-ctx-menu');
  if (m) m.remove();
  const ov = document.getElementById('session-ctx-overlay');
  if (ov) ov.remove();
}

// showSessionContextMenu renders a floating menu anchored near (x, y) with
// rename / copy-key / delete actions for the given session card. Clamps the
// menu inside the viewport so long-pressing near a screen edge doesn't push
// the menu off-screen. Uses a transparent overlay to capture outside clicks
// (cheaper than attaching a document-level click handler that would need
// careful removal). Items array shape is [{ label, icon, action, danger }]
// so future extensions (pin / favorite) drop in without refactoring.
function showSessionContextMenu(x, y, items) {
  closeContextMenu();
  const ov = document.createElement('div');
  ov.id = 'session-ctx-overlay';
  ov.className = 'ctx-menu-overlay';
  ov.addEventListener('click', closeContextMenu, {passive:true});
  ov.addEventListener('touchstart', e => {
    // Prevent the overlay's touchstart from triggering a scroll on mobile
    // while the menu is open — users tapping outside expect "close" not
    // "keep scrolling through the underlying list".
    if (e.target === ov) { closeContextMenu(); }
  }, {passive:true});
  document.body.appendChild(ov);

  const menu = document.createElement('div');
  menu.id = 'session-ctx-menu';
  menu.className = 'ctx-menu';
  menu.setAttribute('role', 'menu');
  menu.setAttribute('aria-label', '会话操作');
  menu.innerHTML = items.map((it, i) =>
    '<div class="ctx-menu-item' + (it.danger ? ' danger' : '') + '"' +
    ' role="menuitem" tabindex="0" data-idx="' + i + '">' +
    '<span class="ctx-icon" aria-hidden="true">' + esc(it.icon || '') + '</span>' +
    '<span>' + esc(it.label) + '</span></div>'
  ).join('');
  document.body.appendChild(menu);

  // Clamp menu position inside viewport with 8px padding. Measure first so we
  // know actual rendered size (padding/border/content-driven width).
  const rect = menu.getBoundingClientRect();
  const pad = 8;
  let left = x, top = y;
  if (left + rect.width + pad > window.innerWidth) left = window.innerWidth - rect.width - pad;
  if (top + rect.height + pad > window.innerHeight) top = window.innerHeight - rect.height - pad;
  if (left < pad) left = pad;
  if (top < pad) top = pad;
  menu.style.left = left + 'px';
  menu.style.top = top + 'px';

  menu.addEventListener('click', e => {
    const it = e.target.closest('.ctx-menu-item');
    if (!it) return;
    const idx = parseInt(it.dataset.idx, 10);
    closeContextMenu();
    if (items[idx] && typeof items[idx].action === 'function') items[idx].action();
  });
  menu.addEventListener('keydown', e => {
    if (e.key === 'Escape') { e.preventDefault(); closeContextMenu(); }
  });
  // Focus the first item so keyboard users (rare on mobile but happens with
  // BT keyboards / accessibility tools) have a landing point after the menu
  // opens. Desktop right-click path also benefits.
  const first = menu.querySelector('.ctx-menu-item');
  if (first) first.focus();
}

// copyStringToClipboard writes a string to the system clipboard using the
// modern navigator.clipboard API with a document.execCommand fallback for
// older browsers / non-HTTPS contexts. Returns a Promise<boolean>.
async function copyStringToClipboard(s) {
  try {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      await navigator.clipboard.writeText(s);
      return true;
    }
  } catch (_) { /* fall through */ }
  const ta = document.createElement('textarea');
  try {
    ta.value = s;
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    return document.execCommand('copy');
  } catch (_) {
    return false;
  } finally {
    // Always detach — if execCommand throws (sandboxed iframes, locked
    // clipboards) the caught return skipped an inline removeChild before,
    // leaking the <textarea> (containing the user-supplied string) into
    // the DOM for the page lifetime.
    if (ta.parentNode) ta.parentNode.removeChild(ta);
  }
}

// openSessionContextMenu assembles the items for a given session card and
// opens the menu anchored near the touch coordinates. Rename reuses the
// existing modal-prompt pattern by selecting the session first, then
// deferring to deps.renameSession(); copy-key writes the key to clipboard with
// a toast confirmation; delete routes through deps.dismissSession() which
// surfaces the existing deps.confirmDialog flow on its own.
function openSessionContextMenu(card, x, y) {
  const key = card.dataset.key;
  const node = card.dataset.node || 'local';
  if (!key) return;
  showSessionContextMenu(x, y, [
    {
      label: '重命名', icon: deps.ICONS.edit,
      action: () => {
        // deps.renameSession() reads nzState.selectedKey/nzState.selectedNode and repaints only the
        // header of the CURRENT shell (deps.renderMainHeader), so the target must
        // be properly selected first — flipping the globals alone would stamp
        // this card's header onto whatever conversation is on screen.
        // deps.selectSession is a no-op re-select when the card is already open.
        deps.selectSession(key, node);
        deps.renameSession();
      },
    },
    {
      label: '复制 key', icon: deps.ICONS.copy,
      action: async () => {
        const ok = await copyStringToClipboard(key);
        showToast(ok ? '已复制 key' : '复制失败', ok ? 'success' : 'warning');
      },
    },
    {
      label: '删除', icon: deps.ICONS.trash, danger: true,
      action: () => { deps.dismissSession(key, node); },
    },
  ]);
}

function initSwipeDelete() {
  const list = document.getElementById('session-list');
  if (!list) return;
  let card = null, startX = 0, startY = 0, tracking = false, cardW = 0;
  // cancelLongPress clears the in-flight long-press timer + any visual
  // target state. Called from every exit path so a jittery touch cannot
  // leave the card stuck in the .long-pressing style.
  const cancelLongPress = () => {
    if (_longPressTimer) { clearTimeout(_longPressTimer); _longPressTimer = null; }
    if (card) card.classList.remove('long-pressing');
  };
  list.addEventListener('touchstart', e => {
    if (e.touches.length !== 1) { card = null; cancelLongPress(); return; }
    const c = e.target.closest('.session-card[data-key]');
    if (!c) return;
    card = c; startX = e.touches[0].clientX; startY = e.touches[0].clientY; tracking = false;
    _longPressFired = false;
    // Schedule long-press. If the user lifts / moves before the timer
    // fires, the cancel path below wipes it; otherwise we trigger the
    // context menu AND null out `card` so the subsequent touchend does
    // not accidentally also trigger a select/click on the card beneath.
    _longPressTimer = setTimeout(() => {
      _longPressTimer = null;
      if (!card) return;
      _longPressFired = true;
      const x = startX, y = startY;
      const target = card;
      card = null; tracking = false;
      target.classList.remove('long-pressing');
      openSessionContextMenu(target, x, y);
    }, LONG_PRESS_MS);
    // Mild visual feedback on press — users need to know "something is
    // happening" before the 500ms elapses. Kept to a subtle background
    // tint so it doesn't read as a selection.
    card.classList.add('long-pressing');
  }, {passive:true});
  list.addEventListener('touchmove', e => {
    if (!card) return;
    const dx = e.touches[0].clientX - startX;
    const dy = e.touches[0].clientY - startY;
    // Cancel long-press as soon as directional intent emerges. Threshold
    // is slightly looser than swipe's 5px trigger so small jitters don't
    // cancel long-press unnecessarily, but any real swipe intent kills
    // the menu before it can fire.
    if (Math.abs(dx) >= LONG_PRESS_MOVE_CANCEL_PX || Math.abs(dy) >= LONG_PRESS_MOVE_CANCEL_PX) {
      cancelLongPress();
    }
    if (!tracking) {
      if (Math.abs(dx) < 5 && Math.abs(dy) < 5) return;
      if (Math.abs(dy) >= Math.abs(dx)) { card = null; return; }
      tracking = true;
      // #1772: cache the card width once, here — before any transform write
      // this gesture. Reading card.offsetWidth inside the per-frame transform
      // write below is a getter that the browser must keep coherent with
      // pending style writes; caching it (the width can't change mid-swipe)
      // keeps the touchmove hot loop free of layout reads. Read now while the
      // card is still in its untransformed layout position (cheap).
      cardW = card.offsetWidth || 1;
    }
    if (dx >= 0) return;
    card.classList.add('swiping');
    card.style.transform = 'translateX(' + dx + 'px)';
    card.style.background = 'rgba(218,54,51,' + Math.min(0.35, -dx / cardW * 0.6) + ')';
  }, {passive:true});
  list.addEventListener('touchend', e => {
    cancelLongPress();
    if (!card || !tracking) { card = null; tracking = false; return; }
    const dx = e.changedTouches[0].clientX - startX;
    const c = card; card = null; tracking = false;
    c.classList.remove('swiping');
    if (dx < -c.offsetWidth * 0.4) {
      c.style.transition = 'transform .2s ease, opacity .2s ease';
      c.style.transform = 'translateX(-100%)';
      c.style.opacity = '0';
      // Swipe past the threshold is an explicit gesture — skip the modal
      // confirm here so the user doesn't have to re-confirm after already
      // dragging 40% of the card width. Button-click path still confirms.
      setTimeout(() => deps.dismissSession(c.dataset.key, c.dataset.node || 'local', { skipConfirm: true }), 180);
    } else {
      c.style.transition = 'transform .2s ease, background .2s ease';
      c.style.transform = '';
      c.style.background = '';
      setTimeout(() => { c.style.transition = ''; }, 200);
    }
  }, {passive:true});
  // touchcancel fires when the system interrupts the gesture (incoming call,
  // scroll takeover by browser UI). Mirror cleanup so _longPressTimer
  // can't fire after the finger has already gone.
  list.addEventListener('touchcancel', () => {
    cancelLongPress();
    if (card) {
      card.classList.remove('swiping');
      card.style.transform = '';
      card.style.background = '';
    }
    card = null; tracking = false;
  }, {passive:true});
  // Click bubbles up after touchend. If a long-press just fired we have
  // already null'd `card`, but the underlying anchor click (deps.selectSession
  // via onclick) still fires. Swallow it when _longPressFired is set.
  list.addEventListener('click', e => {
    if (_longPressFired) {
      _longPressFired = false;
      e.preventDefault();
      e.stopPropagation();
    }
  }, true);
  // Desktop right-click also surfaces the same menu for parity with the
  // mobile long-press path. Power users can reach every action via the
  // hover buttons too; this just gives keyboard-unfriendly trackpad users
  // a discoverable alternative.
  list.addEventListener('contextmenu', e => {
    const c = e.target.closest('.session-card[data-key]');
    if (!c) return;
    e.preventDefault();
    openSessionContextMenu(c, e.clientX, e.clientY);
  });
}

function initSwipeBack() {
  const main = document.getElementById('main');
  if (!main) return;
  let startX = 0, startY = 0, tracking = false, swiping = false;
  main.addEventListener('touchstart', e => {
    if (!isMobile() || e.touches.length !== 1) return;
    startX = e.touches[0].clientX; startY = e.touches[0].clientY;
    tracking = false; swiping = false;
    // Only trigger from left edge (within 40px)
    if (startX > 40) return;
    tracking = true;
  }, {passive:true});
  main.addEventListener('touchmove', e => {
    if (!tracking) return;
    const dx = e.touches[0].clientX - startX;
    const dy = e.touches[0].clientY - startY;
    if (!swiping) {
      if (Math.abs(dx) < 8 && Math.abs(dy) < 8) return;
      if (Math.abs(dy) > Math.abs(dx)) { tracking = false; return; }
      if (dx < 0) { tracking = false; return; }
      swiping = true;
    }
    const progress = Math.min(dx / window.innerWidth, 1);
    main.style.transform = 'translateX(' + dx + 'px)';
    main.style.opacity = String(1 - progress * 0.3);
  }, {passive:true});
  main.addEventListener('touchend', e => {
    if (!tracking || !swiping) { tracking = false; swiping = false; return; }
    const dx = e.changedTouches[0].clientX - startX;
    tracking = false; swiping = false;
    if (dx > window.innerWidth * 0.3) {
      main.style.transition = 'transform .2s ease, opacity .2s ease';
      main.style.transform = 'translateX(100%)';
      main.style.opacity = '0';
      setTimeout(() => {
        main.style.transition = ''; main.style.transform = ''; main.style.opacity = '';
        mobileBack();
      }, 200);
    } else {
      main.style.transition = 'transform .2s ease, opacity .2s ease';
      main.style.transform = ''; main.style.opacity = '';
      setTimeout(() => { main.style.transition = ''; }, 200);
    }
  }, {passive:true});
}


/* ===== Sidebar fully-collapse (PC only) =====
   Toggle body.sidebar-collapsed so .main occupies the full viewport. State is
   persisted via deps.lsSet so a refresh keeps the user's preference. The mobile
   layout (≤768px) already treats the sidebar as a fixed drawer overlay, so
   the toggle is a no-op there: we suppress the click and let mobile's own
   list/chat-view classes drive visibility. Keyboard shortcut: `[` (mirroring
   editor conventions like VS Code's Ctrl+B / Cursor's `[`). */
const LS_SIDEBAR_COLLAPSED = 'sidebar_collapsed';

function isMobileViewport() {
  return window.matchMedia && window.matchMedia('(max-width: 768px)').matches;
}

// applySidebarCollapsed is the single state-mutator. moveFocus drives whether
// to relocate keyboard focus to the now-visible button — true on user-driven
// toggle (the previously-focused button is about to be display:none'd, which
// would punt focus back to <body>); false on cold-start / viewport-boundary
// re-apply (don't steal focus from the user's first interaction).
function applySidebarCollapsed(collapsed, moveFocus) {
  document.body.classList.toggle('sidebar-collapsed', !!collapsed);
  // Single mid-line handle on the resizer serves both directions; flip the
  // aria-expanded + label so AT and tooltip agree with the visual state.
  const btn = document.getElementById('btn-sidebar-toggle');
  if (btn) {
    btn.setAttribute('aria-expanded', collapsed ? 'false' : 'true');
    btn.setAttribute('aria-label', collapsed ? '展开侧边栏' : '收起侧边栏');
    btn.setAttribute('title', collapsed ? '展开侧边栏 (按 [)' : '收起侧边栏 (按 [)');
  }
  if (moveFocus && btn && typeof btn.focus === 'function') {
    try { btn.focus({preventScroll: true}); } catch (_) { btn.focus(); }
  }
}

function toggleSidebarCollapsed() {
  // Mobile drawer has its own list/chat-view contract — do not piggyback on
  // it; just bail so the existing back-button + drawer flow stays canonical.
  if (isMobileViewport()) return;
  // A manual toggle means the user took control of the sidebar state — a
  // pending drawer-driven collapse must no longer be undone on drawer close.
  _sidebarAutoCollapsed = false;
  const next = !document.body.classList.contains('sidebar-collapsed');
  applySidebarCollapsed(next, true);
  deps.lsSet(LS_SIDEBAR_COLLAPSED, next ? 1 : 0);
}

// _sidebarAutoCollapsed tracks whether the CURRENT collapse was applied by
// collapseSidebarForDrawer (drawer-driven) rather than the user's own toggle.
// Only a drawer-driven collapse is undone by restoreSidebarAfterDrawer when
// the last drawer closes; a user-chosen collapse (persisted preference, or a
// manual `[` while a drawer was open) is left alone.
let _sidebarAutoCollapsed = false;

// collapseSidebarForDrawer auto-collapses the sidebar when a right-side drawer
// (追问 / file preview / code-block preview) opens, freeing horizontal space
// for the drawer. Intentionally does NOT persist to localStorage — this is a
// transient, context-driven collapse, so a later cold-load still honors the
// user's own toggle preference rather than this side effect. moveFocus=false
// since focus belongs to the drawer being opened, not the toggle handle.
// Mobile viewport is skipped (sidebar is a modal drawer there, not a column).
function collapseSidebarForDrawer() {
  if (isMobileViewport()) return;
  if (document.body.classList.contains('sidebar-collapsed')) return;
  _sidebarAutoCollapsed = true;
  applySidebarCollapsed(true, false);
}

// restoreSidebarAfterDrawer re-expands the sidebar when a right-side drawer
// closes, undoing a collapse that collapseSidebarForDrawer applied. Preview
// and 追问 can be docked at once, so bail while either is still open — only
// the LAST close restores (same deps.nzAnyDrawerOpen guard deps.nzSplitExit uses; the
// close paths strip their open class before calling here). Like the collapse,
// intentionally no localStorage write: the user's persisted preference was
// never touched, the screen just returns to it.
function restoreSidebarAfterDrawer() {
  if (isMobileViewport()) return;
  if (!_sidebarAutoCollapsed) return;
  if (deps.nzAnyDrawerOpen && deps.nzAnyDrawerOpen()) return;
  _sidebarAutoCollapsed = false;
  applySidebarCollapsed(false, false);
}

// initSidebarCollapsed honors the persisted preference on cold-load. Skip on
// mobile so a previously collapsed PC session doesn't black-box the drawer
// when the user pops the dashboard open on a phone (different viewport,
// different mental model). Called from configureMobileNav — it reads an
// injected dep, so it must not run at module-evaluation time (#2558 D4-3).
function initSidebarCollapsed() {
  if (isMobileViewport()) return;
  if (deps.lsGet(LS_SIDEBAR_COLLAPSED, 0)) {
    applySidebarCollapsed(true, false);
  }
}

// Re-apply preference when the viewport crosses the mobile boundary (DevTools,
// tablet rotation, manual resize). On mobile we drop the PC class so the
// drawer rules win; switching back to PC re-applies the saved flag.
if (window.matchMedia) {
  const mql = window.matchMedia('(max-width: 768px)');
  const onMqlChange = (e) => {
    // Crossing the boundary re-derives the sidebar state below (mobile drawer
    // rules / persisted preference), so a pending drawer-driven collapse is
    // moot — disarm it so a later drawer close can't replay a stale restore.
    _sidebarAutoCollapsed = false;
    if (e.matches) {
      document.body.classList.remove('sidebar-collapsed');
    } else {
      applySidebarCollapsed(!!deps.lsGet(LS_SIDEBAR_COLLAPSED, 0), false);
    }
  };
  if (typeof mql.addEventListener === 'function') {
    mql.addEventListener('change', onMqlChange);
  } else if (typeof mql.addListener === 'function') {
    mql.addListener(onMqlChange); // Safari ≤13 fallback
  }
}

document.addEventListener('keydown', function(e) {
  // `[` toggles collapse on PC. Skip when typing into an input/textarea/
  // contenteditable, when any modifier is held, while an IME composition is
  // active (CJK input fires `[` for fullwidth bracket), or while a modal/
  // palette is open.
  if (e.key !== '[') return;
  if (e.isComposing) return;
  if (e.ctrlKey || e.metaKey || e.altKey || e.shiftKey) return;
  const tgt = e.target;
  if (tgt && (tgt.tagName === 'INPUT' || tgt.tagName === 'TEXTAREA' || tgt.isContentEditable)) return;
  if (document.querySelector('.modal-overlay, .cmd-palette-overlay')) return;
  if (isMobileViewport()) return;
  e.preventDefault();
  toggleSidebarCollapsed();
});


export {
  collapseSidebarForDrawer,
  initMobile,
  initSwipeBack,
  initSwipeDelete,
  initViewportTracking,
  isMobile,
  mobileBack,
  mobileEnterChat,
  mobileShowList,
  restoreSidebarAfterDrawer,
  toggleSidebarCollapsed,
};

// nz.test surface for the Playwright specs (#2557 PR-E3 pattern).
Object.assign(nzTest, { isMobile, mobileEnterChat, mobileShowList, toggleSidebarCollapsed });
