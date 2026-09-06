// voice.js — hold-to-talk voice input (#2558 D4-2).
//
// Verbatim move out of dashboard.js (`git diff --color-moved` shows a pure
// move; the import/dep-wiring lines and escCloseVoiceOverlay are the only
// additions). Owns the MediaRecorder lifecycle, the persistent mic stream,
// the touch/mouse hold gesture, the 30 s cap timer and the transcription
// round-trip.
//
// Layering (D4-1 rule): a module dashboard imports must NOT import dashboard
// back. Dashboard's mutable state is read through nz.state accessors; its
// helpers are injected once via configureVoice().
import { nzState, nzTest, showToast } from './nz_util.js';

const deps = {
  ICONS: null,
  getMsgValue: null,
  getToken: null,
  sendMessage: null,
  setMsgValue: null,
  sid: null,
  updateSendButton: null,
};
export function configureVoice(impl) {
  for (const k of Object.keys(deps)) {
    if (typeof impl[k] === 'undefined') throw new Error('voice dep missing: ' + k);
    deps[k] = impl[k];
  }
}

// --- Voice recording (WeChat-style hold-to-talk) ---

let mediaRecorder = null;
let audioChunks = [];
let isUnloading = false;
let voiceRecTimer = null;
let voiceRecStart = 0;
const MAX_REC_SECS = 30;
let pendingMic = false;
let voiceInputMode = false;
let voiceTouchStartY = 0;
let voiceCancelled = false;
let voiceActive = false; // true while hold gesture is in progress
// voiceState is the recording lifecycle, independent of the finger gesture
// (voiceActive): 'idle' → 'recording' (MediaRecorder started) → 'finalizing'
// (recorder stopped, onstop / transcription pending) → 'idle'. #2435: the
// 30s cap stops the recorder while the finger is still down; without this
// state the trailing swipe/lift re-ran the send/cancel logic against a
// recorder that was already finalized.
let voiceState = 'idle';
let persistentMicStream = null; // keep mic stream alive to avoid repeated permission prompts

window.addEventListener('pagehide', () => {
  isUnloading = true;
  voiceActive = false;
  cleanupVoiceTouchListeners();
  if (mediaRecorder && mediaRecorder.state !== 'inactive') mediaRecorder.stop();
  if (persistentMicStream) { persistentMicStream.getTracks().forEach(t => t.stop()); persistentMicStream = null; }
});

function acquireMicStream() {
  if (persistentMicStream && persistentMicStream.getAudioTracks().some(t => t.readyState === 'live')) {
    return Promise.resolve(persistentMicStream);
  }
  if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia) {
    return Promise.reject(new Error('not supported'));
  }
  return navigator.mediaDevices.getUserMedia({ audio: true }).then(stream => {
    persistentMicStream = stream;
    return stream;
  });
}

function releaseMicStream() {
  if (persistentMicStream) {
    persistentMicStream.getTracks().forEach(t => t.stop());
    persistentMicStream = null;
  }
}

function toggleInputMode() {
  if (pendingMic) return;
  // Mode switch abandons any in-flight recording/transcription and returns
  // the lifecycle to idle so a hung transcription can never lock recording.
  if (mediaRecorder && mediaRecorder.state === 'recording') {
    voiceActive = false;
    cleanupVoiceTouchListeners();
    voiceCancelled = true;
    mediaRecorder.stop();
  }
  hideVoiceOverlay();
  voiceInputMode = !voiceInputMode;
  const ia = document.getElementById('input-area');
  if (ia) ia.classList.toggle('voice-mode', voiceInputMode);
  const btn = document.getElementById('btn-mic');
  if (btn) {
    btn.innerHTML = voiceInputMode ? deps.ICONS.keyboard : deps.ICONS.mic;
    btn.title = voiceInputMode ? '\u5207\u6362\u952e\u76d8' : '\u5207\u6362\u8bed\u97f3';
  }
  if (voiceInputMode) {
    // Pre-acquire mic permission so hold-to-talk won't prompt again
    acquireMicStream().catch(() => {});
  } else {
    releaseMicStream();
  }
  // Sync send/stop button visibility after mode toggle
  const sd = nzState.sessionsData[deps.sid(nzState.selectedKey, nzState.selectedNode || 'local')];
  deps.updateSendButton(sd ? sd.state || '' : '');
}

// --- Touch handlers for hold-to-talk ---
// touchmove/touchend registered on document (not button) so the overlay cannot block them.

