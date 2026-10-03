// cron_trigger.js — the 立即执行 (TriggerNow) flow and its visual-feedback
// cooldown state machine (#2715 D4 follow-up: cron_view.js four-region
// split, region 4).
//
// Owns the 200ms cooldown tick that walks sending → sent → clear. The
// per-jobId trigger timestamps (cronJustTriggered) and the primary button's
// disable/label matrix (cronTriggerButtonState) live in cron_state.js, where
// the drawer reads them too; the WS run_started handler (cron_view.js)
// clears the cooldown through cron_state's cronTriggerCooldownClear.

import { NZ_CONTRACT } from './contract.js';
import { getToken } from './platform.js';
import { showToast } from './nz_util.js';
import { showAPIError, showNetworkError } from './utilities.js';
import { renderCronDrawer } from './cron_drawer.js';
import { CRON_TRIGGER_COOLDOWN_MS, cronDrawerState, cronJustTriggered, cronStore, cronTriggerButtonState, cronTriggerCooldownClear } from './cron_state.js';

// cronDrawerRefreshTriggerBtn — targeted repaint of the drawer's primary
// action button(s) for the open job. Called from the cooldown tick every
// 200 ms; touching only the button keeps text selection, <details> open
// state and scroll position inside the drawer intact (a full
// renderCronDrawer here used to wipe all three for 10 s after 立即执行).
function cronDrawerRefreshTriggerBtn() {
  if (cronDrawerState.jobId === null) return;
  const job = (cronStore.jobs || []).find(x => x && x.id === cronDrawerState.jobId);
  if (!job) return;
  const btns = document.querySelectorAll('#cron-detail-pane .cron-drawer-actions .cda-btn.primary');
  if (!btns.length) return;
  const st = cronTriggerButtonState(job);
  for (const btn of btns) {
    if (btn.className !== st.cls) btn.className = st.cls;
    if (btn.textContent !== st.label) btn.textContent = st.label;
    if (btn.title !== st.tooltip) btn.title = st.tooltip;
    if (btn.disabled !== st.disabled) {
      btn.disabled = st.disabled;
      if (st.disabled) btn.setAttribute('aria-disabled', 'true');
      else btn.removeAttribute('aria-disabled');
    }
  }
}

// cronTriggerCooldownTickTimer drives label transitions (sending → sent →
// cooldown) and final unlock. Runs at 200 ms while any job is in cooldown
// to make the spinner→✓ transition feel snappy without burning CPU when
// the button is idle.
let cronTriggerCooldownTickTimer = null;
function ensureCronTriggerCooldownTick() {
  const anyHot = Object.keys(cronJustTriggered).length > 0;
  if (anyHot && !cronTriggerCooldownTickTimer) {
    cronTriggerCooldownTickTimer = setInterval(() => {
      // Sweep expired entries; the in-place button patch below then lets
      // the drawer's trigger button leave cooldown.
      const now = Date.now();
      for (const k of Object.keys(cronJustTriggered)) {
        if (now - cronJustTriggered[k] >= CRON_TRIGGER_COOLDOWN_MS) {
          delete cronJustTriggered[k];
        }
      }
      // Patch only the drawer's trigger button so the spinner→✓→idle
      // transitions happen even if no other event fires, without rebuilding
      // the drawer (which wiped text selection / <details> state every 200 ms).
      cronDrawerRefreshTriggerBtn();
      if (Object.keys(cronJustTriggered).length === 0) {
        clearInterval(cronTriggerCooldownTickTimer);
        cronTriggerCooldownTickTimer = null;
      }
    }, 200);
  }
}

// cronTriggerNow calls POST /api/cron/trigger to kick off a job immediately
// without waiting for the next scheduled tick. Useful when the operator
// wants to verify a prompt edit or rerun after a transient failure.
//
// Round 2 review R-4: visual-feedback contract (cron-panel-consolidation-ui
// RFC §4.3.1). The backend's jobRunningGuard already serializes against
// double-click — the issue is *user perception*. WS cron_run_started lands
// 200-500 ms after the API ACK, so a naive "fire-and-forget + toast" leaves
// the button looking pristine for that whole window and operators reflexively
// click again. The flow we want is:
//
//   click → button locks (spinner) → API returns OK → "已派发 ✓" 2 s
//        → debounce floor stays in effect another N s → unlock when WS
//          cron_run_started lands OR debounce floor elapses, whichever
//          is later.
//
// 10 s is the debounce floor: longer than the worst-case API + WS round
// trip we've measured (~3 s under load) but short enough that a real
// scheduled tick during the window won't get visually swallowed.
//
// Contract notes:
//   - Backend rejects paused jobs with 409 ErrJobPaused; the button is
//     hidden for paused jobs (cronJobCardHtml), so 409 here usually means a
//     pause landed between render and click — surface it via showAPIError
//     and immediately clear cronJustTriggered so the user can retry.
//   - 409 "already running" maps to the same "请等待结束" path the
//     disabled-running-state already shows; we reuse showAPIError so the
//     status code remains visible for L2 support.
//   - We do NOT wait for cron_run_started before unlocking — under WS
//     disconnection the event might never arrive. The 10 s floor + the
//     subsequent fetchCronJobs poll will reconcile.
async function cronTriggerNow(id) {
  // Reentrancy guard: if a cooldown is already in flight for this id, drop
  // the click silently — the disabled button state should have prevented it
  // already, but keyboard activation paths (Enter on a non-disabled button
  // in the same paint window) can still slip through.
  if (cronJustTriggered[id]) return;
  cronJustTriggered[id] = Date.now();
  ensureCronTriggerCooldownTick();
  // Repaint drawer immediately so the button flips to the spinner without
  // waiting for the 200 ms tick. List ghost Run isn't repainted per row
  // (would be expensive on 500-job dashboards) — its disabled-after-trigger
  // state is read at render time.
  if (cronDrawerState.jobId === id) renderCronDrawer();
  try {
    const headers = { 'Content-Type': 'application/json' };
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const r = await fetch(NZ_CONTRACT.API.cron_trigger, { method: 'POST', headers, body: JSON.stringify({ id }) });
    if (!r.ok) {
      // Failure — clear the cooldown immediately so the user can retry.
      // The 10 s floor would be punishing on a transient 502.
      cronTriggerCooldownClear(id);
      if (cronDrawerState.jobId === id) renderCronDrawer();
      const raw = await r.text().catch(() => '');
      showAPIError('立即执行定时任务', r.status, raw);
      return;
    }
    // Success — leave cooldown in place; the tick timer will transition
    // the label and finally clear it. cronStore.jobs row state will be updated
    // by the WS cron_run_started event (which also clears the cooldown
    // via the dispatch handler — see ws msg case below).
    showToast('已派发执行', 'success', 1500);
  } catch (e) {
    cronTriggerCooldownClear(id);
    if (cronDrawerState.jobId === id) renderCronDrawer();
    showNetworkError('立即执行定时任务', e);
  }
}


export {
  cronDrawerRefreshTriggerBtn,
  cronTriggerNow,
};
