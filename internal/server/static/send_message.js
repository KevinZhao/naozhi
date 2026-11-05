// send_message.js — sending from the composer: the WS / HTTP send paths, the
// optimistic user bubble and running flip, and their rollback.
// Layering: never import dashboard.js back. event_stream.js and
// session_list.js import this module, so fetchEvents and fetchSessions are
// shell slots, as is dashboard's selectSession.
import { NZ_CONTRACT } from './contract.js';
import { composer, perSession, selection, sessionList, timers } from './state.js';
import { interruptSession, startTurnTimer, turnState, updateSendButton } from './running_banner.js';
import { fetchJSON, showToast, patchCardHealth } from './nz_util.js';
import { wsm } from './ws_manager.js';
import { featureForCurrent } from './features.js';
import { EVENT_DIVIDER_GAP_MS, getMsgValue, lastDividerTime, persistPending, removeSidebarCard, setMsgValue, showAPIError, showAuthModal, showNetworkError, stickEventsBottom, timeDividerHtml } from './utilities.js';
import { discoveredKey, dropDiscovered, sid } from './session_ident.js';
import { eventHtml } from './event_render.js';
import { awaitPendingOrients, filesBlockSend, renderFilePreviews } from './composer_files.js';
import { navSync } from './msg_nav.js';
import { shell } from './shell.js';

// --- Send message ---

// Esc in the input: first press arms, second press (within 600ms) actually
// interrupts the running turn. Prevents thumb-on-Esc misfires.
let _lastEscAt = 0;
function handleKey(e) {
  if (e.key === 'Escape') {
    e.preventDefault();
    const sd = sessionList.sessionsData[sid(selection.key, selection.node || 'local')];
    const running = sd && sd.state === 'running';
    if (!running) { _lastEscAt = 0; return; }
    const now = Date.now();
    if (now - _lastEscAt < 600) {
      _lastEscAt = 0;
      interruptSession();
    } else {
      _lastEscAt = now;
      showToast('再按一次 Esc 发送中断', 'warning', 1000);
    }
    return;
  }
  if (e.key === 'Enter' && !e.shiftKey && !e.isComposing && Date.now() - composer.lastCompositionEnd > 30) { e.preventDefault(); sendMessage(); }
}

function clearMsg(el) { if (el) el.textContent = ''; }

// validateComposerForSend runs the synchronous pre-send checks that depend on
// the live composer (text-level backend feature gates, byte cap, in-flight
// uploads). Toasts and returns false when the send must abort. sendMessage
// calls it twice: once before closing the reentrancy gate and again after
// `await awaitPendingOrients()` — the composer stays editable during that wait,
// so text and attachments captured before the await can be stale.
function validateComposerForSend(text) {
  // Multi-Backend RFC §8.3 D9 — `/urgent` preempts the running turn, which
  // needs the backend's `passthrough` feature (kiro / ACP have none; the
  // server would error or queue it). It is typed text, so check at send time.
  if (text && /^\s*\/urgent\b/.test(text) && !featureForCurrent('passthrough')) {
    showToast('当前后端不支持 /urgent 抢占（请用 Esc 中断后再发）', 'warning');
    return false;
  }

  // Multi-Backend RFC §8.3 D13 — `@-mention` needs a backend that reads paths
  // in the prompt (claude, not kiro). Abort rather than silently strip it.
  if (text && /(?:^|\s)@[\w./-]/.test(text) && !featureForCurrent('embedded_context')) {
    showToast('当前后端不支持 @ 文件 mention，请粘贴绝对路径或文件内容', 'warning');
    return false;
  }

  // Per-field byte cap matches server maxWSSendTextBytes (1 MB). Reject
  // up-front so oversize pastes don't round-trip and return a silent
  // send_ack error that the optimistic bubble would have already printed.
  const byteLen = new Blob([text]).size;
  if (byteLen > 1024 * 1024) {
    showToast('消息过长 (' + Math.ceil(byteLen / 1024) + ' KB > 1024 KB 上限)', 'warning');
    return false;
  }

  // A send references file_ids only, so one mid-upload would silently drop
  // the image (errored uploads: composerSendBlocked).
  if (composer.pendingFiles.some(f => f.status === 'uploading')) {
    showToast('图片上传中，请稍候…', 'warning');
    return false;
  }
  return true;
}

