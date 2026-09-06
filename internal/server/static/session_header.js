// session_header.js — the chat header's run-history panel and status chips
// (#2558 D4-3).
//
// Verbatim move out of dashboard.js (two adjacent regions: the run-history
// timeline from docs/rfc/session-run-metrics.md §8, and the git / effort /
// overlay-drift / spawn-diag chips). Only the import + dep-wiring lines are
// new.
//
// Layering (D4-1 rule): never import dashboard back — its mutable state is
// read via nz.state, its helpers are injected once via
// configureSessionHeader().
import { esc, escAttr, formatCostUSD, formatDurationShort, formatRunDuration, nzState, nzTest } from './nz_util.js';

const deps = {
  fetchSessions: null,
  formatAbsTime: null,
  getToken: null,
  renderMainShell: null,
  sid: null,
};
export function configureSessionHeader(impl) {
  for (const k of Object.keys(deps)) {
    if (typeof impl[k] === 'undefined') throw new Error('session_header dep missing: ' + k);
    deps[k] = impl[k];
  }
}

// ---- Session run-history timeline (docs/rfc/session-run-metrics.md §8) ----
//
// Best-effort: a fetch failure or empty history just leaves the panel hidden;
// the conversation surface is never blocked on it.

// sessionRunOutcomeMeta maps an outcome to its dot class + Chinese label,
// reusing the green/red/purple status vocabulary the cron timeline established.
function sessionRunOutcomeMeta(outcome) {
  switch (outcome) {
    case 'completed': return { dot: 'ok', label: '完成' };
    case 'timeout': return { dot: 'timeout', label: '超时' };
    case 'canceled': return { dot: 'cancel', label: '已取消' };
    case 'error': return { dot: 'err', label: '出错' };
    default: return { dot: '', label: outcome || '—' };
  }
}

function sessionRunStatLabel(ms) {
  // Reuse the cron duration formatter (cron_view.js) for visual parity. It is
  // a top-level function in the shared script scope.
  return formatRunDuration(ms) || '0ms';
  return Math.round(ms) + 'ms';
}

// sessionRunTotalLabel formats a CUMULATIVE duration for the header's
// 「共 X」total. Unlike sessionRunStatLabel (per-run, reuses formatRunDuration
// which tops out at "Xm Ys"), a session's total across many runs routinely
// exceeds an hour — so we use formatDurationShort, which carries an "Xh YYm"
// tier and renders "3h 07m" instead of an unreadable "187m 3s". Per-run rows
// keep sessionRunStatLabel so their second-level precision ("26.5s") is intact.
function sessionRunTotalLabel(ms) {
  return formatDurationShort(ms);
  return sessionRunStatLabel(ms);
}

function sessionRunsStatsHtml(stats) {
  if (!stats || !stats.count) return '';
  const parts = [];
  // 头部总览只显示「总计」三项——一共多少轮、总耗时、总花费——回答用户首屏
  // 关心的"这个会话累计跑了多少"。逐条 run 的耗时/成本/首字节延迟等明细
  // 保留在下方折叠的「运行记录」面板里（sessionRunRowHtml），保持头部简洁。
  // 对话的单位是「轮」(round)：一次 run = 一轮对话往返。
  parts.push('<span class="srp-stat" title="累计运行轮次">' + esc(stats.count + ' 轮') + '</span>');
  parts.push('<span class="srp-stat" title="累计耗时">共 ' + esc(sessionRunTotalLabel(stats.total_ms || 0)) + '</span>');
  // 总花费：仅当有成本数据时显示（本机 claude run 通常带 cost_usd；某些
  // backend / 旧记录可能为 0）。formatCostUSD 对 <$0.01 保留 4 位小数，返回 ''
  // 时整条不渲染，避免出现无意义的"花费 $0.00"。
  const costStr = formatCostUSD(stats.total_cost_usd || 0);
  if (costStr) {
    parts.push('<span class="srp-stat" title="累计花费">花费 ' + esc(costStr) + '</span>');
  }
  if (stats.timeout_count > 0) {
    parts.push('<span class="srp-stat bad" title="超时次数">⚠ ' + esc(String(stats.timeout_count)) + '</span>');
  }
  return parts.join('<span class="srp-stat-sep">·</span>');
}

