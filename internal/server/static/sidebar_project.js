// sidebar_project.js — extracted from dashboard.js (#2558 D4).
//
// Verbatim region move: `git diff --color-moved` shows the body as a pure
// move; the import block, the deps table and the export block below are the
// only additions.
//
// Layering (D4-1 rule): a module dashboard imports must NOT import dashboard
// back — that cycle puts dashboard's own top-level consts in TDZ while this
// module evaluates. Dashboard state is read through nz.state; its helpers are
// injected once via configureSidebarProject(), called from dashboard's module body.
import { esc, escAttr, fetchJSON, nzState, showToast, trapFocus } from './nz_util.js';

const deps = {
  PICKER_SELECT_ONLY_STYLE: null,
  PICKER_SELECT_STYLE: null,
  accessProfileChipInfo: null,
  debouncedFetchSessions: null,
  fetchAccessProfiles: null,
  fetchCLIBackends: null,
  fetchSessions: null,
  getToken: null,
  projectDisplayLabel: null,
  projectDisplayPrefix: null,
  renderAccessProfilePicker: null,
  renderBackendPicker: null,
  renderSidebar: null,
  showAPIError: null,
  showNetworkError: null,
};
export function configureSidebarProject(impl) {
  for (const k of Object.keys(deps)) {
    if (typeof impl[k] === 'undefined') throw new Error('sidebar_project dep missing: ' + k);
    deps[k] = impl[k];
  }
}

// --- Project section header (favorite + github icons) ---

// The star glyph is identical in both states — CSS class `star-on` + `fill:currentColor`
// controls the visual fill. A single constant avoids the misleading dead ternary
// that previously implied a per-state SVG difference.
const STAR_SVG = '<svg viewBox="0 0 24 24" aria-hidden="true"><polygon points="12,2 15.09,8.26 22,9.27 17,14.14 18.18,21.02 12,17.77 5.82,21.02 7,14.14 2,9.27 8.91,8.26"/></svg>';
// "Clawd" pixel mascot for claude-backend assistant turns. Sourced from
// the Custom Brand Icons set, icon `cbi:claude-clawd`
// (https://github.com/elax46/custom-brand-icons), licensed CC BY-NC-SA
// 4.0. Naozhi ships under BSL 1.1 (non-commercial Additional Use Grant
// through 2030-03-21), so the NC clause is compatible for the current
// licensed term — see ATTRIBUTIONS.md. Fill flows from currentColor so
// the rust hex lives once in dashboard.html as --nz-clawd-rust (CSS sets
// .cc-clawd { color: var(--nz-clawd-rust) }) — no inline hex in JS.
const CLAWD_SVG = '<svg class="cc-clawd" viewBox="0 0 24 24" xmlns="http://www.w3.org/2000/svg" aria-hidden="true"><path fill="currentColor" d="M4.5 6h15v5H22v2h-2.5v3h-1v2H17v-2h-1v2h-1.5v-2h-5v2H8v-2H7v2H5.5v-2h-1v-3H2v-2h2.5ZM7 8v3h1V8Zm9 0v3h1V8Z"/></svg>';
const GITHUB_SVG = '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M9 19c-5 1.5-5-2.5-7-3m14 6v-3.87a3.37 3.37 0 0 0-.94-2.61c3.14-.35 6.44-1.54 6.44-7A5.44 5.44 0 0 0 20 4.77 5.07 5.07 0 0 0 19.91 1S18.73.65 16 2.48a13.38 13.38 0 0 0-7 0C6.27.65 5.09 1 5.09 1A5.07 5.07 0 0 0 5 4.77a5.44 5.44 0 0 0-1.5 3.78c0 5.42 3.3 6.61 6.44 7A3.37 3.37 0 0 0 9 18.13V22"/></svg>';
// Chevron: points down when expanded (`▾`-like), rotated 90deg via CSS
// when collapsed so the same glyph serves both states.
const CHEVRON_SVG = '<svg viewBox="0 0 24 24" aria-hidden="true"><polyline points="6 9 12 15 18 9"/></svg>';

