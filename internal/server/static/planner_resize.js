// planner_resize.js — the width of the Planner page's left column (等你 ·
// 任务): dragged on the separator between it and the conversation, or moved
// with the arrow keys on it (Home / End to the bounds), kept in localStorage;
// a double click goes back to the default. The width is the --pl-left-w of
// #planner-main, so the phone layout (the column full width, the separator
// hidden) overrides it without fighting an inline width.
import { lsGet, lsRemove, lsSet } from './platform.js';

const LS_KEY = 'planner_left_w';
export const PL_LEFT_DEFAULT = 340;
export const PL_LEFT_MIN = 260;
// The column stops at PL_LEFT_MAX or at half the window, whichever is less,
// so the conversation keeps its room.
export const PL_LEFT_MAX = 640;
const KEY_STEP = 16;

const maxWidth = () => Math.max(PL_LEFT_MIN, Math.min(PL_LEFT_MAX, Math.floor(window.innerWidth * 0.5)));
const clamp = (w) => Math.round(Math.min(Math.max(w, PL_LEFT_MIN), maxWidth()));

function apply(main, sep, w) {
  main.style.setProperty('--pl-left-w', w + 'px');
  sep.setAttribute('aria-valuenow', String(w));
  sep.setAttribute('aria-valuemax', String(maxWidth()));
}

export function initPlannerResize() {
  const main = document.getElementById('planner-main');
  const sep = document.getElementById('pl-resizer');
  const left = document.getElementById('pl-left');
  if (!main || !sep || !left) return;
  sep.setAttribute('aria-valuemin', String(PL_LEFT_MIN));
  const saved = Number(lsGet(LS_KEY, 0));
  let width = saved >= PL_LEFT_MIN ? clamp(saved) : PL_LEFT_DEFAULT;
  apply(main, sep, width);
  const set = (w, keep) => {
    width = clamp(w);
    apply(main, sep, width);
    if (keep) lsSet(LS_KEY, width);
  };

  let startX = 0;
  let startW = 0;
  const onMove = (e) => set(startW + e.clientX - startX, false);
  const onUp = (e) => {
    sep.classList.remove('dragging');
    document.body.classList.remove('pl-resizing');
    sep.removeEventListener('pointermove', onMove);
    sep.removeEventListener('pointerup', onUp);
    sep.removeEventListener('pointercancel', onUp);
    if (sep.hasPointerCapture?.(e.pointerId)) sep.releasePointerCapture(e.pointerId);
    lsSet(LS_KEY, width);
  };
  sep.addEventListener('pointerdown', (e) => {
    if (e.button !== 0) return;
    e.preventDefault();
    startX = e.clientX;
    startW = left.getBoundingClientRect().width;
    sep.setPointerCapture?.(e.pointerId);
    sep.classList.add('dragging');
    document.body.classList.add('pl-resizing');
    sep.addEventListener('pointermove', onMove);
    sep.addEventListener('pointerup', onUp);
    sep.addEventListener('pointercancel', onUp);
  });
  sep.addEventListener('dblclick', () => {
    width = PL_LEFT_DEFAULT;
    apply(main, sep, clamp(width));
    lsRemove(LS_KEY);
  });
  sep.addEventListener('keydown', (e) => {
    const to = { ArrowLeft: width - KEY_STEP, ArrowRight: width + KEY_STEP, Home: PL_LEFT_MIN, End: maxWidth() }[e.key];
    if (to === undefined) return;
    e.preventDefault();
    set(to, true);
  });
  // A narrower window pulls a wide column back in; the saved choice stays.
  window.addEventListener('resize', () => apply(main, sep, clamp(width)));
}
