// send_message.js — extracted from dashboard.js (#2558 D4).
//
// Verbatim region move: `git diff --color-moved` shows the body as a pure
// move; the import block, the deps table and the export block below are the
// only additions.
//
// Layering (D4-1 rule): a module dashboard imports must NOT import dashboard
// back — that cycle puts dashboard's own top-level consts in TDZ while this
// module evaluates. Shared state is read from the state.js objects; its helpers are
// injected once via configureSendMessage(), called from dashboard's module body.
import { NZ_CONTRACT } from './contract.js';
import { composer, perSession, selection, sessionList, timers } from './state.js';
import { turnState } from './running_banner.js';
import { showToast, patchCardExitChip } from './nz_util.js';
import { wsm } from './ws_manager.js';
import { featureForCurrent } from './features.js';

const deps = {
  EVENT_DIVIDER_GAP_MS: null,
  awaitPendingOrients: null,
  discoveredKey: null,
  dropDiscovered: null,
  eventHtml: null,
  fetchEvents: null,
  fetchSessions: null,
  getToken: null,
  interruptSession: null,
  lastDividerTime: null,
  navSync: null,
  persistPending: null,
  removeSidebarCard: null,
  renderFilePreviews: null,
  selectSession: null,
  showAPIError: null,
  showAuthModal: null,
  showNetworkError: null,
  sid: null,
  startTurnTimer: null,
  stickEventsBottom: null,
  timeDividerHtml: null,
  updateSendButton: null,
};
export function configureSendMessage(impl) {
  for (const k of Object.keys(deps)) {
    if (typeof impl[k] === 'undefined') throw new Error('send_message dep missing: ' + k);
    deps[k] = impl[k];
  }
}

// --- Send message ---

// Esc in the input: first press arms, second press (within 600ms) actually
// interrupts the running turn. Prevents thumb-on-Esc misfires.
let _lastEscAt = 0;
function handleKey(e) {
  if (e.key === 'Escape') {
    e.preventDefault();
    const sd = sessionList.sessionsData[deps.sid(selection.key, selection.node || 'local')];
    const running = sd && sd.state === 'running';
    if (!running) { _lastEscAt = 0; return; }
    const now = Date.now();
    if (now - _lastEscAt < 600) {
      _lastEscAt = 0;
      deps.interruptSession();
    } else {
      _lastEscAt = now;
      showToast('再按一次 Esc 发送中断', 'warning', 1000);
    }
    return;
  }
  if (e.key === 'Enter' && !e.shiftKey && !e.isComposing && Date.now() - composer.lastCompositionEnd > 30) { e.preventDefault(); sendMessage(); }
}

function getMsgValue(el) { return (el ? el.innerText : '').trim(); }
function setMsgValue(el, v) { if (el) el.innerText = v; }
function clearMsg(el) { if (el) el.textContent = ''; }

// validateComposerForSend runs the synchronous pre-send checks that depend on
// the live composer (text-level backend feature gates, byte cap, in-flight
// uploads). Toasts and returns false when the send must abort. sendMessage
// calls it twice: once before closing the reentrancy gate and again after
// `await deps.awaitPendingOrients()` — the composer stays editable during that wait,
// so text and attachments captured before the await can be stale (#2405).
function validateComposerForSend(text) {
  // Multi-Backend RFC §8.3 D9 — `/urgent` requires the backend's
  // `passthrough` feature (preempt the running turn with a fresh user
  // message). kiro / ACP backends don't preempt; the server-side
  // dispatcher would either error or queue the message confusingly.
  // Toast and abort send so the operator is told *why* before they
  // wonder where their preemption went. Title-attr on /urgent button
  // would be ideal but /urgent is a text prefix typed in the input;
  // detect at send time instead.
  if (text && /^\s*\/urgent\b/.test(text) && !featureForCurrent('passthrough')) {
    showToast('当前后端不支持 /urgent 抢占（请用 Esc 中断后再发）', 'warning');
    return false;
  }

  // Multi-Backend RFC §8.3 D13 — `@-mention` embedded context only
  // works when the backend reads file paths from inside the prompt
  // (claude does; kiro doesn't). Strip-and-warn would silently change
  // the prompt; better to abort + toast so the operator can paste the
  // absolute path or content explicitly.
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

  // Block send while any attachment is still uploading or errored —
  // we only reference file_ids on the server, so partial uploads would
  // silently drop images. User can retry or remove the bad one.
  if (composer.pendingFiles.some(f => f.status === 'uploading')) {
    showToast('图片上传中，请稍候…', 'warning');
    return false;
  }
  return true;
}