function voiceTouchStart(e) {
  e.preventDefault();
  voiceTouchStartY = e.touches[0].clientY;
  voiceActive = true;
  document.addEventListener('touchmove', voiceTouchMove, {passive: false});
  document.addEventListener('touchend', voiceTouchEnd, {passive: false});
  document.addEventListener('touchcancel', voiceTouchCancel, {passive: false});
  startVoiceRecording();
}

function voiceTouchMove(e) {
  if (!voiceActive) return;
  e.preventDefault();
  if (voiceState !== 'recording') return; // cap already finalized: gesture can no longer cancel
  const touch = e.touches[0];
  if (!touch) return;
  const dy = voiceTouchStartY - touch.clientY;
  const overlay = document.getElementById('voice-overlay');
  const hint = document.getElementById('vo-hint');
  if (dy > 80) {
    voiceCancelled = true;
    if (overlay) overlay.classList.add('cancel');
    if (hint) hint.textContent = '\u677e\u5f00\u53d6\u6d88';
  } else {
    voiceCancelled = false;
    if (overlay) overlay.classList.remove('cancel');
    if (hint) hint.textContent = '\u677e\u5f00\u53d1\u9001 \u00b7 \u4e0a\u6ed1\u53d6\u6d88';
  }
}

function voiceTouchEnd(e) {
  if (!voiceActive) return;
  e.preventDefault();
  voiceActive = false;
  cleanupVoiceTouchListeners();
  finishVoiceGesture(!voiceCancelled);
}

function voiceTouchCancel() {
  voiceActive = false;
  cleanupVoiceTouchListeners();
  finishVoiceGesture(false);
}

// finishVoiceGesture ends the hold gesture. While recording (or still waiting
// on the mic) it stops the recorder, nzState.sending or cancelling per the gesture.
// Once the recording was already finalized by the MAX_REC_SECS cap it only
// clears the pressed look: the "正在识别" overlay stays up until transcription
// settles, and the lift/swipe can neither cancel nor re-send it (#2435).
function finishVoiceGesture(shouldSend) {
  if (voiceState !== 'finalizing') {
    stopVoiceRecording(shouldSend);
    return;
  }
  const holdBtn = document.getElementById('btn-hold-talk');
  if (holdBtn) holdBtn.classList.remove('active');
}

function cleanupVoiceTouchListeners() {
  document.removeEventListener('touchmove', voiceTouchMove);
  document.removeEventListener('touchend', voiceTouchEnd);
  document.removeEventListener('touchcancel', voiceTouchCancel);
}

function voiceMouseDown(e) {
  e.preventDefault();
  voiceActive = true;
  startVoiceRecording();
  const startY = e.clientY;
  const onMove = (me) => {
    if (voiceState !== 'recording') return;
    const dy = startY - me.clientY;
    const overlay = document.getElementById('voice-overlay');
    const hint = document.getElementById('vo-hint');
    if (dy > 80) {
      voiceCancelled = true;
      if (overlay) overlay.classList.add('cancel');
      if (hint) hint.textContent = '\u677e\u5f00\u53d6\u6d88';
    } else {
      voiceCancelled = false;
      if (overlay) overlay.classList.remove('cancel');
      if (hint) hint.textContent = '\u677e\u5f00\u53d1\u9001 \u00b7 \u4e0a\u6ed1\u53d6\u6d88';
    }
  };
  const onUp = () => {
    document.removeEventListener('mousemove', onMove);
    document.removeEventListener('mouseup', onUp);
    voiceActive = false;
    finishVoiceGesture(!voiceCancelled);
  };
  document.addEventListener('mousemove', onMove);
  document.addEventListener('mouseup', onUp);
}

