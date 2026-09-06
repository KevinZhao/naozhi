// tuning.js — extracted from dashboard.js (#2558 D4).
//
// Verbatim region move: `git diff --color-moved` shows the body as a pure
// move; the import block, the deps table and the export block below are the
// only additions.
//
// Layering (D4-1 rule): a module dashboard imports must NOT import dashboard
// back — that cycle puts dashboard's own top-level consts in TDZ while this
// module evaluates. Dashboard state is read through nz.state; its helpers are
// injected once via configureTuning(), called from dashboard's module body.
import { esc, escAttr, fetchJSON, isCronSessionKey, nzState, showToast } from './nz_util.js';

const deps = {
  debouncedFetchSessions: null,
  dropDiscovered: null,
  fetchSessions: null,
  findDiscovered: null,
  getToken: null,
  gitChipHtml: null,
  gitStateCache: null,
  isDiscoveredKey: null,
  mainEmptyHtml: null,
  parseDiscoveredPid: null,
  promptDialog: null,
  removePendingSession: null,
  renderMainHeader: null,
  sameDiscovered: null,
  sessionAccessProfiles: null,
  sessionBackends: null,
  sessionWorkspaces: null,
  setHeaderGitChip: null,
  showAPIError: null,
  showNetworkError: null,
  sid: null,
  stopPreviewPolling: null,
  wireQuickAskInput: null,
  wsm: null,
};
export function configureTuning(impl) {
  for (const k of Object.keys(deps)) {
    if (typeof impl[k] === 'undefined') throw new Error('tuning dep missing: ' + k);
    deps[k] = impl[k];
  }
}

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

