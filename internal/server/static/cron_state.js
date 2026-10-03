// cron_state.js — the cron view's shared state, a leaf (caps.leaves): the
// jobs cache and what the server says about it (cronStore), which job the
// drawer shows, the per-job cost and frozen-run maps, the 立即执行 cooldown,
// and the two fetches that fill the cache. The cron modules read these
// objects and write their fields in place; no binding here is reassigned.

import { NZ_CONTRACT } from './contract.js';
import { getToken } from './platform.js';
import { fetchJSON } from './nz_util.js';
import { setCronTimezoneMeta } from './cron_schedule.js';

export const cronStore = {
  jobs: [],
  // Configured default IM target for cron completion notifications, or null
  // when the server has no default configured. Used to render helpful copy
  // alongside the notify toggle in create/edit modals.
  notifyDefault: null,
  // recent_runs_cap from GET /api/cron — the server-side per-job cap on the
  // embedded recent_runs preview (recentRunsPerJob). 0 until the first fetch.
  // renderCronTimelineForJob uses it to tell "history fully in hand" (len <
  // cap) from "first page only" (len == cap) without a second literal here.
  recentRunsCap: 0,
  // The §7.4 confirmation queue's last fetch (cron_attention.js), held so
  // the banner renders synchronously inside cronTimelineHtml.
  attention: { items: [], loaded: false },
};
// cron-panel-consolidation RFC §4.5: cronDrawerState.jobId is the only
// state gate for the per-job drawer. null = drawer closed; otherwise the
// currently-displayed job ID (NOT key — the drawer keys off cron job
// ID, not the synthesised "cron:<id>" session key, since cron stubs
// no longer surface as managed sessions on the dashboard side).
//
// Lifecycle:
//   - openCronDetail(jobId) sets it and re-renders the cron panel.
//   - closeCronDetail() resets to null and removes drawer DOM.
//   - WS run_started / run_ended (subsystem cron) consult this gate (PR5)
//     instead of selectedKey.
//   - F5 / reload does NOT persist (RFC §4.5 Q5).
// Exported as a const object so the other cron modules read the live value.
export const cronDrawerState = { jobId: null };

// cronJobCostCache: jobId → last-30-day ledger totals for the job (local +
// sandbox runs), fetched when the drawer opens; complements the timeline's
// "已加载 N 条" sum which only covers loaded rows.
export const cronJobCostCache = {};

// cronFrozenRuns 是 timed_out（或其他非 succeeded/skipped 终态）后
// 冻结事件流的 jobID 集合。命中后，sessionFrames.onEvent 对该 cron session
// 的实时事件直接丢弃，避免 dashboard 在 cron 历史卡显示"超时"
// 的同时事件流仍在追加（CLI 子进程没立刻停，会再吐几个 ghost
// 事件）。下一次 run_started（cron）同 job 时清空。
//
// 后端 cron deadline 已经主动 InterruptViaControl 让 CLI 收尾，
// 但 control_request 到 result 事件之间还有 ~几百 ms ~ 几秒延迟；
// 这里是第二道防线，让 dashboard 视觉上立即冻结。
export const cronFrozenRuns = new Set();

// cronRunClearedAtLocal: jobId → 本地清除 current_run 的时刻。与
// current_run.applied_at_local 互为镜像，挡住反方向的同一竞态：一个生成于
// run_ended 帧之前、落地于其后的 list 响应仍带 current_run，会让刚熄灭的
// 运行中徽章复活一拍。
export const cronRunClearedAtLocal = new Map();

// isCronSessionFrozen 判断当前 selectedKey 是否是被冻结的 cron session。
// cron session key 的形态是 "cron:" + jobID（见 session.CronKey）；只有
// dashboard 当前看的就是这条 cron 的实时面板时才需要丢事件，其他视图
// 不受影响。
export function isCronSessionFrozen(key) {
  if (!key || typeof key !== 'string') return false;
  if (!key.startsWith('cron:')) return false;
  return cronFrozenRuns.has(key.slice('cron:'.length));
}

// cronTriggerButtonState — the 立即执行 button's disable matrix (see the
// comment block in cronDrawerHtml). Pure; shared by the drawer render and
// cronDrawerRefreshTriggerBtn so the 200 ms cooldown tick can patch the
// button in place instead of rebuilding the whole drawer.
export function cronTriggerButtonState(j) {
  const id = j.id || '';
  const isPaused = !!j.paused;
  const isRunning = !!(j.current_run && j.current_run.started_at);
  const cooldown = cronTriggerCooldownState(id);
  const st = { disabled: false, label: '\u25B7 立即执行', tooltip: '立即执行一次', cls: 'cda-btn primary' };
  if (isPaused) {
    st.disabled = true;
    st.tooltip = '已暂停。请先恢复任务。';
  } else if (isRunning) {
    st.disabled = true;
    st.label = '\u25B7 运行中…';
    st.tooltip = '上一次执行尚未完成，请等待结束。';
    st.cls += ' is-running';
  } else if (cooldown) {
    st.disabled = true;
    st.label = '\u25B7 ' + cooldown.label;
    st.cls += cooldown.phase === 'sending' ? ' is-sending' : ' is-sent';
    st.tooltip = '刚已触发一次，请稍候。';
  }
  return st;
}