// TAKEOVER_FAIL_TEXT words the class GET discovered_takeover_status reports
// for a takeover that failed after the external CLI was terminated.
const TAKEOVER_FAIL_TEXT = { max_procs: '进程数已满', in_progress: '该会话正被另一次接管占用', shutting_down: '服务正在重启',
  startup_failed: 'CLI 反复启动失败', shim_stuck: '旧会话进程尚未释放', spawn_failed: '新会话启动失败' };
function takeoverFailText(cls) {
  return '接管失败：' + (TAKEOVER_FAIL_TEXT[cls] || TAKEOVER_FAIL_TEXT.spawn_failed) + '，外部 CLI 已终止；'
    + (cls === 'max_procs' ? '请关闭一个空闲会话后' : '请稍后') + '从历史记录重新打开该会话（对话记录仍在）';
}
const TAKEOVER_PROMPT = 'send a message to take over...';
function endTakeover(input, btn, placeholder) {
  if (input) { input.dataset.placeholder = placeholder; input.contentEditable = 'true'; }
  composer.sending = false;
  if (btn) btn.classList.remove('sending');
}

// takeoverThenSend takes over the discovered session the composer shows, waits
// for it to appear among the managed sessions, then sends text to it.
async function takeoverThenSend(input, text) {
  composer.sending = true;
  const btn = document.getElementById('btn-send');
  if (btn) btn.classList.add('sending');
  if (input) { input.dataset.placeholder = '正在接管会话…'; input.contentEditable = 'false'; }
  const pd = selection.pendingDiscovered;
  try {
    const r = await fetch(NZ_CONTRACT.API.discovered_takeover, {
      method: 'POST', headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({pid: pd.pid, session_id: pd.sessionId, cwd: pd.cwd, proc_start_time: pd.procStartTime || 0, node: pd.node || ''})
    });
    if (!r.ok) {
      showAPIError('接管进程', r.status, await r.text().catch(() => ''));
      return endTakeover(input, btn, TAKEOVER_PROMPT);
    }
    const data = await r.json();
    if (!data.key) {
      showToast('接管进程失败：未返回会话标识', 'error');
      return endTakeover(input, btn, TAKEOVER_PROMPT);
    }
    // Remove from discoveredItems so renderSidebar won't re-create the card
    dropDiscovered(pd.pid, pd.node);
    removeSidebarCard(discoveredKey(pd.pid, pd.node));
    selection.pendingDiscovered = null;
    // Poll up to 10s. A local takeover waits for its own attempt to report ready,
    // so it never sends into a session another actor put on the key; any failure
    // ends the wait. A remote takeover or an expired outcome ("unknown") goes by the key alone.
    const takenKey = data.key;
    const takenNode = pd.node || 'local';
    const statusURL = data.takeover_id && takenNode === 'local' ? NZ_CONTRACT.API.discovered_takeover_status + '?id=' + encodeURIComponent(data.takeover_id) : '';
    let own = { state: statusURL ? 'pending' : 'unknown', class: '' }, ready = false;
    for (const end = Date.now() + 10000; !ready && own.state !== 'failed' && Date.now() < end;) {
      await new Promise(resolve => setTimeout(resolve, 500));
      sessionList.lastVersion = 0;
      const [, st] = await Promise.all([shell.fetchSessions(),
        statusURL && fetchJSON(statusURL, { timeoutMs: 2000 }).catch(() => null)]);
      if (st && st.state) own = st;
      ready = (own.state === 'ready' || own.state === 'unknown') && !!sessionList.sessionsData[sid(takenKey, takenNode)];
    }
    if (!ready) {
      showToast(own.state === 'failed' ? takeoverFailText(own.class) : '接管超时：外部 CLI 已终止，但新会话未就绪。对话记录仍在，可稍后从历史记录重新打开', 'error', 8000);
      return endTakeover(input, btn, 'send a message...');
    }
    composer.sending = false;
    shell.selectSession(takenKey, takenNode);
    setMsgValue(document.getElementById('msg-input'), text);
    await sendMessage();
  } catch (e) {
    showNetworkError('接管进程', e);
    endTakeover(input, btn, TAKEOVER_PROMPT);
  }
}

