// msg_nav.js — extracted from dashboard.js (#2558 D4).
//
// Verbatim region move: `git diff --color-moved` shows the body as a pure
// move; the import block, the deps table and the export block below are the
// only additions.
//
// Layering (D4-1 rule): a module dashboard imports must NOT import dashboard
// back — that cycle puts dashboard's own top-level consts in TDZ while this
// module evaluates. Dashboard state is read through nz.state; its helpers are
// injected once via configureMsgNav(), called from dashboard's module body.
import { esc, nzState, nzViews, nzTest} from './nz_util.js';

const deps = {
  closeHistoryPopover: null,
  createNewSession: null,
  debouncedFetchSessions: null,
  escCloseVoiceOverlay: null,
  handleFiles: null,
  refreshBanner: null,
  resetTurnState: null,
  selectSession: null,
  sid: null,
};
export function configureMsgNav(impl) {
  for (const k of Object.keys(deps)) {
    if (typeof impl[k] === 'undefined') throw new Error('msg_nav dep missing: ' + k);
    deps[k] = impl[k];
  }
}

// --- Message navigation ---
let navUserEls = [];
let navPopoverCloseHandler = null;
// #1772: synchronous "is the nav popover mounted" flag. Set true the moment the
// popover is appended, false when dismissed. Lets the per-scroll-tick handler
// skip a getElementById on the common (no-popover) path without the race of
// reading navPopoverCloseHandler, which is only assigned in a deferred
// setTimeout(0) after mount.
let navPopoverOpen = false;
let navIdx = -1; // -1 = not navigating

function navRebuild() {
  navUserEls = [...document.querySelectorAll('#events-scroll .event.user')];
  navIdx = -1;
  navUpdatePill();
}

// Infer which user message is "at" the current scroll position. Returns the
// index of the last user message whose top edge sits at or above the viewport
// center; falls back to the first message below when the viewport is above
// every user message, or -1 when there are none.
function navCurrentIdxFromScroll() {
  const scroller = document.getElementById('events-scroll');
  if (!scroller || navUserEls.length === 0) return -1;
  const anchor = scroller.getBoundingClientRect().top + scroller.clientHeight * 0.3;
  let lastAbove = -1;
  for (let i = 0; i < navUserEls.length; i++) {
    const top = navUserEls[i].getBoundingClientRect().top;
    if (top <= anchor) lastAbove = i;
    else break;
  }
  return lastAbove;
}

function navMsg(dir) {
  if (navUserEls.length === 0) return;
  // Shell-history 语义：第一次按方向键只定位到「视图锚点」消息本身
  // （prev → 最近一条用户消息；next → 视图内第一条用户消息），
  // 不额外再走一步。只有已在导航中（navIdx >= 0）时才做 ±1 步进。
  const firstPress = navIdx < 0;
  if (firstPress) navIdx = navCurrentIdxFromScroll();
  let target;
  if (dir === 'prev') {
    target = firstPress
      ? (navIdx < 0 ? navUserEls.length - 1 : navIdx)
      : Math.max(0, navIdx - 1);
  } else {
    target = firstPress
      ? (navIdx < 0 ? 0 : navIdx)
      : Math.min(navUserEls.length - 1, navIdx + 1);
  }
  if (!firstPress && target === navIdx) {
    // Already at the edge — flash the current one so the user sees the no-op.
    const cur = navUserEls[navIdx];
    if (cur) {
      cur.classList.add('nav-highlight');
      setTimeout(() => cur.classList.remove('nav-highlight'), 600);
    }
    return;
  }
  navIdx = target;
  const el = navUserEls[navIdx];
  if (!el) return;
  el.scrollIntoView({ behavior: 'smooth', block: 'center' });
  // highlight flash
  document.querySelectorAll('.event.nav-highlight').forEach(e => e.classList.remove('nav-highlight'));
  el.classList.add('nav-highlight');
  setTimeout(() => el.classList.remove('nav-highlight'), 1200);
  navUpdatePill();
}

function navUpdatePill() {
  const pill = document.getElementById('nav-pill');
  const counter = document.getElementById('nav-counter');
  if (!pill) return;
  if (navUserEls.length < 2) {
    pill.classList.remove('visible');
    return;
  }
  pill.classList.add('visible');
  if (navIdx < 0) {
    counter.textContent = navUserEls.length;
  } else {
    counter.textContent = (navIdx + 1) + '/' + navUserEls.length;
  }
}

