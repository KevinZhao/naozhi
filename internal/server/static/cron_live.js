// @ts-check
// cron_live.js — the cron live stream (cron-live RFC): a second subscription
// channel beside the session one (sessionStream). While a cron drawer is
// open on a running job it subscribes 'cron:<jobId>' so the operator sees the
// claude child's streamed output. Fresh mode (scheduler_run.go:318) resets the
// stub before every run, so the first subscribe always returns suspended + 0
// events and re-subscribes on session_state running — the default path, not an
// edge case. See docs/rfc and plan §1.3.
//
// Owns cronLive, its five frame claims and their paint into the drawer's
// #cron-live-events. cron_view.js imports this module, so it evaluates first
// and its onReady re-subscribe runs before cron_view's.

import { NZ_CONTRACT } from './contract.js';
import { selection } from './state.js';
import { wsm } from './ws_manager.js';
import { CRON_LIVE_AGENT_ONLY_HTML, CRON_LIVE_MAX_EVENTS, EVENT_DIVIDER_GAP_MS, lastDividerTime, timeDividerHtml } from './utilities.js';
import { eventHtml, renderEventsWithDividers } from './event_render.js';
import { processEventsForDisplay, regroupAvatars } from './file_refs.js';
import { isInternalEvent } from './session_ident.js';
import { cronDrawerState, cronStore, isCronSessionFrozen } from './cron_state.js';

export const cronLive = {
  jobId: null,
  pendingJobId: null,
  subscribedKey: null,
  lastEventTimeMs: 0,
  runStartedAt: 0,
  events: [],
  truncatedCount: 0,
  suspended: false,
  status: 'idle', // 'idle' | 'pending' | 'live' | 'stopped'
};

// cron-live RFC §1.2: 镜像 sessionStream.subscribe()，订阅 cron stub session 拿实时事件流。
// after = lastEventTimeMs || runStartedAtMs：避免拉到上轮 run 的残留（cron
// stub EventLog 可能跨 run 持续；fresh 模式下被 Reset 销毁后是空的）。
export function subscribeCronLive(jobId, runStartedAtMs) {
  if (!jobId) return;
  if (cronLive.jobId === jobId && cronLive.subscribedKey) return; // already subscribed
  if (cronLive.jobId && cronLive.jobId !== jobId) unsubscribeCronLive();
  const key = 'cron:' + jobId;
  cronLive.jobId = jobId;
  cronLive.pendingJobId = jobId;
  cronLive.runStartedAt = runStartedAtMs || 0;
  cronLive.status = 'pending';
  setCronLiveStatus('pending');
  const msg = { type: NZ_CONTRACT.WS.subscribe, key: key };
  const after = cronLive.lastEventTimeMs || runStartedAtMs || 0;
  if (after > 0) msg.after = after;
  wsm.send(msg);
}

export function unsubscribeCronLive() {
  if (cronLive.subscribedKey) {
    wsm.send({ type: NZ_CONTRACT.WS.unsubscribe, key: cronLive.subscribedKey });
  }
  cronLive.jobId = null;
  cronLive.pendingJobId = null;
  cronLive.subscribedKey = null;
  cronLive.lastEventTimeMs = 0;
  cronLive.runStartedAt = 0;
  cronLive.events = [];
  cronLive.truncatedCount = 0;
  cronLive.suspended = false;
  cronLive.status = 'idle';
}

// isCronLiveKey 判断一条 WS 消息的 key 是否属于 cron live 订阅。带双保险：
// 既已订阅 (subscribedKey) 或 pending 中，且与主订阅 selectedKey 不撞键
// （cron drawer 打开时 openCronPanel 已清空 selectedKey，撞键不可能但兜底）。
function isCronLiveKey(key) {
  if (!key) return false;
  if (key === selection.key) return false;
  if (cronLive.subscribedKey && key === cronLive.subscribedKey) return true;
  if (cronLive.pendingJobId && key === ('cron:' + cronLive.pendingJobId)) return true;
  return false;
}