// ICONS — single source of truth for the non-SVG glyph set (#2027). Before
// this map the dashboard carried four parallel icon notations (SVG consts,
// `&#x..;` / `&#..;` HTML entities, bare Unicode glyphs like `✎ ⎘`, and raw
// `\u{..}` escapes) with the same semantic icon spelled differently at each
// call site. Centralising here means one icon → one definition → one notation.
//
// Notation rule: literal Unicode glyph characters (one form, no mixing decimal
// `&#128277;` with hex `&#x2328;`, no mixing `\u{1f464}` lowercase with
// `\u{1F916}` uppercase). Literal glyphs are the only notation that renders
// correctly in BOTH consumption contexts used here: dropped raw into innerHTML
// / template strings, and passed through esc() (which would HTML-escape an
// entity's `&` into `&amp;` and show it verbatim). SVG-backed icons
// (star/github/chevron/clawd) keep their dedicated *_SVG consts above.
const ICONS = {
  close:    '×', // dismiss / close affordance
  back:     '←', // mobile back
  navUp:    '▲', // previous user message
  navDown:  '▼', // next user message
  edit:     '✎', // rename / edit
  copy:     '⎘', // copy key
  trash:    '🗑', // delete
  attach:   '📎', // attach file
  mic:      '🎤', // voice input
  keyboard: '⌨', // keyboard input
  send:     '➤', // send message
  stop:     '■', // interrupt turn
  download: '⬇', // download
  downArrow:'↓', // file-row download (thinner, paired with ↗)
  preview:  '↗', // preview / ask-aside
  gear:     '⚙', // init / system event
  user:     '&gt;_', // user event — brand ">_" terminal prompt mark (rust mono, see .event.user .event-icon)
  spark:    '✦', // assistant text event (non-claude backends)
  todo:     '☰', // todo event
  robot:    '🤖', // subagent badge / agent count
  galleryPrev:  '‹', // lightbox previous image
  galleryNext:  '›', // lightbox next image
  zoomOut:      '−', // lightbox zoom out
  zoomIn:       '+', // lightbox zoom in
  rotateLeft:   '↺', // lightbox rotate left
  rotateRight:  '↻', // lightbox rotate right
};

// sectionHeaderFallbackHtml renders the minimal header for ad-hoc workspace
// groups (p.fallback === true). The group's "project name" is just the
// workspace basename — it is NOT a registered ProjectManager project — so
// favorite / GitHub / + buttons have no stable semantics and are omitted.
// Split out of sectionHeaderHtml to preserve the R110-P2 invariant that
// sectionHeaderHtml has a single unconditional `return '<div...` with
// `newBtn` concatenated directly (locked by static_ux_contract_test).
function sectionHeaderFallbackHtml(p) {
  const node = p.node || 'local';
  const workspace = p.workspace || '';
  // Collapse key matches the group key used in deps.renderSidebar (node:name:ws)
  // so two folders with the same basename each own their own fold state.
  const ck = node + ':' + p.name + ':' + workspace;
  const collapsed = nzState.collapsedProjects.has(ck);
  const count = typeof p._sessionCount === 'number' ? p._sessionCount : 0;
  const cCls = collapsed ? 'sh-btn sh-collapse collapsed' : 'sh-btn sh-collapse';
  const cTitle = collapsed ? '展开' : '收起';
  const collapseBtn = '<button type="button" class="' + cCls + '" data-action="project-collapse" data-key="' + escAttr(ck) + '" title="' + cTitle + ' ' + escAttr(p.name) + '" aria-label="' + cTitle + ' ' + escAttr(p.name) + '" aria-expanded="' + (collapsed ? 'false' : 'true') + '">' + CHEVRON_SVG + '</button>';
  const countBadge = collapsed && count > 0 ? '<span class="sh-count">' + count + '</span>' : '';
  const nameTitle = workspace ? escAttr(p.name + ' — ' + workspace) : escAttr(p.name);
  const collapsedCls = collapsed ? ' is-collapsed' : '';
  return '<div class="section-header section-header-fallback' + collapsedCls + '" role="group" aria-label="' + escAttr(p.name) + '">' +
    collapseBtn +
    '<span class="sh-name" title="' + nameTitle + '">' + esc(p.name) + '</span>' +
    countBadge +
    '</div>';
}