function navDismissPopover() {
  const pop = document.getElementById('nav-list-popover');
  if (pop) pop.remove();
  navPopoverOpen = false;
  if (navPopoverCloseHandler) {
    document.removeEventListener('click', navPopoverCloseHandler);
    navPopoverCloseHandler = null;
  }
}

function navShowList() {
  if (navUserEls.length === 0) return;
  let existing = document.getElementById('nav-list-popover');
  if (existing) { navDismissPopover(); return; } // toggle off
  const items = navUserEls.map((el, i) => {
    const txt = (el.querySelector('.event-content')?.textContent || '').trim();
    const summary = txt.length > 50 ? txt.slice(0, 50) + '...' : txt;
    const active = i === navIdx ? ' style="color:var(--nz-accent);font-weight:600"' : '';
    return '<div class="nav-list-item" data-idx="' + i + '"' + active + '>' +
      '<span style="color:var(--nz-text-faint);margin-right:6px">' + (i+1) + '.</span>' + esc(summary) + '</div>';
  });
  const pill = document.getElementById('nav-pill');
  const popover = document.createElement('div');
  popover.id = 'nav-list-popover';
  const maxW = Math.min(280, (document.getElementById('main')?.offsetWidth || 280) - 70);
  popover.style.cssText = 'position:absolute;right:44px;bottom:0;width:' + maxW + 'px;max-height:300px;overflow-y:auto;background:var(--nz-overlay-pill-bg);backdrop-filter:blur(8px);border:1px solid var(--nz-border);border-radius:10px;padding:6px 0;z-index:11;font-size:13px;scrollbar-width:thin;scrollbar-color:var(--nz-border) transparent';
  popover.innerHTML = items.join('');
  pill.appendChild(popover);
  navPopoverOpen = true;
  popover.querySelectorAll('.nav-list-item').forEach(item => {
    item.style.cssText += 'padding:8px 12px;cursor:pointer;color:var(--nz-text);transition:background .1s;border-bottom:1px solid var(--nz-bg-2);overflow:hidden;text-overflow:ellipsis;white-space:nowrap';
    item.onmouseenter = () => item.style.background = 'var(--nz-hover-bg)';
    item.onmouseleave = () => item.style.background = '';
    item.onclick = () => {
      navIdx = parseInt(item.dataset.idx);
      const el = navUserEls[navIdx];
      if (el) {
        el.scrollIntoView({ behavior: 'smooth', block: 'center' });
        document.querySelectorAll('.event.nav-highlight').forEach(e => e.classList.remove('nav-highlight'));
        el.classList.add('nav-highlight');
        setTimeout(() => el.classList.remove('nav-highlight'), 1200);
      }
      navUpdatePill();
      navDismissPopover();
    };
  });
  // Close on outside click
  setTimeout(() => {
    navPopoverCloseHandler = (e) => {
      if (!popover.contains(e.target) && e.target.id !== 'nav-counter') {
        navDismissPopover();
      }
    };
    document.addEventListener('click', navPopoverCloseHandler);
  }, 0);
}

// Reset nav on scroll to bottom
(function() {
  let scrollListenerAttached = false;
  function attachNavScroll() {
    const el = document.getElementById('events-scroll');
    if (!el || scrollListenerAttached) return;
    scrollListenerAttached = true;
    // Debounce after scrolling settles: if the tracked nav target is no
    // longer near the viewport center (i.e. user scrolled manually), drop it
    // so the next arrow-key press re-seeds from what the user actually sees.
    let scrollResetTimer = null;
    el.addEventListener('scroll', () => {
      // #1772: only touch the DOM to dismiss the nav popover when one is
      // actually open, skipping a per-scroll-tick getElementById on the
      // overwhelmingly common path (no popover) during inertial scrolling.
      if (navPopoverOpen) navDismissPopover();
      if (scrollResetTimer) clearTimeout(scrollResetTimer);
      scrollResetTimer = setTimeout(() => {
        if (navIdx < 0 || !navUserEls[navIdx]) return;
        const scrollerRect = el.getBoundingClientRect();
        const targetRect = navUserEls[navIdx].getBoundingClientRect();
        const targetCenter = targetRect.top + targetRect.height / 2;
        const viewportCenter = scrollerRect.top + scrollerRect.height / 2;
        if (Math.abs(targetCenter - viewportCenter) > scrollerRect.height / 2) {
          navIdx = -1;
          navUpdatePill();
        }
      }, 300);
    }, { passive: true });
  }
  // Re-attach after renderMainShell rebuilds the DOM
  const obs = new MutationObserver(() => {
    scrollListenerAttached = false;
    attachNavScroll();
  });
  obs.observe(document.getElementById('main') || document.body, { childList: true, subtree: false });
  attachNavScroll();
})();