// cron-live RFC §1.3 / §2.3: cron stub spawn 完成会广播 session_state running，
// suspended sub 此时升级 —— re-sub 才能拿到 eventPushLoop 推送。fresh 模式下
// 这是默认路径（每次 run 前 Reset 销毁旧 stub）。
function onCronLiveSessionState(/** @type {WsFrames['session_state']} */ msg) {
  if (msg.state === 'running' && cronLive.suspended) {
    const jobId = cronLive.jobId;
    if (jobId) {
      cronLive.suspended = false;
      // 不清 lastEventTimeMs / events —— after= 用最末事件时间继续接续
      cronLive.subscribedKey = null;
      cronLive.pendingJobId = jobId;
      const key = 'cron:' + jobId;
      const after = cronLive.lastEventTimeMs || cronLive.runStartedAt || 0;
      const subMsg = { type: NZ_CONTRACT.WS.subscribe, key: key };
      if (after > 0) subMsg.after = after;
      wsm.send(subMsg);
    }
    return;
  }
  // 后端可能发来 'dead' (process 死亡) 或 reason='subscription_timeout'
  // (resubscribeEvents 60s 窗口超时, wshub_eventpush.go:308)。两者均表示
  // 流不会再有事件 —— 切到 stopped，事件保留可回看。
  if (msg.state === 'dead' || msg.reason === 'subscription_timeout') {
    cronLive.status = 'stopped';
    setCronLiveStatus('stopped');
  }
}

// cron-live RFC §5: 首批 history 帧到达。EventEntriesSince(after) 后端无条数
// 上限（After>0 时 Limit 被忽略），前端必须自己截尾到 CRON_LIVE_MAX_EVENTS。
function onCronLiveHistory(/** @type {WsFrames['history']} */ msg) {
  if (isCronSessionFrozen(msg.key)) return;
  const incoming = msg.events || [];
  if (incoming.length === 0) return;
  const lastTime = cronLive.lastEventTimeMs;
  // Same-ms siblings pass the time gate; same-ms replays are dropped by uuid
  // against the buffered array (mirrors onHistory's same-ms rule).
  const seen = new Set((cronLive.events || []).map(e => e.uuid).filter(Boolean));
  const newOnes = incoming.filter(e => !e.time || e.time > lastTime ||
    (e.time === lastTime && !(e.uuid && seen.has(e.uuid))));
  let merged = (cronLive.events || []).concat(newOnes);
  if (merged.length > CRON_LIVE_MAX_EVENTS) {
    const dropped = merged.length - CRON_LIVE_MAX_EVENTS;
    cronLive.truncatedCount = (cronLive.truncatedCount || 0) + dropped;
    merged = merged.slice(-CRON_LIVE_MAX_EVENTS);
  }
  cronLive.events = merged;
  if (newOnes.length > 0) {
    const last = newOnes[newOnes.length - 1];
    if (last.time && last.time > cronLive.lastEventTimeMs) cronLive.lastEventTimeMs = last.time;
  }
  cronLive.status = 'live';
  repaintCronLive();
}

function onCronLiveEvent(/** @type {WsFrames['event']} */ msg) {
  if (isCronSessionFrozen(msg.key)) return;
  const ev = msg.event;
  if (!ev) return;
  if (ev.time && ev.time < cronLive.lastEventTimeMs) return;
  if (ev.time && ev.time === cronLive.lastEventTimeMs && ev.uuid &&
      (cronLive.events || []).some(e => e.uuid === ev.uuid)) return;
  cronLive.events = cronLive.events || [];
  cronLive.events.push(ev);
  if (cronLive.events.length > CRON_LIVE_MAX_EVENTS) {
    cronLive.events.shift();
    cronLive.truncatedCount = (cronLive.truncatedCount || 0) + 1;
  }
  if (ev.time) cronLive.lastEventTimeMs = ev.time;
  cronLive.status = 'live';
  appendEventsToContainer(document.getElementById('cron-live-events'), [ev]);
  setCronLiveStatus('live');
  updateCronLiveTruncated();
}

// setCronLiveStatus 将 cronLive.status 字符串投影到 DOM 上。
// 三态：'pending' / 'live' / 'stopped'，'idle' 时清空文本。
export function setCronLiveStatus(state) {
  const el = document.getElementById('cron-live-status');
  if (!el) return;
  const labels = {
    idle: '',
    pending: '等待事件…',
    live: '实时',
    stopped: '已停止',
  };
  el.textContent = labels[state] || '';
  el.className = 'cdl-status cdl-status-' + state;
}