function sessionRunRowHtml(r) {
  const meta = sessionRunOutcomeMeta(r.outcome);
  // deps.formatAbsTime is the dashboard's single timestamp formatter; use it for
  // both the visible label and the hover title (ux-contract: timestamps carry
  // a deps.formatAbsTime title). A relative/colloquial label could be layered later.
  const started = r.started_at ? deps.formatAbsTime(r.started_at) : '';
  const startedShort = started || '—';
  const dur = sessionRunStatLabel(r.duration_ms || 0);
  const sub = [];
  if (typeof r.first_byte_ms === 'number' && r.first_byte_ms > 0) {
    sub.push('<span title="首字节延迟">首字节 ' + esc(sessionRunStatLabel(r.first_byte_ms)) + '</span>');
  }
  if (r.cost_usd) {
    sub.push('<span title="本次成本估算">' + esc(formatCostUSD(r.cost_usd)) + '</span>');
  }
  const subRow = sub.length
    ? '<div class="srr-sub">' + sub.join('<span class="srr-sep">·</span>') + '</div>'
    : '';
  return '<div class="srr">' +
      '<div class="srr-main">' +
        '<span class="srr-dot ' + meta.dot + '" aria-hidden="true"></span>' +
        '<span class="srr-state">' + esc(meta.label) + '</span>' +
        '<span class="srr-time"' + (started ? ' title="' + escAttr(started) + '"' : '') + '>' + esc(startedShort) + '</span>' +
        '<span class="srr-dur">' + esc(dur) + '</span>' +
      '</div>' +
      subRow +
    '</div>';
}

// setHeaderRunStats writes (or clears) the aggregate run-stats node that lives
// in the session-detail header's .detail line. The run-history stats used to
// sit inside the panel <summary>; they were promoted to the header so the
// per-session "N 轮 · 均 X · 最长 X" overview is always visible without
// expanding the (collapsed-by-default) timeline. The header node is built
// empty by deps.renderMainShell, so absence = no-op rather than throw.
function setHeaderRunStats(html) {
  const el = document.getElementById('header-runstats');
  if (el) el.innerHTML = html || '';
}

function renderSessionRunsPanel(data) {
  const panel = document.getElementById('session-runs-panel');
  if (!panel) return;
  const runs = (data && Array.isArray(data.runs)) ? data.runs : [];
  const stats = data && data.stats;
  if (!runs.length) {
    // Hidden entirely when there's no history (mirrors cron :empty behaviour).
    panel.hidden = true;
    panel.innerHTML = '';
    setHeaderRunStats('');
    return;
  }
  panel.hidden = false;
  // Stats are surfaced in the header; the panel keeps only the per-run detail
  // rows behind a collapsed disclosure.
  setHeaderRunStats(sessionRunsStatsHtml(stats));
  const rowsHtml = runs.map(sessionRunRowHtml).join('');
  // Collapsed by default everywhere: the run-history timeline grows without
  // bound as more runs accumulate, so leaving it open would steadily push the
  // conversation + composer down. The user's manual expand/collapse is honoured
  // afterwards via data-user-toggled.
  if (!panel.hasAttribute('data-user-toggled')) {
    panel.open = false;
  }
  panel.innerHTML =
    '<summary class="srp-summary">' +
      '<span class="srp-title">运行记录</span>' +
    '</summary>' +
    '<div class="srp-body">' +
      (rowsHtml ? '<div class="srp-rows">' + rowsHtml + '</div>'
                : '<div class="srp-empty">本会话暂无运行记录</div>') +
    '</div>';
}

// Remember the user's manual expand/collapse so a later refresh doesn't fight
// their choice (one listener, delegated on the panel).
document.addEventListener('toggle', function (e) {
  const panel = e.target;
  if (panel && panel.id === 'session-runs-panel') {
    panel.setAttribute('data-user-toggled', '1');
  }
}, true);

