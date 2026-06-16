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

  // state.project: selected root name. state.dir: workspace-relative dir ("" = root).
  var state = { roots: [], project: '', dir: '', loaded: false, wired: false };

  function fetchJSON(url) {
    if (U.fetchJSON) return U.fetchJSON(url);
    return fetch(url, { credentials: 'same-origin', headers: { Accept: 'application/json' } })
      .then(function (r) { if (!r.ok) throw new Error('HTTP ' + r.status); return r.json(); });
  }

  // ---- roots (projects) ----
  function loadRoots() {
    return fetchJSON('/api/projects').then(function (list) {
      // Local, path-bearing projects only — remote nodes have no readable FS.
      state.roots = (list || []).filter(function (p) {
        return p && p.path && (!p.node || p.node === 'local');
      });
      if (!state.project && state.roots.length) state.project = state.roots[0].name;
      buildRootSelect();
    });
  }

  function buildRootSelect() {
    var sel = $('files-root');
    if (!sel) return;
    if (!state.roots.length) {
      sel.innerHTML = '<option value="">（无可浏览的项目）</option>';
      return;
    }
    sel.innerHTML = state.roots.map(function (p) {
      return '<option value="' + esc(p.name) + '"' + (p.name === state.project ? ' selected' : '') + '>' + esc(p.name) + '</option>';
    }).join('');
  }

  // ---- directory listing ----
  function loadDir(dir) {
    if (!state.project) { renderEmpty('请选择一个项目'); return; }
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
      if (!e.is_dir && !e.symlink) {
        html += '<span class="files-acts">' +
          '<button type="button" class="files-act" data-act="preview" data-name="' + esc(e.name) + '" title="预览">预览</button>' +
          '<button type="button" class="files-act" data-act="download" data-name="' + esc(e.name) + '" title="下载">下载</button>' +
          '</span>';
      }
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
  function download(name) {
    var url = fileApiUrl(state.project, 'local', relOf(name), 'download');
    var a = document.createElement('a');
    a.href = url; a.download = name; a.rel = 'noopener';
    document.body.appendChild(a); a.click(); a.remove();
  }

  function preview(name) {
    var rel = relOf(name);
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
    if (!state.project) { toast('请先选择项目'); return; }
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

    var sel = $('files-root');
    if (sel) sel.addEventListener('change', function () {
      state.project = sel.value; state.dir = '';
      loadDir('');
    });

    var crumbs = $('files-crumbs');
    if (crumbs) crumbs.addEventListener('click', function (e) {
      var b = e.target.closest('.files-crumb'); if (!b) return;
      loadDir(b.dataset.dir || '');
    });

    var list = $('files-list');
    if (list) list.addEventListener('click', function (e) {
      var act = e.target.closest('.files-act');
      if (act) {
        e.stopPropagation();
        if (act.dataset.act === 'download') download(act.dataset.name);
        else preview(act.dataset.name);
        return;
      }
      var row = e.target.closest('.files-row'); if (!row) return;
      var kind = row.dataset.kind;
      if (kind === 'up') { loadDir(parentDir()); return; }
      if (kind === 'dir') { loadDir(relOf(row.dataset.name)); return; }
      if (kind === 'file') { preview(row.dataset.name); }
      // symlink rows are inert.
    });

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
