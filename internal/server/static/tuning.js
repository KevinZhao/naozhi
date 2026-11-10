// tuning.js — per-session tuning: the tuning popover, rename, dismiss and
// the git-state chip.
// Layering: never import dashboard.js back. session_list.js imports this
// module, so the session_list functions it calls are shell slots, as is
// dashboard's renderMainHeader.
import { NZ_CONTRACT } from './contract.js';
import { perSession, selection, serverInfo, sessionList } from './state.js';
import { esc, escAttr, fetchJSON, isCronSessionKey, sessionTitleText, showToast } from './nz_util.js';
import { sessionStream } from './session_stream.js';
import { mainEmptyHtml, promptDialog, removePendingSession, removeSidebarCard, showAPIError, showNetworkError, stopPreviewPolling } from './utilities.js';
import { dropDiscovered, findDiscovered, isDiscoveredKey, parseDiscoveredPid, sameDiscovered, sid } from './session_ident.js';
import { gitChipHtml, gitStateCache, setHeaderGitChip } from './session_header.js';
import { wireQuickAskInput } from './auth_modal.js';
import { shell } from './shell.js';
import { pendingBackendID } from './features.js';
import { fetchAccessProfiles } from './backend_catalog.js';

// ===== Session tuning popover =====
// Per-session model/effort switching from the header chips.
// docs/rfc/dashboard-model-effort-control.md §4.1. Control lives where the
// state is displayed: clicking the model label / effort tag opens a picker;
// the choice POSTs /api/sessions/override and the server decides the apply
// path (rpc / respawn / deferred — F9 split is server-side, the frontend
// only renders the returned applied_via).

const TUNING_EFFORT_TIERS = ['low', 'medium', 'high', 'xhigh', 'max'];
let tuningPopoverCloseHandler = null;

// #1980 PR-2: the header tuning chips' document-level listener merged into
// the global nz.actions registry (keys tuning-model / tuning-effort) — the
// chips are rebuilt on repaint, so delegation stays the right shape.

function dismissTuningPopover() {
  const el = document.getElementById('tuning-popover');
  if (el) el.remove();
  if (tuningPopoverCloseHandler) {
    document.removeEventListener('click', tuningPopoverCloseHandler);
    tuningPopoverCloseHandler = null;
  }
}

// tuningToast is a minimal self-dismissed notice — the dashboard has no
// global toast helper, and the F7 rejection text (CLI-supplied, sanitized
// server-side) needs a visible surface that outlives the popover.
function tuningToast(msg, isError) {
  let t = document.getElementById('tuning-toast');
  if (t) t.remove();
  t = document.createElement('div');
  t.id = 'tuning-toast';
  t.style.cssText = 'position:fixed;top:16px;left:50%;transform:translateX(-50%);' +
    'max-width:70%;padding:10px 16px;border-radius:10px;z-index:var(--nz-z-toast);font-size:13px;' +
    'background:var(--nz-overlay-pill-bg);backdrop-filter:blur(8px);' +
    'border:1px solid ' + (isError ? 'var(--nz-danger, #d33)' : 'var(--nz-border)') + ';' +
    'color:var(--nz-text)';
  t.textContent = msg; // textContent — CLI-origin text must never hit innerHTML
  document.body.appendChild(t);
  setTimeout(() => t.remove(), isError ? 8000 : 4000);
}

// tuningModelsForSession: the popover's model choices, BackendInfo.models of
// the cached /api/cli/backends (agent-reported for kiro, cli.backends[].models
// for claude). An empty list leaves only the manual-input row.
function tuningModelsForSession(s) {
  const backendID = (s && s.backend) || pendingBackendID(selection.key, selection.node) ||
    (serverInfo.cliBackends && serverInfo.cliBackends.default) || '';
  if (!serverInfo.cliBackends || !Array.isArray(serverInfo.cliBackends.backends)) return { models: [], backendID };
  const entry = serverInfo.cliBackends.backends.find(b => b && b.id === backendID) ||
    serverInfo.cliBackends.backends.find(b => b && b.id === (serverInfo.cliBackends.default || ''));
  return {
    models: (entry && Array.isArray(entry.models)) ? entry.models : [],
    backendID: entry ? entry.id : backendID,
    // BackendInfo.protocol ("acp" | "stream-json") decides the empty-manifest
    // hint: only ACP backends ever report a list after their first session.
    protocol: entry ? (entry.protocol || '') : '',
  };
}