async function fetchSessionRuns(key, node) {
  const panel = document.getElementById('session-runs-panel');
  if (!panel) return;
  // Run history is a local-node concern; remote-node sessions skip it for now.
  // Clear the header stats too so a remote session doesn't inherit the
  // previously-selected local session's "N 轮" overview.
  if (node && node !== 'local') { panel.hidden = true; setHeaderRunStats(''); return; }
  try {
    const headers = {};
    const t = deps.getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const resp = await fetch(NZ_CONTRACT.API.sessions_runs + '?key=' + encodeURIComponent(key), { headers });
    // Stale-check the error branch too: the header we would clear belongs to
    // whichever session is selected NOW, not the one this fetch was for.
    if (!resp.ok) { if (nzState.selectedKey !== key) return; panel.hidden = true; setHeaderRunStats(''); return; }
    const data = await resp.json();
    // Guard against a stale response landing after the user switched sessions.
    if (nzState.selectedKey !== key) return;
    renderSessionRunsPanel(data);
  } catch (_) {
    if (nzState.selectedKey !== key) return;
    panel.hidden = true;
    setHeaderRunStats('');
  }
}


// ---- Git branch / worktree chip ----
//
// Tells the operator which checkout the selected session is editing, so a
// conversation running in `.claude/worktrees/feat-x` is never mistaken for one
// on master. Best-effort in exactly the same way as the run-history block: a
// fetch failure, a non-repo workspace, or a remote-node session just leaves
// the chip empty and the conversation surface is never blocked on it.

// gitChipHtml renders the chip for a /api/sessions/git payload, or '' when
// there is nothing to show. Two shapes:
//   - worktree present → "⑂ <worktree> · <branch>" (the worktree name is the
//     stronger signal, so it leads)
//   - main tree        → "⑂ <branch>"
// A detached HEAD shows the abbreviated sha with a warning tint, because
// "which branch am I on" has no answer there and silently showing nothing
// would read as "not a repo".
function gitChipHtml(g) {
  if (!g || !g.is_repo) return '';
  const branch = g.detached
    ? (g.head_sha || '')
    : (g.branch || '');
  if (!branch && !g.worktree) return '';

  // Tooltip carries the full picture; the chip itself stays short so it can't
  // crowd out the model / run-stats cells on a narrow header.
  const tipParts = [];
  if (g.repo) tipParts.push('仓库: ' + g.repo);
  if (g.worktree) tipParts.push('worktree: ' + g.worktree);
  tipParts.push(g.detached ? '分离 HEAD: ' + branch : '分支: ' + branch);
  if (g.root) tipParts.push('根目录: ' + g.root);
  if (g.workspace && g.workspace !== g.root) tipParts.push('工作目录: ' + g.workspace);

  const cls = 'git-chip' +
    (g.worktree ? ' git-chip-worktree' : '') +
    (g.detached ? ' git-chip-detached' : '');
  const label = g.worktree
    ? esc(g.worktree) + '<span class="git-chip-sep">·</span>' + esc(branch)
    : esc(branch);
  return '<span class="' + cls + '" title="' + escAttr(tipParts.join('\n')) + '">' +
      '<span class="git-chip-icon" aria-hidden="true">⑂</span>' +
      '<span class="git-chip-text">' + label + '</span>' +
    '</span>';
}

// gitStateCache holds the last resolved payload per deps.sid(key, node). The header
// is rebuilt from scratch by deps.renderMainShell on every rename / re-select, which
// wipes the chip node; repainting from cache keeps the chip from flickering
// out and back on each rebuild, and avoids a redundant fetch per repaint.
const gitStateCache = {};

// setHeaderGitChip writes (or clears) the header chip node. The node is built
// empty by deps.renderMainShell, so absence = no-op rather than throw (mirrors
// setHeaderRunStats).
function setHeaderGitChip(html) {
  const el = document.getElementById('header-git');
  if (el) el.innerHTML = html || '';
}