async function sendMessage() {
  if (composer.sending) return;

  // Auto-takeover: if viewing a discovered session, takeover first then send
  if (selection.pendingDiscovered && !selection.key) {
    const input = document.getElementById('msg-input');
    const text = getMsgValue(input);
    if (text) await takeoverThenSend(input, text);
    return;
  }

  if (!selection.key) return;
  const input = document.getElementById('msg-input');
  const text = getMsgValue(input);
  if (!text && composer.pendingFiles.length === 0) return;
  if (!validateComposerForSend(text)) return;

  // #2405 — close the reentrancy gate BEFORE the first await: sendComposerTurn
  // can wait up to ORIENT_MAX_WAIT_MS on orients while the composer stays
  // editable, and every Enter in that wait used to fire a text-only ghost
  // send. `.sending` is the visible cue; the finally is the ONLY reset.
  composer.sending = true;
  const btn = document.getElementById('btn-send');
  if (btn) btn.classList.add('sending');
  try {
    await sendComposerTurn(selection.key, selection.node);
  } finally {
    composer.sending = false;
    if (btn) btn.classList.remove('sending');
  }
}

// composerSendBlocked runs the checks sendComposerTurn still needs after
// validateComposerForSend: a failed upload must be removed/retried, and the
// image batch must fit the shim's line cap (filesBlockSend, shared with the
// Planner's composer).
function composerSendBlocked() {
  return filesBlockSend();
}

// newSendID returns a fresh send ID ('s' + 32 hex). getRandomValues, not
// randomUUID: the dashboard is also served over plain-HTTP LAN addresses,
// where randomUUID does not exist.
function newSendID() {
  const b = new Uint8Array(16);
  crypto.getRandomValues(b);
  return 's' + Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');
}

// sentRemoteFrames holds each remote send as a WS frame until its answer
// settles it, so an "unknown" answer (the node may or may not have it) can be
// retried as the very same frame — same send_id, same spawn picks. An HTTP
// fallback send is held as the frame it would have been; nothing settles an
// accepted one, hence the cap (oldest first).
const sentRemoteFrames = new Map();
const MAX_SENT_REMOTE_FRAMES = 64;

function holdRemoteFrame(id, frame) {
  sentRemoteFrames.set(id, frame);
  if (sentRemoteFrames.size > MAX_SENT_REMOTE_FRAMES) sentRemoteFrames.delete(sentRemoteFrames.keys().next().value);
}

// settleSend forgets send id's frame and the unknown chip on its bubble; the
// ack for it said how the node took it.
export function settleSend(id) {
  if (!id) return;
  sentRemoteFrames.delete(id);
  const chip = bubbleFor(id)?.querySelector('.msg-unknown-chip');
  if (chip) chip.remove();
}

function bubbleFor(id) {
  const sel = (typeof CSS !== 'undefined' && CSS.escape) ? CSS.escape(id) : id;
  return document.querySelector('#events-scroll .optimistic-msg[data-send-id="' + sel + '"]');
}

// markSendUnknown keeps send id's bubble and puts "状态未知，可重试" with a retry
// button on it. An HTTP fallback send made while the socket was down has no
// bubble yet; it gets one when its session is on screen.
export function markSendUnknown(id) {
  const frame = sentRemoteFrames.get(id);
  if (!bubbleFor(id) && frame && frame.key === selection.key && frame.node === selection.node) {
    renderOptimisticUserMsg(frame.text, id);
  }
  const content = bubbleFor(id)?.querySelector('.event-content');
  if (!content) return;
  let chip = content.querySelector('.msg-unknown-chip');
  if (!chip) {
    chip = document.createElement('div');
    chip.className = 'msg-unknown-chip';
    content.appendChild(chip);
  }
  chip.textContent = '状态未知，可重试 ';
  const btn = document.createElement('button');
  btn.type = 'button';
  btn.className = 'msg-retry-btn';
  btn.textContent = '重试';
  btn.addEventListener('click', () => retryUnknownSend(id));
  chip.appendChild(btn);
}

