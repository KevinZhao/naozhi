// cron_attention.js — the §7.4 confirmation queue: fetch, card rendering and
// the confirm action (cron_view.js split, attention region).
//
// It fetches into cron_state's cronStore.attention and never paints the
// timeline itself: cron_timeline.js imports this module for the banner, so
// the callers (cron_view, cron_drawer) repaint after a refresh or confirm.

import { NZ_CONTRACT } from './contract.js';
import { cronStore } from './cron_state.js';
import { authHeaders } from './platform.js';
import { esc, escAttr, fetchJSON } from './nz_util.js';
import { showAPIError } from './utilities.js';

// ── §7.4 人工确认队列 (PR-6) ────────────────────────────────────────────────
// The confirmation queue is the UI face of §6.2 double-run containment: a
// side-effecting sandbox run that ended failed-transport (or was orphaned by a
// naozhi restart) waits here for a human to decide. Two actions per card:
//   确认已完成 → POST /confirm (no replay; operator verified the side effect
//                landed already)
//   确认未完成，重放 → POST /replay (server Stops the original microVM first
//                       — §6.2 rule 1 — then re-injects the input snapshot)
//
// cronStore.attention holds the last fetched queue (array of items);
// cronAttentionRefresh repopulates it.

// cronAttentionReasonLabel maps a queue item's reason to operator-facing text.
function cronAttentionReasonLabel(reason) {
  switch (reason) {
    case 'transport': return '断流（云端状态未知）';
    case 'orphaned': return '重启中断（孤儿 run）';
    case 'unreadable': return '记录损坏（原 run 是否已终止无法确认）';
    default: return reason || '待确认';
  }
}

// cronAttentionQueueHtml renders the queue banner from cronStore.attention.
// Returns '' when the queue is empty so a healthy setup shows nothing.
export function cronAttentionQueueHtml() {
  const items = Array.isArray(cronStore.attention.items) ? cronStore.attention.items : [];
  if (items.length === 0) return '';
  const cards = items.map(cronAttentionCardHtml).join('');
  return '<div class="ctr-queue" role="region" aria-label="待确认的云沙箱 run">' +
      '<div class="ctr-queue-head">' +
        '<span class="ctr-queue-title">⚠ 待确认 ' + items.length + ' 项</span>' +
        '<span class="ctr-queue-sub">断流或重启中断的有副作用任务——请先确认副作用是否已发生</span>' +
      '</div>' +
      '<div class="ctr-queue-list">' + cards + '</div>' +
    '</div>';
}

// cronAttentionCardHtml renders one queue card with both resolve actions. An
// unreadable record names no job, and replay needs one, so its card offers
// confirm only.
function cronAttentionCardHtml(it) {
  if (!it || !it.run_id) return '';
  if (it.unreadable) return cronAttentionUnreadableCardHtml(it);
  if (!it.job_id) return '';
  const label = it.job_label ? esc(it.job_label) : esc(it.job_id.slice(0, 8));
  const reason = cronAttentionReasonLabel(it.reason);
  const when = it.started_at_ms ? cronFormatTime(it.started_at_ms) : '';
  return '<div class="ctr-queue-card" data-run-id="' + escAttr(it.run_id) + '">' +
      '<div class="ctr-queue-card-main">' +
        '<span class="ctr-queue-job">' + label + '</span>' +
        '<span class="ctr-queue-reason">' + esc(reason) + '</span>' +
        (when ? '<span class="ctr-queue-when">' + esc(when) + '</span>' : '') +
      '</div>' +
      '<div class="ctr-queue-actions">' +
        '<button type="button" class="ctr-queue-confirm"' +
          ' data-action="cron-att-confirm" data-run="' + escAttr(it.run_id) + '"' +
          ' title="' + escAttr('副作用已发生 / 不需重跑——从队列移除') + '">确认已完成</button>' +
        '<button type="button" class="ctr-queue-replay"' +
          ' data-action="cron-att-replay" data-job="' + escAttr(it.job_id) + '" data-run="' + escAttr(it.run_id) + '"' +
          ' title="' + escAttr('副作用未发生——先终止原微VM再用快照重放') + '">确认未完成，重放</button>' +
      '</div>' +
    '</div>';
}

// cronAttentionUnreadableCardHtml renders the confirm-only card for a record
// the server could not read. Confirming removes the file; whether the
// original microVM was stopped has to be checked on the cloud side first.
function cronAttentionUnreadableCardHtml(it) {
  const when = it.created_at_ms ? cronFormatTime(it.created_at_ms) : '';
  return '<div class="ctr-queue-card" data-run-id="' + escAttr(it.run_id) + '" data-unreadable="1">' +
      '<div class="ctr-queue-card-main">' +
        '<span class="ctr-queue-job">run ' + esc(it.run_id.slice(0, 8)) + '</span>' +
        '<span class="ctr-queue-reason">' + esc(cronAttentionReasonLabel('unreadable')) + '</span>' +
        (when ? '<span class="ctr-queue-when">' + esc(when) + '</span>' : '') +
      '</div>' +
      '<div class="ctr-queue-actions">' +
        '<button type="button" class="ctr-queue-confirm"' +
          ' data-action="cron-att-confirm" data-run="' + escAttr(it.run_id) + '"' +
          ' title="' + escAttr('先在云端确认原微VM已终止，再移除这条损坏的记录') + '">移除损坏记录</button>' +
      '</div>' +
    '</div>';
}

// cronFormatTime renders a unix-ms timestamp as a short local time for queue
// cards. Defensive against bad input.
function cronFormatTime(ms) {
  if (!ms || ms <= 0) return '';
  try {
    return new Date(ms).toLocaleString();
  } catch (e) {
    return '';
  }
}

// cronAttentionRefresh fetches GET /api/cron/attention into cronStore.attention.
// Best-effort; auth errors are swallowed (the periodic poll retries). Resolves
// true when it stored a queue, false when it kept the last good one.
export async function cronAttentionRefresh() {
  try {
    const headers = authHeaders();
    const data = await fetchJSON(NZ_CONTRACT.API.cron_attention, { headers, timeoutMs: 8000 });
    cronStore.attention = { items: (data && Array.isArray(data.items)) ? data.items : [], loaded: true };
  } catch (e) {
    if (e && e.status) return false; // auth / rate-limit — leave the last good state
    cronStore.attention = { items: [], loaded: true };
  }
  return true;
}

// cronAttentionConfirm resolves a queue item as "already done" (no replay),
// resolving true once a re-fetched queue was stored.
export async function cronAttentionConfirm(runId) {
  try {
    const headers = { 'Content-Type': 'application/json', ...authHeaders() };
    const r = await fetch(NZ_CONTRACT.API.cron_runs + '/' + encodeURIComponent(runId) + '/confirm', { method: 'POST', headers });
    if (!r.ok) {
      const raw = await r.text().catch(() => '');
      showAPIError('确认 run', r.status, raw);
      return false;
    }
  } catch (e) {
    showAPIError('确认 run', 0, String(e));
    return false;
  }
  return cronAttentionRefresh();
}