function sectionHeaderHtml(p) {
  const node = p.node || 'local';
  const fav = !!p.favorite;
  const starCls = fav ? 'sh-btn star-on' : 'sh-btn';
  const starTitle = fav ? 'Unfavorite' : 'Favorite';
  const ck = node + ':' + p.name;
  const collapsed = nzState.collapsedProjects.has(ck);
  const count = typeof p._sessionCount === 'number' ? p._sessionCount : 0;
  const cCls = collapsed ? 'sh-btn sh-collapse collapsed' : 'sh-btn sh-collapse';
  const cTitle = collapsed ? '展开' : '收起';
  const collapseBtn = '<button type="button" class="' + cCls + '" data-action="project-collapse" data-key="' + escAttr(ck) + '" title="' + cTitle + ' ' + escAttr(p.name) + '" aria-label="' + cTitle + ' ' + escAttr(p.name) + '" aria-expanded="' + (collapsed ? 'false' : 'true') + '">' + CHEVRON_SVG + '</button>';
  const countBadge = collapsed && count > 0 ? '<span class="sh-count">' + count + '</span>' : '';

  // No longer pass `data-fav` — the handler derives current state from the
  // authoritative `nzState.projectsData` at click time, avoiding a stale DOM attribute
  // that could cause a fast second click (before re-render) to send a
  // redundant or wrong-polarity toggle.
  const starBtn = '<button type="button" class="' + starCls + '" data-action="project-favorite" data-name="' + escAttr(p.name) + '" data-node="' + escAttr(node) + '" title="' + starTitle + '" aria-label="' + starTitle + ' ' + escAttr(p.name) + '">' + STAR_SVG + '</button>';

  // Project settings gear (RFC project-access-profile §8.1). Opens the
  // right-side settings drawer for this project. Remote projects are edited on
  // their own node's dashboard, so the gear is local-only (the PUT
  // /api/projects/config remote proxy exists but the drawer's live pickers read
  // the LOCAL backend/access-profile registries).
  let gearBtn = '';
  if (node === 'local') {
    gearBtn = '<button type="button" class="sh-btn" data-action="project-settings" data-name="' + escAttr(p.name) + '" title="项目设置：' + escAttr(p.name) + '" aria-label="项目设置 ' + escAttr(p.name) + '">' + ICONS.gear + '</button>';
  }

  let ghBtn = '';
  if (p.github) {
    const url = p.git_remote_url || '';
    // R110-P2 tooltip clarity: the old "GitHub: <url>" left the CTA implicit
    // — click-to-open was only discoverable by trial. Lead with the verb
    // "在 GitHub 打开仓库" so the affordance is explicit; append the URL so
    // operators can still eyeball the remote for the common case where
    // they're verifying the repo match before clicking.
    ghBtn = '<button type="button" class="sh-btn github-on" data-action="project-github" data-url="' + escAttr(url) + '" title="在 GitHub 打开仓库：' + escAttr(url) + '" aria-label="在 GitHub 打开仓库 ' + escAttr(p.name) + '">' + GITHUB_SVG + '</button>';
  }

  const collapsedCls = collapsed ? ' is-collapsed' : '';
  // R110-P2 / #448: prefix the display name with the configured emoji
  // (if any) and use display_name when set; aria-label / title still
  // carry p.name so screen-readers + tooltips disambiguate when the
  // dirname differs from the human-friendly label.
  const emojiPrefix = deps.projectDisplayPrefix(p);
  const displayName = deps.projectDisplayLabel(p);
  const labelTitle = (displayName && displayName !== p.name)
    ? p.name + ' — ' + displayName
    : p.name;
  return '<div class="section-header' + collapsedCls + '" role="group" aria-label="' + escAttr(labelTitle) + '">' +
    collapseBtn + starBtn +
    '<span class="sh-name" title="' + escAttr(labelTitle) + '">' +
      (emojiPrefix ? esc(emojiPrefix) : '') + esc(displayName) +
    '</span>' +
    countBadge +
    ghBtn +
    gearBtn +
    '</div>';
}

// SIDEBAR_PROJECT_ACTIONS maps the `data-action` token on a project-header
// control to the handler it invokes, reading arguments from the button's
// own dataset. This is the data-action dispatch idiom already used by the
// cron menu (CRON_MENU_ACTIONS / handleCronMenuClick) — it lets the section
// header buttons drop their inline click attributes, shrinking the
// script-src 'unsafe-inline' surface (#922 / #1734) without changing
// behaviour. Keys must match the data-action values emitted in
// sectionHeaderHtml / sectionHeaderFallbackHtml.
// #1980 PR-2: the project-header buttons' scoped #session-list delegation
// (SIDEBAR_PROJECT_ACTIONS / initSidebarProjectActions) merged into the
// global nz.actions registry at the tail of this file — closest() single
// dispatch subsumes the old stopPropagation, and the capture-phase
// long-press swallow from initSwipeDelete still runs first (capture).