// retryUnknownSend sends id's frame again. The node answers a send_id it has
// taken with that first answer, so the retry cannot start a second turn.
function retryUnknownSend(id) {
  const frame = sentRemoteFrames.get(id);
  if (!frame) { settleSend(id); return; }
  if (!wsm.isConnected() || !wsm.send(frame)) {
    showToast('连接已断开，恢复后再重试', 'warning');
    return;
  }
  // The unknown answer gave the picks back (restoreSpawnFields); the retry
  // carries them again, so it takes them again.
  consumeSpawnFields(frame.key, spawnFields(frame.key), id);
  persistPending();
  const chip = bubbleFor(id)?.querySelector('.msg-unknown-chip');
  if (chip) chip.textContent = '重试中…';
  markSessionOptimisticRunning(frame.key, frame.node);
}

// trySendViaWS is the preferred path for TEXT-ONLY sends. File-bearing sends
// MUST go over HTTP instead: a WS send's uploadStore owner is frozen at
// WebSocket-upgrade time (wsDeriveUploadOwner → setUploadOwner), while
// /api/sessions/upload reads the CURRENT nz_anon cookie — the two can diverge
// once the cookie's 1h TTL (anonCookieMaxAgeSeconds) elapses on a long-lived
// tab, breaking TakeAll with "file not found or expired". HTTP always uses
// the upload's own cookie, so it never hits this. Returns true once the frame
// is out (caller must stop); false to fall through to HTTP (not connected,
// carries files, or wsm.send itself failed).
function trySendViaWS(text, fileIDs, input) {
  if (!wsm.isConnected() || fileIDs.length > 0) return false;
  // One ID names the frame (send_ack echoes it), its bubble and, as send_id,
  // the message across retries: a remote node runs it once per ID.
  const id = newSendID();
  const sendMsg = { type: 'send', key: selection.key, text: text, id: id, send_id: id };
  const remote = selection.node && selection.node !== 'local';
  if (remote) sendMsg.node = selection.node;
  const spawn = spawnFields(selection.key);
  if (!wsm.send(Object.assign(sendMsg, spawn))) return false;
  if (remote) holdRemoteFrame(id, sendMsg);
  // Consume only now that the frame is out: a failed wsm.send above falls
  // through to HTTP, which must still see them.
  consumeSpawnFields(selection.key, spawn, id);
  // Optimistic render: show the user message immediately without waiting
  // for the CLI to echo it back as a "user" event.
  renderOptimisticUserMsg(text, id);
  if (input) clearMsg(input);
  delete perSession.drafts[selection.key];
  clearPendingFiles();
  if (text) perSession.lastSent[sid(selection.key, selection.node)] = text;
  // Confirmed send: workspace/node/backend were consumed above, so rewrite
  // the durable blob without this key — only on this success path, so a
  // failed send (falling through to HTTP) keeps the entry for that retry.
  persistPending();
  return true;
}

// spawnFields returns the spawn-time fields of key's first send: the pending
// workspace, backend and access_profile, each omitted when unset.
function spawnFields(key) {
  const f = {};
  if (perSession.workspaces[key]) f.workspace = perSession.workspaces[key];
  if (perSession.backends[key]) f.backend = perSession.backends[key];
  if (perSession.accessProfiles[key]) f.access_profile = perSession.accessProfiles[key];
  return f;
}

// consumeSpawnFields forgets the fields send sendId carried: the server reads
// them once, at spawn, so no later send may carry them. They move to
// sentPicks, display-only until listed, and restoreSpawnFields' source.
function consumeSpawnFields(key, f, sendId) {
  if (f.workspace || f.backend || f.access_profile) perSession.sentPicks[key] = { backend: f.backend || '', accessProfile: f.access_profile || '', workspace: f.workspace || '', node: perSession.nodes[key] || '', sendId };
  if (f.workspace) { delete perSession.workspaces[key]; delete perSession.nodes[key]; }
  delete perSession.backends[key];
  delete perSession.accessProfiles[key];
}

// restoreSpawnFields undoes it for send sendId once rejected before spawn (HTTP
// 4xx/5xx, send_ack error), so the retry carries the picks. Another
// send's rejection, or a listed key, keeps them consumed.
export function restoreSpawnFields(key, sendId) {
  const p = perSession.sentPicks[key];
  if (!p || p.sendId !== sendId || sessionList.sessionsData[sid(key, p.node || 'local')]) return;
  if (p.workspace) { perSession.workspaces[key] = p.workspace; if (p.node) perSession.nodes[key] = p.node; }
  if (p.backend) perSession.backends[key] = p.backend;
  if (p.accessProfile) perSession.accessProfiles[key] = p.accessProfile;
  delete perSession.sentPicks[key];
  persistPending();
}

