// msg_nav.js — extracted from dashboard.js (#2558 D4).
//
// Verbatim region move: `git diff --color-moved` shows the body as a pure
// move; the import block and the export block below are the only additions.
//
// Layering (D4-1 rule): a module dashboard imports must NOT import dashboard
// back — that cycle puts dashboard's own top-level consts in TDZ while this
// module evaluates. Shared state is read from the state.js objects; dashboard's
// selectSession is reached through shell.js.
import { selection, sessionList } from './state.js';
import { esc } from './nz_util.js';
import { shell } from './shell.js';
import { handleFiles } from './composer_files.js';

// --- Message navigation ---
let navPopoverCloseHandler = null;
// navState is the navigation position, exported for the e2e suite
// (test/e2e/e2e-shim.js); this module is its only writer.
export const navState = {
  userEls: [],
  // #1772: synchronous "is the nav popover mounted" flag. Set true the moment
  // the popover is appended, false when dismissed. Lets the per-scroll-tick
  // handler skip a getElementById on the common (no-popover) path without the
  // race of reading navPopoverCloseHandler, which is only assigned in a
  // deferred setTimeout(0) after mount.
  popoverOpen: false,
  idx: -1, // -1 = not navigating
};

function navRebuild() {
  navState.idx = -1;
  navSync();
}

// navSync re-reads the user messages after events were added in place,
// keeping the position while it still exists. The list and position are
// this module's state: other modules call this rather than writing them.
function navSync() {
  navState.userEls = [...document.querySelectorAll('#events-scroll .event.user')];
  if (navState.idx >= navState.userEls.length) navState.idx = -1;
  navUpdatePill();
}

// Infer which user message is "at" the current scroll position. Returns the
// index of the last user message whose top edge sits at or above the viewport
// center; falls back to the first message below when the viewport is above
// every user message, or -1 when there are none.
function navCurrentIdxFromScroll() {
  const scroller = document.getElementById('events-scroll');
  if (!scroller || navState.userEls.length === 0) return -1;
  const anchor = scroller.getBoundingClientRect().top + scroller.clientHeight * 0.3;
  let lastAbove = -1;
  for (let i = 0; i < navState.userEls.length; i++) {
    const top = navState.userEls[i].getBoundingClientRect().top;
    if (top <= anchor) lastAbove = i;
    else break;
  }
  return lastAbove;
}

function navMsg(dir) {
  if (navState.userEls.length === 0) return;
  // Shell-history 语义：第一次按方向键只定位到「视图锚点」消息本身
  // （prev → 最近一条用户消息；next → 视图内第一条用户消息），
  // 不额外再走一步。只有已在导航中（navState.idx >= 0）时才做 ±1 步进。
  const firstPress = navState.idx < 0;
  if (firstPress) navState.idx = navCurrentIdxFromScroll();
  let target;
  if (dir === 'prev') {
    target = firstPress
      ? (navState.idx < 0 ? navState.userEls.length - 1 : navState.idx)
      : Math.max(0, navState.idx - 1);
  } else {
    target = firstPress
      ? (navState.idx < 0 ? 0 : navState.idx)
      : Math.min(navState.userEls.length - 1, navState.idx + 1);
  }
  if (!firstPress && target === navState.idx) {
    // Already at the edge — flash the current one so the user sees the no-op.
    const cur = navState.userEls[navState.idx];
    if (cur) {
      cur.classList.add('nav-highlight');
      setTimeout(() => cur.classList.remove('nav-highlight'), 600);
    }
    return;
  }
  navState.idx = target;
  const el = navState.userEls[navState.idx];
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
  if (navState.userEls.length < 2) {
    pill.classList.remove('visible');
    return;
  }
  pill.classList.add('visible');
  if (navState.idx < 0) {
    counter.textContent = navState.userEls.length;
  } else {
    counter.textContent = (navState.idx + 1) + '/' + navState.userEls.length;
  }
}

function navDismissPopover() {
  const pop = document.getElementById('nav-list-popover');
  if (pop) pop.remove();
  navState.popoverOpen = false;
  if (navPopoverCloseHandler) {
    document.removeEventListener('click', navPopoverCloseHandler);
    navPopoverCloseHandler = null;
  }
}

