// settings_auth.js — the settings page's 访问档与模型 section: the access
// profiles of config.yaml on the left, the models of naozhi's own Claude
// settings file on the right. Each save is one request; the server keeps the
// keys this page does not know and backs the old file up as *.bak-<time>.

import { NZ_CONTRACT } from './contract.js';
import { serverInfo } from './state.js';
import { esc, escAttr, fetchJSON, showToast } from './nz_util.js';

const API_PROFILES = NZ_CONTRACT.API.access_profiles;
const API_MODELS = NZ_CONTRACT.API.settings_models;

// The profile fields this page edits; env stays as configured.
const PROFILE_FIELDS = [
  ['display_name', '显示名', 'Isengard · 788668107894'],
  ['aws_profile', 'aws_profile', '~/.aws/config 里的 profile 名'],
  ['default_model', '默认模型', '空 = 跟随全局'],
];

let mountEl = null;
let profiles = [];
let models = [];
let modelsEnabled = false;
// editing: null, '' for a new profile, or the id being edited.
let editing = null;

export async function mountAuthSettings(el) {
  if (!el) return;
  mountEl = el;
  editing = null;
  await reload();
}

async function reload() {
  try {
    const [p, m] = await Promise.all([
      fetchJSON(API_PROFILES, { credentials: 'same-origin' }),
      fetchJSON(API_MODELS, { credentials: 'same-origin' }),
    ]);
    profiles = (p && Array.isArray(p.profiles)) ? p.profiles : [];
    models = (m && Array.isArray(m.models)) ? m.models : [];
    modelsEnabled = !!(m && m.enabled);
  } catch (e) {
    profiles = []; models = []; modelsEnabled = false;
  }
  // The header's access-profile picker caches the list for a minute.
  serverInfo.accessProfilesFetchedAt = 0;
  render();
}

function errText(e) {
  const m = String((e && e.message) || e).match(/\{.*\}$/);
  if (m) {
    try { return JSON.parse(m[0]).error || String(e.message); } catch (_) { /* not JSON */ }
  }
  return String((e && e.message) || e);
}

async function send(url, method, body) {
  try {
    await fetchJSON(url, {
      method, credentials: 'same-origin',
      headers: body ? { 'Content-Type': 'application/json' } : undefined,
      body: body ? JSON.stringify(body) : undefined,
    });
    return true;
  } catch (e) {
    showToast('保存失败：' + errText(e), 'error', 5000);
    return false;
  }
}

function modelOptions(selected) {
  let html = '<option value="">（跟随全局）</option>';
  const ids = models.map(m => m.id);
  if (selected && !ids.includes(selected)) ids.push(selected);
  for (const id of ids) {
    html += '<option value="' + escAttr(id) + '"' + (id === selected ? ' selected' : '') + '>' + esc(id) + '</option>';
  }
  return html;
}

function profileFormHtml(p) {
  const isNew = !p;
  p = p || {};
  let html = '<form class="sa-form" data-sa-form="profile">';
  if (isNew) {
    html += '<label class="sa-field"><span>ID</span><input name="id" required pattern="[A-Za-z0-9][A-Za-z0-9._\\-]{0,63}" placeholder="isengard"></label>';
  }
  for (const [k, label, ph] of PROFILE_FIELDS) {
    if (k === 'default_model') {
      html += '<label class="sa-field"><span>' + esc(label) + '</span><select name="' + k + '">' + modelOptions(p[k] || '') + '</select></label>';
    } else {
      html += '<label class="sa-field"><span>' + esc(label) + '</span><input name="' + k + '" value="' + escAttr(p[k] || '') + '" placeholder="' + escAttr(ph) + '"></label>';
    }
  }
  html += '<div class="sa-actions"><button type="submit" class="sa-btn sa-primary">保存</button>' +
    '<button type="button" class="sa-btn" data-sa="cancel">取消</button></div></form>';
  return html;
}

function profilesHtml() {
  let html = '<div class="sa-col"><h3>访问档 <span class="sa-file">config.yaml</span></h3>';
  html += '<div class="sa-row sa-global"><div class="sa-main"><div class="sa-name">（全局默认）</div>' +
    '<div class="sa-sub">naozhi-settings.json 的凭证</div></div></div>';
  for (const p of profiles) {
    if (editing === p.id) {
      html += '<div class="sa-row is-editing"><div class="sa-name">' + esc(p.id) + '</div>' + profileFormHtml(p) + '</div>';
      continue;
    }
    const sub = [p.aws_profile ? 'aws_profile: ' + p.aws_profile : '', p.default_model ? '默认 ' + p.default_model : '']
      .filter(Boolean).join(' · ');
    html += '<div class="sa-row"><div class="sa-main"><div class="sa-name">' + esc(p.display_name || p.id) +
      (p.display_name ? ' <span class="sa-id">' + esc(p.id) + '</span>' : '') + '</div>' +
      (sub ? '<div class="sa-sub">' + esc(sub) + '</div>' : '') + '</div>' +
      '<button type="button" class="sa-btn" data-sa="edit" data-id="' + escAttr(p.id) + '">编辑</button>' +
      '<button type="button" class="sa-btn sa-danger" data-sa="del-profile" data-id="' + escAttr(p.id) + '">删除</button></div>';
  }
  if (editing === '') {
    html += '<div class="sa-row is-editing">' + profileFormHtml(null) + '</div>';
  } else {
    html += '<button type="button" class="sa-btn sa-add" data-sa="new-profile">＋ 新增访问档</button>';
  }
  return html + '</div>';
}

