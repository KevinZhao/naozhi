// send_message.js — extracted from dashboard.js (#2558 D4).
//
// Verbatim region move: `git diff --color-moved` shows the body as a pure
// move; the import block, the deps table and the export block below are the
// only additions.
//
// Layering (D4-1 rule): a module dashboard imports must NOT import dashboard
// back — that cycle puts dashboard's own top-level consts in TDZ while this
// module evaluates. Dashboard state is read through nz.state; its helpers are
// injected once via configureSendMessage(), called from dashboard's module body.
import { nzState, showToast } from './nz_util.js';

const deps = {
  EVENT_DIVIDER_GAP_MS: null,
  awaitPendingOrients: null,
  discoveredKey: null,
  dropDiscovered: null,
  eventHtml: null,
  featureForCurrent: null,
  fetchEvents: null,
  fetchSessions: null,
  getToken: null,
  httpSendPending: null,
  interruptSession: null,
  lastDividerTime: null,
  navUpdatePill: null,
  persistPending: null,
  removeSidebarCard: null,
  renderFilePreviews: null,
  selectSession: null,
  sessionAccessProfiles: null,
  sessionBackends: null,
  sessionNodes: null,
  sessionOptimisticPrevState: null,
  sessionOptimisticRunning: null,
  sessionWorkspaces: null,
  showAPIError: null,
  showAuthModal: null,
  showNetworkError: null,
  sid: null,
  startTurnTimer: null,
  stickEventsBottom: null,
  timeDividerHtml: null,
  updateSendButton: null,
  wsm: null,
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
    const sd = nzState.sessionsData[deps.sid(nzState.selectedKey, nzState.selectedNode || 'local')];
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
  if (e.key === 'Enter' && !e.shiftKey && !e.isComposing && Date.now() - nzState.lastCompositionEnd > 30) { e.preventDefault(); sendMessage(); }
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
  if (text && /^\s*\/urgent\b/.test(text) && !deps.featureForCurrent('passthrough')) {
    showToast('当前后端不支持 /urgent 抢占（请用 Esc 中断后再发）', 'warning');
    return false;
  }

  // Multi-Backend RFC §8.3 D13 — `@-mention` embedded context only
  // works when the backend reads file paths from inside the prompt
  // (claude does; kiro doesn't). Strip-and-warn would silently change
  // the prompt; better to abort + toast so the operator can paste the
  // absolute path or content explicitly.
  if (text && /(?:^|\s)@[\w./-]/.test(text) && !deps.featureForCurrent('embedded_context')) {
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
  if (nzState.pendingFiles.some(f => f.status === 'uploading')) {
    showToast('图片上传中，请稍候…', 'warning');
    return false;
  }
  return true;
}

async function sendMessage() {
  if (nzState.sending) return;

  // Auto-takeover: if viewing a discovered session, takeover first then send
  if (nzState.pendingDiscovered && !nzState.selectedKey) {
    const input = document.getElementById('msg-input');
    const text = getMsgValue(input);
    if (!text) return;
    nzState.sending = true;
    const btn = document.getElementById('btn-send');
    if (btn) btn.classList.add('sending');
    if (input) input.dataset.placeholder = '正在接管会话…';
    if (input) input.contentEditable = 'false';
    const pd = nzState.pendingDiscovered;
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
        nzState.sending = false;
        if (btn) btn.classList.remove('sending');
        return;
      }
      const data = await r.json();
      if (!data.key) {
        showToast('接管进程失败：未返回会话标识', 'error');
        if (input) { input.dataset.placeholder = 'send a message to take over...'; input.contentEditable = 'true'; }
        nzState.sending = false;
        if (btn) btn.classList.remove('sending');
        return;
      }
      // Remove from discoveredItems so renderSidebar won't re-create the card
      deps.dropDiscovered(pd.pid, pd.node);
      // Remove the discovered card from sidebar
      deps.removeSidebarCard(deps.discoveredKey(pd.pid, pd.node));
      nzState.pendingDiscovered = null;
      // Poll until the session appears in managed sessions (up to 10s)
      const takenKey = data.key;
      const takenNode = pd.node || 'local';
      let ready = false;
      for (let i = 0; i < 20; i++) {
        await new Promise(resolve => setTimeout(resolve, 500));
        nzState.lastVersion = 0;
        await deps.fetchSessions();
        if (nzState.sessionsData[deps.sid(takenKey, takenNode)]) { ready = true; break; }
      }
      if (!ready) {
        showToast('接管超时：会话未就绪，请稍后重试', 'error');
        if (input) { input.dataset.placeholder = 'send a message...'; input.contentEditable = 'true'; }
        nzState.sending = false;
        if (btn) btn.classList.remove('sending');
        return;
      }
      // Session is ready — switch to it and send the message
      nzState.sending = false;
      deps.selectSession(takenKey, takenNode);
      // Restore the message text and send
      const newInput = document.getElementById('msg-input');
      if (newInput) setMsgValue(newInput, text);
      await sendMessage();
      return;
    } catch (e) {
      deps.showNetworkError('接管进程', e);
      if (input) { input.dataset.placeholder = 'send a message to take over...'; input.contentEditable = 'true'; }
      nzState.sending = false;
      if (btn) btn.classList.remove('sending');
      return;
    }
  }

  if (!nzState.selectedKey) return;
  const input = document.getElementById('msg-input');
  const text = getMsgValue(input);
  if (!text && nzState.pendingFiles.length === 0) return;
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
  nzState.sending = true;
  const btn = document.getElementById('btn-send');
  if (btn) btn.classList.add('sending');
  try {
    await sendComposerTurn(nzState.selectedKey, nzState.selectedNode);
  } finally {
    nzState.sending = false;
    if (btn) btn.classList.remove('sending');
  }
}