// EFFORT_LABELS maps kiro's thinking-effort tiers to a Chinese gloss for the
// tooltip. The tag itself keeps kiro's raw lowercase token so what the
// dashboard shows matches what `/effort`, `kiro-cli acp --effort` and
// ~/.kiro/settings/cli.json use — translating the tier names would break that
// mapping for the operator. Unlisted tiers still render (see effortTagHtml).
const EFFORT_LABELS = {
  low: '低 — 最快、最省，质量最弱',
  medium: '中 — kiro 默认档',
  high: '高',
  xhigh: '极高 — 更慢、更贵',
  max: '最高 — 最慢、最贵',
};

// effortTagHtml renders the backend-reported thinking-effort tier as a bare
// inline tag. Visually it joins the .model-label / .detail-turn-timer family
// (no border, no fill): the header's .detail row already carries two bordered
// chips (.sc-origin, .git-chip) and a third would outweigh the primary
// cli/model label.
//
// An unrecognised tier renders unstyled rather than being dropped — kiro owns
// this vocabulary, and silently hiding a tier naozhi hasn't heard of would
// misreport the session as having no effort at all (same reasoning as
// backendChipInfo's orphan-backend handling).
//
// s.effort is a string the kiro process controls via _kiro.dev/metadata, so
// it is escaped on both the text and attribute paths.
function effortTagHtml(effort) {
  const raw = effort || '';
  if (!raw) return '';
  const gloss = EFFORT_LABELS[raw];
  const tip = 'thinking effort: ' + raw + (gloss ? ' — ' + gloss : '');
  // Only the two top tiers get emphasis: they are the first thing to check
  // when a turn was unusually slow or expensive. Emphasis is weight + full
  // text colour, never a warning hue — a high tier is a deliberate setting,
  // not a fault.
  const hot = raw === 'max' || raw === 'xhigh';
  // aria-label rather than the bare title the neighbouring chips rely on: read
  // aloud, a lone "max" carries no meaning, and the leading "·" separator would
  // be announced as "middle dot". Labelling the span supplies the context and
  // suppresses the decorative punctuation in one go.
  return '<span class="effort-tag' + (hot ? ' effort-hot' : '') +
      ' nz-clickable" title="' + escAttr(tip + ' — 点击切换档位') + '" aria-label="' + escAttr(tip) + '"' +
      ' data-action="tuning-effort">' +
      esc(raw) + '</span>';
}

// setHeaderEffortChip repaints the header effort tag for the selected session.
//
// Called from two places, for two different reasons:
//   - deps.renderMainShell tail: the header was just rebuilt, emptying the mount.
//     No argument — read the cached nzState.sessionsData.
//   - deps.fetchSessions, BEFORE its version short-circuit: a tier change does not
//     advance stats.version, so this is the only path that gets a new tier onto
//     the screen. nzState.sessionsData hasn't been updated at that point, hence the
//     optional `sessions` argument carrying the fresh response rows.
//
// docs/rfc/kiro-effort-visibility.md §5.1
function setHeaderEffortChip(sessions) {
  const el = document.getElementById('header-effort');
  if (!el) return;
  let effort = '';
  if (nzState.selectedKey) {
    if (sessions) {
      const node = nzState.selectedNode || 'local';
      const row = sessions.find(s => s && s.key === nzState.selectedKey && (s.node || 'local') === node);
      // A poll that no longer lists the session (deleted / filtered) clears
      // the tag rather than leaving the previous session's tier behind.
      effort = row ? row.effort : '';
    } else {
      effort = (nzState.sessionsData[deps.sid(nzState.selectedKey, nzState.selectedNode)] || {}).effort;
    }
    if (!effort && !nzState.sessionsData[deps.sid(nzState.selectedKey, nzState.selectedNode)] && nzState.sessionPendingTuning[nzState.selectedKey]) {
      effort = nzState.sessionPendingTuning[nzState.selectedKey].effort || '';
    }
  }
  const html = effortTagHtml(effort);
  if (el.innerHTML !== html) el.innerHTML = html;
}

