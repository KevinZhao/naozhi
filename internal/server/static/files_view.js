// files_view.js — dashboard "工作区文件" (workspace files) browser.
// Activity-Bar view (NOT a popup): the rail toggles body.nz-view-files, which
// swaps the chat sidebar/main for the files sidebar/main in place. Mirrors
// asset_browser.js' shell. Reads GET /api/projects (roots) +
// GET /api/projects/files/list (directory listing), serves file content via
// the existing GET /api/projects/file modes, and uploads via
// POST /api/projects/files/upload.
//
// Self-contained: served from /static/files_view.js, loaded via <script>. The
// only dashboard.js touch is the activity-bar wiring (window.nzFilesView).
(function () {
  'use strict';

  var U = (window.nz && window.nz.util) || {};
  function esc(s) {
    if (U.esc) return U.esc(s);
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }
  function toast(m) { if (U.showToast) U.showToast(m); }
  function $(id) { return document.getElementById(id); }

  // state.project: the current browse root's project name. Initialised to the
  // workspace root (is_root) so the operator lands at the top of the tree and
  // navigates DOWN into subdirectories — no project dropdown. A quick-jump
  // chip reassigns it to that project (the chosen project then BECOMES the
  // browse root, and the breadcrumb's leading segment shows its name — so the
  // breadcrumb is project-relative, not always workspace-relative). state.dir:
  // dir relative to state.project ("" = its root). state.rootName: cached name
  // of the is_root project, used to exclude the root from the quick-jump chips.
  var state = { roots: [], project: '', rootName: '', dir: '', loaded: false, wired: false };

  // Recent browse roots (favorite/recent quick-jump panel). master had no
  // recent-projects store, so this view owns its own localStorage key. Each
  // entry is a project name; most-recent first, capped.
  var RECENT_KEY = 'nz_files_recent';
  var RECENT_MAX = 8;

  function loadRecent() {
    try {
      var raw = window.localStorage.getItem(RECENT_KEY);
      var arr = raw ? JSON.parse(raw) : [];
      return Array.isArray(arr) ? arr.filter(function (n) { return typeof n === 'string'; }) : [];
    } catch (e) { return []; }
  }
  function saveRecent(arr) {
    try {
      window.localStorage.setItem(RECENT_KEY, JSON.stringify(arr.slice(0, RECENT_MAX)));
    } catch (e) { /* storage unavailable — quick-jump just won't persist */ }
  }
  function pushRecent(name) {
    if (!name) return;
    var arr = loadRecent().filter(function (n) { return n !== name; });
    arr.unshift(name);
    saveRecent(arr);
  }

  function fetchJSON(url) {
    if (U.fetchJSON) return U.fetchJSON(url);
    return fetch(url, { credentials: 'same-origin', headers: { Accept: 'application/json' } })
      .then(function (r) { if (!r.ok) throw new Error('HTTP ' + r.status); return r.json(); });
  }

  // ---- roots (projects) ----
  function loadRoots() {
    return fetchJSON('/api/projects').then(function (list) {
      // Local, path-bearing projects only — remote nodes have no readable FS.
      // Array.isArray guards against a non-array error body ({error:…}) which
      // would otherwise throw on .filter and surface a confusing message.
      state.roots = (Array.isArray(list) ? list : []).filter(function (p) {
        return p && p.path && (!p.node || p.node === 'local');
      });
      // Find the workspace root (include_root). When present, default the
      // browse root to it so the operator starts at the workspace top and
      // navigates down. Fall back to the shortest path (the closest thing to
      // a root) and finally roots[0] so the view still works without
      // include_root enabled.
      var root = null;
      state.roots.forEach(function (p) {
        if (p.is_root) root = p;
      });
      if (!root && state.roots.length) {
        root = state.roots.reduce(function (a, b) {
          return (b.path || '').length < (a.path || '').length ? b : a;
        });
      }
      state.rootName = root ? root.name : '';
      if (!state.project) state.project = state.rootName || (state.roots[0] && state.roots[0].name) || '';
      buildQuickJump();
    });
  }

  // buildQuickJump renders the favorite + recent quick-jump chips. Replaces
  // the old project <select>: the primary flow is navigate-down from the
  // workspace root, with these chips as a shortcut to a known project.
  function buildQuickJump() {
    var wrap = $('files-quickjump');
    if (!wrap) return;
    var byName = {};
    state.roots.forEach(function (p) { byName[p.name] = p; });
    // Favorites (from /api/projects), then recent names that still exist and
    // aren't already shown as favorites. Exclude the workspace root itself —
    // jumping there is redundant when it's already the default browse root.
    var seen = {};
    var chips = [];
    state.roots.forEach(function (p) {
      if (p.favorite && p.name !== state.rootName && !seen[p.name]) {
        seen[p.name] = 1; chips.push({ name: p.name, fav: true });
      }
    });
    // Prune recents that no longer resolve to a known project (deleted /
    // renamed) so stale names don't accumulate in localStorage indefinitely.
    var recent = loadRecent();
    var live = recent.filter(function (n) { return byName[n]; });
    if (live.length !== recent.length) saveRecent(live);
    live.forEach(function (n) {
      if (n !== state.rootName && !seen[n]) {
        seen[n] = 1; chips.push({ name: n, fav: false });
      }
    });
    if (!chips.length) { wrap.innerHTML = ''; wrap.hidden = true; return; }
    wrap.hidden = false;
    wrap.innerHTML = '<span class="files-qj-label">快速跳转</span>' + chips.map(function (c) {
      return '<button type="button" class="files-qj" data-project="' + esc(c.name) + '" title="' +
        esc(byName[c.name].path || c.name) + '">' + (c.fav ? '★ ' : '') + esc(c.name) + '</button>';
    }).join('');
  }

  // jumpToProject switches the browse root to a named project (quick-jump
  // chip) and resets to its root dir. Only accepts a name that resolves to a
  // known local project — chips are built from state.roots, but validate here
  // too so a stale chip (roots refreshed since render) can't drive a request
  // for a vanished project.
  function jumpToProject(name) {
    if (!name) return;
    var known = state.roots.some(function (p) { return p.name === name; });
    if (!known) { buildQuickJump(); return; }
    if (name === state.project) { loadDir(''); return; }
    state.project = name;
    pushRecent(name);
    buildQuickJump();
    loadDir('');
  }

  // ---- directory listing ----
  function loadDir(dir) {
    if (!state.project) { renderEmpty('没有可浏览的工作区'); return; }
    state.dir = dir || '';
    var box = $('files-list');
    if (box) box.innerHTML = '<div class="files-empty">加载中…</div>';
    var qs = 'project=' + encodeURIComponent(state.project) +
      (state.dir ? '&dir=' + encodeURIComponent(state.dir) : '');
    fetchJSON('/api/projects/files/list?' + qs).then(function (res) {
      state.loaded = true;
      renderCrumbs();
      renderList(res);
    }).catch(function (e) {
      renderEmpty('加载失败：' + esc(e.message));
    });
  }

  function renderEmpty(msg) {
    var box = $('files-list');
    if (box) box.innerHTML = '<div class="files-empty">' + esc(msg) + '</div>';
    renderCrumbs();
  }

  function renderCrumbs() {
    var wrap = $('files-crumbs');
    if (!wrap) return;
    var parts = state.dir ? state.dir.split('/') : [];
    var html = '<button type="button" class="files-crumb" data-dir="">' + esc(state.project || '项目') + '</button>';
    var acc = '';
    parts.forEach(function (seg) {
      if (!seg) return;
      acc = acc ? acc + '/' + seg : seg;
      html += '<span class="files-crumb-sep">/</span>' +
        '<button type="button" class="files-crumb" data-dir="' + esc(acc) + '">' + esc(seg) + '</button>';
    });
    wrap.innerHTML = html;
  }

  function fmtSize(n) {
    if (typeof window.formatFileSize === 'function') return window.formatFileSize(n);
    if (n == null) return '';
    if (n < 1024) return n + ' B';
    if (n < 1048576) return (n / 1024).toFixed(1) + ' KB';
    return (n / 1048576).toFixed(1) + ' MB';
  }

  function renderList(res) {
    var box = $('files-list');
    if (!box) return;
    var entries = (res && res.entries) || [];
    var html = '';
    if (state.dir) {
      html += '<div class="files-row files-up" data-kind="up"><span class="files-ic">↑</span>' +
        '<span class="files-name">上级目录</span></div>';
    }
    if (!entries.length && !state.dir) {
      box.innerHTML = '<div class="files-empty">空目录</div>';
      return;
    }
    entries.forEach(function (e) {
      var icon = e.is_dir ? '📁' : (e.symlink ? '🔗' : '📄');
      var meta = e.is_dir ? '' : (e.symlink ? '链接' : fmtSize(e.size));
      var cls = 'files-row' + (e.is_dir ? ' files-dir' : '') + (e.symlink ? ' files-link' : '');
      var kind = e.symlink ? 'symlink' : (e.is_dir ? 'dir' : 'file');
      html += '<div class="' + cls + '" data-kind="' + kind + '" data-name="' + esc(e.name) + '">' +
        '<span class="files-ic">' + icon + '</span>' +
        '<span class="files-name">' + esc(e.name) + '</span>' +
        '<span class="files-meta">' + esc(meta) + '</span>';
      // No per-row action buttons: clicking the row previews (and the preview
      // pane header carries the 下载 button). Keeps the narrow sidebar to
      // icon + name + size. A folder row navigates; a file row previews.
      html += '</div>';
    });
    if (res && res.truncated) {
      html += '<div class="files-empty">条目过多，仅显示前若干项，请进入子目录细化。</div>';
    }
    box.innerHTML = html;
  }

  // ---- relative path helpers ----
  function relOf(name) { return state.dir ? state.dir + '/' + name : name; }
  function parentDir() {
    if (!state.dir) return '';
    var i = state.dir.lastIndexOf('/');
    return i < 0 ? '' : state.dir.slice(0, i);
  }

  // ---- preview / download (reuse the existing /api/projects/file modes) ----
  // download(rel, name): rel is workspace-relative (so the header button works
  // regardless of the current dir). When called with one arg it downloads the
  // currently-previewed file.
  function download(rel, name) {
    if (rel == null) { rel = state.previewRel; name = state.previewName; }
    if (!rel) return;
    var url = fileApiUrl(state.project, 'local', rel, 'download');
    var a = document.createElement('a');
    a.href = url; a.download = name || (rel.split('/').pop() || 'file'); a.rel = 'noopener';
    document.body.appendChild(a); a.click(); a.remove();
  }

  function preview(name) {
    var rel = relOf(name);
    state.previewRel = rel; state.previewName = name;
    document.body.classList.add('files-reading');
    $('files-main-head').hidden = false;
    $('files-main-title').textContent = name;
    $('files-main-sub').textContent = rel;
    var body = $('files-main-body');
    body.innerHTML = '<div class="files-empty">加载中…</div>';

    // Decide render mode by extension; the server re-validates and pins MIME.
    var ext = (name.split('.').pop() || '').toLowerCase();
    var imgExt = { png: 1, jpg: 1, jpeg: 1, gif: 1, webp: 1, bmp: 1, ico: 1 };
    if (ext === 'svg') {
      body.innerHTML = '';
      renderSandboxedBlob(state.project, 'local', rel, body, 'image/svg+xml');
      return;
    }
    if (imgExt[ext]) {
      body.innerHTML = '';
      var img = document.createElement('img');
      img.src = fileApiUrl(state.project, 'local', rel, 'raw');
      img.alt = name; img.loading = 'lazy';
      body.appendChild(img);
      return;
    }
    if (ext === 'pdf') {
      body.innerHTML = '';
      var frame = document.createElement('iframe');
      frame.src = fileApiUrl(state.project, 'local', rel, 'raw');
      frame.title = name;
      body.appendChild(frame);
      return;
    }
    if (ext === 'html' || ext === 'htm' || ext === 'xhtml') {
      body.innerHTML = '';
      renderSandboxedBlob(state.project, 'local', rel, body, 'text/html');
      return;
    }
    // Text / unknown → preview JSON {content, truncated, size, mime}.
    fetchJSON(fileApiUrl(state.project, 'local', rel, 'preview')).then(function (res) {
      if (res && res.mime && !/^text\//.test(res.mime) && res.content == null) {
        body.innerHTML = '<div class="files-empty">该文件不可预览，请下载查看。</div>';
        return;
      }
      var note = (res && res.truncated) ? '<div class="files-note">文件过大，仅显示前一部分。</div>' : '';
      body.innerHTML = note + '<pre class="files-raw">' + esc((res && res.content) || '') + '</pre>';
    }).catch(function (e) {
      body.innerHTML = '<div class="files-empty">不可预览（' + esc(e.message) + '），可尝试下载。</div>';
    });
  }

  // ---- upload (XHR for progress) ----
  function uploadFiles(fileList) {
    if (!state.project) { toast('没有可浏览的工作区'); return; }
    var files = Array.prototype.slice.call(fileList || []);
    if (!files.length) return;
    var i = 0;
    function next() {
      if (i >= files.length) { loadDir(state.dir); return; }
      var f = files[i++];
      uploadOne(f, false, next);
    }
    next();
  }

  function uploadOne(file, overwrite, done) {
    var fd = new FormData();
    fd.append('project', state.project);
    if (state.dir) fd.append('dir', state.dir);
    fd.append('file', file, file.name);
    var url = '/api/projects/files/upload' + (overwrite ? '?overwrite=1' : '');
    var bar = showProgress(file.name);
    var xhr = new XMLHttpRequest();
    xhr.open('POST', url);
    xhr.withCredentials = true;
    if (xhr.upload) {
      xhr.upload.onprogress = function (e) {
        if (e.lengthComputable) bar.set(e.loaded / e.total);
      };
    }
    xhr.onload = function () {
      bar.done();
      if (xhr.status >= 200 && xhr.status < 300) {
        toast('已上传 ' + file.name);
        done();
        return;
      }
      if (xhr.status === 409) {
        if (window.confirm('“' + file.name + '” 已存在，覆盖？')) {
          uploadOne(file, true, done);
          return;
        }
        done();
        return;
      }
      if (xhr.status === 413) { toast('文件过大：' + file.name); done(); return; }
      if (xhr.status === 403) { toast('该文件名不允许上传：' + file.name); done(); return; }
      if (xhr.status === 429) { toast('上传过于频繁，请稍后重试'); done(); return; }
      toast('上传失败（' + xhr.status + '）：' + file.name);
      done();
    };
    xhr.onerror = function () { bar.done(); toast('上传出错：' + file.name); done(); };
    xhr.send(fd);
  }

  function showProgress(name) {
    var wrap = $('files-progress');
    if (!wrap) return { set: function () {}, done: function () {} };
    var row = document.createElement('div');
    row.className = 'files-prow';
    row.innerHTML = '<span class="files-pname">' + esc(name) + '</span><span class="files-pbar"><i></i></span>';
    wrap.appendChild(row);
    var fill = row.querySelector('.files-pbar > i');
    return {
      set: function (frac) { if (fill) fill.style.width = Math.round(frac * 100) + '%'; },
      done: function () { setTimeout(function () { if (row.parentNode) row.parentNode.removeChild(row); }, 800); }
    };
  }

  function backToList() {
    document.body.classList.remove('files-reading');
  }

  // ---- wiring ----
  function wire() {
    if (state.wired) return; state.wired = true;

    // Quick-jump chips (favorite + recent) replace the old project <select>.
    var qj = $('files-quickjump');
    if (qj) qj.addEventListener('click', function (e) {
      var b = e.target.closest('.files-qj'); if (!b) return;
      jumpToProject(b.dataset.project || '');
    });

    var crumbs = $('files-crumbs');
    if (crumbs) crumbs.addEventListener('click', function (e) {
      var b = e.target.closest('.files-crumb'); if (!b) return;
      loadDir(b.dataset.dir || '');
    });

    var list = $('files-list');
    if (list) list.addEventListener('click', function (e) {
      var row = e.target.closest('.files-row'); if (!row) return;
      var kind = row.dataset.kind;
      if (kind === 'up') { loadDir(parentDir()); return; }
      if (kind === 'dir') { loadDir(relOf(row.dataset.name)); return; }
      if (kind === 'file') { preview(row.dataset.name); }
      // symlink rows are inert.
    });

    var dl = $('files-download'); if (dl) dl.addEventListener('click', function () { download(); });
    var bk = $('files-back'); if (bk) bk.addEventListener('click', backToList);
    var rf = $('files-refresh'); if (rf) rf.addEventListener('click', function () { loadDir(state.dir); });

    // Upload button → hidden input.
    var btn = $('files-upload-btn'), input = $('files-upload-input');
    if (btn && input) {
      btn.addEventListener('click', function () { input.value = ''; input.click(); });
      input.addEventListener('change', function () { uploadFiles(input.files); });
    }

    // Drag-drop onto the list pane.
    var drop = $('files-droparea');
    if (drop) {
      ['dragenter', 'dragover'].forEach(function (ev) {
        drop.addEventListener(ev, function (e) { e.preventDefault(); drop.classList.add('files-dragover'); });
      });
      ['dragleave', 'drop'].forEach(function (ev) {
        drop.addEventListener(ev, function (e) { e.preventDefault(); if (ev === 'dragleave' && e.target !== drop) return; drop.classList.remove('files-dragover'); });
      });
      drop.addEventListener('drop', function (e) {
        e.preventDefault(); drop.classList.remove('files-dragover');
        if (e.dataTransfer && e.dataTransfer.files) uploadFiles(e.dataTransfer.files);
      });
    }
  }

  // ---- view show/hide, driven by the activity bar ----
  function show() {
    wire();
    $('files-sidebar').hidden = false;
    $('files-main').hidden = false;
    document.body.classList.add('nz-view-files');
    loadRoots().then(function () { loadDir(state.loaded ? state.dir : ''); })
      .catch(function (e) { renderEmpty('加载项目失败：' + esc(e.message)); });
  }
  function hide() {
    document.body.classList.remove('nz-view-files');
    document.body.classList.remove('files-reading');
  }

  window.nzFilesView = { show: show, hide: hide };
})();