function openTuningPopover(kind) {
  dismissTuningPopover();
  if (!selection.key) return;
  // NG4: override API is local-only in this slice; remote sessions get an
  // explanation instead of a dead control (mirrors git chip's local-only).
  if ((selection.node || 'local') !== 'local') {
    tuningToast('远程节点会话暂不支持切换模型/档位', false);
    return;
  }
  const s = sessionList.sessionsData[sid(selection.key, selection.node)] ||
    // Not spawned yet: show the parked pick as current so a re-open marks it.
    (perSession.pendingTuning[selection.key] || {});
  const running = s.state === 'running';
  const rows = [];
  const current = kind === 'model' ? (s.model || '') : (s.effort || '');

  if (kind === 'model') {
    const { models, protocol } = tuningModelsForSession(s);
    for (const m of models) {
      const active = m.id === current || (current && current.indexOf(m.id) !== -1);
      rows.push({ value: m.id, label: m.id, desc: m.description || '', active });
    }
    if (models.length === 0) {
      // ACP backends (kiro) report their manifest on the first session;
      // stream-json backends (claude) never do — telling a claude operator to
      // wait would be a lie, so point at the config knob instead.
      rows.push({ header: true, label: protocol === 'acp'
        ? '清单在该 backend 首次会话后可用；可手动输入：'
        : '该 backend 不上报模型清单；可在 config.yaml 的 cli.backends[].models 配置候选，或手动输入：' });
    }
    rows.push({ input: true });
    rows.push({ value: '', label: '恢复默认（配置链）', reset: true });
  } else {
    for (const tier of TUNING_EFFORT_TIERS) {
      rows.push({ value: tier, label: tier, active: tier === current });
    }
    rows.push({ value: '', label: '恢复默认（配置链）', reset: true });
  }

  const anchor = document.getElementById(kind === 'model' ? 'header-model' : 'header-effort');
  if (!anchor) return;
  const pop = document.createElement('div');
  pop.id = 'tuning-popover';
  pop.style.cssText = 'position:fixed;min-width:220px;max-width:320px;max-height:340px;' +
    'overflow-y:auto;background:var(--nz-overlay-pill-bg);backdrop-filter:blur(8px);' +
    'border:1px solid var(--nz-border);border-radius:10px;padding:6px 0;z-index:120;' +
    'font-size:13px;scrollbar-width:thin';
  const rect = anchor.getBoundingClientRect();
  pop.style.left = Math.max(8, Math.min(rect.left, window.innerWidth - 340)) + 'px';
  pop.style.top = (rect.bottom + 6) + 'px';

  const hint = kind === 'effort'
    ? 'ⓘ 将重启 CLI 进程并恢复上下文' + (running ? '（会中断当前回合）' : '')
    : 'ⓘ 生效时机以返回的应用路径为准';
  let html = '<div class="nz-list-head">' +
    (kind === 'model' ? '切换模型' : '切换 effort 档位') + '</div>';
  for (const rowSpec of rows) {
    if (rowSpec.header) {
      html += '<div class="nz-list-empty">' + esc(rowSpec.label) + '</div>';
      continue;
    }
    if (rowSpec.input) {
      html += '<div class="nz-list-row"><input id="tuning-manual-input" type="text" placeholder="model id…" ' +
        'class="nz-tuning-input"></div>';
      continue;
    }
    const mark = rowSpec.active ? '● ' : (rowSpec.reset ? '↺ ' : '○ ');
    html += '<div class="tuning-opt nz-tuning-opt' +
      (rowSpec.active ? ' is-active' : '') +
      (rowSpec.reset ? ' is-reset' : '') +
      '" data-value="' + escAttr(rowSpec.value) + '"' +
      (rowSpec.desc ? ' title="' + escAttr(rowSpec.desc) + '"' : '') + '>' +
      mark + esc(rowSpec.label) + '</div>';
  }
  html += '<div class="nz-list-foot">' + esc(hint) + '</div>';
  pop.innerHTML = html;
  document.body.appendChild(pop);

  pop.querySelectorAll('.tuning-opt').forEach(item => {
    item.onmouseenter = () => item.style.background = 'var(--nz-hover-bg)';
    item.onmouseleave = () => item.style.background = '';
    item.addEventListener('click', () => {
      const v = item.dataset.value;
      dismissTuningPopover();
      // Respawn-family switches interrupt a running turn — confirm first
      // (§4.1 运行中防护; the server decides the actual path, we only warn
      // for the case that ALWAYS respawns: effort changes).
      if (kind === 'effort' && running &&
          !confirm('会话正在运行：切换档位将中断当前回合并重启 CLI 进程（上下文保留）。继续？')) {
        return;
      }
      postTuningOverride(kind, v);
    });
  });
  const manual = pop.querySelector('#tuning-manual-input');
  if (manual) {
    manual.addEventListener('click', (e) => e.stopPropagation());
    manual.onkeydown = (e) => {
      if (e.key === 'Enter') {
        const v = manual.value.trim();
        dismissTuningPopover();
        if (v) postTuningOverride('model', v);
      }
    };
  }
  setTimeout(() => {
    tuningPopoverCloseHandler = (e) => {
      if (!pop.contains(e.target)) dismissTuningPopover();
    };
    document.addEventListener('click', tuningPopoverCloseHandler);
  }, 0);
}