// Paste handler for #msg-input:
//   1. Image files on the clipboard (screenshot Cmd/Ctrl+V, "copy image" from
//      another app) are routed to deps.handleFiles so they land in pendingFiles and
//      ride the same upload / file_ids path as the paperclip button. Without
//      this branch the browser's default paste embeds the image as
//      `<img src="data:...">` inside the contenteditable — `innerText.trim()`
//      drops it silently so the send ends up carrying neither text nor
//      file_ids, and Claude never sees the image the user thought they sent.
//   2. Plain text is forced in via execCommand('insertText') so rich
//      formatting from Word / web pages doesn't leak into the contenteditable.
document.addEventListener('paste', function(e) {
  const t = e.target;
  if (!t || !t.closest || !t.closest('#msg-input')) return;
  const cd = e.clipboardData || window.clipboardData;
  if (!cd) return;

  // Image branch: walk clipboardData.files first (most reliable on Chromium
  // + Safari), fall back to clipboardData.items for older paths. Any image
  // file short-circuits the default paste so the browser doesn't also embed
  // a stray `<img>` into the contenteditable.
  const imageFiles = [];
  if (cd.files && cd.files.length) {
    for (const f of cd.files) {
      if (f && f.type && f.type.startsWith('image/')) imageFiles.push(f);
    }
  }
  if (imageFiles.length === 0 && cd.items) {
    for (const it of cd.items) {
      if (it && it.kind === 'file' && it.type && it.type.startsWith('image/')) {
        const f = it.getAsFile();
        if (f) imageFiles.push(f);
      }
    }
  }
  if (imageFiles.length > 0) {
    e.preventDefault();
    deps.handleFiles(imageFiles);
    return;
  }

  const text = cd.getData('text/plain');
  if (!text) return;
  e.preventDefault();
  if (document.queryCommandSupported && document.queryCommandSupported('insertText')) {
    document.execCommand('insertText', false, text);
    return;
  }
  const sel = window.getSelection();
  if (!sel || sel.rangeCount === 0) return;
  const range = sel.getRangeAt(0);
  range.deleteContents();
  const node = document.createTextNode(text);
  range.insertNode(node);
  range.setStartAfter(node);
  range.setEndAfter(node);
  sel.removeAllRanges();
  sel.addRange(range);
});

// Keyboard shortcut: Alt+Up/Down for message nav, Alt+N for new session.
// Cmd/Ctrl+N is left alone so the browser's "new window" still works.
document.addEventListener('keydown', function(e) {
  if (e.altKey && e.key === 'ArrowUp') { e.preventDefault(); navMsg('prev'); }
  if (e.altKey && e.key === 'ArrowDown') { e.preventDefault(); navMsg('next'); }
  if (e.altKey && (e.key === 'n' || e.key === 'N')) {
    const tag = (e.target.tagName || '').toLowerCase();
    if (tag === 'input' || tag === 'textarea' || e.target.isContentEditable) return;
    e.preventDefault();
    deps.createNewSession();
  }
});

