// mem_popover.js — the memory [[wiki-link]] popover: a lazy hover/click
// preview for the [[slug]] markers inlineMd emits. dashboard's module body
// runs initMemPopover(), which adds the document listeners.
// Single popover element (#mem-popover, declared in dashboard.html); event
// delegation watches the whole document so the popover works for messages
// rendered after page load (live WS updates, scratch drawer, agent-view).
//
// Lifecycle:
//   mouseenter span → 300ms debounce → fetch + show (anchored)
//   mouseleave span → 200ms grace → hide if not pinned
//   click span      → fetch + show + pin (sticks until ESC / outside click)
//   ESC / outside click on pinned popover → unpin + hide
//
// Cache: module-level Map<slug, response>. Cleared on full page reload.
// 404 results poison the slug span with .md-memlink-broken so subsequent
// hovers skip the network.
//
// docs/rfc/memory-link-rendering.md
import { NZ_CONTRACT } from './contract.js';
import { authHeaders } from './platform.js';
import { esc, escAttr, fetchJSON } from './nz_util.js';
import { renderMd, runPendingAsync } from './render_md.js';

const memCache = new Map();
// NOT_FOUND: the negative-cache entry, compared by identity.
const NOT_FOUND = Object.freeze({});
// mp: the popover's elements (looked up on first show) and its hover/pin state.
const mp = { pop: null, popContent: null, popClose: null, pinned: false, currentSlug: null, hoverTimer: 0, leaveTimer: 0 };

function ensurePopover() {
  if (mp.pop) return true;
  mp.pop = document.getElementById('mem-popover');
  mp.popContent = document.getElementById('mem-pop-content');
  mp.popClose = document.getElementById('mem-pop-close');
  if (!mp.pop || !mp.popContent) return false;
  if (mp.popClose) {
    mp.popClose.addEventListener('click', (e) => {
      e.stopPropagation();
      hidePopover();
    });
  }
  mp.pop.addEventListener('mouseenter', () => {
    if (mp.leaveTimer) { clearTimeout(mp.leaveTimer); mp.leaveTimer = 0; }
  });
  mp.pop.addEventListener('mouseleave', () => {
    if (!mp.pinned) scheduleHide();
  });
  return true;
}

function showPopover(anchor) {
  if (!ensurePopover()) return;
  mp.pop.classList.add('show');
  mp.pop.setAttribute('aria-hidden', 'false');
  positionPopover(anchor);
}

function hidePopover() {
  if (!mp.pop) return;
  mp.pop.classList.remove('show', 'pinned');
  mp.pop.setAttribute('aria-hidden', 'true');
  mp.pinned = false;
  mp.currentSlug = null;
}

function scheduleHide() {
  if (mp.leaveTimer) clearTimeout(mp.leaveTimer);
  mp.leaveTimer = setTimeout(() => {
    if (!mp.pinned) hidePopover();
  }, 200);
}

function positionPopover(anchor) {
  const rect = anchor.getBoundingClientRect();
  const vw = window.innerWidth;
  const vh = window.innerHeight;
  let top = rect.bottom + 6;
  let left = rect.left;
  const popW = mp.pop.offsetWidth || 400;
  const popH = mp.pop.offsetHeight || 200;
  if (left + popW > vw - 12) left = Math.max(12, vw - popW - 12);
  if (top + popH > vh - 12) {
    const above = rect.top - 6 - popH;
    top = above > 12 ? above : 12;
  }
  mp.pop.style.top = top + 'px';
  mp.pop.style.left = left + 'px';
}