// buildSendPayload builds the HTTP POST body, consuming the spawn fields.
function buildSendPayload(text, fileIDs, sendId) {
  const payload = { key: selection.key, text: text, send_id: sendId };
  if (fileIDs.length > 0) payload.file_ids = fileIDs;
  if (selection.node && selection.node !== 'local') payload.node = selection.node;
  const spawn = spawnFields(selection.key);
  consumeSpawnFields(selection.key, spawn, sendId);
  if (payload.node && fileIDs.length === 0) {
    holdRemoteFrame(sendId, Object.assign({ type: 'send', key: payload.key, text: text, id: sendId, send_id: sendId, node: payload.node }, spawn));
  }
  return Object.assign(payload, spawn);
}

// handleSendRejected runs the !r.ok branch: restores the composer and the
// optimistic-running flip, then surfaces 401/403 (auth modal) or the
// generic API error. files_consumed means the server already
// took the pre-uploaded attachments out of the uploadStore before rejecting
// (post-TakeAll 4xx/5xx) — drop the now-dead ids and ask for a re-attach;
// pre-TakeAll rejections never set the flag, so unsent attachments stay put.
async function handleSendRejected(r, sentSid, text, input) {
  perSession.httpSendPending.delete(sentSid); // rejected synchronously — no async frame will follow
  if (input) setMsgValue(input, text);
  rollbackOptimisticRunning(selection.key, selection.node);
  // Some error paths still write text/plain; fall back to text() so we
  // always surface the real message instead of a generic "send failed".
  const raw = await r.text().catch(() => '');
  let detail = '', filesConsumed = false;
  try {
    const j = JSON.parse(raw);
    if (j && j.error) detail = j.error;
    if (j && j.files_consumed) filesConsumed = true;
  } catch (_) { if (raw) detail = raw; }
  if (filesConsumed) {
    clearPendingFiles();
    showToast('附件已失效，请重新添加后再发送', 'warning');
  }
  if (r.status === 401 || r.status === 403) {
    showAuthModal();
    return;
  }
  showAPIError('发送消息', r.status, detail);
}

// finishHttpSend runs the 2xx branch: clears the composer, then rolls the
// pre-send optimistic flip back on ack:"reset" (/clear, /new — no turn
// spawned) and otherwise mirrors the WS path's optimistic bubble while WS
// is live (the WS-down fallback keeps its legacy no-bubble behaviour).
async function finishHttpSend(r, sentSid, text, input, sendId) {
  // Record the sent text BEFORE awaiting the body (interrupt re-fill
  // source; also a secondary onSendError gate).
  if (text) perSession.lastSent[sentSid] = text;
  let ackStatus = '';
  try { const j = await r.json(); if (j && j.status) ackStatus = j.status; } catch (_) {}

  if (input) clearMsg(input);
  delete perSession.drafts[selection.key];
  clearPendingFiles();
  // Confirmed send: the pending maps were consumed in buildSendPayload;
  // rewrite the durable blob without this key, same as the WS path.
  persistPending();
  if (ackStatus === 'reset') {
    rollbackOptimisticRunning(selection.key, selection.node);
    delete perSession.lastSent[sentSid]; // no turn ran, nothing to re-fill on interrupt
    perSession.httpSendPending.delete(sentSid);
  } else if (wsm.isConnected()) {
    renderOptimisticUserMsg(text, sendId);
  }
  armFallbackEventPoll();
}

// armFallbackEventPoll speeds up event polling for 15s after an HTTP send
// lands while WS is down, then backs off to the normal interval. No-op when
// WS is connected: the live event stream already pushes updates.
function armFallbackEventPoll() {
  if (wsm.isConnected()) return;
  if (timers.events) clearInterval(timers.events);
  timers.events = setInterval(() => shell.fetchEvents(false), 500);
  setTimeout(() => {
    if (timers.events) clearInterval(timers.events);
    if (!wsm.isConnected()) timers.events = setInterval(() => shell.fetchEvents(false), 1000);
  }, 15000);
}