// cronJustTriggered tracks the per-jobId trigger timestamp (ms).
// Used by cronTriggerCooldownState() to compute disable + label state for
// both the drawer's primary action and the list row's ghost Run button so
// they stay in sync. Cleared by cronTriggerCooldownClear() on WS
// cron_run_started (preferred) or after the 10 s floor elapses.
export const cronJustTriggered = Object.create(null);
export const CRON_TRIGGER_COOLDOWN_MS = 10 * 1000;

function cronTriggerCooldownState(id) {
  const t = cronJustTriggered[id];
  if (!t) return null;
  const dt = Date.now() - t;
  if (dt < 0 || dt >= CRON_TRIGGER_COOLDOWN_MS) {
    delete cronJustTriggered[id];
    return null;
  }
  // 0..1000 ms → spinner; 1000..3000 ms → ✓; 3000..10000 ms → quiet hold.
  if (dt < 1000) return { phase: 'sending', label: '触发中…' };
  if (dt < 3000) return { phase: 'sent',    label: '已派发 ✓' };
  return { phase: 'cooldown', label: '已派发 ✓' };
}

export function cronTriggerCooldownClear(id) {
  if (cronJustTriggered[id]) delete cronJustTriggered[id];
}

// keepRefetchedPrompts carries a re-fetched full prompt across a compact poll.
//
// The 1 Hz poll replaces the whole cache, so the non-truncated row
// cronRefetchFullJob splices in on drawer/editor open used to survive about one
// millisecond — measured 726 ms -> 727 ms in a MutationObserver trace, i.e. the
// drawer's 做什么 section never actually showed the prompt it re-fetched (#494
// case 4). The retired source anchor could only see that the call existed.
//
// The prefix guard makes it safe: a clipped body is by construction a prefix of
// what it was clipped from, so an edit landing between refreshes changes the
// clipped text and the stale full copy is dropped instead of resurrected.
//
// prompt_truncated deliberately STAYS true on a merged row. It means "the wire
// row was clipped", and cronRefetchFullJob early-returns { ok: true, job: cached }
// when it is false — so clearing it would let the editor open from cache and Save
// a prompt that changed since the merge, which is the data loss #494's follow-up
// exists to prevent. Carrying a full body for display costs nothing; skipping the
// editor's re-fetch costs the user their prompt.
function keepRefetchedPrompts(prev, fresh) {
  const full = new Map();
  for (const p of prev || []) {
    if (p && p.id && typeof p.prompt === 'string' && p.prompt.length > 0) full.set(p.id, p.prompt);
  }
  if (full.size === 0) return fresh;
  return fresh.map(j => {
    const had = (j && j.prompt_truncated && typeof j.prompt === 'string') ? full.get(j.id) : undefined;
    if (typeof had !== 'string' || had.length <= j.prompt.length || !had.startsWith(j.prompt)) return j;
    return Object.assign({}, j, { prompt: had });
  });
}