async function sendMessage() {
  if (composer.sending) return;

  // Auto-takeover: if viewing a discovered session, takeover first then send
  if (selection.pendingDiscovered && !selection.key) {
    const input = document.getElementById('msg-input');
    const text = getMsgValue(input);
    if (!text) return;
    composer.sending = true;
    const btn = document.getElementById('btn-send');
    if (btn) btn.classList.add('sending');
    if (input) input.dataset.placeholder = '正在接管会话…';
    if (input) input.contentEditable = 'false';
    const pd = selection.pendingDiscovered;
    try {
      const headers = {'Content-Type': 'application/json'};
      const token = deps.getToken();
      if (token) headers['Authorization'] = 'Bearer ' + token;
      const r = await fetch(NZ_CONTRACT.API.discovered_takeover, {
        method: 'POST', headers,
        body: JSON.stringify({pid: pd.pid, session_id: pd.sessionId, cwd: pd.cwd, proc_start_time: pd.procStartTime || 0, node: pd.node || ''})
      });
      if (!r.ok) {
        const errText = await r.text().catch(() => '');
        deps.showAPIError('接管进程', r.status, errText);
        if (input) { input.dataset.placeholder = 'send a message to take over...'; input.contentEditable = 'true'; }
        composer.sending = false;
        if (btn) btn.classList.remove('sending');
        return;
      }
      const data = await r.json();
      if (!data.key) {
        showToast('接管进程失败：未返回会话标识', 'error');
        if (input) { input.dataset.placeholder = 'send a message to take over...'; input.contentEditable = 'true'; }
        composer.sending = false;
        if (btn) btn.classList.remove('sending');
        return;
      }
      // Remove from discoveredItems so renderSidebar won't re-create the card
      deps.dropDiscovered(pd.pid, pd.node);
      // Remove the discovered card from sidebar
      deps.removeSidebarCard(deps.discoveredKey(pd.pid, pd.node));
      selection.pendingDiscovered = null;
      // Poll until the session appears in managed sessions (up to 10s)
      const takenKey = data.key;
      const takenNode = pd.node || 'local';
      let ready = false;
      for (let i = 0; i < 20; i++) {
        await new Promise(resolve => setTimeout(resolve, 500));
        sessionList.lastVersion = 0;
        await deps.fetchSessions();
        if (sessionList.sessionsData[deps.sid(takenKey, takenNode)]) { ready = true; break; }
      }
      if (!ready) {
        showToast('接管超时：会话未就绪，请稍后重试', 'error');
        if (input) { input.dataset.placeholder = 'send a message...'; input.contentEditable = 'true'; }
        composer.sending = false;
        if (btn) btn.classList.remove('sending');
        return;
      }
      // Session is ready — switch to it and send the message
      composer.sending = false;
      deps.selectSession(takenKey, takenNode);
      // Restore the message text and send
      const newInput = document.getElementById('msg-input');
      if (newInput) setMsgValue(newInput, text);
      await sendMessage();
      return;
    } catch (e) {
      deps.showNetworkError('接管进程', e);
      if (input) { input.dataset.placeholder = 'send a message to take over...'; input.contentEditable = 'true'; }
      composer.sending = false;
      if (btn) btn.classList.remove('sending');
      return;
    }
  }

  if (!selection.key) return;
  const input = document.getElementById('msg-input');
  const text = getMsgValue(input);
  if (!text && composer.pendingFiles.length === 0) return;
  if (!validateComposerForSend(text)) return;

  // #2405 — close the reentrancy gate BEFORE the first await. sendComposerTurn
  // starts with `await deps.awaitPendingOrients()`, which can block for up to
  // ORIENT_MAX_WAIT_MS while the composer stays editable. With the gate set
  // only after that await, every Enter pressed during the wait spawned another
  // sendMessage that captured the same text; once orient settled, waiter #1
  // sent text+file_ids and cleared the composer while #2..N each fired a
  // text-only ghost over WS. The `.sending` class is the visible cue that the
  // click registered. The finally is the ONLY reset — every exit path of the
  // gated half goes through it.
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

// composerSendBlocked runs the two checks sendComposerTurn still needs after
// validateComposerForSend: a failed upload must be removed/retried, and the
// shim's 12 MB NDJSON line cap (base64 inflates ~1.33×, so raw image bytes
// must stay under ~9 MB) needs a clear toast. PDFs are excluded — they
// travel as file_ref, so a mixed image+PDF send must not trip the cap on
// the PDF's own 20 MB limit.
function composerSendBlocked() {
  const failed = composer.pendingFiles.filter(f => f.status === 'error');
  if (failed.length > 0) {
    const detail = failed[0].error || '';
    const tail = detail ? '（' + detail.slice(0, 120) + '）' : '';
    showToast('图片上传失败' + tail + '，请移除或重试', 'error');
    return true;
  }
  const totalBytes = composer.pendingFiles.reduce((n, f) => {
    if (f.kind === 'pdf' || f.serverKind === 'file_ref') return n;
    return n + (f.normalizedSize || f.file.size || 0);
  }, 0);
  if (totalBytes > 9 * 1024 * 1024) {
    showToast('图片总大小 ' + Math.ceil(totalBytes / 1024 / 1024) + ' MB 超过 9 MB 上限，请分批发送或减少图片', 'warning');
    return true;
  }
  return false;
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
  const id = 'r' + (++wsm.sendCounter);
  const sendMsg = { type: 'send', key: selection.key, text: text, id: id };
  if (selection.node && selection.node !== 'local') sendMsg.node = selection.node;
  if (perSession.workspaces[selection.key]) sendMsg.workspace = perSession.workspaces[selection.key];
  if (perSession.backends[selection.key]) sendMsg.backend = perSession.backends[selection.key];
  if (perSession.accessProfiles[selection.key]) sendMsg.access_profile = perSession.accessProfiles[selection.key];
  if (!wsm.send(sendMsg)) return false;
  // Workspace/backend/access profile are consumed once on session spawn;
  // forget them only now that the frame is out — a failed wsm.send above
  // falls through to HTTP, which must still see them.
  if (sendMsg.workspace) {
    delete perSession.workspaces[selection.key];
    delete perSession.nodes[selection.key];
  }
  delete perSession.backends[selection.key];
  delete perSession.accessProfiles[selection.key];
  // Optimistic render: show the user message immediately without waiting
  // for the CLI to echo it back as a "user" event.
  renderOptimisticUserMsg(text, id);
  if (input) clearMsg(input);
  delete perSession.drafts[selection.key];
  clearPendingFiles();
  if (text) perSession.lastSent[deps.sid(selection.key, selection.node)] = text;
  // Confirmed send: workspace/node/backend were consumed above, so rewrite
  // the durable blob without this key — only on this success path, so a
  // failed send (falling through to HTTP) keeps the entry for that retry.
  deps.persistPending();
  return true;
}

// buildSendPayload builds the HTTP POST body and, in the same pass, consumes
// (deletes) the pending workspace/backend/access_profile for this session —
// they are spawn-time-only inputs the server reads once, so they must not
// ride along on a retry of the same POST.
function buildSendPayload(text, fileIDs) {
  const payload = { key: selection.key, text: text };
  if (fileIDs.length > 0) payload.file_ids = fileIDs;
  if (selection.node && selection.node !== 'local') payload.node = selection.node;
  if (perSession.workspaces[selection.key]) {
    payload.workspace = perSession.workspaces[selection.key];
    delete perSession.workspaces[selection.key];
    delete perSession.nodes[selection.key];
  }
  if (perSession.backends[selection.key]) {
    payload.backend = perSession.backends[selection.key];
    delete perSession.backends[selection.key];
  }
  if (perSession.accessProfiles[selection.key]) {
    payload.access_profile = perSession.accessProfiles[selection.key];
    delete perSession.accessProfiles[selection.key];
  }
  return payload;
}

// handleSendRejected runs the !r.ok branch: restores the composer and the
// optimistic-running flip, then surfaces 401/403 (auth modal), 429 (rate
// limiter) or the generic API error. files_consumed means the server already
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
    deps.showAuthModal();
    return;
  }
  if (r.status === 429) {
    // The server names the limiter that fired; there is no queue-full 429 here.
    showToast(detail || '请求过于频繁，请稍后重试', 'warning');
    return;
  }
  deps.showAPIError('发送消息', r.status, detail);
}