function updateCronLiveTruncated() {
  const trunc = document.getElementById('cron-live-truncated');
  if (!trunc) return;
  const n = cronLive.truncatedCount || 0;
  if (n > 0) {
    trunc.hidden = false;
    trunc.textContent = '已折叠 ' + n + ' 条更早事件，请等任务结束后查看历史详情';
  } else {
    trunc.hidden = true;
  }
}

// repaintCronLive 把 cronLive.events 数组重渲到 #cron-live-events 容器。
// 在 renderCronDrawer 重渲后调一次，让重建的 DOM 立刻显示已累积的事件。
// jobId 一致性守卫：若 cronLive.jobId 与当前 drawer 的 jobId 不一致就清空，
// 避免 ensureCronLiveSubscription 还未完成切换前一帧渲到错的 drawer。
// 事件到了但全被 INTERNAL_EVENT_TYPES 过滤光（典型 parallel agent team：
// 整段都是 agent / task_* / tool_use）。若留空 innerHTML，CSS
// .cdl-events:empty::before 会误报"暂无事件"，与顶部"已折叠 N 条"自相矛盾。
// 渲染占位文案，对齐主面板 appendEvents 的同款兜底。
export function repaintCronLive() {
  const el = document.getElementById('cron-live-events');
  if (!el) return;
  const drawerJobId = cronDrawerState.jobId;
  if (drawerJobId && cronLive.jobId && cronLive.jobId !== drawerJobId) {
    el.innerHTML = '';
    return;
  }
  const events = cronLive.events || [];
  const display = processEventsForDisplay(events);
  const html = renderEventsWithDividers(display, 0);
  if (html) {
    el.innerHTML = html;
  } else if (events.length > 0) {
    el.innerHTML = CRON_LIVE_AGENT_ONLY_HTML;
  } else {
    el.innerHTML = '';
  }
  regroupAvatars(el);
  el.scrollTop = el.scrollHeight;
  updateCronLiveTruncated();
  setCronLiveStatus(cronLive.status);
}

// appendEventsToContainer 是 appendEvents 的容器化变体：不动主面板的
// turnState / banner / navUserEls / optimistic-msg，只把事件 HTML 追加到
// 指定容器。供 cron live 增量推送复用。
function appendEventsToContainer(el, events) {
  if (!el) return;
  const wasBottom = el.scrollTop + el.clientHeight >= el.scrollHeight - 30;
  // 若容器当前只挂着 agent-only 占位（repaintCronLive 渲过），在追加真实
  // 事件前清掉它，避免占位与事件并存。lastDividerTime 等读取也不会被它干扰。
  if (el.querySelector('.cdl-agent-only')) el.innerHTML = '';
  let prevT = lastDividerTime(el);
  events.forEach(e => {
    if (isInternalEvent(e)) return;
    const h = eventHtml(e); if (!h) return;
    const t = e.time || 0;
    if (t && (prevT === 0 || t - prevT >= EVENT_DIVIDER_GAP_MS)) {
      el.insertAdjacentHTML('beforeend', timeDividerHtml(t));
    }
    el.insertAdjacentHTML('beforeend', h);
    if (t) prevT = t;
  });
  trimCronLiveDom(el);
  // 头像分组：cron live 容器在 #events-scroll 之外，不被主 observer 覆盖，
  // 追加后显式重算 .nz-grouped（与 appendEvents/抽屉同款）。
  regroupAvatars(el);
  if (wasBottom) el.scrollTop = el.scrollHeight;
}

// #398-sibling: onCronLiveEvent caps the data model at CRON_LIVE_MAX_EVENTS
// (events.shift) but the incremental push only ever appends to the container,
// so its DOM grew unbounded across a long cron run. Trim the oldest .event
// bubbles from the top to keep the DOM in sync with the data cap.
function trimCronLiveDom(el) {
  let bubbles = el.querySelectorAll(':scope > .event').length;
  if (bubbles > CRON_LIVE_MAX_EVENTS) {
    let node = el.firstChild;
    while (node && bubbles > CRON_LIVE_MAX_EVENTS) {
      const next = node.nextSibling;
      if (node.nodeType === 1 && node.classList && node.classList.contains('event')) bubbles--;
      el.removeChild(node);
      node = next;
    }
  }
}