// postTuningOverride sends the switch and renders the outcome. Pending
// visual: the source chip dims until the next sessions poll repaints it
// with the server-confirmed value (no optimistic promotion — §4.1 三态;
// a rollback is visible because the poll simply keeps the old value).
async function postTuningOverride(kind, value) {
  const key = selection.key;
  const chip = document.getElementById(kind === 'model' ? 'header-model' : 'header-effort');
  if (chip) chip.style.opacity = '0.45';
  const restore = () => { if (chip) chip.style.opacity = ''; };
  try {
    const headers = { 'Content-Type': 'application/json' };
    const body = { key };
    body[kind] = value;
    const resp = await fetch(NZ_CONTRACT.API.sessions_override, {
      method: 'POST', headers, body: JSON.stringify(body),
    });
    if (!resp.ok) {
      const text = (await resp.text()).trim();
      restore();
      // 409 = CLI rejection (F7/F15): surface the CLI's own text verbatim.
      tuningToast(resp.status === 409 ? ('切换被 CLI 拒绝：' + text) : ('切换失败：' + text), true);
      return;
    }
    const data = await resp.json();
    const via = data.applied_via || '';
    const label = kind === 'model' ? '模型' : '档位';
    // No server row for this key = the session has not spawned yet; the pick
    // was parked server-side. Mirror it so the chips show it until promotion.
    const isPending = !sessionList.sessionsData[sid(key, selection.node)];
    if (isPending) {
      const prev = perSession.pendingTuning[key] || {};
      const next = Object.assign({}, prev);
      next[kind] = value;
      perSession.pendingTuning[key] = next;
      tuningToast(label + (value ? '已记录，发送首条消息时生效' : '已恢复默认'), false);
    } else if (via === 'rpc') {
      tuningToast(label + '已切换（对下一轮生效）', false);
    } else if (via === 'respawn') {
      tuningToast(label + '已记录，CLI 进程将重启并恢复上下文（下条消息生效）', false);
    } else {
      tuningToast(label + '已记录，将于下次会话进程启动时生效', false);
    }
    // Pull fresh state now rather than waiting out the poll interval; the
    // repaint clears the pending dim with the server-confirmed value.
    setTimeout(() => { restore(); shell.fetchSessions(); }, 800);
  } catch (e) {
    restore();
    tuningToast('切换请求失败：网络错误', true);
  }
}