function renderLoading() {
  mp.popContent.innerHTML = '<div class="mem-pop-error">加载中…</div>';
}
function renderError(msg) {
  mp.popContent.innerHTML = '<div class="mem-pop-error">' + esc(msg) + '</div>';
}
function renderResponse(data) {
  if (!data || !data.found) {
    mp.popContent.innerHTML = '<div class="mem-pop-error">未找到该记忆</div>';
    return;
  }
  const parts = [];
  const headerBits = [];
  if (data.type) {
    headerBits.push('<span class="mem-pop-type" data-type="' + escAttr(data.type) + '">' + esc(data.type) + '</span>');
  }
  if (data.scope === 'external' && data.project) {
    const projLabel = String(data.project).split('-').filter(Boolean).pop() || data.project;
    headerBits.push('<span class="mem-pop-scope">来自 ' + esc(projLabel) + ' 项目</span>');
  }
  if (headerBits.length > 0) {
    parts.push('<div class="mem-pop-header">' + headerBits.join('') + '</div>');
  }
  parts.push('<div class="mem-pop-slug">' + esc(data.slug || '') + '</div>');
  if (data.description) {
    parts.push('<div class="mem-pop-desc">' + esc(data.description) + '</div>');
  }
  if (data.body) {
    parts.push('<div class="mem-pop-body">' + renderMd(data.body) + '</div>');
  }
  mp.popContent.innerHTML = parts.join('');
  runPendingAsync();
}