// ensureCronLiveSubscription 是 cron live 订阅状态的中心协调器。语义：
//   - drawer 关闭 → 撤销订阅
//   - drawer 开 + 任务跑 + 未订阅 → 订阅
//   - drawer 开 + 任务跑 + 已订阅同 jobId → no-op
//   - drawer 开 + 任务跑 + 已订阅别的 jobId → 切换
//   - drawer 开 + 任务空闲 → no-op（保留已订内容供回看；首次开 idle 任务则不订）
// 故意不在 cronApplyRunEnded 钩 unsub —— 让操作员看完本轮事件，关 drawer 才撤。
export function ensureCronLiveSubscription() {
  const jobId = cronDrawerState.jobId;
  if (!jobId) {
    if (cronLive.jobId) unsubscribeCronLive();
    return;
  }
  if (cronLive.jobId && cronLive.jobId !== jobId) unsubscribeCronLive();
  if (cronLive.jobId === jobId) return;
  const job = cronStore.jobs.find(j => j && j.id === jobId);
  const isRunning = !!(job && job.current_run && job.current_run.started_at);
  if (!isRunning) return;
  subscribeCronLive(jobId, job.current_run.started_at);
}

// cron-live RFC §2.2: the acks and errors that answer a pending cron live
// subscribe stay out of the session subscription's bookkeeping.
const cronLivePending = (/** @type {WsFrames['subscribed' | 'error']} */ msg) => cronLive.pendingJobId && msg.key === ('cron:' + cronLive.pendingJobId);
wsm.on(NZ_CONTRACT.WS.subscribed, (msg) => {
  cronLive.subscribedKey = msg.key;
  cronLive.pendingJobId = null;
  cronLive.suspended = (msg.reason === 'suspended');
  cronLive.status = cronLive.suspended ? 'pending' : 'live';
  setCronLiveStatus(cronLive.status);
}, cronLivePending);
wsm.on(NZ_CONTRACT.WS.error, () => {
  cronLive.pendingJobId = null;
  cronLive.subscribedKey = null;
  cronLive.status = 'stopped';
  setCronLiveStatus('stopped');
}, (msg) => msg.key && cronLivePending(msg));
const cronLiveKey = (/** @type {WsFrames['history' | 'event' | 'session_state']} */ msg) => isCronLiveKey(msg.key);
wsm.on(NZ_CONTRACT.WS.history, (msg) => onCronLiveHistory(msg), cronLiveKey);
wsm.on(NZ_CONTRACT.WS.event, (msg) => onCronLiveEvent(msg), cronLiveKey);
wsm.on(NZ_CONTRACT.WS.session_state, (msg) => onCronLiveSessionState(msg), cronLiveKey);

// cron-live RFC §3: 重连后若已有 cron live 订阅，后端 conn 已亡 sub 已丢，
// 必须重发 subscribe 帧。直接走 subscribeCronLive 不行 —— 它的"已订同 jobId
// 直接 no-op"短路会让我们什么都不做。
//
// 仅在任务"仍在跑"时重 sub。若断网期间任务已经结束，重 sub 只会拉到一个
// suspended 的 stub（fresh 模式 stub 已销毁），status 卡在 'pending' 看起
// 来像还在等事件 —— 但事件永远不会来。让它停留在终态视图（events 数组
// 仍含上轮事件，可回看）。
wsm.onReady(() => {
  const jobId = cronLive.jobId;
  if (!jobId) return;
  const job = cronStore.jobs.find(j => j && j.id === jobId);
  if (!(job && job.current_run && job.current_run.started_at)) return;
  cronLive.subscribedKey = null;
  cronLive.pendingJobId = null;
  cronLive.suspended = false;
  // 清 jobId 让 subscribeCronLive 不被 "已订同 jobId" 短路命中
  cronLive.jobId = null;
  subscribeCronLive(jobId, cronLive.runStartedAt);
});