// ===== Access-profile switch =====
// The header's access-profile chip opens this popover: pick a profile (the
// account the session bills) and, optionally, the model to run there, then
// 切换. The server closes the CLI and the next message resumes the same
// conversation under the new profile (docs/rfc/project-access-profile.md §7.1).
// A model change alone stays on the model popover: it keeps the account.

// The model <select>'s 保持当前 (no model sent) and 手动输入… values; no model
// id is spelled like them.
const TUNING_MODEL_KEEP = '__keep__';
const TUNING_MODEL_MANUAL = '__manual__';

async function openAccessProfilePopover() {
  dismissTuningPopover();
  if (!selection.key) return;
  if ((selection.node || 'local') !== 'local') {
    tuningToast('远程节点会话暂不支持切换访问档', false);
    return;
  }
  const key = selection.key;
  const s = sessionList.sessionsData[sid(key, selection.node)];
  if (!s) return;
  if (s.state === 'running') {
    tuningToast('会话正在运行：等这一轮结束再切换访问档', false);
    return;
  }
  const data = await fetchAccessProfiles();
  if (!data || !Array.isArray(data.profiles) || data.profiles.length === 0) return;
  if (selection.key !== key) return;
  const anchor = document.getElementById('header-access-profile');
  if (!anchor) return;

  const current = s.access_profile || '';
  const { models } = tuningModelsForSession(s);
  const profileRows = [{ id: '', label: '（全局默认）', ok: true }].concat(data.profiles.map(p => ({
    id: p.id || '', label: p.display_name || p.id || '', ok: p.secret_ok !== false, defaultModel: p.default_model || '',
  })));
  let html = '<div class="nz-list-head">切换访问档</div>';
  for (const p of profileRows) {
    const active = p.id === current;
    html += '<div class="nz-tuning-opt ap-opt' + (active ? ' is-active' : '') + (p.ok ? '' : ' is-disabled') +
      '" data-profile="' + escAttr(p.id) + '" data-default-model="' + escAttr(p.defaultModel || '') + '">' +
      (active ? '● ' : '○ ') + esc(p.label) + (p.ok ? '' : ' ⚠ 凭证缺失') + '</div>';
  }
  html += '<div class="nz-list-row"><label class="nz-field-label" for="ap-model-select">模型</label>' +
    '<select id="ap-model-select" class="nz-picker-select">' +
    '<option value="' + escAttr(TUNING_MODEL_KEEP) + '">保持当前' + (s.model ? '（' + esc(s.model) + '）' : '') + '</option>' +
    models.map(m => '<option value="' + escAttr(m.id) + '">' + esc(m.id) + '</option>').join('') +
    '<option value="' + escAttr(TUNING_MODEL_MANUAL) + '">手动输入…</option>' +
    '</select>' +
    '<input id="ap-model-input" type="text" class="nz-tuning-input nz-hidden" placeholder="model id，如 global.anthropic.claude-fable-5-2[1m]"></div>';
  html += '<div class="nz-list-row ap-actions"><button type="button" id="ap-submit" disabled>切换</button></div>';
  html += '<div class="nz-list-foot">ⓘ 会关闭当前 CLI 进程；下一条消息用新访问档恢复同一段对话</div>';

  const pop = document.createElement('div');
  pop.id = 'tuning-popover';
  pop.className = 'ap-popover';
  pop.style.cssText = 'position:fixed;min-width:260px;max-width:360px;max-height:420px;' +
    'overflow-y:auto;background:var(--nz-overlay-pill-bg);backdrop-filter:blur(8px);' +
    'border:1px solid var(--nz-border);border-radius:10px;padding:6px 0;z-index:120;' +
    'font-size:13px;scrollbar-width:thin';
  const rect = anchor.getBoundingClientRect();
  pop.style.left = Math.max(8, Math.min(rect.left, window.innerWidth - 380)) + 'px';
  pop.style.top = (rect.bottom + 6) + 'px';
  pop.innerHTML = html;
  document.body.appendChild(pop);

  const sel = pop.querySelector('#ap-model-select');
  const input = pop.querySelector('#ap-model-input');
  const submit = pop.querySelector('#ap-submit');
  let picked = null;
  const modelChoice = () => {
    if (sel.value === TUNING_MODEL_KEEP) return null;
    if (sel.value === TUNING_MODEL_MANUAL) return input.value.trim() || null;
    return sel.value;
  };
  const refresh = () => {
    // Staying on the current profile is a model change, which the model
    // popover makes without closing the CLI.
    submit.disabled = picked === null || picked === current;
  };
  pop.querySelectorAll('.ap-opt').forEach(item => {
    if (item.classList.contains('is-disabled')) return;
    item.addEventListener('click', () => {
      picked = item.dataset.profile;
      pop.querySelectorAll('.ap-opt').forEach(o => {
        const on = o === item;
        o.classList.toggle('is-picked', on);
        o.textContent = (on ? '● ' : '○ ') + o.textContent.slice(2);
      });
      const dm = item.dataset.defaultModel;
      if (dm && sel.value === TUNING_MODEL_KEEP) {
        if (![...sel.options].some(o => o.value === dm)) {
          const o = document.createElement('option');
          o.value = dm; o.textContent = dm;
          sel.insertBefore(o, sel.lastElementChild);
        }
        sel.value = dm;
      }
      refresh();
    });
  });
  sel.addEventListener('change', () => {
    input.classList.toggle('nz-hidden', sel.value !== TUNING_MODEL_MANUAL);
    if (sel.value === TUNING_MODEL_MANUAL) input.focus();
  });
  input.addEventListener('keydown', (e) => { if (e.key === 'Enter' && !submit.disabled) submit.click(); });
  submit.addEventListener('click', () => {
    if (picked === null) return;
    const model = modelChoice();
    dismissTuningPopover();
    postAccessProfileSwitch(key, picked, model);
  });
  setTimeout(() => {
    tuningPopoverCloseHandler = (e) => {
      if (!pop.contains(e.target)) dismissTuningPopover();
    };
    document.addEventListener('click', tuningPopoverCloseHandler);
  }, 0);
}

