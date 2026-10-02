// cron_live.js — the cron live stream (cron-live RFC): a second subscription
// channel beside the session one (wsm.subscribedKey). While a cron drawer is
// open on a running job it subscribes 'cron:<jobId>' so the operator sees the
// claude child's streamed output. Fresh mode (scheduler_run.go:318) resets the
// stub before every run, so the first subscribe always returns suspended + 0
// events and re-subscribes on session_state running — the default path, not an
// edge case. See docs/rfc and plan §1.3.
//
// Owns cronLive and its five frame claims; cron_view.js paints it. The two
// import each other and only call across at frame / event time, never at load.
// cron_view is the graph entry, so this module evaluates first and its onReady
// re-subscribe runs before cron_view's ensureCronLiveSubscription.

import { NZ_CONTRACT } from './contract.js';
import { hooks, selection } from './state.js';
import { wsm } from './dashboard.js';
import { CRON_LIVE_MAX_EVENTS } from './utilities.js';
import { appendEventsToContainer, isCronSessionFrozen, repaintCronLive, setCronLiveStatus, updateCronLiveTruncated } from './cron_view.js';

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

// cron-live RFC §1.2: 镜像 wsm.subscribe()，订阅 cron stub session 拿实时事件流。
// after = lastEventTimeMs || runStartedAtMs：避免拉到上轮 run 的残留（cron
// stub EventLog 可能跨 run 持续；fresh 模式下被 Reset 销毁后是空的）。
export function subscribeCronLive(jobId, runStartedAtMs) {
  if (!jobId) return;
  if (cronLive.jobId === jobId && cronLive.subscribedKey) return; // already subscribed
  if (cronLive.jobId && cronLive.jobId !== jobId) {
    unsubscribeCronLive();
  }
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
function onCronLiveSessionState(msg) {
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
function onCronLiveHistory(msg) {
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

function onCronLiveEvent(msg) {
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

// cron-live RFC §2.2: the acks and errors that answer a pending cron live
// subscribe stay out of the session subscription's bookkeeping.
const cronLivePending = (msg) => cronLive.pendingJobId && msg.key === ('cron:' + cronLive.pendingJobId);
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
const cronLiveKey = (msg) => isCronLiveKey(msg.key);
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
  const jobs = hooks.cronJobs();
  const job = Array.isArray(jobs) ? jobs.find(j => j && j.id === jobId) : null;
  if (!(job && job.current_run && job.current_run.started_at)) return;
  cronLive.subscribedKey = null;
  cronLive.pendingJobId = null;
  cronLive.suspended = false;
  // 清 jobId 让 subscribeCronLive 不被 "已订同 jobId" 短路命中
  cronLive.jobId = null;
  subscribeCronLive(jobId, cronLive.runStartedAt);
});
