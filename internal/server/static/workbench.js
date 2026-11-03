// workbench.js — the right-hand 工作台 (workbench) pane: tabs 预览 · 产出 ·
// 文件 · 详情 over the #fv-drawer, docked as the desktop split and a
// full-screen panel on phone (docs/rfc/planner.md §10, §10.2, §15.1 W1).
// This module is only the shell: open / close / tab / 放大. file_refs.js
// fills the 预览 tab, workbench_panes.js the other three and the auto-open.
// Layering: a leaf over split_view / mobile_nav, so any view can reuse it.
//
// Events on nzBus: 'workbench:open' and 'workbench:close' when the pane
// opens or closes, 'workbench:tab' (detail.tab) whenever a tab is shown,
// including on open.
import { nzBus } from './nz_util.js';
import { splitDock } from './split_view.js';
import { collapseSidebarForDrawer, restoreSidebarAfterDrawer } from './mobile_nav.js';

const TABS = ['preview', 'outputs', 'files', 'details'];

function drawer() { return document.getElementById('fv-drawer'); }

function isOpen() {
  const d = drawer();
  return !!d && d.classList.contains('fv-open');
}

function tab() {
  const d = drawer();
  return (d && d.dataset.wbTab) || 'preview';
}

function showTab(name) {
  const d = drawer();
  if (!d || !TABS.includes(name)) return;
  d.dataset.wbTab = name;
  d.querySelectorAll('.wb-tab').forEach((b) => {
    b.setAttribute('aria-selected', b.dataset.wbTab === name ? 'true' : 'false');
  });
  d.querySelectorAll('.wb-pane').forEach((p) => { p.hidden = p.dataset.wbPane !== name; });
  if (isOpen()) nzBus.dispatchEvent(new CustomEvent('workbench:tab', { detail: { tab: name } }));
}

// open shows the pane on tab (the current one when omitted).
function open(name) {
  const d = drawer();
  if (!d) return;
  const was = isOpen();
  d.classList.remove('hidden');
  d.classList.add('fv-open');
  // Dock as a right-hand split on desktop so the transcript stays visible
  // beside it (no-op on phone — falls back to the overlay).
  splitDock.enter();
  // Opened last → stack on top of the 追问 pane if both are docked.
  splitDock.bringToFront('preview');
  collapseSidebarForDrawer();
  if (!was) nzBus.dispatchEvent(new CustomEvent('workbench:open'));
  showTab(name || tab());
}

function close() {
  const d = drawer();
  if (!d || !isOpen()) return;
  d.classList.remove('fv-open', 'wb-max');
  d.classList.add('hidden');
  syncMaxBtn();
  // Undock the split (no-op if the 追问 drawer is still open).
  splitDock.exit();
  // Re-expand the sidebar if the open path auto-collapsed it (no-op if the
  // 追问 drawer is still open or the user collapsed it themselves).
  restoreSidebarAfterDrawer();
  nzBus.dispatchEvent(new CustomEvent('workbench:close'));
}

function toggleMax() {
  const d = drawer();
  if (!d || !isOpen()) return;
  d.classList.toggle('wb-max');
  syncMaxBtn();
}

function syncMaxBtn() {
  const d = drawer();
  const b = document.getElementById('wb-btn-max');
  if (!d || !b) return;
  const max = d.classList.contains('wb-max');
  b.setAttribute('aria-pressed', max ? 'true' : 'false');
  b.textContent = max ? '⤡ 还原' : '⤢ 放大';
  b.title = max ? '还原为分栏' : '占满窗口';
}

document.addEventListener('DOMContentLoaded', () => {
  const d = drawer();
  if (!d) return;
  d.querySelector('.wb-tabs')?.addEventListener('click', (e) => {
    const b = e.target.closest('.wb-tab');
    if (b) showTab(b.dataset.wbTab);
  });
  document.getElementById('wb-btn-max')?.addEventListener('click', toggleMax);
  document.getElementById('fv-btn-close')?.addEventListener('click', close);
  // Esc closes the pane (but only when nothing else is handling Esc).
  document.addEventListener('keydown', (evt) => {
    if (evt.key !== 'Escape' || !isOpen()) return;
    evt.stopPropagation();
    close();
  }, true);
});

export const workbench = { close, isOpen, open, showTab, tab, toggleMax };