// postAccessProfileSwitch sends the switch. Like postTuningOverride it does
// not promote the pick optimistically: the chip dims until the sessions poll
// repaints it with the server's value.
async function postAccessProfileSwitch(key, profile, model) {
  const chip = document.getElementById('header-access-profile');
  if (chip) chip.style.opacity = '0.45';
  const restore = () => { if (chip) chip.style.opacity = ''; };
  const body = { key, access_profile: profile };
  if (model) body.model = model;
  try {
    const resp = await fetch(NZ_CONTRACT.API.sessions_override, {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
    });
    if (!resp.ok) {
      const text = (await resp.text()).trim();
      restore();
      tuningToast(resp.status === 409 ? '会话正在运行：等这一轮结束再切换访问档' : ('切换失败：' + text), true);
      return;
    }
    const data = await resp.json();
    tuningToast(data.applied_via === 'respawn'
      ? '访问档已切换，CLI 进程已关闭，下条消息用新访问档恢复对话'
      : '访问档已切换，下条消息生效', false);
    setTimeout(() => { restore(); shell.fetchSessions(); }, 800);
  } catch (e) {
    restore();
    tuningToast('切换请求失败：网络错误', true);
  }
}

// repaintGitChip re-renders the chip for the currently selected session from
// cache. Called at the end of renderMainShell so a header rebuild triggered by
// something unrelated (rename, model update) doesn't drop the chip.
function repaintGitChip() {
  if (!selection.key) { setHeaderGitChip(''); return; }
  setHeaderGitChip(gitChipHtml(gitStateCache[sid(selection.key, selection.node)]));
}