// finishHttpSend runs the 2xx branch: clears the composer, then rolls the
// pre-send optimistic flip back on ack:"reset" (/clear, /new — no turn
// spawned) and otherwise mirrors the WS path's optimistic bubble while WS
// is live (the WS-down fallback keeps its legacy no-bubble behaviour).
async function finishHttpSend(r, sentSid, text, input) {
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
  deps.persistPending();
  if (ackStatus === 'reset') {
    rollbackOptimisticRunning(selection.key, selection.node);
    delete perSession.lastSent[sentSid]; // no turn ran, nothing to re-fill on interrupt
    perSession.httpSendPending.delete(sentSid);
  } else if (wsm.isConnected()) {
    renderOptimisticUserMsg(text);
  }
  armFallbackEventPoll();
}

// armFallbackEventPoll speeds up event polling for 15s after an HTTP send
// lands while WS is down, then backs off to the normal interval. No-op when
// WS is connected: the live event stream already pushes updates.
function armFallbackEventPoll() {
  if (wsm.isConnected()) return;
  if (timers.events) clearInterval(timers.events);
  timers.events = setInterval(() => deps.fetchEvents(false), 500);
  setTimeout(() => {
    if (timers.events) clearInterval(timers.events);
    if (!wsm.isConnected()) timers.events = setInterval(() => deps.fetchEvents(false), 1000);
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
  await deps.awaitPendingOrients();
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
    const token = deps.getToken();
    if (token) headers['Authorization'] = 'Bearer ' + token;
    const payload = buildSendPayload(text, fileIDs);

    // Mark this tab as the originator BEFORE the request leaves (text or
    // image-only alike): a send_error for this turn can arrive over the WS
    // any time after the server has the request.
    const sentSid = deps.sid(selection.key, selection.node);
    perSession.httpSendPending.add(sentSid);
    const r = await fetch(NZ_CONTRACT.API.sessions_send, {method:'POST', headers, body: JSON.stringify(payload)});
    if (!r.ok) {
      await handleSendRejected(r, sentSid, text, input);
      return;
    }
    await finishHttpSend(r, sentSid, text, input);
  } catch (e) {
    perSession.httpSendPending.delete(deps.sid(selection.key, selection.node));
    if (input) setMsgValue(input, text);
    rollbackOptimisticRunning(selection.key, selection.node);
    deps.showNetworkError('发送消息', e);
  }
}

// renderOptimisticUserMsg appends the just-sent text as an optimistic user
// bubble at the bottom of the events scroller. The real "user" event pushed
// by the server (onHistory/onEvent) removes the `.optimistic-msg` element
// when it arrives. Shared by the WS send path and the HTTP send path used
// for file-bearing sends (owner-divergence fix) — both run under a live WS
// subscription, which is what guarantees the removal side fires. No-op when
// text is empty (image-only sends have no text to echo; the thumbnails
// arrive with the real user event). `sendId` (WS path) is stamped on the
// bubble so a busy/error send_ack can roll back exactly this send.
function renderOptimisticUserMsg(text, sendId) {
  const el = document.getElementById('events-scroll');
  if (!el || !text) return;
  const now = Date.now();
  const html = deps.eventHtml({type: 'user', detail: text, time: now});
  if (!html) return;
  const prevT = deps.lastDividerTime(el);
  if (prevT === 0 || now - prevT >= deps.EVENT_DIVIDER_GAP_MS) {
    el.insertAdjacentHTML('beforeend', deps.timeDividerHtml(now));
  }
  el.insertAdjacentHTML('beforeend', html);
  el.lastElementChild.classList.add('optimistic-msg');
  if (sendId) el.lastElementChild.setAttribute('data-send-id', sendId);
  // Always force-bottom after a send: the user just posted something and
  // expects to see it, even if they had scrolled up to browse earlier
  // history. deps.stickEventsBottom handles async layout changes from input-area
  // collapse and lazy images.
  deps.stickEventsBottom();
  deps.navSync();
}

function clearPendingFiles() {
  composer.pendingFiles.forEach(f => { if (f.blobUrl) URL.revokeObjectURL(f.blobUrl); });
  composer.pendingFiles = [];
  deps.renderFilePreviews();
}

// markSessionOptimisticRunning flips the selected session's local state to
// 'running' immediately after send succeeds so the running-banner shows
// without waiting for the server's session_state broadcast. The server can
// take 100ms–several seconds to emit BroadcastSessionReady when GetOrCreate
// has to spawn a new CLI subprocess, during which the dashboard previously
// looked idle even though the turn was already queued. Rolled back by
// onSendAck on 'busy'/'error' so a rejected send doesn't leave a stuck banner.
// Tracked with a 20s safety timer so a lost session_state push can't keep
// the banner stuck forever.
const _optimisticRunningTimers = {};

// patchSidebarCardState updates the sidebar card's status dot + label text in
// place so an optimistic running flip is reflected on the left list at the
// same instant as the main conversation, instead of lagging behind the
// server's session_state push (which can be several hundred ms when a CLI
// subprocess has to spawn) or the 5s sessions poll. Mirrors the DOM-patch
// branch in onSessionState. No-op if the card isn't currently rendered.
function patchSidebarCardState(key, node, state) {
  const msgNode = node || 'local';
  const displayState = state === 'dead' ? 'ready' : state;
  let card = null;
  document.querySelectorAll('.session-card').forEach(c => {
    if (c.dataset.key === key && (c.dataset.node || 'local') === msgNode) card = c;
  });
  if (!card) return;
  const dot = card.querySelector('.sc-dot');
  if (dot) {
    dot.className = 'sc-dot ' + (displayState === 'running' ? 'dot-running' : (displayState === 'ready' ? 'dot-ready' : 'dot-new'));
  }
  const meta = card.querySelector('.sc-meta');
  if (meta) {
    const stateSpan = meta.querySelectorAll('span')[1]; // [0]=dot, [1]=state text
    if (stateSpan && !stateSpan.classList.contains('sc-node')) stateSpan.textContent = displayState;
  }
  const sd = sessionList.sessionsData[deps.sid(key, msgNode)];
  patchCardExitChip(card, state, sd ? sd.death_reason : '');
}

function markSessionOptimisticRunning(key, node) {
  if (!key) return;
  const sKey = deps.sid(key, node || 'local');
  const sd = sessionList.sessionsData[sKey];
  if (sd && sd.state === 'running') return; // server already said running
  // A just-created session has no sessionList.sessionsData entry until the next list
  // fetch. Still flip the button/banner below (#2405: the missing feedback on
  // a new session's first send invited Enter mashing); deps.fetchSessions keeps the
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
    deps.startTurnTimer();
    deps.updateSendButton('running');
  }
}

function rollbackOptimisticRunning(key, node) {
  if (!key) return;
  const sKey = deps.sid(key, node || 'local');
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
  if (key === selection.key && (node || 'local') === selection.node) deps.updateSendButton('ready');
}


export {
  _optimisticRunningTimers,
  clearPendingFiles,
  getMsgValue,
  handleKey,
  markSessionOptimisticRunning,
  renderOptimisticUserMsg,
  rollbackOptimisticRunning,
  sendMessage,
  setMsgValue,
};