// sendComposerTurn is the gated half of sendMessage: the caller holds the
// `nzState.sending` gate for its whole duration. targetKey/targetNode are the session
// the operator hit send on; a switch during the orient wait aborts the send
// rather than redirecting the captured text to the newly selected session.
async function sendComposerTurn(targetKey, targetNode) {
  // Auto-orient runs as a fire-and-forget vision side-call after upload
  // (maybeAutoOrient). If the user hits send within its ~12s window, the
  // server would TakeAll the upload BEFORE the rotation's in-place Replace
  // lands — nzState.sending the original sideways image. Transparently wait for any
  // in-flight orient to settle (hard-capped at ORIENT_MAX_WAIT_MS) so the
  // rotated bytes are in the store before we consume the file_ids. Silent by
  // design: no toast, the user already clicked send and the rotation is
  // best-effort.
  await deps.awaitPendingOrients();
  if (!nzState.selectedKey || nzState.selectedKey !== targetKey || nzState.selectedNode !== targetNode) return;
  // The composer stayed editable during the wait: re-read the text and re-run
  // the synchronous checks. Without the uploading re-check an id-less upload
  // dropped mid-wait was filtered out of fileIDs and then deleted by
  // clearPendingFiles() — the attachment vanished without a trace.
  const input = document.getElementById('msg-input');
  const text = getMsgValue(input);
  if (!text && nzState.pendingFiles.length === 0) return;
  if (!validateComposerForSend(text)) return;
  const failed = nzState.pendingFiles.filter(f => f.status === 'error');
  if (failed.length > 0) {
    const detail = failed[0].error || '';
    const tail = detail ? '（' + detail.slice(0, 120) + '）' : '';
    showToast('图片上传失败' + tail + '，请移除或重试', 'error');
    return;
  }
  // The shim NDJSON line cap is 12 MB; base64 inflates by ~1.33× so the
  // raw image batch must stay under ~9 MB to fit alongside the JSON
  // envelope. Pre-check here so users get a clear "too large — split into
  // fewer pictures" message instead of a silent "没 working" (R192 regression).
  //
  // PDFs do NOT count toward this budget — they travel as file_ref (server
  // persists the bytes to the session workspace; only the path string ends
  // up in the NDJSON line). Filtering by kind here keeps mixed image+PDF
  // sends from tripping the cap on the PDF's 20 MB that will never hit
  // stdin anyway.
  const totalBytes = nzState.pendingFiles.reduce((n, f) => {
    if (f.kind === 'pdf' || f.serverKind === 'file_ref') return n;
    return n + (f.normalizedSize || f.file.size || 0);
  }, 0);
  const batchCap = 9 * 1024 * 1024;
  if (totalBytes > batchCap) {
    showToast('图片总大小 ' + Math.ceil(totalBytes / 1024 / 1024) + ' MB 超过 9 MB 上限，请分批发送或减少图片', 'warning');
    return;
  }
  const fileIDs = nzState.pendingFiles.map(f => f.id).filter(Boolean);

  // Flip the send→stop button + running banner BEFORE the network round trip,
  // not after — a resumed session has no CLI process yet, so the first send
  // triggers a subprocess spawn that can take several hundred ms. Leaving the
  // green send button visible during that window makes the click feel ignored
  // and invites double-sends. onSendAck/rollbackOptimisticRunning undo this on
  // busy/error/reset; the 20s safety timer in markSessionOptimisticRunning
  // prevents a stuck banner if the server never responds.
  markSessionOptimisticRunning(nzState.selectedKey, nzState.selectedNode);

  // WS path: preferred for TEXT-ONLY sends. Sends carrying file_ids MUST go
  // over HTTP instead: the uploadStore owner for a WS send is the one frozen
  // at WebSocket upgrade time (wsDeriveUploadOwner → setUploadOwner, never
  // refreshed in no-token mode), while /api/sessions/upload derives its owner
  // from the CURRENT nz_anon cookie. The nz_anon label expires after
  // anonCookieMaxAgeSeconds (1h) with no sliding renewal on old servers, so a
  // long-lived dashboard tab ends up with upload-owner ≠ WS-owner and every
  // file-bearing WS send fails TakeAll with "file not found or expired".
  // HTTP sends carry the same cookie the upload just used (or freshly
  // minted), so the two owners can never diverge. Token-mode deployments are
  // owner-stable either way; routing on file presence keeps them on the same
  // path for consistency.
  if (deps.wsm.isConnected() && fileIDs.length === 0) {
    const id = 'r' + (++deps.wsm.sendCounter);
    const sendMsg = { type: 'send', key: nzState.selectedKey, text: text, id: id };
    // No file_ids here by construction — file-bearing sends take the HTTP
    // path above so the uploadStore owner matches the upload's cookie.
    if (nzState.selectedNode && nzState.selectedNode !== 'local') sendMsg.node = nzState.selectedNode;
    if (deps.sessionWorkspaces[nzState.selectedKey]) sendMsg.workspace = deps.sessionWorkspaces[nzState.selectedKey];
    if (deps.sessionBackends[nzState.selectedKey]) sendMsg.backend = deps.sessionBackends[nzState.selectedKey];
    if (deps.sessionAccessProfiles[nzState.selectedKey]) sendMsg.access_profile = deps.sessionAccessProfiles[nzState.selectedKey];
    if (deps.wsm.send(sendMsg)) {
      // Workspace/backend/access profile are consumed once on session spawn;
      // forget them only now that the frame is out. A failed deps.wsm.send falls
      // through to the HTTP path below, which must still see them.
      if (sendMsg.workspace) {
        delete deps.sessionWorkspaces[nzState.selectedKey];
        delete deps.sessionNodes[nzState.selectedKey];
      }
      delete deps.sessionBackends[nzState.selectedKey];
      delete deps.sessionAccessProfiles[nzState.selectedKey];
      // Optimistic render: show user message immediately without waiting
      // for the CLI to echo it back as a "user" event.
      renderOptimisticUserMsg(text, id);
      if (input) clearMsg(input);
      delete nzState.sessionDrafts[nzState.selectedKey];
      clearPendingFiles();
      if (text) nzState.sessionLastSent[deps.sid(nzState.selectedKey, nzState.selectedNode)] = text;
      // Confirmed send: the workspace/node/backend were consumed above (and
      // deleted from the in-memory maps), so rewrite the durable blob without
      // this key. Only on the success path — a failed deps.wsm.send falls through to
      // HTTP below and must keep the entry for that retry.
      deps.persistPending();
      return;
    }
    // WS send failed, fall through to HTTP path below
  }

  // HTTP POST fallback — JSON only; files already on server.
  try {
    const headers = { 'Content-Type': 'application/json' };
    const token = deps.getToken();
    if (token) headers['Authorization'] = 'Bearer ' + token;

    const payload = { key: nzState.selectedKey, text: text };
    if (fileIDs.length > 0) payload.file_ids = fileIDs;
    if (nzState.selectedNode && nzState.selectedNode !== 'local') payload.node = nzState.selectedNode;
    if (deps.sessionWorkspaces[nzState.selectedKey]) {
      payload.workspace = deps.sessionWorkspaces[nzState.selectedKey];
      delete deps.sessionWorkspaces[nzState.selectedKey];
      delete deps.sessionNodes[nzState.selectedKey];
    }
    if (deps.sessionBackends[nzState.selectedKey]) {
      payload.backend = deps.sessionBackends[nzState.selectedKey];
      delete deps.sessionBackends[nzState.selectedKey];
    }
    if (deps.sessionAccessProfiles[nzState.selectedKey]) {
      payload.access_profile = deps.sessionAccessProfiles[nzState.selectedKey];
      delete deps.sessionAccessProfiles[nzState.selectedKey];
    }

    // Mark this tab as the originator BEFORE the request leaves (text or
    // image-only alike): a send_error for this turn can arrive over the WS
    // any time after the server has the request.
    const sentSid = deps.sid(nzState.selectedKey, nzState.selectedNode);
    deps.httpSendPending.add(sentSid);
    const r = await fetch(NZ_CONTRACT.API.sessions_send, {method:'POST', headers, body: JSON.stringify(payload)});

    if (!r.ok) {
      deps.httpSendPending.delete(sentSid); // rejected synchronously — no async frame will follow
      if (input) setMsgValue(input, text);
      rollbackOptimisticRunning(nzState.selectedKey, nzState.selectedNode);
      // Some error paths still write text/plain; fall back to text() so we
      // always surface the real message instead of a generic "send failed".
      const raw = await r.text().catch(() => '');
      let detail = '', filesConsumed = false;
      try {
        const j = JSON.parse(raw);
        if (j && j.error) detail = j.error;
        if (j && j.files_consumed) filesConsumed = true;
      } catch (_) { if (raw) detail = raw; }
      // files_consumed: the server already took the pre-uploaded attachments
      // out of the uploadStore before rejecting (post-TakeAll 4xx/5xx), so the
      // chips we still hold reference dead ids — a retry would fail with
      // "file not found or expired". Drop them and ask for a re-attach.
      // Pre-TakeAll rejections (and 401/403/429 from the middleware/limiter)
      // never set the flag, so the user's unsent attachments stay put.
      if (filesConsumed) {
        clearPendingFiles();
        showToast('附件已失效，请重新添加后再发送', 'warning');
      }
      if (r.status === 401 || r.status === 403) {
        deps.showAuthModal();
        return;
      }
      if (r.status === 429) {
        // The server names the limiter that fired (send vs upload rate limit);
        // there is no queue-full 429 on this path, so never invent one.
        showToast(detail || '请求过于频繁，请稍后重试', 'warning');
        return;
      }
      deps.showAPIError('发送消息', r.status, detail);
      return;
    }

    // /clear and /new return status:"reset" — no CLI turn to run, so don't
    // flip to 'running'. Every other success ('accepted'/'queued') should
    // show the banner immediately. Read the body once (before clearing the
    // input) so we can branch on status without reviving the stale text.
    // Record the sent text BEFORE awaiting the body (interrupt re-fill source;
    // also a secondary onSendError gate).
    if (text) nzState.sessionLastSent[sentSid] = text;
    let ackStatus = '';
    try { const j = await r.json(); if (j && j.status) ackStatus = j.status; } catch (_) {}

    // Clear input only after confirmed success
    if (input) clearMsg(input);
    delete nzState.sessionDrafts[nzState.selectedKey];
    clearPendingFiles();
    // Confirmed send: the pending maps were consumed above; rewrite the durable
    // blob without this key. Only on this 2xx path so a failed send keeps the
    // entry for retry.
    deps.persistPending();
    if (ackStatus === 'reset') {
      // /clear and /new do not spawn a turn — undo the pre-send optimistic flip
      // so the running banner doesn't hang on a no-op command.
      rollbackOptimisticRunning(nzState.selectedKey, nzState.selectedNode);
      delete nzState.sessionLastSent[sentSid]; // no turn ran, nothing to re-fill on interrupt
      deps.httpSendPending.delete(sentSid);
    } else {
      // Optimistic running flip already applied above — keep it.
      // Optimistic bubble parity with the WS path — but ONLY while WS is
      // connected: the live event stream (onHistory/onEvent) is what removes
      // .optimistic-msg when the real "user" event arrives. The WS-down
      // fallback keeps its legacy no-bubble behaviour (appendEvents also
      // replaces the bubble now, but the poll echo lags up to a tick).
      if (deps.wsm.isConnected()) renderOptimisticUserMsg(text);
    }

    // Speed up polling when WS not connected
    if (!deps.wsm.isConnected()) {
      if (nzState.eventTimer) clearInterval(nzState.eventTimer);
      nzState.eventTimer = setInterval(() => deps.fetchEvents(false), 500);
      setTimeout(() => {
        if (nzState.eventTimer) clearInterval(nzState.eventTimer);
        if (!deps.wsm.isConnected()) {
          nzState.eventTimer = setInterval(() => deps.fetchEvents(false), 1000);
        }
      }, 15000);
    }
  } catch (e) {
    deps.httpSendPending.delete(deps.sid(nzState.selectedKey, nzState.selectedNode));
    if (input) setMsgValue(input, text);
    rollbackOptimisticRunning(nzState.selectedKey, nzState.selectedNode);
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
  nzState.navUserEls = [...document.querySelectorAll('#events-scroll .event.user')];
  deps.navUpdatePill();
}

function clearPendingFiles() {
  nzState.pendingFiles.forEach(f => { if (f.blobUrl) URL.revokeObjectURL(f.blobUrl); });
  nzState.pendingFiles = [];
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
}

function markSessionOptimisticRunning(key, node) {
  if (!key) return;
  const sKey = deps.sid(key, node || 'local');
  const sd = nzState.sessionsData[sKey];
  if (sd && sd.state === 'running') return; // server already said running
  // A just-created session has no nzState.sessionsData entry until the next list
  // fetch. Still flip the button/banner below (#2405: the missing feedback on
  // a new session's first send invited Enter mashing); deps.fetchSessions keeps the
  // flag-forced 'running' once the entry lands, onSessionState clears it.
  if (sd) {
    deps.sessionOptimisticPrevState[sKey] = sd.state;
    sd.state = 'running';
  }
  deps.sessionOptimisticRunning[sKey] = true;
  // Sidebar parity: flip the card's dot/label to running right now so the
  // left list never looks idle while the main banner already says working.
  patchSidebarCardState(key, node, 'running');
  if (_optimisticRunningTimers[sKey]) clearTimeout(_optimisticRunningTimers[sKey]);
  _optimisticRunningTimers[sKey] = setTimeout(() => {
    delete _optimisticRunningTimers[sKey];
    // Only rollback if still optimistic (no real running state arrived).
    if (deps.sessionOptimisticRunning[sKey]) {
      rollbackOptimisticRunning(key, node);
    }
  }, 20000);
  if (key === nzState.selectedKey && (node || 'local') === nzState.selectedNode) {
    // justSent makes the banner's first line read "已发送，正在处理…" until the
    // first real event (tool/thinking/output) arrives, so the operator gets a
    // distinct "received, starting up" signal during CLI spawn rather than a
    // generic static "处理中…".
    nzState.turnState.justSent = true;
    deps.startTurnTimer();
    deps.updateSendButton('running');
  }
}

function rollbackOptimisticRunning(key, node) {
  if (!key) return;
  const sKey = deps.sid(key, node || 'local');
  if (!deps.sessionOptimisticRunning[sKey]) return;
  delete deps.sessionOptimisticRunning[sKey];
  delete deps.sessionOptimisticPrevState[sKey];
  if (_optimisticRunningTimers[sKey]) {
    clearTimeout(_optimisticRunningTimers[sKey]);
    delete _optimisticRunningTimers[sKey];
  }
  const sd = nzState.sessionsData[sKey];
  if (sd && sd.state === 'running') {
    sd.state = 'ready';
    patchSidebarCardState(key, node, 'ready');
  }
  // The flip may have been applied without a nzState.sessionsData entry (new session's
  // first send) — restore the button either way.
  if (key === nzState.selectedKey && (node || 'local') === nzState.selectedNode) deps.updateSendButton('ready');
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