// toggleProjectCollapsed flips a project section's fold state, persists
// it, and re-renders from the last sidebar payload (no network round-trip).
// Key format: "<node>:<name>" matching the grouping key in deps.renderSidebar.
function toggleProjectCollapsed(key) {
  if (!key) return;
  if (nzState.collapsedProjects.has(key)) nzState.collapsedProjects.delete(key);
  else nzState.collapsedProjects.add(key);
  try {
    localStorage.setItem('nz_collapsedProjects', JSON.stringify([...nzState.collapsedProjects]));
  } catch (_) {}
  if (nzState._lastSidebarData) {
    deps.renderSidebar(nzState._lastSidebarData);
  } else {
    deps.debouncedFetchSessions();
  }
}

// In-flight guard against a double-click race: the star button's DOM state
// lags behind nzState.projectsData until the next deps.fetchSessions re-render. Without
// this set, a second click inside that window would read a stale DOM hint and
// potentially fire the same or opposite polarity. Keyed by (node, name).
const _favInFlight = new Set();

async function toggleFavorite(name, node) {
  const nodeID = node || 'local';
  const key = nodeID + ':' + name;
  if (_favInFlight.has(key)) return; // drop re-entry
  // Derive current state from the source of truth (nzState.projectsData), not the
  // button's data-fav attribute which may not have been re-rendered yet.
  const proj = nzState.projectsData.find(x => x.name === name && (x.node || 'local') === nodeID);
  if (!proj) return;
  const next = !proj.favorite;
  _favInFlight.add(key);
  try {
    const headers = {};
    const t = deps.getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const qs = 'name=' + encodeURIComponent(name) + '&favorite=' + (next ? 'true' : 'false') +
      (node && node !== 'local' ? '&node=' + encodeURIComponent(node) : '');
    try {
      await fetchJSON(NZ_CONTRACT.API.projects_favorite + '?' + qs, { timeoutMs: 10000, method: 'POST', headers });
    } catch (err) {
      if (err && err.status) {
        deps.showAPIError(next ? '收藏项目' : '取消收藏', err.status, '');
      } else {
        deps.showNetworkError(next ? '收藏项目' : '取消收藏', err);
      }
      // Re-render from the server so the star's visual hover/click state
      // snaps back to the authoritative `nzState.projectsData` value; otherwise the
      // user sees a phantom success.
      deps.fetchSessions();
      return;
    }
    // Optimistic update then refresh.
    proj.favorite = next;
    showToast(next ? '已收藏 ' + name : '已取消收藏 ' + name, 'success');
    deps.fetchSessions();
  } finally {
    _favInFlight.delete(key);
  }
}