// sendComposerTurn is the gated half of sendMessage: the caller holds the
// `composer.sending` gate for its whole duration. targetKey/targetNode are the session
// the operator hit send on; a switch during the orient wait aborts the send
// rather than redirecting the captured text to the newly selected session.
async function sendComposerTurn(targetKey, targetNode) {
  // Auto-orient (maybeAutoOrient) runs as a fire-and-forget vision side-call
  // after upload; transparently wait for it to settle (capped at
  // ORIENT_MAX_WAIT_MS) so the rotated bytes land before we consume
  // file_ids. Silent by design — the user already clicked send.
  await awaitPendingOrients();
  if (!selection.key || selection.key !== targetKey || selection.node !== targetNode) return;
  // The composer stayed editable during the wait: re-read text/files so an
  // id-less upload dropped mid-wait doesn't silently vanish from fileIDs.
  const input = document.getElementById('msg-input');
  const text = getMsgValue(input);
  if (!text && composer.pendingFiles.length === 0) return;
  if (!validateComposerForSend(text)) return;
  if (composerSendBlocked()) return;
  const fileIDs = composer.pendingFiles.map(f => f.id).filter(Boolean);

  // Flip the send→stop button + running banner BEFORE the network round trip,
  // not after — a resumed session has no CLI process yet, so the first send
  // triggers a subprocess spawn that can take several hundred ms. Leaving the
  // green send button visible during that window makes the click feel ignored
  // and invites double-sends. onSendAck/rollbackOptimisticRunning undo this on
  // busy/error/reset; the 20s safety timer in markSessionOptimisticRunning
  // prevents a stuck banner if the server never responds.
  markSessionOptimisticRunning(selection.key, selection.node);

  if (trySendViaWS(text, fileIDs, input)) return;

  // HTTP POST fallback — JSON only; files already on server.
  try {
    const headers = { 'Content-Type': 'application/json' };
    const sendId = newSendID();
    const payload = buildSendPayload(text, fileIDs, sendId);

    // Mark this tab as the originator BEFORE the request leaves (text or
    // image-only alike): a send_error for this turn can arrive over the WS
    // any time after the server has the request.
    const sentSid = sid(selection.key, selection.node);
    perSession.httpSendPending.add(sentSid);
    const r = await fetch(NZ_CONTRACT.API.sessions_send, {method:'POST', headers, body: JSON.stringify(payload)});
    if (!r.ok) {
      restoreSpawnFields(targetKey, sendId);
      await handleSendRejected(r, sentSid, text, input);
      return;
    }
    await finishHttpSend(r, sentSid, text, input, sendId);
  } catch (e) {
    perSession.httpSendPending.delete(sid(selection.key, selection.node));
    if (input) setMsgValue(input, text);
    rollbackOptimisticRunning(selection.key, selection.node);
    showNetworkError('发送消息', e);
  }
}

// renderOptimisticUserMsg appends the just-sent text as an optimistic user
// bubble; the server's real "user" event (onHistory/onEvent) removes the
// `.optimistic-msg`, which is why both callers (WS sends, HTTP sends while WS
// is live) need a live subscription. No-op without text (an image-only send's
// thumbnails come with the real event). `sendId` (WS path) lets a busy/error
// send_ack roll back exactly this send.
function renderOptimisticUserMsg(text, sendId) {
  const el = document.getElementById('events-scroll');
  if (!el || !text) return;
  const now = Date.now();
  const html = eventHtml({type: 'user', detail: text, time: now});
  if (!html) return;
  const prevT = lastDividerTime(el);
  if (prevT === 0 || now - prevT >= EVENT_DIVIDER_GAP_MS) {
    el.insertAdjacentHTML('beforeend', timeDividerHtml(now));
  }
  el.insertAdjacentHTML('beforeend', html);
  el.lastElementChild.classList.add('optimistic-msg');
  if (sendId) el.lastElementChild.setAttribute('data-send-id', sendId);
  // Always force-bottom after a send, even if scrolled up; stickEventsBottom
  // outlasts the input-area collapse and lazy images.
  stickEventsBottom();
  navSync();
}

