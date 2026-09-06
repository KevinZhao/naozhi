// split_view.js — extracted from dashboard.js (#2558 D4).
//
// Verbatim region move: `git diff --color-moved` shows the body as a pure
// move; the import block, the deps table and the export block below are the
// only additions.
//
// Layering (D4-1 rule): a module dashboard imports must NOT import dashboard
// back — that cycle puts dashboard's own top-level consts in TDZ while this
// module evaluates. Dashboard state is read through nz.state; its helpers are
// injected once via configureSplitView(), called from dashboard's module body.

const deps = {
  lsGet: null,
  lsRemove: null,
  lsSet: null,
  stickEventsBottom: null,
};
let initSplitWidth = null;

export function configureSplitView(impl) {
  for (const k of Object.keys(deps)) {
    if (typeof impl[k] === 'undefined') throw new Error('split_view dep missing: ' + k);
    deps[k] = impl[k];
  }
  // Deps are live now — run the load-time bootstrap that needs them.
  if (initSplitWidth) initSplitWidth();
}

// Late-bound hooks: assigned by the code below, read by other modules at event
// time (never at load time) — the shape they had as dashboard module-scope
// lets before this extraction.
let nzAnyDrawerOpen = null;
let nzSplitBringToFront = null;
let nzSplitEnter = null;
let nzSplitExit = null;

/* ===== Split-view docking (desktop only) =====
   The preview (#fv-drawer) and 追问 (#aside-drawer) panes used to overlay the
   transcript as a fixed slide-over. On desktop we now dock them as a right-hand
   split: body.nz-split-open reserves `--nz-split-w` on .container so the
   transcript compresses into the remaining width and stays fully visible
   beside the pane. nzSplitEnter/nzSplitExit are called from the drawer
   open/close paths (openFilePreview / closeFilePreview / scratch showDrawer /
   hideDrawer). Phone (≤768px) keeps the original full-width overlay — every
   path here bails on a mobile viewport, and the CSS overrides are gated behind
   the ≥769px breakpoint, so the two layouts never fight.

   The "keep the transcript's latest progress visible" requirement is handled
   by capturing whether the events pane was bottom-anchored BEFORE the width
   change, then re-pinning to the bottom AFTER layout settles — narrowing the
   transcript reflows text taller, which would otherwise leave the newest
   bubbles scrolled off-screen. */