function startVoiceRecording() {
  // A previous recording still transcribing owns the overlay; starting another
  // would let its completion hide the new recording's overlay mid-hold.
  if (pendingMic || voiceState === 'finalizing') return;
  // Cleared only once a recording really starts: a press landing in the
  // stop()→onstop window of a cancelled recording must not flip that
  // recording's pending "cancel" into "send".
  voiceCancelled = false;
  pendingMic = true;
  const holdBtn = document.getElementById('btn-hold-talk');
  if (holdBtn) holdBtn.classList.add('active');

  acquireMicStream().then(stream => {
    pendingMic = false;
    // If finger was released during async acquireMicStream, abort immediately
    if (!voiceActive) {
      if (holdBtn) holdBtn.classList.remove('active');
      return;
    }
    audioChunks = [];
    const mimeType = MediaRecorder.isTypeSupported('audio/webm;codecs=opus') ? 'audio/webm;codecs=opus'
      : MediaRecorder.isTypeSupported('audio/ogg;codecs=opus') ? 'audio/ogg;codecs=opus' : '';
    mediaRecorder = mimeType ? new MediaRecorder(stream, { mimeType }) : new MediaRecorder(stream);
    mediaRecorder.ondataavailable = e => { if (e.data.size > 0) audioChunks.push(e.data); };
    mediaRecorder.onstop = () => {
      voiceState = 'finalizing';
      clearInterval(voiceRecTimer);
      voiceRecTimer = null;
      // Do NOT stop persistent stream tracks — keep them alive for next recording
      if (holdBtn) holdBtn.classList.remove('active');
      if (isUnloading) return;

      if (voiceCancelled) {
        hideVoiceOverlay();
        showToast('\u5df2\u53d6\u6d88');
        audioChunks = [];
        return;
      }

      const blob = new Blob(audioChunks, { type: mediaRecorder.mimeType });
      audioChunks = [];
      if (blob.size < 1000) {
        hideVoiceOverlay();
        showToast('\u5f55\u97f3\u592a\u77ed');
        return;
      }
      // Show transcribing state on overlay
      const overlay = document.getElementById('voice-overlay');
      if (overlay) overlay.classList.add('transcribing');
      const hint = document.getElementById('vo-hint');
      if (hint) hint.textContent = '\u6b63\u5728\u8bc6\u522b...';
      transcribeAudio(blob, true);
    };
    mediaRecorder.start();
    voiceState = 'recording';
    voiceRecStart = Date.now();
    voiceRecTimer = setInterval(updateVoiceTimer, 200);
    updateVoiceTimer();
    // Show overlay
    const overlay = document.getElementById('voice-overlay');
    if (overlay) { overlay.classList.remove('cancel', 'transcribing'); overlay.classList.add('show'); }
    const hint = document.getElementById('vo-hint');
    if (hint) hint.textContent = '\u677e\u5f00\u53d1\u9001 \u00b7 \u4e0a\u6ed1\u53d6\u6d88';
  }).catch(err => {
    pendingMic = false;
    voiceActive = false;
    cleanupVoiceTouchListeners();
    if (holdBtn) holdBtn.classList.remove('active');
    hideVoiceOverlay();
    showToast(describeMicError(err), 'error', 5000);
    console.warn('mic error:', err);
  });
}

// describeMicError converts a MediaDevices/getUserMedia error into a concrete,
// user-actionable Chinese message. Previously we collapsed all failures to
// "权限被拒绝", which masked genuine browser-unsupported, no-device, or
// hardware-busy cases that need different recovery steps.
function describeMicError(err) {
  if (!err) return '\u9ea6\u514b\u98ce\u8c03\u7528\u5931\u8d25';
  if (err.message === 'not supported' || err.name === 'NotSupportedError') {
    return '\u6d4f\u89c8\u5668\u4e0d\u652f\u6301\u5f55\u97f3\uff0c\u8bf7\u6539\u7528 Chrome/Firefox/Safari \u6700\u65b0\u7248';
  }
  if (err.name === 'NotAllowedError' || err.name === 'SecurityError') {
    return '\u9ea6\u514b\u98ce\u6743\u9650\u88ab\u62d2\u7edd\uff0c\u8bf7\u5728\u6d4f\u89c8\u5668\u5730\u5740\u680f\u7684\u9501\u5934\u56fe\u6807\u4e2d\u5141\u8bb8';
  }
  if (err.name === 'NotFoundError' || err.name === 'OverconstrainedError') {
    return '\u672a\u68c0\u6d4b\u5230\u53ef\u7528\u9ea6\u514b\u98ce\uff0c\u8bf7\u68c0\u67e5\u786c\u4ef6\u8fde\u63a5';
  }
  if (err.name === 'NotReadableError') {
    return '\u9ea6\u514b\u98ce\u88ab\u5176\u4ed6\u7a0b\u5e8f\u5360\u7528\uff0c\u8bf7\u5173\u95ed\u5176\u4ed6\u5f55\u97f3\u5e94\u7528\u540e\u91cd\u8bd5';
  }
  if (err.name === 'AbortError') {
    return '\u5f55\u97f3\u88ab\u7ec8\u6b62\uff0c\u8bf7\u91cd\u65b0\u5c1d\u8bd5';
  }
  return '\u9ea6\u514b\u98ce\u8c03\u7528\u5931\u8d25\uff1a' + (err.message || err.name || 'unknown');
}