function modelsHtml() {
  let html = '<div class="sa-col"><h3>模型 <span class="sa-file">naozhi-settings.json</span></h3>';
  if (!modelsEnabled) {
    return html + '<div class="sa-sub">未开 naozhi_settings.enabled，没有 naozhi 自己的 settings 文件可改。</div></div>';
  }
  for (const m of models) {
    const sub = [m.label, m.profile && m.profile !== m.id ? '→ ' + m.profile : ''].filter(Boolean).join(' · ');
    html += '<div class="sa-row"><div class="sa-main"><div class="sa-name">' + esc(m.id) +
      (m.default ? ' <span class="sa-tag">默认</span>' : '') + '</div>' +
      (sub ? '<div class="sa-sub">' + esc(sub) + '</div>' : '') + '</div>' +
      (m.default ? '' : '<button type="button" class="sa-btn sa-danger" data-sa="del-model" data-id="' + escAttr(m.id) + '">删除</button>') +
      '</div>';
  }
  html += '<form class="sa-form sa-addmodel" data-sa-form="model">' +
    '<label class="sa-field"><span>模型 ID</span><input name="model" required placeholder="global.anthropic.claude-fable-5-2[1m]"></label>' +
    '<label class="sa-field"><span>显示名</span><input name="label" placeholder="可空，按 ID 生成"></label>' +
    '<div class="sa-actions"><button type="submit" class="sa-btn sa-primary">添加模型</button></div>' +
    '<div class="sa-hint">公司清单外的 profile id 会自动写一条「别名 → 该 id」的 modelOverrides 映射，否则用不了。</div>' +
    '</form>';
  return html + '</div>';
}

function render() {
  if (!mountEl) return;
  mountEl.innerHTML = '<h2>访问档与模型</h2><div class="sa-grid">' + profilesHtml() + modelsHtml() + '</div>' +
    '<div class="sa-hint">保存即写回文件（旧文件备份为 *.bak-时间）。新模型与访问档对之后启动的会话生效。</div>';
  mountEl.querySelectorAll('[data-sa]').forEach(b => b.addEventListener('click', onAction));
  mountEl.querySelectorAll('form[data-sa-form]').forEach(f => f.addEventListener('submit', onSubmit));
}

async function onAction(e) {
  const b = e.currentTarget;
  const id = b.dataset.id || '';
  switch (b.dataset.sa) {
    case 'edit': editing = id; render(); return;
    case 'new-profile': editing = ''; render(); return;
    case 'cancel': editing = null; render(); return;
    case 'del-profile':
      b.disabled = true;
      if (await send(API_PROFILES + '?id=' + encodeURIComponent(id), 'DELETE')) showToast('已删除访问档 ' + id);
      return reload();
    case 'del-model':
      b.disabled = true;
      if (await send(API_MODELS + '?model=' + encodeURIComponent(id), 'DELETE')) showToast('已删除模型 ' + id);
      return reload();
  }
}

async function onSubmit(e) {
  e.preventDefault();
  const f = e.currentTarget;
  const v = Object.fromEntries(new FormData(f).entries());
  for (const k in v) v[k] = String(v[k]).trim();
  const btn = f.querySelector('button[type=submit]');
  if (btn) btn.disabled = true;
  let ok;
  if (f.dataset.saForm === 'model') {
    ok = await send(API_MODELS, 'POST', { model: v.model, label: v.label });
    if (ok) showToast('已添加模型 ' + v.model);
  } else if (editing === '') {
    ok = await send(API_PROFILES, 'POST', { id: v.id, display_name: v.display_name, aws_profile: v.aws_profile, default_model: v.default_model });
    if (ok) showToast('已新增访问档 ' + v.id);
  } else {
    const cur = profiles.find(p => p.id === editing) || {};
    ok = await send(API_PROFILES, 'PUT', {
      id: editing, display_name: v.display_name, aws_profile: v.aws_profile, default_model: v.default_model,
      chip_color: cur.chip_color || '', default_backend: cur.default_backend || '',
    });
    if (ok) showToast('已保存访问档 ' + editing);
  }
  if (!ok) { if (btn) btn.disabled = false; return; }
  editing = null;
  await reload();
}