async function fetchGitState(key, node) {
  node = node || 'local';
  // Git state is a local-node concern: a remote session's workspace lives on
  // that node's filesystem, so resolving it here would describe the wrong
  // tree. Clear the chip so a remote session doesn't inherit the previously
  // selected local session's branch.
  if (!key || node !== 'local') { setHeaderGitChip(''); return; }
  const cacheKey = sid(key, node);
  try {
    const headers = {};
    const resp = await fetch(NZ_CONTRACT.API.sessions_git + '?key=' + encodeURIComponent(key), { headers });
    // The cache entry is per-session so dropping it is always right; the
    // header chip is only cleared when this session is still the selected one.
    if (!resp.ok) { delete gitStateCache[cacheKey]; if (selection.key !== key || selection.node !== node) return; setHeaderGitChip(''); return; }
    const data = await resp.json();
    gitStateCache[cacheKey] = data;
    // Guard against a stale response landing after the user switched sessions.
    if (selection.key !== key || selection.node !== node) return;
    setHeaderGitChip(gitChipHtml(data));
  } catch (_) {
    delete gitStateCache[cacheKey];
    if (selection.key !== key || selection.node !== node) return;
    setHeaderGitChip('');
  }
}

// invalidateGitState drops the cached payload for a session and re-resolves it.
// Called after /cd (the session's workspace moved, so the branch may differ)
// and on session dismissal so a recycled key cannot inherit a stale branch.
function invalidateGitState(key, node) {
  if (!key) return;
  delete gitStateCache[sid(key, node || 'local')];
  if (key === selection.key) fetchGitState(key, node || 'local');
}

// clearMainIfSelected empties the main panel when the dismissed session is
// the one on screen.
function clearMainIfSelected(key) {
  if (selection.key !== key) return;
  selection.key = null;
  if (sessionStream.subscribedKey === key) sessionStream.unsubscribe();
  showMainEmpty();
}

function showMainEmpty() {
  document.getElementById('main').innerHTML = mainEmptyHtml();
  wireQuickAskInput();
}

function resyncSidebar() {
  sessionList.lastVersion = 0;
  shell.debouncedFetchSessions();
}

// dismissDiscovered kills an external (discovered) CLI via
// /api/discovered/close; its card goes once the server confirms.
async function dismissDiscovered(key, node) {
  const d = findDiscovered(parseDiscoveredPid(key), node);
  if (!d) { showToast('未找到该外部会话', 'warning'); return; }
  try {
    const headers = {'Content-Type': 'application/json'};
    try {
      await fetchJSON(NZ_CONTRACT.API.discovered_close, {
        timeoutMs: 10000,
        method: 'POST', headers,
        body: JSON.stringify({pid: d.pid, session_id: d.session_id || '', cwd: d.cwd || '', proc_start_time: d.proc_start_time || 0, node: node || ''})
      });
    } catch (err) {
      if (err && err.status) showAPIError('关闭外部会话', err.status, err.message || '');
      else showNetworkError('关闭外部会话', err);
      return;
    }
    dropDiscovered(d.pid, d.node);
    if (selection.pendingDiscovered && sameDiscovered(selection.pendingDiscovered, d.pid, d.node)) {
      selection.pendingDiscovered = null;
      stopPreviewPolling();
      showMainEmpty();
    }
    removeSidebarCard(key);
    resyncSidebar();
  } catch (e) { showNetworkError('关闭外部会话', e); }
}

// UNDO_MS is how long a removed session can be brought back (已移除 · 撤销)
// before its DELETE goes out.
const UNDO_MS = 5000;
// pendingRemoval is the removal whose DELETE waits out UNDO_MS, null for none.
let pendingRemoval = null;