// tuningModelsForSession resolves the popover's model choices from the
// cached /api/cli/backends payload (BackendInfo.models: agent-reported for
// kiro, cli.backends[].models fallback for claude). Empty list → the
// popover shows its manual-input row only.
function tuningModelsForSession(s) {
  const backendID = (s && s.backend) || deps.sessionBackends[nzState.selectedKey] ||
    (nzState.cliBackends && nzState.cliBackends.default) || '';
  if (!nzState.cliBackends || !Array.isArray(nzState.cliBackends.backends)) return { models: [], backendID };
  const entry = nzState.cliBackends.backends.find(b => b && b.id === backendID) ||
    nzState.cliBackends.backends.find(b => b && b.id === (nzState.cliBackends.default || ''));
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
  if (!nzState.selectedKey) return;
  // NG4: override API is local-only in this slice; remote sessions get an
  // explanation instead of a dead control (mirrors git chip's local-only).
  if ((nzState.selectedNode || 'local') !== 'local') {
    tuningToast('远程节点会话暂不支持切换模型/档位', false);
    return;
  }
  const s = nzState.sessionsData[deps.sid(nzState.selectedKey, nzState.selectedNode)] ||
    // Not spawned yet: show the parked pick as current so a re-open marks it.
    (nzState.sessionPendingTuning[nzState.selectedKey] || {});
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
  const key = nzState.selectedKey;
  const chip = document.getElementById(kind === 'model' ? 'header-model' : 'header-effort');
  if (chip) chip.style.opacity = '0.45';
  const restore = () => { if (chip) chip.style.opacity = ''; };
  try {
    const headers = { 'Content-Type': 'application/json' };
    const t = deps.getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
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
    const isPending = !nzState.sessionsData[deps.sid(key, nzState.selectedNode)];
    if (isPending) {
      const prev = nzState.sessionPendingTuning[key] || {};
      const next = Object.assign({}, prev);
      next[kind] = value;
      nzState.sessionPendingTuning[key] = next;
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
    setTimeout(() => { restore(); deps.fetchSessions(); }, 800);
  } catch (e) {
    restore();
    tuningToast('切换请求失败：网络错误', true);
  }
}

// repaintGitChip re-renders the chip for the currently selected session from
// cache. Called at the end of renderMainShell so a header rebuild triggered by
// something unrelated (rename, model update) doesn't drop the chip.
function repaintGitChip() {
  if (!nzState.selectedKey) { deps.setHeaderGitChip(''); return; }
  deps.setHeaderGitChip(deps.gitChipHtml(deps.gitStateCache[deps.sid(nzState.selectedKey, nzState.selectedNode)]));
}

async function fetchGitState(key, node) {
  node = node || 'local';
  // Git state is a local-node concern: a remote session's workspace lives on
  // that node's filesystem, so resolving it here would describe the wrong
  // tree. Clear the chip so a remote session doesn't inherit the previously
  // selected local session's branch.
  if (!key || node !== 'local') { deps.setHeaderGitChip(''); return; }
  const cacheKey = deps.sid(key, node);
  try {
    const headers = {};
    const t = deps.getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const resp = await fetch(NZ_CONTRACT.API.sessions_git + '?key=' + encodeURIComponent(key), { headers });
    // The cache entry is per-session so dropping it is always right; the
    // header chip is only cleared when this session is still the selected one.
    if (!resp.ok) { delete deps.gitStateCache[cacheKey]; if (nzState.selectedKey !== key || nzState.selectedNode !== node) return; deps.setHeaderGitChip(''); return; }
    const data = await resp.json();
    deps.gitStateCache[cacheKey] = data;
    // Guard against a stale response landing after the user switched sessions.
    if (nzState.selectedKey !== key || nzState.selectedNode !== node) return;
    deps.setHeaderGitChip(deps.gitChipHtml(data));
  } catch (_) {
    delete deps.gitStateCache[cacheKey];
    if (nzState.selectedKey !== key || nzState.selectedNode !== node) return;
    deps.setHeaderGitChip('');
  }
}

// invalidateGitState drops the cached payload for a session and re-resolves it.
// Called after /cd (the session's workspace moved, so the branch may differ)
// and on session dismissal so a recycled key cannot inherit a stale branch.
function invalidateGitState(key, node) {
  if (!key) return;
  delete deps.gitStateCache[deps.sid(key, node || 'local')];
  if (key === nzState.selectedKey) fetchGitState(key, node || 'local');
}

// removeSidebarCard drops a session card from the DOM without waiting for
// the next renderSidebar. It MUST also reset nzState._lastSidebarHtml: renderSidebar
// skips `list.innerHTML = html` when the rebuilt string equals the cache, so
// a DOM-only removal would leave the cache describing a card that is no
// longer mounted and the next (identical) render would never bring it back
// — e.g. after a failed DELETE whose .finally re-fetches the list.
function removeSidebarCard(key) {
  // Escape like setActiveSessionCard: discovered keys embed the node name, so
  // a `"` or `\` would otherwise make querySelector throw mid-takeover/dismiss.
  const card = document.querySelector('.session-card[data-key="' + (window.CSS && CSS.escape ? CSS.escape(key) : key) + '"]');
  if (card) card.remove();
  nzState._lastSidebarHtml = null;
}

// dismissSession removes a session from the sidebar. The × button deletes
// immediately with no confirmation — per operator preference, the friction
// isn't worth it. Accidental deletes are recoverable by re-entering the
// prompt (pending) or reopening the CLI (remote/discovered).
async function dismissSession(key, node, opts) {
  node = node || 'local';
  delete nzState.sessionDrafts[key];
  delete nzState.sessionScrollPos[deps.sid(key, node)];
  // Drop the cached git state so a later key reuse can't inherit this
  // session's branch chip before its own fetch resolves.
  delete deps.gitStateCache[deps.sid(key, node)];
  // deps.sessionBackends is normally consumed on first sendMessage. A dismiss
  // before any send leaves the entry behind; clear it defensively so a
  // subsequent re-create with the same key (unlikely but possible if the
  // ms timestamp collides on rapid double-create) doesn't inherit a
  // stale backend pick.
  delete deps.sessionBackends[key];
  delete deps.sessionAccessProfiles[key];

  // cron-panel-consolidation RFC §4.2: defensive guard. Cron stubs are
  // filtered server-side so this branch should never run in production —
  // but if a future server bug ever leaks a cron key through, we must
  // NOT call DELETE /api/sessions (the scheduler still owns the stub).
  if (isCronSessionKey(key)) {
    // cron-panel-consolidation RFC §4.2: cron stubs are filtered server-side
    // and should never appear in the sidebar at all — this branch only
    // executes if a future server bug leaks one through. Guard-rail behaviour:
    // remove the rogue card from the DOM but DO NOT call DELETE /api/sessions
    // (the cron scheduler still owns the stub) and DO NOT mutate any cron
    // panel state. Single source of truth for cron-job lifecycle remains
    // the 定时任务 panel (cronDelete → DELETE /api/cron).
    if (nzState.selectedKey === key) {
      nzState.selectedKey = null;
      if (deps.wsm.subscribedKey === key) deps.wsm.unsubscribe();
      document.getElementById('main').innerHTML = deps.mainEmptyHtml();
      deps.wireQuickAskInput();
    }
    removeSidebarCard(key);
    nzState.lastVersion = 0;
    deps.debouncedFetchSessions();
    return;
  }

  // If it's a pending (never-sent) session, just remove from localStorage
  if (deps.sessionWorkspaces[key] !== undefined) {
    deps.removePendingSession(key);
    delete nzState.sessionsData[deps.sid(key, node)];
    if (nzState.selectedKey === key) {
      nzState.selectedKey = null;
      document.getElementById('main').innerHTML = deps.mainEmptyHtml();
      deps.wireQuickAskInput();
    }
    nzState.lastVersion = 0;
    deps.debouncedFetchSessions();
    return;
  }

  // Discovered session — kill external process via /api/discovered/close
  if (deps.isDiscoveredKey(key)) {
    const d = deps.findDiscovered(deps.parseDiscoveredPid(key), node);
    if (!d) { showToast('未找到该外部会话', 'warning'); return; }
    try {
      const headers = {'Content-Type': 'application/json'};
      const token = deps.getToken();
      if (token) headers['Authorization'] = 'Bearer ' + token;
      try {
        await fetchJSON(NZ_CONTRACT.API.discovered_close, {
          timeoutMs: 10000,
          method: 'POST', headers,
          body: JSON.stringify({pid: d.pid, session_id: d.session_id || '', cwd: d.cwd || '', proc_start_time: d.proc_start_time || 0, node: node || ''})
        });
      } catch (err) {
        if (err && err.status) deps.showAPIError('关闭外部会话', err.status, err.message || '');
        else deps.showNetworkError('关闭外部会话', err);
        return;
      }
      deps.dropDiscovered(d.pid, d.node);
      if (nzState.pendingDiscovered && deps.sameDiscovered(nzState.pendingDiscovered, d.pid, d.node)) {
        nzState.pendingDiscovered = null;
        deps.stopPreviewPolling();
        document.getElementById('main').innerHTML = deps.mainEmptyHtml();
        deps.wireQuickAskInput();
      }
      removeSidebarCard(key);
      nzState.lastVersion = 0;
      deps.debouncedFetchSessions();
    } catch (e) { deps.showNetworkError('关闭外部会话', e); }
    return;
  }

  // Optimistic delete: the card vanishes immediately rather than freezing
  // for the server's teardown round-trip. The backend's DELETE /api/sessions
  // now unregisters the session synchronously and runs the slow teardown
  // (proc.Close up to 8s + event-log/attachment cleanup) in a detached
  // goroutine (RemoveAsync), so 200 means "gone from the list" and arrives
  // fast — but we don't even wait for it to update the UI.
  const skey = deps.sid(key, node);
  // Mark dismissed so an in-flight poll / sessions_update event can't
  // resurrect the card before DELETE confirms (cleared in finally below).
  nzState._optimisticDeleteKeys.add(skey);
  delete nzState.sessionsData[skey];
  if (nzState.selectedKey === key) {
    nzState.selectedKey = null;
    if (deps.wsm.subscribedKey === key) deps.wsm.unsubscribe();
    document.getElementById('main').innerHTML = deps.mainEmptyHtml();
    deps.wireQuickAskInput();
  }
  removeSidebarCard(key);

  const headers = {'Content-Type': 'application/json'};
  const token = deps.getToken();
  if (token) headers['Authorization'] = 'Bearer ' + token;
  const body = {key: key};
  if (node && node !== 'local') body.node = node;
  // Fire-and-forget: do NOT await — the UI is already updated. On failure we
  // re-sync from the server so a genuinely-undeleted session reappears.
  fetchJSON(NZ_CONTRACT.API.sessions, {timeoutMs: 10000, method: 'DELETE', headers, body: JSON.stringify(body)})
    .catch(err => {
      // 404 means the session was already gone — that's the outcome we want,
      // so swallow it. Any other error means the delete may not have landed:
      // surface it and let the re-sync below pull the real list back.
      if (err && err.status !== 404) {
        if (err.status) deps.showAPIError('删除会话', err.status, err.message || '');
        else deps.showNetworkError('删除会话', err);
      }
    })
    .finally(() => {
      // Stop suppressing this key so the next fetch reflects server truth:
      // if the delete stuck, the session stays gone; if it failed, the card
      // comes back (operator must re-select it — we intentionally don't
      // restore the cleared main panel to avoid masking a failed delete).
      nzState._optimisticDeleteKeys.delete(skey);
      nzState.lastVersion = 0;
      deps.debouncedFetchSessions();
    });
}

// Operator-facing rename flow. Prompts for a new display label; empty input
// clears any prior label and falls back to the summary/last_prompt display
// chain. Uses PATCH /api/sessions/label so the mutation round-trips through
// the server and persists across reloads.
async function renameSession() {
  if (!nzState.selectedKey) return;
  const s = nzState.sessionsData[deps.sid(nzState.selectedKey, nzState.selectedNode)] || {};
  const current = s.user_label || '';
  // RNEW-UX-013: replaced window.prompt with themed deps.promptDialog so the
  // rename flow matches the rest of the dashboard (dark theme, trapFocus,
  // Esc/backdrop cancel) and doesn't block the event loop on mobile.
  const input = await deps.promptDialog({
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
  const token = deps.getToken();
  if (token) headers['Authorization'] = 'Bearer ' + token;
  const body = {key: nzState.selectedKey, label: next};
  if (nzState.selectedNode && nzState.selectedNode !== 'local') body.node = nzState.selectedNode;
  try {
    await fetchJSON(NZ_CONTRACT.API.sessions_label, {
      timeoutMs: 10000,
      method: 'PATCH', headers,
      body: JSON.stringify(body),
    });
  } catch (err) {
    if (err && err.status) deps.showAPIError('重命名', err.status, err.message || '');
    else deps.showNetworkError('重命名', err);
    return;
  }
  // Patch local cache so the title refreshes before the next poll lands.
  const cacheKey = deps.sid(nzState.selectedKey, nzState.selectedNode);
  if (nzState.sessionsData[cacheKey]) {
    nzState.sessionsData[cacheKey].user_label = next;
  }
  nzState.lastVersion = 0;
  deps.debouncedFetchSessions();
  // Header-only repaint: a full renderMainShell would rebuild #events-scroll
  // empty with nothing refetching the conversation (see deps.renderMainHeader).
  deps.renderMainHeader();
  showToast(next ? '已重命名' : '已恢复默认标题');
}


export {
  dismissSession,
  fetchGitState,
  invalidateGitState,
  openTuningPopover,
  removeSidebarCard,
  renameSession,
  repaintGitChip,
};