// spawnDiagChipHtml renders the "配置未生效" warning chip for a session whose
// spawn gates dropped or ignored configured input (#2532: --effort stripped
// for months with only a log line as evidence). Empty when everything took
// effect; the tooltip lists each ineffective item.
function spawnDiagChipHtml(diags) {
  if (!diags || !diags.length) return '';
  const lines = diags.map(function (d) {
    return d.key + ' (' + d.action + ')' + (d.reason ? ': ' + d.reason : '');
  });
  const tip = '以下配置未生效：\n' + lines.join('\n');
  return '<span class="spawn-diag-tag" title="' + escAttr(tip) + '" aria-label="' + escAttr(tip) + '">' +
      '⚠ 配置未生效</span>';
}

// setHeaderSpawnDiagChip mirrors setHeaderEffortChip — same two call sites,
// same "before the stats.version short-circuit" constraint: spawn_diags is a
// runtime observation on each /api/sessions row and never advances
// stats.version, so the poll path must repaint it unconditionally.
function setHeaderSpawnDiagChip(sessions) {
  const el = document.getElementById('header-spawndiag');
  if (!el) return;
  let diags = null;
  if (nzState.selectedKey) {
    if (sessions) {
      const node = nzState.selectedNode || 'local';
      const row = sessions.find(s => s && s.key === nzState.selectedKey && (s.node || 'local') === node);
      diags = row ? row.spawn_diags : null;
    } else {
      diags = (nzState.sessionsData[deps.sid(nzState.selectedKey, nzState.selectedNode)] || {}).spawn_diags;
    }
  }
  const html = spawnDiagChipHtml(diags);
  if (el.innerHTML !== html) el.innerHTML = html;
}

// overlayDriftChipHtml renders the "配置漂移" chip for a session whose live
// argv no longer matches what a fresh spawn under the current config would
// use (#2543). The remedy is restarting the session — nothing auto-restarts
// a live session over drift.
function overlayDriftChipHtml(drift) {
  if (!drift || !drift.length) return '';
  const lines = drift.map(function (d) {
    return d.field + ': ' + (d.stored || '(空)') + ' → ' + (d.current || '(空)');
  });
  const tip = '配置漂移，重启会话以应用新配置：\n' + lines.join('\n');
  return '<span class="overlay-drift-tag" title="' + escAttr(tip) + '" aria-label="' + escAttr(tip) + '">' +
      '⟳ 配置漂移</span>';
}

// setHeaderOverlayDriftChip mirrors setHeaderSpawnDiagChip — same two call
// sites, same before-version-gate constraint: overlay_drift is refreshed by
// the 30s reconcile and never advances stats.version.
function setHeaderOverlayDriftChip(sessions) {
  const el = document.getElementById('header-overlaydrift');
  if (!el) return;
  let drift = null;
  if (nzState.selectedKey) {
    if (sessions) {
      const node = nzState.selectedNode || 'local';
      const row = sessions.find(s => s && s.key === nzState.selectedKey && (s.node || 'local') === node);
      drift = row ? row.overlay_drift : null;
    } else {
      drift = (nzState.sessionsData[deps.sid(nzState.selectedKey, nzState.selectedNode)] || {}).overlay_drift;
    }
  }
  const html = overlayDriftChipHtml(drift);
  if (el.innerHTML !== html) el.innerHTML = html;
}


export {
  fetchSessionRuns,
  gitChipHtml,
  gitStateCache,
  renderSessionRunsPanel,
  setHeaderEffortChip,
  setHeaderGitChip,
  setHeaderOverlayDriftChip,
  setHeaderRunStats,
  setHeaderSpawnDiagChip,
};

// nz.test surface for the Playwright specs (#2557 PR-E3 pattern).
Object.assign(nzTest, { renderSessionRunsPanel, setHeaderRunStats });