// openProjectSettings opens the per-project settings modal (RFC
// project-access-profile §8.1). It reads GET /api/projects/config for the
// current values, renders editable fields (display name / emoji / access
// profile / backend / planner model + prompt), and writes back via PUT. The
// access-profile + backend pickers reuse the same registries the new-session
// modal consumes, so a project can be pinned to an auth chain / backend without
// hand-editing project.yaml. Local projects only (see gear-button gating).
async function openProjectSettings(name) {
  if (!name) return;
  // Load the three inputs in parallel: current config + the two registries the
  // pickers render from. Config failure is fatal (nothing to edit); registry
  // failures degrade to hidden pickers (same as new-session modal).
  let cfg;
  try {
    const headers = {};
    const t = deps.getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    const [c] = await Promise.all([
      fetchJSON(NZ_CONTRACT.API.projects_config + '?name=' + encodeURIComponent(name), { timeoutMs: 10000, headers, credentials: 'same-origin' }),
      deps.fetchCLIBackends(),
      deps.fetchAccessProfiles(),
    ]);
    cfg = c || {};
  } catch (err) {
    if (err && err.status) deps.showAPIError('加载项目设置', err.status, '');
    else deps.showNetworkError('加载项目设置', err);
    return;
  }

  const accessProfilePicker = deps.renderAccessProfilePicker(nzState.accessProfiles, { selectId: 'ps-access-profile', selectedId: cfg.access_profile || '' });
  const backendPicker = deps.renderBackendPicker(nzState.cliBackends, { selectId: 'ps-backend', selectedId: cfg.backend || '' });

  const overlay = document.createElement('div');
  overlay.className = 'modal-overlay';
  overlay.innerHTML =
    '<div class="modal" role="dialog" aria-modal="true" aria-label="项目设置：' + escAttr(name) + '">' +
      '<h3>项目设置 · ' + esc(name) + '</h3>' +
      '<div style="margin-bottom:12px">' +
        '<label style="font-size:12px;color:var(--nz-text-mute);display:block;margin-bottom:4px" for="ps-display-name">显示名称</label>' +
        '<input id="ps-display-name" style="' + deps.PICKER_SELECT_STYLE + '" maxlength="200" value="' + escAttr(cfg.display_name || '') + '" placeholder="' + escAttr(name) + '">' +
      '</div>' +
      '<div style="margin-bottom:12px">' +
        '<label style="font-size:12px;color:var(--nz-text-mute);display:block;margin-bottom:4px" for="ps-emoji">Emoji</label>' +
        '<input id="ps-emoji" style="' + deps.PICKER_SELECT_STYLE + '" maxlength="16" value="' + escAttr(cfg.emoji || '') + '" placeholder="🗂">' +
      '</div>' +
      accessProfilePicker +
      '<div style="margin:-6px 0 12px"><button type="button" class="linklike" data-action="ps-new-profile" style="background:none;border:none;color:var(--nz-accent);font-size:12px;cursor:pointer;padding:0">+ 新建访问档…</button></div>' +
      backendPicker +
      '<div style="margin-bottom:12px">' +
        '<label style="font-size:12px;color:var(--nz-text-mute);display:block;margin-bottom:4px" for="ps-planner-model">Planner model（留空则继承）</label>' +
        '<input id="ps-planner-model" style="' + deps.PICKER_SELECT_STYLE + '" maxlength="256" value="' + escAttr(cfg.planner_model || '') + '" placeholder="' + escAttr(accessProfileDefaultModel(cfg.access_profile) || '（继承默认）') + '">' +
      '</div>' +
      '<div style="margin-bottom:12px">' +
        '<label style="font-size:12px;color:var(--nz-text-mute);display:block;margin-bottom:4px" for="ps-planner-prompt">Planner prompt（可选，单行）</label>' +
        '<textarea id="ps-planner-prompt" rows="3" style="' + deps.PICKER_SELECT_STYLE + ';resize:vertical" maxlength="8192" placeholder="附加系统提示…">' + esc(cfg.planner_prompt || '') + '</textarea>' +
      '</div>' +
      '<div id="ps-preview" style="font-size:12px;color:var(--nz-text-mute);margin-bottom:12px;padding:8px;background:var(--nz-bg-0);border-radius:4px"></div>' +
      '<div id="ps-error" style="display:none;color:var(--nz-danger,#e5484d);font-size:12px;margin-bottom:8px"></div>' +
      '<div class="modal-btns">' +
        '<button type="button" data-action="modal-close">取消</button>' +
        '<button type="button" class="primary" data-action="ps-save" data-name="' + escAttr(name) + '">保存</button>' +
      '</div>' +
    '</div>';
  document.body.appendChild(overlay);
  trapFocus(overlay);

  // Live "effective link" preview: shows how the next session under this
  // project will resolve (profile → resolved model). Non-sensitive only — no
  // base-URL/token, just the profile label + model (§8.1 联动预览 / §8.4).
  const updatePreview = () => {
    const apEl = document.getElementById('ps-access-profile');
    const apID = apEl ? apEl.value : (cfg.access_profile || '');
    const pmEl = document.getElementById('ps-planner-model');
    const model = (pmEl && pmEl.value.trim()) || accessProfileDefaultModel(apID) || '（继承默认）';
    const info = deps.accessProfileChipInfo(apID);
    const label = info ? info.label : '全局默认';
    const box = document.getElementById('ps-preview');
    if (box) box.textContent = '生效链路：' + label + ' → ' + model;
  };
  const apSel = document.getElementById('ps-access-profile');
  if (apSel) apSel.addEventListener('change', updatePreview);
  const pmInput = document.getElementById('ps-planner-model');
  if (pmInput) pmInput.addEventListener('input', updatePreview);
  updatePreview();

  const saveBtn = overlay.querySelector('[data-action="ps-save"]');
  if (saveBtn) saveBtn.addEventListener('click', () => saveProjectSettings(name, cfg, overlay));

  // "+ 新建访问档" opens the create form; on success it refreshes the registry
  // and re-selects the new profile in this drawer's picker.
  const newBtn = overlay.querySelector('[data-action="ps-new-profile"]');
  if (newBtn) newBtn.addEventListener('click', () => {
    openCreateAccessProfile((newID) => {
      const sel = document.getElementById('ps-access-profile');
      if (sel) {
        // The picker was rendered before the new profile existed; add + select
        // it so the drawer immediately reflects the creation without a reopen.
        if (![...sel.options].some(o => o.value === newID)) {
          const opt = document.createElement('option');
          opt.value = newID;
          opt.textContent = deps.accessProfileChipInfo(newID)?.label || newID;
          sel.appendChild(opt);
        }
        sel.value = newID;
        updatePreview();
      }
    });
  });
}