// Global Esc: close open popovers (history / nav list) when no modal/input has focus.
document.addEventListener('keydown', function(e) {
  if (e.key !== 'Escape') return;
  // Overlays with their own Esc trapFocus handling take precedence.
  if (document.querySelector('.modal-overlay, .cmd-palette-overlay')) return;
  const tag = (e.target.tagName || '').toLowerCase();
  if (tag === 'input' || tag === 'textarea' || e.target.isContentEditable) return;
  let closed = false;
  // voice-overlay (R20260610-UI-3): the recording overlay had no Esc handler,
  // so a stuck recording could only be dismissed by clicking it. Mirror the
  // click escape-hatch (see #voice-overlay click listener) on Esc for parity
  // with every other overlay.
  if (deps.escCloseVoiceOverlay()) closed = true;
  if (nzState.activePopover) { deps.closeHistoryPopover(); closed = true; }
  if (document.getElementById('nav-list-popover')) { navDismissPopover(); closed = true; }
  // §16 inline-expand 回归 + cron-panel-consolidation RFC §6.4: Esc 关 cron 的
  // 行内展开 / drawer。优先级（行展开先于 drawer）与关闭逻辑都收在 cron_view.js
  // 的 cronEscClose 里，dashboard.js 仅经委托——绝不跨脚本裸引用 cron 内部状态
  // （cronExpandedRunId / cronDetailJobId），否则 cron_view.js 未加载时这里会抛
  // `cronExpandedRunId is not defined`（dashboard-cron-view-extraction §2.6 B1）。
  // nz.views.cron 缺席（cron_view.js 没加载）时优雅降级，不影响其它 Esc 分支。
  // 独立 if（非 else if）：忠实保留迁移前语义——cron 分支独立于上方 popover 分支，
  // 即便同一次 Esc 已关掉 history/nav-list popover，仍会继续关 cron 展开/drawer。
  if (nzViews.cron && nzViews.cron.escClose()) { closed = true; }
  if (closed) e.preventDefault();
});

// §16 inline-expand 回归: ↑↓ 切上一条 / 下一条 run 的全局快捷键已随 cron 状态一并
// 迁入 cron_view.js（B1 修复）——handler 与它读的 cronExpandedRunId / navigateExpandedRun
// 同处一个 <script>，绑定必然就绪；cron_view.js 缺席则该快捷键自然不注册，不再
// 拖垮 dashboard.js。Cmd/Ctrl+Up/Down 的会话切换仍在下方（有 metaKey 守卫，错开）。

// Keyboard shortcut: Cmd/Ctrl+1..9 — switch to Nth session in current project group
// Cmd/Ctrl+Up/Down — prev/next session in group
document.addEventListener('keydown', function(e) {
  // Skip when typing in input fields
  const tag = (e.target.tagName || '').toLowerCase();
  if (tag === 'input' || tag === 'textarea' || e.target.isContentEditable) return;

  const isMeta = e.metaKey || e.ctrlKey;
  if (!isMeta) return;

  // Cmd+1..9: jump to Nth session in group
  const digit = parseInt(e.key);
  if (digit >= 1 && digit <= 9) {
    e.preventDefault();
    const group = currentProjectSessions();
    if (digit <= group.length) {
      const s = group[digit - 1];
      deps.selectSession(s.key, s.node || 'local');
    }
    return;
  }

  // Cmd+Up/Down: prev/next session in group
  if (e.key === 'ArrowUp' || e.key === 'ArrowDown') {
    e.preventDefault();
    const group = currentProjectSessions();
    if (group.length === 0) return;
    const idx = group.findIndex(s => s.key === nzState.selectedKey && (s.node || 'local') === nzState.selectedNode);
    let next;
    if (idx < 0) {
      next = 0;
    } else {
      next = e.key === 'ArrowUp' ? idx - 1 : idx + 1;
      if (next < 0) next = group.length - 1;
      if (next >= group.length) next = 0;
    }
    const s = group[next];
    deps.selectSession(s.key, s.node || 'local');
    return;
  }
});

// Get sessions in the same project group as the current selection (sidebar order).
// Fallback groups are workspace-basename pseudo-projects, so two sessions
// sharing the same project name but different workspaces belong to different
// groups — include workspace in the match to mirror the sidebar's grouping.
function currentProjectSessions() {
  if (!nzState.allSessionsCache || nzState.allSessionsCache.length === 0) return [];
  const cur = nzState.allSessionsCache.find(s => s.key === nzState.selectedKey && (s.node || 'local') === nzState.selectedNode);
  if (!cur) return [];
  const proj = cur.project || '';
  const isFallback = !!cur.project_fallback;
  const ws = cur.workspace || '';
  return nzState.allSessionsCache.filter(s => {
    if ((s.project || '') !== proj) return false;
    if (isFallback || s.project_fallback) {
      return !!s.project_fallback === isFallback && (s.workspace || '') === ws;
    }
    return true;
  });
}