// dismissManaged removes a session visibly and stoppably rather than behind
// a confirm: the card goes at once and a toast offers 撤销 for UNDO_MS; only
// then does DELETE /api/sessions go out. A second removal sends the first
// one's DELETE now (the toast offers the latest only), as does leaving the
// page. forget drops the browser's own state for the key, also only then.
function dismissManaged(key, node, forget) {
  const skey = sid(key, node);
  if (pendingRemoval) pendingRemoval.commit();
  // Mark dismissed so an in-flight poll / sessions_update event can't
  // resurrect the card while it waits or before DELETE confirms.
  sessionList.optimisticDeleteKeys.add(skey);
  const row = sessionList.sessionsData[skey];
  const wasOpen = selection.key === key && (selection.node || 'local') === node;
  delete sessionList.sessionsData[skey];
  clearMainIfSelected(key);
  removeSidebarCard(key);

  const p = { skey };
  const settle = () => {
    clearTimeout(p.timer);
    if (pendingRemoval === p) { pendingRemoval = null; hideUndoToast(p); }
  };
  p.commit = (leaving) => {
    settle();
    forget();
    deleteManaged(key, node, skey, leaving === true);
  };
  p.undo = () => {
    settle();
    sessionList.optimisticDeleteKeys.delete(skey);
    if (row && !sessionList.sessionsData[skey]) sessionList.sessionsData[skey] = row;
    sessionList.lastVersion = 0;
    shell.debouncedFetchSessions().then(() => {
      if (wasOpen && !selection.key) shell.selectSession(key, node);
    });
  };
  pendingRemoval = p;
  p.timer = setTimeout(p.commit, UNDO_MS);
  showUndoToast(p, '已移除' + (row && sessionTitleText(row) ? '「' + sessionTitleText(row) + '」' : '会话'));
}

// A page left within UNDO_MS still removes what it was told to.
window.addEventListener('pagehide', () => { if (pendingRemoval) pendingRemoval.commit(true); });

// deleteManaged sends the DELETE. DELETE /api/sessions unregisters
// synchronously and tears down in a detached goroutine (RemoveAsync), so 200
// means "gone from the list"; the UI does not wait for it. leaving keeps the
// request alive past the page's unload.
function deleteManaged(key, node, skey, leaving) {
  const headers = {'Content-Type': 'application/json'};
  const body = {key: key};
  if (node && node !== 'local') body.node = node;
  // Fire-and-forget: do NOT await — the UI is already updated. On failure we
  // re-sync from the server so a genuinely-undeleted session reappears.
  fetchJSON(NZ_CONTRACT.API.sessions, {timeoutMs: 10000, method: 'DELETE', headers, body: JSON.stringify(body), keepalive: leaving})
    .catch(err => {
      // 404 means the session was already gone — that's the outcome we want,
      // so swallow it. Any other error means the delete may not have landed:
      // surface it and let the re-sync below pull the real list back.
      if (err && err.status !== 404) {
        if (err.status) showAPIError('删除会话', err.status, err.message || '');
        else showNetworkError('删除会话', err);
      }
    })
    .finally(() => {
      // Stop suppressing this key so the next fetch reflects server truth:
      // if the delete stuck, the session stays gone; if it failed, the card
      // comes back (operator must re-select it — we intentionally don't
      // restore the cleared main panel to avoid masking a failed delete).
      sessionList.optimisticDeleteKeys.delete(skey);
      resyncSidebar();
    });
}

// showUndoToast shows removal p's "已移除 · 撤销" toast: its own element,
// apart from #toast, which takes no clicks.
function showUndoToast(p, text) {
  let el = document.getElementById('undo-toast');
  if (!el) {
    el = document.createElement('div');
    el.id = 'undo-toast';
    el.className = 'undo-toast';
    el.setAttribute('role', 'status');
    el.setAttribute('aria-live', 'polite');
    el.innerHTML = '<span class="undo-toast-text"></span><button type="button" class="undo-toast-btn">撤销</button>';
    el.querySelector('button').addEventListener('click', () => { if (pendingRemoval) pendingRemoval.undo(); });
    document.body.appendChild(el);
  }
  el.querySelector('.undo-toast-text').textContent = text;
  el.hidden = false;
  el.dataset.key = p.skey;
}

function hideUndoToast(p) {
  const el = document.getElementById('undo-toast');
  if (el && el.dataset.key === p.skey) el.hidden = true;
}

