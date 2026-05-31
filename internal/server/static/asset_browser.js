// asset_browser.js — dashboard "installed assets" browser (RFC cc-asset-browser).
// Activity-Bar view (NOT a popup): the leftmost rail toggles body.nz-view-assets,
// which swaps the chat sidebar/main for the asset sidebar/main in place. This
// module renders into the #asset-sidebar-* and #asset-main-* panels defined in
// dashboard.html and reads GET /api/cc/assets + /api/cc/assets/raw (read-only).
//
// Self-contained: served from /static/asset_browser.js, loaded via <script>.
// The only dashboard.js touch is the activity-bar wiring (which calls the
// exported window.nzAssetView.{show,hide,toggle}).
(function () {
  'use strict';

  var KINDS = [
    { k: 'skill', label: 'Skills' },
    { k: 'plugin', label: 'Plugins' },
    { k: 'agent', label: 'Agents' },
    { k: 'command', label: 'Commands' },
    { k: 'hook', label: 'Hooks' },
    { k: 'mcp', label: 'MCP' },
    { k: 'memory', label: 'Memory' }
  ];
  var SRC_LABEL = { user: '用户级', project: '项目级', plugin: '插件', memory_project: '项目 memory' };
  var SRC_RANK = { user: 0, project: 1, memory_project: 2, plugin: 3 };

  var state = { inv: null, kind: 'skill', sel: null, collapsed: {}, loaded: false, wired: false };

  function esc(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }
  function $(id) { return document.getElementById(id); }
  function cssEsc(s) { return String(s).replace(/["\\]/g, '\\$&'); }

  function fetchJSON(url) {
    return fetch(url, { credentials: 'same-origin', headers: { Accept: 'application/json' } })
      .then(function (r) { if (!r.ok) throw new Error('HTTP ' + r.status); return r.json(); });
  }

  function load(force) {
    if (state.loaded && !force) { render(); return; }
    var box = $('asset-sidebar-list');
    if (box) box.innerHTML = '<div class="asset-empty">加载中…</div>';
    fetchJSON('/api/cc/assets').then(function (inv) {
      state.inv = inv; state.loaded = true; buildKindTabs(); render();
    }).catch(function (e) {
      if (box) box.innerHTML = '<div class="asset-empty">加载失败：' + esc(e.message) + '</div>';
    });
  }

  function buildKindTabs() {
    var wrap = $('asset-kindtabs');
    if (!wrap) return;
    var totals = (state.inv && state.inv.totals) || {};
    wrap.innerHTML = KINDS.map(function (kd) {
      var n = kd.k === 'plugin' ? ((state.inv && state.inv.plugins) || []).length : (totals[kd.k] || 0);
      return '<button class="asset-kt' + (kd.k === state.kind ? ' active' : '') + '" role="tab" data-k="' + kd.k + '">' +
        esc(kd.label) + ' <span class="kc">' + n + '</span></button>';
    }).join('');
  }

  function groupKey(a) {
    if (a.source.kind === 'plugin') return 'plugin:' + a.source.plugin;
    if (a.source.kind === 'memory_project') return 'memory:' + a.source.project;
    return a.source.kind;
  }

  function render() {
    var box = $('asset-sidebar-list'), cards = $('asset-sidebar-cards');
    if (!box || !cards) return;

    if (state.kind === 'plugin') {
      box.style.display = 'none'; cards.style.display = 'block';
      renderPlugins(cards);
      return;
    }
    cards.style.display = 'none'; box.style.display = 'block';

    var q = (($('asset-search') || {}).value || '').trim().toLowerCase();
    var items = ((state.inv && state.inv.assets) || []).filter(function (a) {
      return a.kind === state.kind &&
        (!q || a.name.toLowerCase().indexOf(q) >= 0 || (a.description || '').toLowerCase().indexOf(q) >= 0);
    });
    if (!items.length) { box.innerHTML = '<div class="asset-empty">没有 ' + esc(state.kind) + ' 资产</div>'; return; }

    var groups = {}, order = [];
    items.forEach(function (a) {
      var g = groupKey(a);
      if (!groups[g]) { groups[g] = { label: a.source.kind === 'plugin' ? a.source.plugin : (SRC_LABEL[a.source.kind] || a.source.kind), src: a.source.kind, items: [] }; order.push(g); }
      groups[g].items.push(a);
    });
    order.sort(function (x, y) {
      var rx = SRC_RANK[groups[x].src], ry = SRC_RANK[groups[y].src];
      return (rx == null ? 9 : rx) - (ry == null ? 9 : ry);
    });

    var html = '';
    order.forEach(function (g) {
      var grp = groups[g];
      var collapsed = state.collapsed[g] != null ? state.collapsed[g] : (grp.src === 'plugin');
      html += '<div class="asset-grp ' + (collapsed ? 'collapsed' : '') + '" data-g="' + esc(g) + '">' +
        '<svg class="agchev" viewBox="0 0 24 24"><polyline points="6 9 12 15 18 9"/></svg>' +
        '<span class="agl">' + esc(grp.label) + '</span>' +
        '<span class="asset-srcchip asrc-' + esc(grp.src) + '">' + esc(SRC_LABEL[grp.src] || grp.src) + '</span>' +
        '<span class="agcount">' + grp.items.length + '</span></div>';
      html += '<div class="asset-grpitems ' + (collapsed ? 'collapsed' : '') + '" data-gi="' + esc(g) + '">';
      grp.items.forEach(function (a, i) {
        var d = a.description ? '<div class="asset-rdesc">' + esc(a.description) + '</div>'
          : '<div class="asset-rdesc asset-nodesc">（无 frontmatter — 名取目录名）</div>';
        html += '<div class="asset-row' + (state.sel === a ? ' active' : '') + '" data-g="' + esc(g) + '" data-i="' + i + '">' +
          '<div class="asset-rname">' + esc(a.name) + '</div>' + d + '</div>';
      });
      html += '</div>';
    });
    box.innerHTML = html;
    box._groups = groups;
  }

  function renderPlugins(cards) {
    var plugins = (state.inv && state.inv.plugins) || [];
    if (!plugins.length) { cards.innerHTML = '<div class="asset-empty">没有已安装插件</div>'; return; }
    cards.innerHTML = plugins.map(function (p) {
      var counts = Object.keys(p.asset_counts || {}).map(function (k) {
        return '<span class="acc"><b>' + p.asset_counts[k] + '</b> ' + esc(k) + '</span>';
      }).join('') || '<span class="acc acc-dim">无内容资产</span>';
      return '<div class="asset-card"><h3>' + esc(p.id.split('@')[0]) +
        ' <span class="acver">' + esc(p.version) + '</span></h3>' +
        '<div class="acrepo">' + esc(p.marketplace || '') + '</div>' +
        '<div class="acmeta">scope ' + esc(p.scope || '') +
        (p.installed_at ? ' · ' + esc(p.installed_at.slice(0, 10)) : '') +
        (p.commit_sha ? ' · ' + esc(p.commit_sha) : '') + '</div>' +
        '<div class="accounts">' + counts + '</div></div>';
    }).join('');
  }

  function openRaw(a) {
    state.sel = a;
    document.body.classList.add('asset-reading'); // mobile: slide reader over list
    // highlight selected row
    var prev = document.querySelector('#asset-sidebar-list .asset-row.active');
    if (prev) prev.classList.remove('active');

    $('asset-main-head').hidden = false;
    $('asset-main-title').textContent = a.name;
    $('asset-main-sub').innerHTML =
      (a.source.kind === 'plugin' ? '<span class="asset-from">来自 ' + esc(a.source.plugin) + '</span>' : '') +
      '<span class="asset-rel">' + esc(a.rel_path) + (a.anchor ? ' › ' + esc(a.anchor) : '') + '</span>';
    var body = $('asset-main-body');
    body.innerHTML = '<div class="asset-empty">加载中…</div>';

    var qs = '?kind=' + encodeURIComponent(a.kind) +
      '&source=' + encodeURIComponent(a.source.kind) +
      (a.source.plugin ? '&plugin=' + encodeURIComponent(a.source.plugin) : '') +
      (a.source.project ? '&project=' + encodeURIComponent(a.source.project) : '') +
      '&rel=' + encodeURIComponent(a.rel_path) +
      (a.anchor ? '&anchor=' + encodeURIComponent(a.anchor) : '');
    fetch('/api/cc/assets/raw' + qs, { credentials: 'same-origin' })
      .then(function (r) { if (!r.ok) throw new Error('HTTP ' + r.status); return r.text(); })
      .then(function (txt) {
        var html = '';
        if (a.anchor) {
          html += '<div class="asset-anchor-note">该资产位于含多条目的文件，已返回整文件；定位条目：<code>' + esc(a.anchor) + '</code></div>';
        }
        html += '<pre class="araw">' + esc(txt) + '</pre>';
        body.innerHTML = html;
      })
      .catch(function (e) { body.innerHTML = '<div class="asset-empty">读取失败：' + esc(e.message) + '</div>'; });
  }

  function backToList() {
    document.body.classList.remove('asset-reading');
    state.sel = null;
    var prev = document.querySelector('#asset-sidebar-list .asset-row.active');
    if (prev) prev.classList.remove('active');
  }

  function wire() {
    if (state.wired) return; state.wired = true;
    buildKindTabs();

    var tabs = $('asset-kindtabs');
    if (tabs) tabs.addEventListener('click', function (e) {
      var t = e.target.closest('.asset-kt'); if (!t) return;
      tabs.querySelectorAll('.asset-kt').forEach(function (x) { x.classList.remove('active'); });
      t.classList.add('active');
      state.kind = t.dataset.k; state.sel = null;
      var s = $('asset-search'); if (s) s.value = '';
      render();
    });

    var s = $('asset-search'); if (s) s.addEventListener('input', render);
    var rf = $('asset-refresh'); if (rf) rf.addEventListener('click', function () { state.loaded = false; load(true); });
    var bk = $('asset-back'); if (bk) bk.addEventListener('click', backToList);

    var list = $('asset-sidebar-list');
    if (list) list.addEventListener('click', function (e) {
      var grp = e.target.closest('.asset-grp');
      if (grp) {
        var g = grp.dataset.g;
        var cur = grp.classList.toggle('collapsed');
        state.collapsed[g] = cur;
        var items = list.querySelector('.asset-grpitems[data-gi="' + cssEsc(g) + '"]');
        if (items) items.classList.toggle('collapsed', cur);
        return;
      }
      var row = e.target.closest('.asset-row');
      if (row && list._groups) {
        list.querySelectorAll('.asset-row.active').forEach(function (x) { x.classList.remove('active'); });
        row.classList.add('active');
        var gd = list._groups[row.dataset.g];
        if (gd) openRaw(gd.items[+row.dataset.i]);
      }
    });
  }

  // ---- view show/hide, driven by the activity bar ----
  function show() {
    wire();
    $('asset-sidebar').hidden = false;
    $('asset-main').hidden = false;
    document.body.classList.add('nz-view-assets');
    load(false);
  }
  function hide() {
    document.body.classList.remove('nz-view-assets');
    document.body.classList.remove('asset-reading');
  }

  window.nzAssetView = { show: show, hide: hide };
})();