function clearPendingFiles() {
  composer.pendingFiles.forEach(f => { if (f.blobUrl) URL.revokeObjectURL(f.blobUrl); });
  composer.pendingFiles = [];
  renderFilePreviews();
}

// markSessionOptimisticRunning flips the selected session's local state to
// 'running' immediately after send succeeds so the running-banner shows
// without waiting for the server's session_state broadcast. The server can
// take 100ms–several seconds to emit BroadcastSessionReady when GetOrCreate
// has to spawn a new CLI subprocess, while the turn is already queued.
// Rolled back by onSendAck on 'busy'/'error' so a rejected send doesn't leave
// a stuck banner; a 20s safety timer stops a lost session_state push from
// keeping the banner stuck forever.
const _optimisticRunningTimers = {};

// patchSidebarCardState updates the sidebar card's status dot and exit chip in
// place so an optimistic running flip is reflected on the left list at the
// same instant as the main conversation, instead of lagging behind the
// server's session_state push (which can be several hundred ms when a CLI
// subprocess has to spawn) or the 5s sessions poll. Mirrors the DOM-patch
// branch in onSessionState. No-op if the card isn't currently rendered.
function patchSidebarCardState(key, node, state) {
  const msgNode = node || 'local';
  let card = null;
  document.querySelectorAll('.session-card').forEach(c => {
    if (c.dataset.key === key && (c.dataset.node || 'local') === msgNode) card = c;
  });
  if (!card) return;
  patchCardHealth(card, Object.assign({}, sessionList.sessionsData[sid(key, msgNode)], { state }));
}

function markSessionOptimisticRunning(key, node) {
  if (!key) return;
  const sKey = sid(key, node || 'local');
  const sd = sessionList.sessionsData[sKey];
  if (sd && sd.state === 'running') return; // server already said running
  // A just-created session has no sessionList.sessionsData entry until the next list
  // fetch. Still flip the button/banner below (#2405: the missing feedback on
  // a new session's first send invited Enter mashing); shell.fetchSessions keeps the
  // flag-forced 'running' once the entry lands, onSessionState clears it.
  if (sd) {
    perSession.optimisticPrevState[sKey] = sd.state;
    sd.state = 'running';
  }
  perSession.optimisticRunning[sKey] = true;
  // Sidebar parity: flip the card's dot/label to running right now so the
  // left list never looks idle while the main banner already says working.
  patchSidebarCardState(key, node, 'running');
  if (_optimisticRunningTimers[sKey]) clearTimeout(_optimisticRunningTimers[sKey]);
  _optimisticRunningTimers[sKey] = setTimeout(() => {
    delete _optimisticRunningTimers[sKey];
    // Only rollback if still optimistic (no real running state arrived).
    if (perSession.optimisticRunning[sKey]) {
      rollbackOptimisticRunning(key, node);
    }
  }, 20000);
  if (key === selection.key && (node || 'local') === selection.node) {
    // justSent makes the banner's first line read "已发送，正在处理…" until the
    // first real event (tool/thinking/output) arrives, so the operator gets a
    // distinct "received, starting up" signal during CLI spawn rather than a
    // generic static "处理中…".
    turnState.justSent = true;
    startTurnTimer();
    updateSendButton('running');
  }
}

function rollbackOptimisticRunning(key, node) {
  if (!key) return;
  const sKey = sid(key, node || 'local');
  if (!perSession.optimisticRunning[sKey]) return;
  delete perSession.optimisticRunning[sKey];
  delete perSession.optimisticPrevState[sKey];
  if (_optimisticRunningTimers[sKey]) {
    clearTimeout(_optimisticRunningTimers[sKey]);
    delete _optimisticRunningTimers[sKey];
  }
  const sd = sessionList.sessionsData[sKey];
  if (sd && sd.state === 'running') {
    sd.state = 'ready';
    patchSidebarCardState(key, node, 'ready');
  }
  // The flip may have been applied without a sessionList.sessionsData entry (new session's
  // first send) — restore the button either way.
  if (key === selection.key && (node || 'local') === selection.node) updateSendButton('ready', { silent: true });
}


export {
  _optimisticRunningTimers,
  clearPendingFiles,
  handleKey,
  markSessionOptimisticRunning,
  renderOptimisticUserMsg,
  rollbackOptimisticRunning,
  sendMessage,
};