function stopVoiceRecording(shouldSend) {
  // Already stopped (MAX_REC_SECS cap or an earlier lift): onstop /
  // transcription own the overlay now, and a later gesture must not flip
  // voiceCancelled or hide the "正在识别" overlay underneath it.
  if (voiceState === 'finalizing') return;
  if (!shouldSend) voiceCancelled = true;
  if (voiceRecTimer) { clearInterval(voiceRecTimer); voiceRecTimer = null; }
  const holdBtn = document.getElementById('btn-hold-talk');
  if (holdBtn) holdBtn.classList.remove('active');
  if (mediaRecorder && mediaRecorder.state === 'recording') {
    voiceState = 'finalizing';
    mediaRecorder.stop(); // triggers onstop handler
  } else {
    hideVoiceOverlay();
  }
}

function hideVoiceOverlay() {
  voiceState = 'idle';
  const overlay = document.getElementById('voice-overlay');
  if (overlay) overlay.classList.remove('show', 'cancel', 'transcribing');
}

// Tap overlay to cancel (escape hatch for stuck states)
document.getElementById('voice-overlay')?.addEventListener('click', function(e) {
  // Normal flow: touchend/mouseup already stopped recording before click fires.
  // This only triggers when genuinely stuck (recording active or overlay visible).
  if (mediaRecorder && mediaRecorder.state === 'recording') {
    voiceActive = false;
    cleanupVoiceTouchListeners();
    stopVoiceRecording(false);
  } else if (this.classList.contains('show')) {
    // Stuck in transcribing state or overlay didn't dismiss
    hideVoiceOverlay();
  }
});

function updateVoiceTimer() {
  const el = document.getElementById('vo-timer');
  if (!el) return;
  const secs = Math.floor((Date.now() - voiceRecStart) / 1000);
  el.textContent = secs + 's';
  if (secs >= MAX_REC_SECS && voiceState === 'recording') {
    // stopVoiceRecording clears voiceRecTimer, so this toast fires once
    // instead of on every 200ms tick until onstop lands (#2435).
    stopVoiceRecording(true);
    showToast('\u5df2\u8fbe\u6700\u957f' + MAX_REC_SECS + '\u79d2');
  }
}

// TRANSCRIBE_TIMEOUT_MS bounds /api/transcribe: without it a stalled upload
// left the overlay in "正在识别" and voiceState in finalizing forever.
const TRANSCRIBE_TIMEOUT_MS = 30000;

function transcribeAudio(blob, autoSend) {
  const fd = new FormData();
  fd.append('audio', blob, 'recording.' + (blob.type.includes('webm') ? 'webm' : blob.type.includes('ogg') ? 'ogg' : 'mp4'));
  const headers = {};
  const token = deps.getToken();
  if (token) headers['Authorization'] = 'Bearer ' + token;
  const ac = new AbortController();
  const timeoutId = setTimeout(() => ac.abort(), TRANSCRIBE_TIMEOUT_MS);
  // Tag fetch-level failures so .catch can distinguish network from server.
  fetch(NZ_CONTRACT.API.transcribe, {
    method: 'POST',
    headers: headers,
    credentials: 'same-origin',
    body: fd,
    signal: ac.signal
  }).then(r => {
    clearTimeout(timeoutId);
    if (!r.ok) return r.text().then(t => {
      const e = new Error(t || ('HTTP ' + r.status));
      e.status = r.status;
      e.body = t;
      throw e;
    });
    return r.json();
  }).then(data => {
    hideVoiceOverlay();
    const input = document.getElementById('msg-input');
    if (input && data.text) {
      // Append to (never replace) whatever is in the composer: in voice mode
      // the textarea is hidden, so an auto-sent transcript used to silently
      // overwrite a typed draft (#2435).
      const cur = deps.getMsgValue(input);
      deps.setMsgValue(input, cur ? cur + ' ' + data.text : data.text);
      if (autoSend) {
        deps.sendMessage();
      } else {
        input.focus();
        showToast('\u8f6c\u5199: ' + data.text.substring(0, 50) + (data.text.length > 50 ? '...' : ''), 'success', 5000);
      }
    } else {
      // Empty transcription — compute recorded duration so the user knows
      // whether the issue is "no speech detected" vs "too quiet" vs "silence".
      const secs = Math.max(0, Math.round((Date.now() - voiceRecStart) / 1000));
      const hint = secs < 2
        ? '\u672a\u68c0\u6d4b\u5230\u8bed\u97f3\uff08\u5f55\u97f3\u592a\u77ed\uff0c\u8bf7\u6309\u4f4f\u8bf4\u8bdd\u81f3\u5c11 2 \u79d2\uff09'
        : '\u672a\u68c0\u6d4b\u5230\u8bed\u97f3\uff08' + secs + 's\uff09\uff0c\u8bf7\u9760\u8fd1\u9ea6\u514b\u98ce\u540e\u91cd\u8bd5';
      showToast(hint, 'warning', 5000);
    }
  }).catch(err => {
    clearTimeout(timeoutId);
    hideVoiceOverlay();
    showToast(describeTranscribeError(err), 'error', 5000);
  });
}