function navShowList() {
  if (navState.userEls.length === 0) return;
  let existing = document.getElementById('nav-list-popover');
  if (existing) { navDismissPopover(); return; } // toggle off
  const items = navState.userEls.map((el, i) => {
    const txt = (el.querySelector('.event-content')?.textContent || '').trim();
    const summary = txt.length > 50 ? txt.slice(0, 50) + '...' : txt;
    const active = i === navState.idx ? ' class="nz-accent-strong"' : '';
    return '<div class="nav-list-item" data-idx="' + i + '"' + active + '>' +
      '<span class="nz-faint-lead">' + (i+1) + '.</span>' + esc(summary) + '</div>';
  });
  const pill = document.getElementById('nav-pill');
  const popover = document.createElement('div');
  popover.id = 'nav-list-popover';
  const maxW = Math.min(280, (document.getElementById('main')?.offsetWidth || 280) - 70);
  popover.style.cssText = 'position:absolute;right:44px;bottom:0;width:' + maxW + 'px;max-height:300px;overflow-y:auto;background:var(--nz-overlay-pill-bg);backdrop-filter:blur(8px);border:1px solid var(--nz-border);border-radius:10px;padding:6px 0;z-index:11;font-size:13px;scrollbar-width:thin;scrollbar-color:var(--nz-border) transparent';
  popover.innerHTML = items.join('');
  pill.appendChild(popover);
  navState.popoverOpen = true;
  popover.querySelectorAll('.nav-list-item').forEach(item => {
    item.style.cssText += 'padding:8px 12px;cursor:pointer;color:var(--nz-text);transition:background .1s;border-bottom:1px solid var(--nz-bg-2);overflow:hidden;text-overflow:ellipsis;white-space:nowrap';
    item.onmouseenter = () => item.style.background = 'var(--nz-hover-bg)';
    item.onmouseleave = () => item.style.background = '';
    item.onclick = () => {
      navState.idx = parseInt(item.dataset.idx);
      const el = navState.userEls[navState.idx];
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
      if (navState.popoverOpen) navDismissPopover();
      if (scrollResetTimer) clearTimeout(scrollResetTimer);
      scrollResetTimer = setTimeout(() => {
        if (navState.idx < 0 || !navState.userEls[navState.idx]) return;
        const scrollerRect = el.getBoundingClientRect();
        const targetRect = navState.userEls[navState.idx].getBoundingClientRect();
        const targetCenter = targetRect.top + targetRect.height / 2;
        const viewportCenter = scrollerRect.top + scrollerRect.height / 2;
        if (Math.abs(targetCenter - viewportCenter) > scrollerRect.height / 2) {
          navState.idx = -1;
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
//      another app) are routed to handleFiles so they land in pendingFiles and
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
    handleFiles(imageFiles);
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

// Keyboard shortcut: Alt+Up/Down for message nav (Alt+N lives in auth_modal.js).
document.addEventListener('keydown', function(e) {
  if (e.altKey && e.key === 'ArrowUp') { e.preventDefault(); navMsg('prev'); }
  if (e.altKey && e.key === 'ArrowDown') { e.preventDefault(); navMsg('next'); }
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
      shell.selectSession(s.key, s.node || 'local');
    }
    return;
  }

  // Cmd+Up/Down: prev/next session in group
  if (e.key === 'ArrowUp' || e.key === 'ArrowDown') {
    e.preventDefault();
    const group = currentProjectSessions();
    if (group.length === 0) return;
    const idx = group.findIndex(s => s.key === selection.key && (s.node || 'local') === selection.node);
    let next;
    if (idx < 0) {
      next = 0;
    } else {
      next = e.key === 'ArrowUp' ? idx - 1 : idx + 1;
      if (next < 0) next = group.length - 1;
      if (next >= group.length) next = 0;
    }
    const s = group[next];
    shell.selectSession(s.key, s.node || 'local');
    return;
  }
});

// Get sessions in the same project group as the current selection (sidebar order).
// Fallback groups are workspace-basename pseudo-projects, so two sessions
// sharing the same project name but different workspaces belong to different
// groups — include workspace in the match to mirror the sidebar's grouping.
function currentProjectSessions() {
  if (!sessionList.allSessionsCache || sessionList.allSessionsCache.length === 0) return [];
  const cur = sessionList.allSessionsCache.find(s => s.key === selection.key && (s.node || 'local') === selection.node);
  if (!cur) return [];
  const proj = cur.project || '';
  const isFallback = !!cur.project_fallback;
  const ws = cur.workspace || '';
  return sessionList.allSessionsCache.filter(s => {
    if ((s.project || '') !== proj) return false;
    if (isFallback || s.project_fallback) {
      return !!s.project_fallback === isFallback && (s.workspace || '') === ws;
    }
    return true;
  });
}


export {
  navDismissPopover,
  navMsg,
  navRebuild,
  navShowList,
  navSync,
  navUpdatePill,
};