// forgetSession drops what this browser keeps for a removed key.
function forgetSession(key, node) {
  delete perSession.drafts[key];
  delete perSession.scrollPos[sid(key, node)];
  // Drop the cached git state so a later key reuse can't inherit this
  // session's branch chip before its own fetch resolves.
  delete gitStateCache[sid(key, node)];
  // A dismiss before the send consumes the picks, or before the server lists
  // the key, would leave them for a re-created key.
  delete perSession.backends[key];
  delete perSession.accessProfiles[key];
  delete perSession.sentPicks[key];
}

// dismissSession removes a session from the sidebar. There is no
// confirmation: a session naozhi runs is removed with 撤销 offered for a
// few seconds (dismissManaged); a never-sent one is recoverable by
// re-entering the prompt, a discovered one by reopening the CLI.
async function dismissSession(key, node, opts) {
  node = node || 'local';
  if (!isCronSessionKey(key) && perSession.workspaces[key] === undefined && !isDiscoveredKey(key)) {
    // DELETE drops the parked tuning server-side; a never-sent key's (below) stays.
    dismissManaged(key, node, () => { forgetSession(key, node); delete perSession.pendingTuning[key]; });
    return;
  }
  forgetSession(key, node);

  // cron-panel-consolidation RFC §4.2: cron stubs are filtered server-side,
  // so this branch only runs if a server bug leaks one into the sidebar.
  // Remove the rogue card but DO NOT call DELETE /api/sessions (the cron
  // scheduler still owns the stub) and DO NOT touch cron panel state.
  if (isCronSessionKey(key)) {
    clearMainIfSelected(key);
    removeSidebarCard(key);
    resyncSidebar();
    return;
  }

  // If it's a pending (never-sent) session, just remove from localStorage
  if (perSession.workspaces[key] !== undefined) {
    removePendingSession(key);
    delete sessionList.sessionsData[sid(key, node)];
    if (selection.key === key) {
      selection.key = null;
      showMainEmpty();
    }
    resyncSidebar();
    return;
  }

  await dismissDiscovered(key, node);
}

// Operator-facing rename flow. Prompts for a new display label; empty input
// clears any prior label and falls back to the summary/last_prompt display
// chain. Uses PATCH /api/sessions/label so the mutation round-trips through
// the server and persists across reloads.
async function renameSession() {
  if (!selection.key) return;
  const s = sessionList.sessionsData[sid(selection.key, selection.node)] || {};
  const current = s.user_label || '';
  // replaced window.prompt with themed promptDialog so the
  // rename flow matches the rest of the dashboard (dark theme, trapFocus,
  // Esc/backdrop cancel) and doesn't block the event loop on mobile.
  const input = await promptDialog({
    title: '重命名会话',
    message: '留空恢复默认标题，最多 128 字节',
    defaultValue: current,
    placeholder: '输入新标题',
    confirmText: '保存',
    maxLength: 128,
  });
  if (input === null) return; // user cancelled
  const next = input.trim();
  if (next === current) return;
  const headers = {'Content-Type': 'application/json'};
  const body = {key: selection.key, label: next};
  if (selection.node && selection.node !== 'local') body.node = selection.node;
  try {
    await fetchJSON(NZ_CONTRACT.API.sessions_label, {
      timeoutMs: 10000,
      method: 'PATCH', headers,
      body: JSON.stringify(body),
    });
  } catch (err) {
    if (err && err.status) showAPIError('重命名', err.status, err.message || '');
    else showNetworkError('重命名', err);
    return;
  }
  // Patch local cache so the title refreshes before the next poll lands.
  const cacheKey = sid(selection.key, selection.node);
  if (sessionList.sessionsData[cacheKey]) {
    sessionList.sessionsData[cacheKey].user_label = next;
  }
  sessionList.lastVersion = 0;
  shell.debouncedFetchSessions();
  // Header-only repaint: a full renderMainShell would rebuild #events-scroll
  // empty with nothing refetching the conversation (see renderMainHeader).
  shell.renderMainHeader();
  showToast(next ? '已重命名' : '已恢复默认标题');
}


export {
  dismissSession,
  fetchGitState,
  invalidateGitState,
  openAccessProfilePopover,
  openTuningPopover,
  renameSession,
  repaintGitChip,
};