// describeTranscribeError turns a fetch/HTTP failure into a user-friendly
// message keyed off HTTP status — previously the raw server body was shown,
// which surfaced internal strings like "transcribe rate limit exceeded".
function describeTranscribeError(err) {
  if (!err) return '\u8f6c\u5199\u5931\u8d25';
  if (err.name === 'AbortError') {
    return '\u8f6c\u5199\u8d85\u65f6\uff0c\u8bf7\u91cd\u8bd5';
  }
  // fetch() rejects with TypeError on network failure; server errors have a status.
  if (!err.status) {
    return '\u7f51\u7edc\u8fde\u63a5\u5f02\u5e38\uff0c\u8bf7\u68c0\u67e5\u7f51\u7edc\u540e\u91cd\u8bd5';
  }
  switch (err.status) {
    case 401:
    case 403:
      return '\u672a\u767b\u5f55\u6216\u4f1a\u8bdd\u5df2\u8fc7\u671f\uff0c\u8bf7\u91cd\u65b0\u767b\u5f55\u540e\u91cd\u8bd5';
    case 413:
      return '\u5f55\u97f3\u6587\u4ef6\u8fc7\u5927\uff0c\u8bf7\u7f29\u77ed\u540e\u91cd\u8bd5';
    case 415:
      return '\u4e0d\u652f\u6301\u7684\u97f3\u9891\u683c\u5f0f\uff0c\u8bf7\u66f4\u6362\u6d4f\u89c8\u5668\u91cd\u8bd5';
    case 429:
      return '\u8f6c\u5199\u8bf7\u6c42\u8fc7\u4e8e\u9891\u7e41\uff0c\u8bf7\u7a0d\u5019\u4e00\u5206\u949f\u540e\u91cd\u8bd5';
    case 500:
    case 502:
    case 503:
    case 504:
      return '\u8f6c\u5199\u670d\u52a1\u6682\u4e0d\u53ef\u7528\uff08HTTP ' + err.status + '\uff09\uff0c\u8bf7\u7a0d\u540e\u91cd\u8bd5';
    default:
      return '\u8f6c\u5199\u5931\u8d25\uff08HTTP ' + err.status + '\uff09';
  }
}


// escCloseVoiceOverlay — the Esc-key escape hatch for a stuck recording
// (R20260610-UI-3). Moved here with the overlay logic it drives; dashboard's
// global Esc handler calls it and uses the boolean to decide whether the key
// was consumed.
export function escCloseVoiceOverlay() {
  const voiceOv = document.getElementById('voice-overlay');
  if (!voiceOv || !voiceOv.classList.contains('show')) return false;
  if (mediaRecorder && mediaRecorder.state === 'recording') {
    voiceActive = false;
    cleanupVoiceTouchListeners();
    stopVoiceRecording(false);
  } else {
    hideVoiceOverlay();
  }
  return true;
}

export {
  MAX_REC_SECS,
  cleanupVoiceTouchListeners,
  hideVoiceOverlay,
  mediaRecorder,
  stopVoiceRecording,
  toggleInputMode,
  updateVoiceTimer,
  voiceCancelled,
  voiceInputMode,
  voiceMouseDown,
  voiceRecStart,
  voiceRecTimer,
  voiceState,
  voiceTouchStart,
};

// nz.test surface for the Playwright voice specs (#2557 PR-E3 pattern):
// registered here because only this module can offer a working setter for
// its own let bindings — an importer's `voiceRecStart = …` would assign to
// a read-only import binding.
Object.defineProperties(nzTest, {
  voiceRecStart: { get: function () { return voiceRecStart; }, set: function (v) { voiceRecStart = v; }, configurable: true },
  voiceRecTimer: { get: function () { return voiceRecTimer; }, set: function (v) { voiceRecTimer = v; }, configurable: true },
  voiceCancelled: { get: function () { return voiceCancelled; }, set: function (v) { voiceCancelled = v; }, configurable: true },
  voiceState: { get: function () { return voiceState; }, set: function (v) { voiceState = v; }, configurable: true },
});
Object.assign(nzTest, { MAX_REC_SECS, updateVoiceTimer });
