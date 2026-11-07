// meetings.js — meeting recording in the dashboard (docs/rfc/meeting-notes.md §6, §7):
//   - the red "录音中 · 名称 · 时长 · 停止" strip on top of every view while one records,
//     following meeting_state frames;
//   - the meetings panel: the list, and one meeting's summary and transcript (a timestamp plays
//     that moment of its track), with the audio's 30-day keep / delete;
//   - the 设置 section: the two macOS grants and the button that brings up their prompts;
//   - a "会议纪要" row on top of the 成品库.
import { NZ_CONTRACT } from './contract.js';
import { wsm } from './ws_manager.js';
import { esc, fetchJSON, nzBus, showToast, trapFocus } from './nz_util.js';
import { renderMd } from './render_md.js';

const API = NZ_CONTRACT.API;
const $ = (id) => document.getElementById(id);

const mt = {
  status: null, // meeting.Status
  timer: 0,
  meetings: [],
};

const STAGES = { transcribing: '转写中', diarizing: '区分说话人', summarizing: '写纪要' };
const STATES = { recording: '录音中', processing: '处理中', done: '已完成', failed: '失败' };
const AUTH = { authorized: '已授权', denied: '已拒绝', not_determined: '未授权', unknown: '未知' };

function clock(sec) {
  sec = Math.max(0, Math.round(sec));
  const h = Math.floor(sec / 3600), m = Math.floor(sec / 60) % 60, s = sec % 60;
  const p = (n) => String(n).padStart(2, '0');
  return (h ? h + ':' : '') + p(m) + ':' + p(s);
}

function meetingPath(id, rest) {
  return API.meetings_id.replace('{id}', encodeURIComponent(id)) + (rest || '');
}

// ─── red strip ───────────────────────────────────────────────────

function strip() {
  let el = $('rec-strip');
  if (el) return el;
  el = document.createElement('div');
  el.id = 'rec-strip';
  el.className = 'rec-strip';
  el.setAttribute('role', 'status');
  el.hidden = true;
  el.innerHTML = '<span class="rec-dot" aria-hidden="true"></span><button type="button" class="rec-title" id="rec-open"></button>' +
    '<span class="rec-time" id="rec-time"></span><button type="button" class="rec-stop" id="rec-stop">停止</button>';
  document.body.prepend(el);
  el.querySelector('#rec-stop').addEventListener('click', stopRecording);
  el.querySelector('#rec-open').addEventListener('click', () => openPanel());
  return el;
}

function paintStrip() {
  const rec = mt.status && mt.status.recording;
  const el = strip();
  document.body.classList.toggle('nz-recording', !!rec);
  el.hidden = !rec;
  clearInterval(mt.timer);
  if (!rec) return;
  el.querySelector('#rec-open').textContent = '录音中 · ' + rec.title;
  const t0 = Date.parse(rec.started);
  const tick = () => { el.querySelector('#rec-time').textContent = clock((Date.now() - t0) / 1000); };
  tick();
  mt.timer = setInterval(tick, 1000);
}

async function stopRecording() {
  const btn = $('rec-stop');
  if (btn) btn.disabled = true;
  try {
    await fetchJSON(API.meetings_stop, { method: 'POST' });
  } catch (e) {
    showToast('停止失败：' + e.message);
  } finally {
    if (btn) btn.disabled = false;
  }
}

function onState(st) {
  mt.status = st;
  paintStrip();
  paintSettings();
  if (!$('mtg-panel')?.hidden) refreshList();
}

async function loadStatus() {
  try {
    const res = await fetchJSON(API.meetings);
    mt.meetings = res.meetings || [];
    onState(res.status || null);
  } catch (_) { /* not available: nothing to show */ }
}

wsm.on(NZ_CONTRACT.WS.meeting_state, (msg) => {
  if (msg.state && typeof msg.state === 'object') onState(msg.state);
});
wsm.onReady(loadStatus);
loadStatus();

// ─── panel ───────────────────────────────────────────────────────

function panel() {
  let el = $('mtg-panel');
  if (el) return el;
  el = document.createElement('div');
  el.id = 'mtg-panel';
  el.className = 'mtg-overlay';
  el.hidden = true;
  el.innerHTML = '<div class="mtg-card" role="dialog" aria-modal="true" aria-label="会议纪要">' +
    '<div class="mtg-head"><button type="button" class="mtg-back" id="mtg-back" hidden>‹ 会议</button>' +
    '<h2 id="mtg-h">会议纪要</h2><button type="button" class="mtg-close" id="mtg-close" aria-label="关闭">×</button></div>' +
    '<div class="mtg-body" id="mtg-body"></div><audio id="mtg-audio" preload="none"></audio></div>';
  document.body.appendChild(el);
  el.addEventListener('click', (e) => { if (e.target === el) closePanel(); });
  el.querySelector('#mtg-close').addEventListener('click', closePanel);
  el.querySelector('#mtg-back').addEventListener('click', () => openPanel());
  el.addEventListener('keydown', (e) => { if (e.key === 'Escape') closePanel(); });
  el.querySelector('#mtg-body').addEventListener('click', onBodyClick);
  return el;
}