(function(){
  const LS_SPLIT_W = 'split_w';
  const MIN_W = 320;
  // Reserve at least this much for the activity-bar + sidebar + transcript so
  // the split can never swallow the whole window.
  const MIN_LEFT = 420;
  const resizer = document.getElementById('split-resizer');

  function isMobileVp() {
    return window.matchMedia && window.matchMedia('(max-width: 768px)').matches;
  }
  // The pane defaults to HALF the dashboard width on PC. clampW still caps it
  // so a tiny window can't drop the transcript below MIN_LEFT — on a narrow
  // desktop the "half" yields to the MIN_LEFT floor.
  function splitDefaultW() {
    return Math.round(window.innerWidth / 2);
  }
  // hasCustomW: the user has dragged the seam, so we stop auto-tracking the
  // half-width on resize and honour their saved value instead. Seeded from
  // whether a width was ever persisted.
  // Seeded by initSplitWidth() once the deps are wired (#2558 D4-4): reading
  // localStorage here would run at module-evaluation time, before
  // configureSplitView.
  let hasCustomW = false;
  function clampW(w) {
    const max = Math.max(MIN_W, window.innerWidth - MIN_LEFT);
    return Math.min(Math.max(w, MIN_W), max);
  }
  function applyW(w) {
    document.documentElement.style.setProperty('--nz-split-w', clampW(w) + 'px');
  }
  // Cold-load: a persisted custom width wins; otherwise default to half.
  // Called from configureSplitView — see hasCustomW above.
  initSplitWidth = function () {
    const savedW = parseFloat(deps.lsGet(LS_SPLIT_W, 0));
    hasCustomW = savedW >= MIN_W;
    applyW(savedW >= MIN_W ? savedW : splitDefaultW());
  };

  function anyDrawerOpen() {
    const fv = document.getElementById('fv-drawer');
    const ad = document.getElementById('aside-drawer');
    return (fv && fv.classList.contains('fv-open')) ||
           (ad && ad.classList.contains('visible'));
  }
  // Exported for restoreSidebarAfterDrawer — keeps the "which drawers exist"
  // knowledge in one place so adding a third drawer can't drift between the
  // split-exit guard and the sidebar-restore guard.
  nzAnyDrawerOpen = anyDrawerOpen;
  // Was the transcript scrolled to (or near) the bottom? 40px slack mirrors
  // the main-window wasBottom checks elsewhere in this file.
  function eventsAtBottom() {
    const el = document.getElementById('events-scroll');
    if (!el) return false;
    return el.scrollTop + el.clientHeight >= el.scrollHeight - 40;
  }
  // Re-pin to the bottom after a width change, but only if the user was
  // already there — never drag them away from history they're reading.
  function preserveBottom(wasBottom) {
    if (!wasBottom) return;
    requestAnimationFrame(() => {
      deps.stickEventsBottom();
    });
  }

  nzSplitEnter = function() {
    if (isMobileVp()) return;  // phone keeps the full-width overlay
    if (document.body.classList.contains('nz-split-open')) return;
    const wasBottom = eventsAtBottom();
    // Refresh to half-width on open unless the user set a custom width — the
    // window may have been resized while the split was closed, and the
    // "half the dashboard" default should reflect the current viewport.
    if (!hasCustomW) applyW(splitDefaultW());
    document.body.classList.add('nz-split-open');
    preserveBottom(wasBottom);
  };
  nzSplitExit = function() {
    // Keep the split open while either drawer is still docked (mutually
    // exclusive today, but cheap to be correct if that ever changes).
    if (anyDrawerOpen()) return;
    if (!document.body.classList.contains('nz-split-open')) return;
    const wasBottom = eventsAtBottom();
    document.body.classList.remove('nz-split-open');
    preserveBottom(wasBottom);
  };
  // Preview and 追问 can be open at once and share the right strip. Whichever
  // was opened LAST should stack on top. Stamp .nz-split-front on the given
  // drawer and strip it from the other so exactly one pane is ever in front.
  // Called from both open paths (openFilePreview / scratch showDrawer).
  nzSplitBringToFront = function(which) {
    const ids = { preview: 'fv-drawer', scratch: 'aside-drawer' };
    const frontId = ids[which];
    if (!frontId) return;
    Object.values(ids).forEach(id => {
      const el = document.getElementById(id);
      if (el) el.classList.toggle('nz-split-front', id === frontId);
    });
  };

  // --- Drag-to-resize the seam ---
  if (resizer) {
    let startX, startW, dragBottom;
    function onMove(e) {
      // Strip grows when dragged left (toward smaller clientX). Width is the
      // distance from the seam to the right viewport edge.
      const w = clampW(startW + (startX - e.clientX));
      applyW(w);
      // Keep the transcript pinned to the bottom live during the drag so the
      // latest progress doesn't scroll out from under a width reflow.
      if (dragBottom) {
        const el = document.getElementById('events-scroll');
        if (el) el.scrollTop = el.scrollHeight;
      }
    }
    function onUp() {
      resizer.classList.remove('dragging');
      document.body.style.cursor = '';
      document.body.style.userSelect = '';
      document.removeEventListener('mousemove', onMove);
      document.removeEventListener('mouseup', onUp);
      const cur = parseFloat(getComputedStyle(document.documentElement).getPropertyValue('--nz-split-w'));
      // A manual drag opts out of half-width auto-tracking and persists the
      // chosen width.
      if (cur >= MIN_W) { deps.lsSet(LS_SPLIT_W, Math.round(cur)); hasCustomW = true; }
    }
    resizer.addEventListener('mousedown', function(e) {
      e.preventDefault();
      startX = e.clientX;
      startW = parseFloat(getComputedStyle(document.documentElement).getPropertyValue('--nz-split-w')) || splitDefaultW();
      dragBottom = eventsAtBottom();
      resizer.classList.add('dragging');
      document.body.style.cursor = 'col-resize';
      document.body.style.userSelect = 'none';
      document.addEventListener('mousemove', onMove);
      document.addEventListener('mouseup', onUp);
    });
    // Double-click resets to the half-width default and re-enables auto-tracking.
    resizer.addEventListener('dblclick', function() {
      const wasBottom = eventsAtBottom();
      applyW(splitDefaultW());
      deps.lsRemove(LS_SPLIT_W);
      hasCustomW = false;
      preserveBottom(wasBottom);
    });
  }

  // Keep the pane at half-width (or a re-clamped custom width) as the viewport
  // changes. Without a custom width the strip TRACKS half the window so the
  // "half the dashboard" contract holds across resizes; with a custom width we
  // only re-clamp so narrowing can't push padding-right past the viewport and
  // crush the transcript. Only runs while the split is open (else we'd clobber
  // a value before it's shown). No-op on mobile (split never open there).
  window.addEventListener('resize', function() {
    if (!document.body.classList.contains('nz-split-open')) return;
    const cur = parseFloat(getComputedStyle(document.documentElement).getPropertyValue('--nz-split-w')) || splitDefaultW();
    const target = hasCustomW ? clampW(cur) : clampW(splitDefaultW());
    if (target === cur) return;
    const wasBottom = eventsAtBottom();
    applyW(target);
    preserveBottom(wasBottom);
  });
})();


export {
  nzAnyDrawerOpen,
  nzSplitBringToFront,
  nzSplitEnter,
  nzSplitExit,
};