function markBroken(slug) {
  document.querySelectorAll('.md-memlink[data-slug="' + slug.replace(/"/g, '\\"') + '"]')
    .forEach((el) => el.classList.add('md-memlink-broken'));
}

async function fetchMemory(slug) {
  const cached = memCache.get(slug);
  if (cached === NOT_FOUND) return null;
  if (cached) return cached;
  // RNEW-UX-003 (#444): fetchJSON wraps fetch with AbortController +
  // 10s timeout. Memory popovers are click-to-open so a hung backend
  // (NAT idle drop) leaves the user staring at a never-resolving
  // popover; fetchJSON guarantees a deterministic failure path that
  // returns `undefined` (preserves caller's "transient — retry next
  // hover" semantics).
  try {
    const data = await fetchJSON(NZ_CONTRACT.API.memory_slug.replace('{slug}', encodeURIComponent(slug)), {
      headers: authHeaders(),
    });
    if (!data || !data.found) {
      memCache.set(slug, NOT_FOUND);
      markBroken(slug);
      return null;
    }
    memCache.set(slug, data);
    return data;
  } catch (e) {
    // 404/400 — slug missing/invalid; cache the negative so we don't
    // re-fetch on every hover. fetchJSON's err.status surfaces the
    // server status so the callsite can branch.
    if (e && (e.status === 404 || e.status === 400)) {
      memCache.set(slug, NOT_FOUND);
      markBroken(slug);
      return null;
    }
    return undefined;
  }
}

async function loadAndShow(slug, anchor, pin) {
  if (!ensurePopover()) return;
  mp.currentSlug = slug;
  if (pin) {
    mp.pinned = true;
    mp.pop.classList.add('pinned');
  }
  const cached = memCache.get(slug);
  if (cached === NOT_FOUND) {
    renderResponse(null);
  } else if (cached) {
    renderResponse(cached);
  } else {
    renderLoading();
  }
  showPopover(anchor);

  if (cached === NOT_FOUND || cached) return;
  const data = await fetchMemory(slug);
  if (mp.currentSlug !== slug) return;
  if (data === undefined) {
    renderError('加载失败');
    return;
  }
  renderResponse(data);
  positionPopover(anchor);
}

function onEnter(e) {
  const span = e.target.closest && e.target.closest('.md-memlink');
  if (!span) return;
  if (mp.leaveTimer) { clearTimeout(mp.leaveTimer); mp.leaveTimer = 0; }
  if (mp.pinned) return;
  const slug = span.getAttribute('data-slug');
  if (!slug) return;
  if (memCache.get(slug) === NOT_FOUND) return;
  if (mp.hoverTimer) clearTimeout(mp.hoverTimer);
  mp.hoverTimer = setTimeout(() => {
    mp.hoverTimer = 0;
    loadAndShow(slug, span, false);
  }, 300);
}

function onLeave(e) {
  const span = e.target.closest && e.target.closest('.md-memlink');
  if (!span) return;
  if (mp.hoverTimer) { clearTimeout(mp.hoverTimer); mp.hoverTimer = 0; }
  if (!mp.pinned) scheduleHide();
}

function onClick(e) {
  const span = e.target.closest && e.target.closest('.md-memlink');
  if (!span) return;
  e.preventDefault();
  e.stopPropagation();
  const slug = span.getAttribute('data-slug');
  if (!slug) return;
  if (mp.hoverTimer) { clearTimeout(mp.hoverTimer); mp.hoverTimer = 0; }
  loadAndShow(slug, span, true);
}

function onDocClick(e) {
  if (!mp.pop || !mp.pinned) return;
  if (mp.pop.contains(e.target)) return;
  if (e.target.closest && e.target.closest('.md-memlink')) return;
  hidePopover();
}

function onKeyDown(e) {
  if (e.key === 'Escape' && mp.pinned) {
    hidePopover();
    return;
  }
  // role=link contract (WCAG 2.1.1): Enter/Space on a focused chip must
  // activate it. Without this branch, keyboard users could tab to a chip
  // and find no way to read the memory body.
  if (e.key === 'Enter' || e.key === ' ' || e.key === 'Spacebar') {
    const span = e.target && e.target.closest && e.target.closest('.md-memlink');
    if (!span) return;
    e.preventDefault();
    const slug = span.getAttribute('data-slug');
    if (!slug) return;
    if (mp.hoverTimer) { clearTimeout(mp.hoverTimer); mp.hoverTimer = 0; }
    loadAndShow(slug, span, true);
  }
}

// Copy fallback: chip renders only the icon + a short tail label, so a raw
// selection-copy would yield e.g. "💡 vs_practice" — the original
// [[full_slug]] wiki-link is lost, breaking round-trip into other docs / IM
// / markdown editors. We rewrite clipboardData when the active selection
// touches at least one chip, replacing each chip's text with its data-slug
// wrapped in [[]].
function onCopy(e) {
  const sel = document.getSelection && document.getSelection();
  if (!sel || sel.rangeCount === 0 || sel.isCollapsed) return;
  // Cheap pre-check: only intervene if the selection actually crosses a chip.
  let touchesChip = false;
  for (let i = 0; i < sel.rangeCount; i++) {
    const r = sel.getRangeAt(i);
    const c = r.commonAncestorContainer;
    const root = c.nodeType === 1 ? c : c.parentNode;
    if (!root) continue;
    if ((root.closest && root.closest('.md-memlink')) ||
        (root.querySelector && root.querySelector('.md-memlink'))) {
      touchesChip = true;
      break;
    }
  }
  if (!touchesChip) return;
  const parts = [];
  for (let i = 0; i < sel.rangeCount; i++) {
    const frag = sel.getRangeAt(i).cloneContents();
    // Replace each chip element inside the cloned fragment with a text node
    // carrying [[slug]]. cloneContents loses parent context, so we walk
    // the fragment itself.
    const chips = frag.querySelectorAll ? frag.querySelectorAll('.md-memlink') : [];
    chips.forEach((chip) => {
      const slug = chip.getAttribute('data-slug') || '';
      chip.replaceWith(document.createTextNode('[[' + slug + ']]'));
    });
    parts.push(frag.textContent || '');
  }
  const text = parts.join('\n');
  if (e.clipboardData) {
    e.clipboardData.setData('text/plain', text);
    e.preventDefault();
  }
}

export function initMemPopover() {
  document.addEventListener('mouseover', onEnter, true);
  document.addEventListener('mouseout', onLeave, true);
  document.addEventListener('click', onClick, true);
  document.addEventListener('mousedown', onDocClick, true);
  document.addEventListener('keydown', onKeyDown);
  document.addEventListener('copy', onCopy);
}
