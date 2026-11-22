// scroll_follow.js — a transcript pane follows its newest content until its
// reader moves up, and again once they are back at the bottom.
//
// Following is a state of the pane, not a measurement taken as each event
// arrives: the running banner growing, a side note, an image or a formula
// rendering all move the bottom away between two events, and a pane measured
// then reads as left behind and never follows again.

// NEAR is how far above the bottom (px) still counts as at the bottom.
const NEAR = 30;

const panes = new WeakMap();

const gap = (el) => el.scrollHeight - el.scrollTop - el.clientHeight;

function record(el, p) {
  p.top = el.scrollTop;
  p.height = el.scrollHeight;
  p.view = el.clientHeight;
}

// note judges a scroll. At the bottom the pane follows; a move up is the
// reader's unless the content shrinking or the pane resizing accounts for it
// (both clamp scrollTop without anyone scrolling).
function note(el, p) {
  if (!el.clientHeight) return;
  const up = p.top - el.scrollTop;
  if (gap(el) <= NEAR) p.on = true;
  else if (up > 1 && el.clientHeight === p.view && up > p.height - el.scrollHeight + 1) p.on = false;
  record(el, p);
}

function settle(el, p) {
  if (!p.on || !el.clientHeight) return;
  el.scrollTop = el.scrollHeight;
  record(el, p);
}

// opened: a click that unfolds something below the reader (a tool call, a long
// reply) leaves them reading it, so the pane stays where the click left it.
// Its own mutations are not followed meanwhile; an arriving event still is.
function opened(el, p) {
  if (!p.on || p.busy) return;
  p.busy = true;
  setTimeout(() => {
    p.busy = false;
    if (gap(el) > NEAR) p.on = false;
    else settle(el, p);
  });
}

function pane(el) {
  let p = panes.get(el);
  if (p) return p;
  p = { on: true, busy: false, top: 0, height: 0, view: 0 };
  record(el, p);
  panes.set(el, p);
  el.addEventListener('scroll', () => note(el, p), { passive: true });
  el.addEventListener('click', () => opened(el, p), true);
  // Images report their size only once loaded; load does not bubble.
  el.addEventListener('load', () => settle(el, p), true);
  el.addEventListener('error', () => settle(el, p), true);
  if (typeof ResizeObserver === 'function') {
    // A hidden pane loses its scroll position: view 0 keeps the scroll event
    // of its return from reading as the reader's move.
    new ResizeObserver(() => { if (el.clientHeight) settle(el, p); else p.view = 0; }).observe(el);
  }
  new MutationObserver(() => { if (!p.busy) settle(el, p); })
    .observe(el, { childList: true, subtree: true, characterData: true });
  return p;
}

// followPane puts the pane at its bottom and keeps it there.
export function followPane(el) {
  if (!el) return;
  const p = pane(el);
  p.on = true;
  settle(el, p);
}

// notePane reads where the reader stands before the pane's content changes: a
// scroll whose event is still on its way must not be taken for the change's.
export function notePane(el) {
  if (el) note(el, pane(el));
}

// settlePane brings a following pane to its bottom after its content changed;
// one whose reader moved up stays where it is.
export function settlePane(el) {
  if (el) settle(el, pane(el));
}

// placedPane is for code that just put the reader somewhere (a restored
// position): the pane follows only if that is the bottom.
export function placedPane(el) {
  if (!el) return;
  const p = pane(el);
  p.on = !el.clientHeight || gap(el) <= NEAR;
  record(el, p);
}

// paneFollows reports whether the pane is following.
export function paneFollows(el) {
  return !!el && pane(el).on;
}