export async function fetchCronJobs() {
  // 响应新旧的判据：比这个时刻更新的本地乐观补丁不被本响应覆盖（下方
  // stale-clobber 保护）。取在请求发出前，宁可偏早（多保留补丁一拍）也
  // 不偏晚（把新补丁误判为旧）。
  const fetchStartedAt = Date.now();
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    // RNEW-UX-003: 8s timeout — cron list is polled periodically; a hung
    // disk/fs call must release before the next tick fires.
    //
    // R236-SEC-08 (#494): poll path opts into compact mode so the wire
    // shape carries `prompt` clipped to 256 UTF-8 bytes per job instead
    // of the legacy full prompt (which scaled to 8 KiB × N jobs every
    // tick). Each list row sets `prompt_truncated:true` for jobs whose
    // full body was clipped — the editor open path (cronEditFetchFull)
    // re-fetches a single job without compact when the user actually
    // needs the bytes.
    let data;
    try {
      data = await fetchJSON(NZ_CONTRACT.API.cron + '?compact=1', { headers, timeoutMs: 8000 });
    } catch (err) {
      if (err.status) return;
      throw err;
    }
    const freshJobs = keepRefetchedPrompts(cronStore.jobs, data.jobs || []);
    // Stale-clobber 保护：这个响应可能生成于一个 WS run_started / run_ended
    // 帧之前、却落地于其后（本地 e2e 用 compactCronListDelayMs 稳定复现；
    // 真实后端在 list 事务较长时同样可能）。整体替换 cronStore.jobs 会让旧响应
    // 冲掉更新的乐观补丁 —— 运行中徽章闪没，或反向复活一拍。规则：只信
    // 比本次 fetch 发起时刻更新的本地补丁，其余以服务端为准。
    for (const nj of freshJobs) {
      if (!nj || !nj.id) continue;
      const prev = (Array.isArray(cronStore.jobs) ? cronStore.jobs : []).find(o => o && o.id === nj.id);
      if (!prev) continue;
      const appliedAt = prev.current_run && prev.current_run.applied_at_local;
      if (prev.current_run && !nj.current_run && appliedAt && appliedAt > fetchStartedAt) {
        nj.current_run = prev.current_run;
      }
      const clearedAt = cronRunClearedAtLocal.get(nj.id);
      if (nj.current_run && !prev.current_run && clearedAt && clearedAt > fetchStartedAt) {
        nj.current_run = null;
      }
    }
    cronStore.jobs = freshJobs;
    cronStore.notifyDefault = data.notify_default || null;
    cronStore.recentRunsCap = (data.recent_runs_cap | 0) > 0 ? (data.recent_runs_cap | 0) : 0;
    setCronTimezoneMeta(data);
    // Badge surfaces jobs needing intervention (last run errored, or a
    // scheduled run was missed across a restart), not the raw total — avoids
    // a persistent red dot on healthy setups. #2435: manually paused jobs are
    // a deliberate operator state, so they no longer light the rail dot; the
    // in-view 需关注 filter / header chip still include paused for context.
    const attention = cronStore.jobs.filter(j => j.last_error || j.missed).length;
    // Surface the attention dot on the rail's 自动化 icon so the alert is
    // visible from any view. (The legacy header cron-badge was removed once
    // the sidebar 定时任务 quick-button folded into the rail's 自动化 entry.)
    const railBadge = document.getElementById('abnav-cron-badge');
    if (railBadge) {
      railBadge.hidden = attention === 0;
    }
  } catch (e) { console.error('fetch cron:', e); }
}

// cronRefetchFullJob refills `cronStore.jobs[i].prompt` for a single job from
// the non-compact /api/cron endpoint (R236-SEC-08 / #494). The poll path
// uses ?compact=1 which clips prompts to 256 bytes — that's fine for the
// list view but the editor / drawer detail need the full body before the
// user can save without truncating their own data.
//
// Returns one of:
//   { ok: true,  job }              — full prompt, safe to edit & save
//   { ok: false, reason: 'missing' }— job not in cronStore.jobs cache
//   { ok: false, reason: 'fetch'  } — cache had truncated prompt and the
//                                     refetch failed; caller MUST refuse
//                                     to open the editor. Saving the
//                                     truncated body would silently
//                                     destroy the user's data.
export async function cronRefetchFullJob(id) {
  const cached = cronStore.jobs.find(j => j.id === id);
  if (!cached) return { ok: false, reason: 'missing' };
  // Skip the round trip only for a row that was never clipped AND never spliced
  // by an earlier refetch. A spliced row carries a full body but says nothing
  // about whether the prompt has changed since, so trusting it let the editor
  // open — and Save — a stale prompt: open a drawer, have the prompt rewritten
  // elsewhere, open the editor, and the old body goes back to disk. Measured in
  // test/e2e/cron_compact_prompt.test.js before this guard existed.
  if (!cached.prompt_truncated && !cached.prompt_refetched) return { ok: true, job: cached };
  try {
    const headers = {};
    const t = getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    // No compact param — list endpoint returns full prompts. We pull
    // the whole list here because there is no per-job GET endpoint
    // exposed; the rate limiter on the list route is shared with the
    // poll, and an editor open is a once-per-user-action event so the
    // extra body is not a hot path.
    const data = await fetchJSON(NZ_CONTRACT.API.cron, { headers, timeoutMs: 8000 });
    const jobs = (data && data.jobs) || [];
    const fresh = jobs.find(j => j.id === id);
    if (fresh && !fresh.prompt_truncated) {
      // Splice the full-prompt copy back into the cache so subsequent
      // editor opens / drawer renders see the full body without another
      // network round trip.
      const idx = cronStore.jobs.findIndex(j => j.id === id);
      const spliced = Object.assign({}, fresh, { prompt_refetched: true });
      if (idx >= 0) cronStore.jobs[idx] = spliced;
      return { ok: true, job: spliced };
    }
  } catch (e) { /* fall through to fetch-failure */ }
  return { ok: false, reason: 'fetch' };
}