// ACCESS_PROFILE_TEMPLATES pre-fill the create form for the two common cases so
// the operator doesn't have to know env-var names (RFC P1-d). Values are the
// literal overlay keys the server accepts; the token goes to a *_FILE.
const ACCESS_PROFILE_TEMPLATES = {
  '1p': {
    label: '个人 Anthropic（1P 直连）',
    display_name: '个人 Anthropic',
    // Reference a design token rather than an inline hex literal (RNEW-UX-015
    // ratchet). The operator can edit the colour later by hand-editing config.
    chip_color: 'var(--nz-accent)',
    env: { CLAUDE_CODE_USE_BEDROCK: '0', ANTHROPIC_BASE_URL: 'https://api.anthropic.com' },
    token_env_key: 'ANTHROPIC_AUTH_TOKEN_FILE',
    token_hint: '粘贴 1P Anthropic 的 auth token（写入 0600 文件，不进 config）',
    default_model: 'claude-fable-5',
  },
  'bedrock': {
    label: '公司 Bedrock（经本机 proxy）',
    display_name: '公司 Bedrock',
    chip_color: 'var(--nz-purple)',
    env: { CLAUDE_CODE_USE_BEDROCK: '1', CLAUDE_CODE_SKIP_BEDROCK_AUTH: '1', ANTHROPIC_BEDROCK_BASE_URL: 'http://127.0.0.1:8889', AWS_REGION: 'us-west-2' },
    token_env_key: '',
    token_hint: '',
    default_model: 'claude-opus-4-8',
  },
};