function closePanel() {
  const el = $('mtg-panel');
  if (!el) return;
  el.hidden = true;
  $('mtg-audio')?.pause();
}

function openPanel(id) {
  const el = panel();
  el.hidden = false;
  trapFocus(el);
  if (id) return openMeeting(id);
  $('mtg-back').hidden = true;
  $('mtg-h').textContent = '会议纪要';
  refreshList();
}

async function refreshList() {
  if (!$('mtg-back') || !$('mtg-back').hidden) return;
  try {
    const res = await fetchJSON(API.meetings);
    mt.meetings = res.meetings || [];
  } catch (e) {
    $('mtg-body').innerHTML = '<div class="files-empty">加载失败：' + esc(e.message) + '</div>';
    return;
  }
  $('mtg-body').innerHTML = mt.meetings.length ? mt.meetings.map(rowHtml).join('') :
    '<div class="files-empty">还没有会议。开启 meeting.enabled 并授权后，线上会议会自动录音。</div>';
}

function rowHtml(m) {
  const state = m.state === 'processing' && STAGES[m.stage] ? STAGES[m.stage] : (STATES[m.state] || m.state);
  const when = new Date(m.started).toLocaleString('zh-CN', { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' });
  return '<button type="button" class="mtg-row" data-open="' + esc(m.id) + '">' +
    '<span class="mtg-row-title">' + esc(m.title) + '</span>' +
    '<span class="mtg-row-meta">' + esc(when) + ' · ' + esc(clock(m.seconds || 0)) + ' · ' + esc(m.app) + '</span>' +
    '<span class="mtg-state mtg-' + esc(m.state) + '">' + esc(state) + '</span></button>';
}

async function openMeeting(id) {
  $('mtg-back').hidden = false;
  const body = $('mtg-body');
  body.innerHTML = '<div class="files-empty">加载中…</div>';
  let d;
  try {
    d = await fetchJSON(meetingPath(id));
  } catch (e) {
    body.innerHTML = '<div class="files-empty">加载失败：' + esc(e.message) + '</div>';
    return;
  }
  const m = d.meeting;
  $('mtg-h').textContent = m.title;
  const audio = d.audio || [];
  let actions = '';
  if (audio.length) {
    const exp = m.audio_expires ? new Date(m.audio_expires).toLocaleDateString('zh-CN') : '';
    actions += '<span class="mtg-keep">' + (m.keep_audio ? '音频永久保留' : (exp ? '音频保留到 ' + esc(exp) : '音频')) + '</span>' +
      '<button type="button" data-keep="' + (m.keep_audio ? '0' : '1') + '" data-id="' + esc(m.id) + '">' + (m.keep_audio ? '改为 30 天' : '永久保留') + '</button>' +
      '<button type="button" data-delaudio="' + esc(m.id) + '">现在删除音频</button>';
  } else if (m.audio_deleted) {
    actions += '<span class="mtg-keep">音频已删除</span>';
  }
  if (m.state === 'failed' && !m.audio_deleted) actions += '<button type="button" data-retry="' + esc(m.id) + '">重试</button>';
  let html = '<div class="mtg-meta">' + esc(new Date(m.started).toLocaleString('zh-CN')) + ' · ' + esc(clock(m.seconds || 0)) +
    ' · ' + esc(m.app) + (m.engine ? ' · ' + esc(m.engine) : '') + '</div>';
  if (m.error) html += '<div class="mtg-err">' + esc(m.error) + '</div>';
  if (m.silent) html += '<div class="mtg-err">对方那一轨全是静音：多半没有「屏幕与系统录音」授权（设置 → 会议录音）。</div>';
  if (actions) html += '<div class="mtg-actions">' + actions + '</div>';
  html += d.summary ? '<div class="mtg-summary md">' + renderMd(d.summary) + '</div>' :
    '<div class="files-empty">' + (m.state === 'failed' ? '纪要没有做成' : '纪要还在做') + '</div>';
  if (d.transcript && d.transcript.length) {
    html += '<h3 class="mtg-sub">文字稿</h3><div class="mtg-lines">' + d.transcript.map(function (l) {
      const track = l.speaker === '你' ? 'mic' : 'remote';
      const play = audio.includes(track) ? ' data-play="' + esc(track) + '" data-at="' + l.start + '" data-id="' + esc(m.id) + '"' : '';
      return '<div class="mtg-line"><button type="button" class="mtg-ts"' + play + (play ? '' : ' disabled') + '>' + esc(clock(l.start)) + '</button>' +
        '<span class="mtg-spk mtg-spk-' + (l.speaker === '你' ? 'me' : 'other') + '">' + esc(l.speaker) + '</span>' +
        '<span class="mtg-txt">' + esc(l.text) + '</span></div>';
    }).join('') + '</div>';
  }
  body.innerHTML = html;
}

async function onBodyClick(e) {
  const t = e.target.closest('button');
  if (!t) return;
  if (t.dataset.open) return openMeeting(t.dataset.open);
  if (t.dataset.play) {
    const a = $('mtg-audio');
    const src = meetingPath(t.dataset.id, '/audio/' + t.dataset.play);
    if (!a.src.endsWith(src)) a.src = src;
    a.currentTime = Math.max(0, Number(t.dataset.at) - 0.3);
    a.play().catch(() => {});
    return;
  }
  try {
    if (t.dataset.keep) {
      await fetchJSON(meetingPath(t.dataset.id, '/keep'), { method: 'POST', body: JSON.stringify({ keep: t.dataset.keep === '1' }) });
      return openMeeting(t.dataset.id);
    }
    if (t.dataset.delaudio) {
      if (!confirm('删除这场会议的音频？文字稿和纪要会保留。')) return;
      await fetchJSON(meetingPath(t.dataset.delaudio, '/audio'), { method: 'DELETE' });
      return openMeeting(t.dataset.delaudio);
    }
    if (t.dataset.retry) {
      await fetchJSON(meetingPath(t.dataset.retry, '/retry'), { method: 'POST' });
      return openMeeting(t.dataset.retry);
    }
  } catch (err) {
    showToast(err.message);
  }
}

// ─── 设置 ────────────────────────────────────────────────────────

function paintSettings() {
  const sec = $('settings-meeting');
  if (!sec) return;
  const st = mt.status;
  if (!st) { sec.hidden = true; return; }
  sec.hidden = false;
  const a = st.auth || {};
  const row = (label, v) => '<div class="settings-about-line"><span class="settings-about-key">' + esc(label) + '</span><span class="mtg-auth-' + esc(v || 'unknown') + '">' + esc(AUTH[v] || v || '未知') + '</span></div>';
  const ready = a.microphone === 'authorized' && a.audio_capture === 'authorized';
  let html = '<h2>会议录音</h2>';
  if (!st.available) {
    html += '<div class="settings-about-line">' + esc(st.error || '没有找到 naozhi-audio') + '</div>';
  } else {
    html += row('麦克风', a.microphone) + row('屏幕与系统录音', a.audio_capture);
    if (st.error) html += '<div class="mtg-err">' + esc(st.error) + '</div>';
    if (!ready) {
      html += '<p class="mtg-hint">点「授权」后，这台 Mac 上会依次弹出两个系统窗口，都点「允许」。如果跳到了系统设置，就在 隐私与安全性 → 屏幕与系统录音 里打开 naozhi-audio。拒绝过的只能在系统设置里改。</p>' +
        '<button type="button" class="settings-syslink-btn" id="mtg-authorize">授权</button>';
    } else {
      html += '<p class="mtg-hint">线上会议（Teams、Zoom、Chime、浏览器…）用上麦克风就自动录音，释放麦克风 2 分钟后结束。告知参会者由你负责。</p>';
    }
  }
  html += '<button type="button" class="settings-syslink-btn" id="mtg-open-list">会议纪要 ›</button>';
  sec.innerHTML = html;
  $('mtg-authorize')?.addEventListener('click', async (e) => {
    e.target.disabled = true;
    try {
      await fetchJSON(API.meetings_authorize, { method: 'POST' });
      showToast('请到这台 Mac 上点「允许」', 'info');
    } catch (err) {
      showToast(err.message);
      e.target.disabled = false;
    }
  });
  $('mtg-open-list')?.addEventListener('click', () => openPanel());
}

nzBus.addEventListener('settings:rendered', paintSettings);

// ─── 成品库 ──────────────────────────────────────────────────────

nzBus.addEventListener('library:rendered', (e) => {
  const box = e.detail && e.detail.box;
  if (!box || !mt.status || box.querySelector('.mtg-libro')) return;
  const n = mt.meetings.length;
  const row = document.createElement('button');
  row.type = 'button';
  row.className = 'mtg-libro';
  row.textContent = '会议纪要 · ' + n + ' 场 ›';
  row.addEventListener('click', () => openPanel());
  box.prepend(row);
});

export { openPanel as openMeetings };