// Turn watchdog: while the selected session is "running", periodically pull
// the authoritative REST snapshot so the banner self-heals if a terminal WS
// signal (the 'result' event and/or the 'ready' session_state broadcast) is
// dropped on a still-open connection. Without this the "处理中..." banner stays
// stuck until the operator switches sessions or reconnects — the bug this fixes.
// fetchSessions reconciles via updateMainState (see the relaxed gate in
// fetchSessions); the watchdog just supplies the missing tick, since the
// session poll is stopped while WS is connected.
let _turnWatchdogTimer = null;
const TURN_WATCHDOG_INTERVAL_MS = 15000;
function startTurnWatchdog() {
  if (_turnWatchdogTimer) return;
  _turnWatchdogTimer = setInterval(() => {
    // Self-heal: if the selected session was cleared without routing through
    // updateSendButton (dismissSession nulls nzState.selectedKey + swaps to the empty
    // shell in three branches), the fetchSessions reconcile is gated on
    // `if (nzState.selectedKey)` and would never stop us — so retire the watchdog here
    // instead of polling /api/sessions forever for the page lifetime.
    if (!nzState.selectedKey) { stopTurnWatchdog(); return; }
    deps.debouncedFetchSessions();
  }, TURN_WATCHDOG_INTERVAL_MS);
}
function stopTurnWatchdog() {
  if (_turnWatchdogTimer) { clearInterval(_turnWatchdogTimer); _turnWatchdogTimer = null; }
}

function updateSendButton(state) {
  if (nzState.selectedKey) nzState._lastAppliedMainState = { key: deps.sid(nzState.selectedKey, nzState.selectedNode), state: state };
  const banner = document.getElementById('running-banner');
  const sendBtn = document.getElementById('btn-send');
  const stopBtn = document.getElementById('btn-stop');
  const inVoiceMode = document.getElementById('input-area')?.classList.contains('voice-mode');
  if (state === 'running') {
    if (banner) banner.style.display = '';
    if (sendBtn) sendBtn.style.display = 'none';
    if (stopBtn) stopBtn.style.display = 'flex';
    if (nzViews.agent) nzViews.agent.initFromSession();
    deps.refreshBanner();
    startTurnWatchdog();
  } else {
    stopTurnWatchdog();
    // deps.resetTurnState → deps.refreshBanner will hide the banner since the session
    // is no longer "running". If background agents are still active (e.g.
    // zero-downtime restart), deps.refreshBanner keeps the banner visible.
    if (sendBtn) sendBtn.style.display = inVoiceMode ? 'none' : 'flex';
    if (stopBtn) stopBtn.style.display = 'none';
    deps.resetTurnState();
    // Replace stale loading indicator if session stopped before events arrived.
    const evEl2 = document.getElementById('events-scroll');
    const loadingEl = evEl2 && evEl2.querySelector('.loading-indicator');
    if (loadingEl) loadingEl.innerHTML = '暂无事件';
  }
  // Banner show/hide changes .events height — keep latest message visible.
  // Only auto-scroll if the user is already near the bottom; otherwise
  // respect their scroll position (e.g. reading history).
  const evEl = document.getElementById('events-scroll');
  if (evEl && evEl.scrollTop + evEl.clientHeight >= evEl.scrollHeight - 50) {
    evEl.scrollTop = evEl.scrollHeight;
  }
}


export {
  navDismissPopover,
  navIdx,
  navMsg,
  navPopoverOpen,
  navRebuild,
  navShowList,
  navUpdatePill,
  navUserEls,
  updateSendButton,
};

// nz.test surface for the Playwright specs (#2557 PR-E3 pattern) — the
// bindings live here now, so only this module can offer working setters.
Object.defineProperties(nzTest, {
  navIdx: { get: function () { return navIdx; }, set: function (v) { navIdx = v; }, configurable: true },
  navUserEls: { get: function () { return navUserEls; }, set: function (v) { navUserEls = v; }, configurable: true },
  navPopoverOpen: { get: function () { return nzState.navPopoverOpen; }, set: function (v) { nzState.navPopoverOpen = v; }, configurable: true },
});