// openCreateAccessProfile renders the guided create-profile form (RFC P1-d).
// A template pre-fills env keys + colour so the operator only supplies an id,
// a display name, and (for 1P) a token. On submit it POSTs /api/access-profiles;
// on success it refreshes the cached registry and calls onCreated(id). The
// token textarea value is sent once and never read back (write-only secret).
function openCreateAccessProfile(onCreated) {
  const tplOptions = Object.keys(ACCESS_PROFILE_TEMPLATES)
    .map(k => '<option value="' + escAttr(k) + '">' + esc(ACCESS_PROFILE_TEMPLATES[k].label) + '</option>').join('');
  const overlay = document.createElement('div');
  overlay.className = 'modal-overlay';
  overlay.innerHTML =
    '<div class="modal" role="dialog" aria-modal="true" aria-label="新建访问档">' +
      '<h3>新建访问档</h3>' +
      '<div style="margin-bottom:12px">' +
        '<label style="font-size:12px;color:var(--nz-text-mute);display:block;margin-bottom:4px" for="cap-template">模板</label>' +
        '<span class="picker-select-wrap"><select id="cap-template" style="' + deps.PICKER_SELECT_ONLY_STYLE + '">' + tplOptions + '</select></span>' +
      '</div>' +
      '<div style="margin-bottom:12px">' +
        '<label style="font-size:12px;color:var(--nz-text-mute);display:block;margin-bottom:4px" for="cap-id">档 ID（英数字 . _ -，唯一）</label>' +
        '<input id="cap-id" style="' + deps.PICKER_SELECT_STYLE + '" maxlength="64" placeholder="1p-fable">' +
      '</div>' +
      '<div style="margin-bottom:12px">' +
        '<label style="font-size:12px;color:var(--nz-text-mute);display:block;margin-bottom:4px" for="cap-display">显示名称</label>' +
        '<input id="cap-display" style="' + deps.PICKER_SELECT_STYLE + '" maxlength="200">' +
      '</div>' +
      '<div style="margin-bottom:12px">' +
        '<label style="font-size:12px;color:var(--nz-text-mute);display:block;margin-bottom:4px" for="cap-model">默认 model（可选）</label>' +
        '<input id="cap-model" style="' + deps.PICKER_SELECT_STYLE + '" maxlength="256">' +
      '</div>' +
      '<div id="cap-token-wrap" style="margin-bottom:12px">' +
        '<label style="font-size:12px;color:var(--nz-text-mute);display:block;margin-bottom:4px" for="cap-token">Token（写入 0600 文件，仅此一次可见）</label>' +
        '<textarea id="cap-token" rows="2" style="' + deps.PICKER_SELECT_STYLE + ';resize:vertical" placeholder="" autocomplete="off"></textarea>' +
        '<div id="cap-token-hint" style="font-size:11px;color:var(--nz-text-mute);margin-top:4px"></div>' +
      '</div>' +
      '<div id="cap-error" style="display:none;color:var(--nz-danger,#e5484d);font-size:12px;margin-bottom:8px"></div>' +
      '<div class="modal-btns">' +
        '<button type="button" data-action="modal-close">取消</button>' +
        '<button type="button" class="primary" data-action="cap-create">创建</button>' +
      '</div>' +
    '</div>';
  document.body.appendChild(overlay);
  trapFocus(overlay);

  const tplSel = overlay.querySelector('#cap-template');
  const applyTemplate = () => {
    const tpl = ACCESS_PROFILE_TEMPLATES[tplSel.value];
    if (!tpl) return;
    const dEl = overlay.querySelector('#cap-display');
    if (dEl && !dEl.value) dEl.value = tpl.display_name || '';
    const mEl = overlay.querySelector('#cap-model');
    if (mEl && !mEl.value) mEl.value = tpl.default_model || '';
    // Token field only relevant when the template references a *_FILE key.
    const wrap = overlay.querySelector('#cap-token-wrap');
    const hint = overlay.querySelector('#cap-token-hint');
    if (wrap) wrap.style.display = tpl.token_env_key ? '' : 'none';
    if (hint) hint.textContent = tpl.token_hint || '';
  };
  tplSel.addEventListener('change', applyTemplate);
  applyTemplate();


  overlay.querySelector('[data-action="cap-create"]').addEventListener('click', async () => {
    const tpl = ACCESS_PROFILE_TEMPLATES[tplSel.value] || {};
    const id = (overlay.querySelector('#cap-id').value || '').trim();
    const errBox = overlay.querySelector('#cap-error');
    const showErr = (m) => { if (errBox) { errBox.textContent = m; errBox.style.display = 'block'; } };
    if (!/^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/.test(id)) {
      showErr('档 ID 无效（英数字 . _ -，1-64 位，不能以 - 开头）');
      return;
    }
    const body = {
      id: id,
      display_name: (overlay.querySelector('#cap-display').value || '').trim(),
      chip_color: tpl.chip_color || '',
      default_model: (overlay.querySelector('#cap-model').value || '').trim(),
      env: Object.assign({}, tpl.env || {}),
    };
    if (tpl.token_env_key) {
      const tok = (overlay.querySelector('#cap-token').value || '').trim();
      if (!tok) { showErr('该模板需要 token'); return; }
      body.token_env_key = tpl.token_env_key;
      body.token_content = tok;
    }
    try {
      const headers = { 'Content-Type': 'application/json' };
      const t = deps.getToken();
      if (t) headers['Authorization'] = 'Bearer ' + t;
      await fetchJSON(NZ_CONTRACT.API.access_profiles, {
        timeoutMs: 10000, method: 'POST', headers, credentials: 'same-origin',
        body: JSON.stringify(body),
      });
    } catch (err) {
      if (err && err.status === 409) showErr('该档 ID 已存在');
      else if (err && err.status === 400) showErr('配置无效：请检查各字段');
      else if (err && err.status) deps.showAPIError('创建访问档', err.status, '');
      else deps.showNetworkError('创建访问档', err);
      return;
    }
    overlay.remove();
    showToast('访问档已创建 · ' + id, 'success');
    // Force a registry refresh (bypass the 60s cache) so the new profile is
    // visible immediately to pickers/chips.
    nzState.accessProfilesFetchedAt = 0;
    await deps.fetchAccessProfiles();
    if (typeof onCreated === 'function') onCreated(id);
  });
}

// accessProfileDefaultModel resolves a profile id to its default_model from the
// cached registry, or "" when unknown / global default. Used only for the
// planner-model placeholder + preview — never a value the form submits.
function accessProfileDefaultModel(profileID) {
  if (!profileID || !nzState.accessProfiles || !Array.isArray(nzState.accessProfiles.profiles)) return '';
  const e = nzState.accessProfiles.profiles.find(p => p && p.id === profileID);
  return (e && e.default_model) ? e.default_model : '';
}

// saveProjectSettings collects the settings-modal fields, merges them onto the
// loaded config (preserving fields the drawer doesn't edit — chat_bindings,
// git_sync, created_at, …), and PUTs. On success it refreshes the sidebar +
// the access-profile chip source; on validation failure it shows the server's
// generic reason inline.
async function saveProjectSettings(name, baseCfg, overlay) {
  const val = (id) => { const el = document.getElementById(id); return el ? el.value : undefined; };
  // Start from the loaded config so unedited fields (chat_bindings, git_sync,
  // memory_file, created_at) round-trip untouched.
  const cfg = Object.assign({}, baseCfg);
  cfg.display_name = (val('ps-display-name') || '').trim();
  cfg.emoji = (val('ps-emoji') || '').trim();
  cfg.access_profile = val('ps-access-profile') || '';
  cfg.backend = val('ps-backend') || '';
  cfg.planner_model = (val('ps-planner-model') || '').trim();
  cfg.planner_prompt = (val('ps-planner-prompt') || '').trim();

  const errBox = overlay.querySelector('#ps-error');
  const showErr = (msg) => { if (errBox) { errBox.textContent = msg; errBox.style.display = 'block'; } };

  try {
    const headers = { 'Content-Type': 'application/json' };
    const t = deps.getToken();
    if (t) headers['Authorization'] = 'Bearer ' + t;
    await fetchJSON(NZ_CONTRACT.API.projects_config + '?name=' + encodeURIComponent(name), {
      timeoutMs: 10000, method: 'PUT', headers, credentials: 'same-origin',
      body: JSON.stringify(cfg),
    });
  } catch (err) {
    if (err && err.status === 400) showErr('配置无效：请检查各字段（未知 backend / 访问档、超长 prompt 等）');
    else if (err && err.status) deps.showAPIError('保存项目设置', err.status, '');
    else deps.showNetworkError('保存项目设置', err);
    return;
  }
  overlay.remove();
  showToast('项目设置已保存 · ' + name, 'success');
  // Access-profile binding change affects the next session's chip; refresh both
  // the profile registry and the sidebar so chips repaint.
  deps.fetchSessions();
}

function showGitRemote(url) {
  if (!url) return;
  // Only open http(s)/git URLs; refuse ssh:// or git@host:user/repo remotes
  // because ssh URLs can include embedded credentials (user:pass@host) that
  // a toast would leak to anyone peering at the screen, and window.open on
  // ssh:// does nothing useful in a browser.
  //
  // R244-SEC-P3-4: explicit positive startsWith allowlist (lowercased) instead
  // of a /^(https?|git):\/\// regex so a future copy-paste cannot accidentally
  // drop the leading anchor and accept "javascript:foo http://" or similar
  // mixed-scheme strings. The lowercased prefix check matches scheme parsing
  // semantics (RFC 3986 §3.1: schemes are case-insensitive).
  const lower = String(url).toLowerCase();
  const allowed = ['https://', 'http://', 'git://'];
  let safe = false;
  for (const scheme of allowed) {
    if (lower.startsWith(scheme)) { safe = true; break; }
  }
  if (safe) {
    window.open(url, '_blank', 'noopener,noreferrer');
    return;
  }
  // Fallback: surface the URL but truncated to keep credentials embedded in
  // ssh URLs from being broadcast via the toast surface.
  const shown = url.length > 80 ? url.slice(0, 77) + '…' : url;
  showToast('GitHub remote: ' + shown);
}


export {
  CLAWD_SVG,
  ICONS,
  openProjectSettings,
  sectionHeaderFallbackHtml,
  sectionHeaderHtml,
  showGitRemote,
  toggleFavorite,
  toggleProjectCollapsed,
};
