// @ts-check
// Shared mock server for naozhi dashboard E2E tests.
// Serves dashboard.html and stubs all API endpoints with configurable responses.
const http = require('http');
const fs = require('fs');
const path = require('path');
const crypto = require('crypto');
const zlib = require('zlib');

const STATIC_DIR = path.join(__dirname, '..', '..', 'internal', 'server', 'static');
// The generated backend contract — the same ES module the dashboard imports.
// Stub paths reference it so a route rename breaks the mock in the same
// commit that regenerates contract.js (#2539). startMockServer listens only
// once it has loaded.
let NZ_CONTRACT;
const contractReady = import(require('url').pathToFileURL(path.join(STATIC_DIR, 'contract.js')).href)
  .then((m) => { NZ_CONTRACT = m.NZ_CONTRACT; });

// sendEntries answers an event route with page.
function sendEntries(res, page, headers = { 'Content-Type': 'application/json' }) {
  res.writeHead(200, headers);
  res.end(JSON.stringify(page));
}

// The dashboard CSP exactly as production serves it (routes.go / #1980).
// A Go drift test (TestDashboardCSP_MockServerHeaderInSync) compares this
// literal against the runtime header, so edit both together.
const MOCK_DASHBOARD_CSP =
  "default-src 'self'; script-src 'self' 'sha256-Dc5Mfm9TcKn7OwTLyG3/T2KjnRh7zV1Xc4ct4adm4/g='; connect-src 'self'; style-src 'self'; font-src 'self'; img-src 'self' data: blob:; frame-src 'self' blob:; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; require-sri-for script style font";

/**
 * workflowReply answers GET /api/sessions/workflow for the fixture of one
 * task, as the Go handler does (ext/workflows): rows=none → no rows,
 * since=V&epoch=E → the rows with rev > V when E is the fixture's epoch and
 * V ≤ its version, else all of them; since with rows=none or a lone epoch
 * is a 400. The fixture is the response minus rows_mode and server_now; its
 * workflow.agents holds every row. Returns {status, body}.
 */
function workflowReply(fx, params) {
  const has = (k) => params.has(k);
  // A decimal uint64, as strconv.ParseUint reads it (leading zeros too).
  const sinceText = has('since') ? params.get('since') : null;
  const since = sinceText !== null && /^\d+$/.test(sinceText) && BigInt(sinceText) <= 0xffffffffffffffffn ? BigInt(sinceText) : null;
  const epoch = has('epoch') ? params.get('epoch') : null;
  if ((has('rows') && params.get('rows') !== 'none') || has('since') !== has('epoch')
    || (has('rows') && has('since')) || (sinceText !== null && since === null)
    || (epoch !== null && !/^[0-9a-f]{16}$/.test(epoch))) {
    return { status: 400, body: { error: 'invalid rows, since or epoch parameter' } };
  }
  const all = fx.workflow.agents || [];
  let rowsMode = 'full';
  let rows = all;
  if (has('rows')) {
    rowsMode = 'none';
    rows = [];
  } else if (since !== null && epoch === fx.epoch && since <= BigInt(fx.version)) {
    rowsMode = 'delta';
    rows = all.filter((a) => BigInt(a.rev) > since);
  }
  const body = { ...fx, server_now: Date.now(), rows_mode: rowsMode, workflow: { ...fx.workflow, agents: rows } };
  return { status: 200, body };
}

function defaultSessions() {
  return {
    sessions: [
      {
        key: 'dashboard:direct:2026-01-01-120000-1:myproject',
        state: 'ready',
        platform: 'dashboard',
        agent: 'general',
        cli_name: 'claude',
        cli_version: '1.0.30',
        workspace: '/home/user/workspace/myproject',
        last_active: Date.now() - 60000,
        last_prompt: 'hello world',
        last_activity: '',
        node: 'local',
        project: 'myproject',
      },
      {
        key: 'dashboard:direct:2026-01-01-120001-2:otherproject',
        state: 'running',
        platform: 'dashboard',
        agent: 'reviewer',
        cli_name: 'claude',
        cli_version: '1.0.30',
        workspace: '/home/user/workspace/otherproject',
        last_active: Date.now() - 30000,
        last_prompt: 'review this code',
        last_activity: 'reviewing code',
        node: 'local',
        project: 'otherproject',
      },
      {
        key: 'dashboard:direct:2026-01-01-120002-3:myproject',
        state: 'ready',
        platform: 'dashboard',
        agent: 'general',
        cli_name: 'claude',
        workspace: '/home/user/workspace/myproject',
        last_active: Date.now() - 300000,
        last_prompt: 'fix the bug',
        node: 'local',
        project: 'myproject',
        session_id: 'sess-001',
      },
    ],
    stats: {
      total: 3,
      running: 1,
      ready: 1,
      active: 3,
      uptime: '2h30m00s',
      backend: 'cc',
      max_procs: 20,
      default_workspace: '/home/user/workspace',
      agents: ['general', 'reviewer', 'researcher'],
      projects: [
        { name: 'myproject', path: '/home/user/workspace/myproject', favorite: false, github: true, git_remote_url: 'https://github.com/acme/myproject.git', stableKey: 'dashboard:pj:0123456789abcdef:general' },
        { name: 'otherproject', path: '/home/user/workspace/otherproject', favorite: false, github: false, git_remote_url: '' },
        { name: 'pinned-empty', path: '/home/user/workspace/pinned-empty', favorite: true, github: true, git_remote_url: 'git@github.com:acme/pinned.git' },
      ],
      version: 1,
    },
    history_sessions: [
      {
        session_id: 'hist-001',
        workspace: '/home/user/workspace/myproject',
        project: 'myproject',
        last_prompt: 'old task from yesterday',
        last_active: Date.now() - 86400000,
      },
    ],
    nodes: { local: { display_name: 'Local', status: 'ok' } },
  };
}

function defaultEvents() {
  return [
    { type: 'system', summary: 'session started', time: Date.now() - 10000, uuid: 'ev-sys-0001' },
    { type: 'user', detail: 'hello world', time: Date.now() - 8000, uuid: 'ev-usr-0002' },
    {
      type: 'text',
      detail: 'Hi! Here is some **bold** and `inline code`.\n\n```javascript\nconsole.log("hello");\n```\n\nAnd a table:\n\n| Col A | Col B |\n|-------|-------|\n| 1     | 2     |',
      time: Date.now() - 5000,
      uuid: 'ev-txt-0003',
    },
  ];
}

// defaultCronRuns — 为 cron-001 生成 N 条 recent_runs（混合状态），覆盖
// PR-1 timeline 行选中、sheet 打开、↑↓ 切换、详情 fetch、followup #1
// scroll-into-view（多于 timeline 可视区） 等场景。
function defaultCronRuns() {
  const now = Date.now();
  const head = [
    { run_id: 'run-aaaa1111', state: 'failed',    started_at: now - 1*60*60*1000, ended_at: now - 1*60*60*1000 + 31000, duration_ms: 31000, trigger: 'cron',   session_id: 'sess-fail0001', error_class: 'network' },
    { run_id: 'run-bbbb2222', state: 'succeeded', started_at: now - 2*60*60*1000, ended_at: now - 2*60*60*1000 + 12000, duration_ms: 12000, trigger: 'cron',   session_id: 'sess-ok000002' },
    { run_id: 'run-cccc3333', state: 'succeeded', started_at: now - 3*60*60*1000, ended_at: now - 3*60*60*1000 + 9000,  duration_ms: 9000,  trigger: 'manual', session_id: 'sess-ok000003' },
    { run_id: 'run-dddd4444', state: 'skipped',   started_at: now - 4*60*60*1000, ended_at: now - 4*60*60*1000 + 100,   duration_ms: 100,   trigger: 'cron'                                  },
    { run_id: 'run-eeee5555', state: 'succeeded', started_at: now - 5*60*60*1000, ended_at: now - 5*60*60*1000 + 11000, duration_ms: 11000, trigger: 'cron',   session_id: 'sess-ok000005' },
  ];
  // 后追 15 条 succeeded 充实 timeline 让 followup #1 测试可触发 scroll
  const tail = [];
  for (let i = 6; i <= 20; i++) {
    const id = String(i).padStart(4, '0');
    tail.push({
      run_id: 'run-tail' + id,
      state: 'succeeded',
      started_at: now - i*60*60*1000,
      ended_at:   now - i*60*60*1000 + 10000,
      duration_ms: 10000,
      trigger: 'cron',
      session_id: 'sess-tail' + id,
    });
  }
  return head.concat(tail);
}

function defaultCronJobs() {
  return [
    {
      id: 'cron-001',
      schedule: '@every 1h',
      prompt: 'check server status',
      work_dir: '/home/user/workspace/myproject',
      paused: false,
      created_at: Date.now() - 86400000,
      next_run: Date.now() + 3600000,
      last_run_at: Date.now() - 3600000,
      last_result: 'Server is healthy',
      recent_runs: defaultCronRuns(),
      stats: { total: 120, succeeded: 110, failed: 8, skipped: 2 },
    },
    {
      id: 'cron-002',
      schedule: '0 9 * * 1-5',
      prompt: 'daily report',
      work_dir: '/home/user/workspace/otherproject',
      paused: true,
      created_at: Date.now() - 172800000,
      next_run: Date.now() + 86400000,
    },
  ];
}

function defaultProjects() {
  return [
    { name: 'myproject', path: '/home/user/workspace/myproject' },
    { name: 'otherproject', path: '/home/user/workspace/otherproject' },
  ];
}

// defaultGitStates maps a session key → GET /api/sessions/git payload, one per
// chip shape: main-tree branch, linked worktree, and (by omission) non-repo.
function defaultGitStates() {
  return {
    'dashboard:direct:2026-01-01-120000-1:myproject': {
      is_repo: true,
      workspace: '/home/user/workspace/myproject',
      root: '/home/user/workspace/myproject',
      repo: 'myproject',
      branch: 'master',
    },
    'dashboard:direct:2026-01-01-120001-2:otherproject': {
      is_repo: true,
      workspace: '/home/user/workspace/otherproject/.claude/worktrees/feat-x',
      root: '/home/user/workspace/otherproject/.claude/worktrees/feat-x',
      repo: 'otherproject',
      branch: 'worktree-feat-x',
      worktree: 'feat-x',
    },
  };
}

/**
 * Start a mock HTTP server.
 * @param {object} [overrides] - Override specific route handlers.
 * @param {boolean} [overrides.shim] - false serves dashboard.html without the e2e shim, as production does.
 * @param {object} [overrides.sessions] - Custom sessions response. A `history_sessions` key
 *   in it is not served there: like the backend, /api/sessions carries stats.history_tag and
 *   GET /api/sessions/history serves the list.
 * @param {object[]} [overrides.events] - Custom events response.
 * @param {object} [overrides.workflows] - task_id → GET /api/sessions/workflow fixture ({epoch, version,
 *   workflow: WireView with every row in agents, result?, logs?, logs_truncated?, result_unavailable?});
 *   the route applies rows=none / since=&epoch= to it and 404s task ids not listed. A test may edit
 *   the object in place; each request reads it then.
 * @param {number} [overrides.workflowDelayMs] - Hold GET /api/sessions/workflow answers this long.
 * @param {object[]} [overrides.cronJobs] - Custom cron jobs response.
 * @param {object} [overrides.cronListMeta] - Extra top-level fields merged into GET /api/cron
 *   (timezone / timezone_abbr / timezone_label ...). recent_runs_cap defaults to 5 like the backend.
 * @param {boolean} [overrides.requireAuth] - If true, require bearer token.
 * @param {string} [overrides.authToken] - Expected token value.
 * @param {string} [overrides.loginStatus400] - The FIRST POST /api/auth/login answers 400 with
 *   this string as the JSON "error" (the trusted_proxy XFF refusal); later logins take the
 *   normal token check.
 * @param {number} [overrides.sendStatus] - Status code for POST /api/sessions/send.
 * @param {boolean} [overrides.ws] - If true, accept /ws upgrades with a minimal
 *   WebSocket server (auth → auth_ok, everything else recorded). Default: no
 *   upgrade listener, so the dashboard falls back to polling.
 * @param {boolean} [overrides.wsHoldAuth] - With ws:true, record the dashboard's
 *   {type:'auth'} but never answer it, so wsm stays in AUTH (authenticating).
 * @param {{html: string, csp: string}} [overrides.dashboardPage] - Serve this page
 *   and CSP at /dashboard instead of the raw file and MOCK_DASHBOARD_CSP: the
 *   page Go renders, written by tools/render-dashboard.
 * @returns {Promise<{server: http.Server, port: number, url: string}>}
 */
function startMockServer(overrides = {}) {
  // e2e-shim (#2941): production exports exactly one global, window.nz,
  // and carries no test surface. The mock injects test/e2e/e2e-shim.js,
  // which imports the modules' exports and mirrors the names the suite
  // probes onto window and window.nz.test. New tests should use nz.test.*.
  // overrides.shim === false serves the page exactly as production does.
  const rendered = overrides.dashboardPage;
  const page = rendered ? rendered.html : fs.readFileSync(path.join(STATIC_DIR, 'dashboard.html'), 'utf8');
  const dashboardCSP = rendered ? rendered.csp : MOCK_DASHBOARD_CSP;
  const html = overrides.shim === false
    ? page
    : page.replace('</body>', '<script type="module" src="/e2e-shim.js"></script>\n</body>');
  const manifest = fs.readFileSync(path.join(STATIC_DIR, 'manifest.json'), 'utf8');

  const sessionsData = overrides.sessions || defaultSessions();
  const historyData = sessionsData.history_sessions || [];
  // historyTag mirrors historyContentTag: the list's content hash, '' for none.
  const tagOf = list => list.length ? crypto.createHash('sha256').update(JSON.stringify(list)).digest('hex').slice(0, 32) : '';
  const historyTag = () => tagOf(historyData);
  // sessionsBody is the /api/sessions body the backend would send for sessionsData.
  const sessionsBody = () => {
    const { history_sessions: _legacy, ...body } = sessionsData;
    const tag = historyTag();
    return tag && body.stats ? { ...body, stats: { ...body.stats, history_tag: tag } } : body;
  };
  const eventsData = overrides.events || defaultEvents();
  // The server's in-memory ring (EventEntries, default 500).
  const eventsRingSize = 500;
  const cronJobsData = overrides.cronJobs || defaultCronJobs();
  // 与后端 cronListResp 对齐：recent_runs_cap 恒为 recentRunsPerJob(5)；时区字段按需注入。
  const cronListMeta = Object.assign({ recent_runs_cap: 5 }, overrides.cronListMeta || {});
  const gitStates = defaultGitStates();
  const workflows = overrides.workflows || {};
  const workflowDelayMs = overrides.workflowDelayMs || 0;
  // Every GET /api/sessions/workflow, as its query parameters.
  const workflowCalls = [];
  const requireAuth = overrides.requireAuth || false;
  const authToken = overrides.authToken || 'test-token-123';
  const loginStatus400 = overrides.loginStatus400 || '';

  let sendCalls = [];
  let bindCalls = [];
  let loginCalls = [];
  let favoriteCalls = [];
  let authedCookies = new Set();

  const server = http.createServer((req, res) => {
    // Auth check helper
    const checkAuth = () => {
      if (!requireAuth) return true;
      const authHeader = req.headers['authorization'];
      if (authHeader === 'Bearer ' + authToken) return true;
      const cookie = req.headers['cookie'] || '';
      if (cookie.includes('naozhi_auth=valid')) return true;
      res.writeHead(401, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ error: 'unauthorized' }));
      return false;
    };

    const url = new URL(req.url, 'http://localhost');
    const pathname = url.pathname;

    // Dashboard stylesheets (#2559 split them out of the inline <style>).
    // Served from disk so a layout-sensitive spec sees the same cascade as
    // production; an unknown name 404s like the Go handler does.
    if (pathname.startsWith('/static/css/')) {
      const name = pathname.slice('/static/css/'.length);
      if (!/^[\w.]+\.css$/.test(name)) { res.writeHead(404); res.end(); return; }
      let body;
      try { body = fs.readFileSync(path.join(STATIC_DIR, 'css', name)); }
      catch { res.writeHead(404); res.end(); return; }
      res.writeHead(200, { 'Content-Type': 'text/css; charset=utf-8' });
      res.end(body);
      return;
    }

    // Vendored libraries (KaTeX, mermaid): the same tree the Go handlers
    // serve, with the same types; a file embedded as <name>.gz is served as
    // <name>; anything outside the tree 404s.
    if (pathname.startsWith('/static/vendor/')) {
      const rel = pathname.slice('/static/'.length);
      const type = { '.js': 'application/javascript', '.css': 'text/css; charset=utf-8', '.woff2': 'font/woff2' }[path.extname(rel)];
      const file = path.join(STATIC_DIR, rel);
      let body;
      try {
        if (!type || rel.split('/').includes('..')) throw new Error('outside the tree');
        body = fs.existsSync(file) ? fs.readFileSync(file) : zlib.gunzipSync(fs.readFileSync(file + '.gz'));
      } catch { res.writeHead(404); res.end(); return; }
      res.writeHead(200, { 'Content-Type': type });
      res.end(body);
      return;
    }

    if (pathname === '/e2e-shim.js') {
      res.writeHead(200, { 'Content-Type': 'application/javascript' });
      res.end(fs.readFileSync(path.join(__dirname, 'e2e-shim.js')));
      return;
    }

    // Static routes
    if (pathname === '/dashboard') {
      // Mirror the production CSP (routes.go handleDashboard) so the whole
      // Playwright suite runs the dashboard under the real policy — inline
      // handlers or scripts that production would block fail here too.
      // The default MOCK_DASHBOARD_CSP is kept in lockstep by
      // TestDashboardCSP_MockServerHeaderInSync; a dashboardPage override
      // serves the CSP tools/render-dashboard wrote out instead.
      res.writeHead(200, {
        'Content-Type': 'text/html',
        'Content-Security-Policy': dashboardCSP,
      });
      res.end(html);
      return;
    }
    if (pathname === '/manifest.json') {
      res.writeHead(200, { 'Content-Type': 'application/manifest+json' });
      res.end(manifest);
      return;
    }
    // dashboard.html 引用 5 个 /static/*.js（nz_util / dashboard / cron_view /
    // agent_view / asset_browser，PR#1954 拆分后）。逐个白名单伺服跟不上拆分
    // 节奏：漏掉 nz_util.js 时 dashboard.js 因 window.nz 未定义直接崩，所有
    // e2e 卡在 .session-card 不出现。改为目录伺服（仅 .js、basename 防穿越）。
    if (pathname.startsWith('/static/') && pathname.endsWith('.js')) {
      try {
        const js = fs.readFileSync(path.join(STATIC_DIR, path.basename(pathname)));
        res.writeHead(200, { 'Content-Type': 'application/javascript' });
        res.end(js);
      } catch {
        res.writeHead(404);
        res.end();
      }
      return;
    }
    if (pathname === '/sw.js') {
      res.writeHead(200, { 'Content-Type': 'application/javascript' });
      res.end('// mock sw');
      return;
    }

    // Auth routes
    if (pathname === NZ_CONTRACT.API.auth_login && req.method === 'POST') {
      let body = '';
      req.on('data', c => (body += c));
      req.on('end', () => {
        loginCalls.push(body);
        if (loginStatus400 && loginCalls.length === 1) {
          res.writeHead(400, { 'Content-Type': 'application/json' });
          res.end(JSON.stringify({ error: loginStatus400 }));
          return;
        }
        try {
          const data = JSON.parse(body);
          if (data.token === authToken) {
            const id = 'cookie-' + Date.now();
            authedCookies.add(id);
            res.writeHead(200, {
              'Content-Type': 'application/json',
              'Set-Cookie': 'naozhi_auth=valid; Path=/; HttpOnly; Max-Age=2592000',
            });
            res.end(JSON.stringify({ ok: true }));
          } else {
            res.writeHead(401, { 'Content-Type': 'application/json' });
            res.end(JSON.stringify({ error: 'invalid token' }));
          }
        } catch {
          res.writeHead(400);
          res.end('bad json');
        }
      });
      return;
    }

    // Session routes
    if (pathname === NZ_CONTRACT.API.scratch_open && req.method === 'POST') {
      let body = '';
      req.on('data', c => (body += c));
      req.on('end', () => {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({
          scratch_id: 'scr-0001',
          key: 'dashboard:scratch:2026-01-01-130000-9:myproject',
          agent_id: 'general',
          context_turns: 0,
          context_truncated: false,
        }));
      });
      return;
    }

    if (pathname === NZ_CONTRACT.API.sessions && req.method === 'GET') {
      if (!checkAuth()) return;
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify(sessionsBody()));
      return;
    }

    if (pathname === NZ_CONTRACT.API.sessions_history && req.method === 'GET') {
      if (!checkAuth()) return;
      const list = historyData;
      const tag = tagOf(list);
      const headers = { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' };
      if (tag) {
        headers.ETag = '"h' + tag + '"';
        if ((req.headers['if-none-match'] || '') === headers.ETag) {
          res.writeHead(304, { ETag: headers.ETag, 'Cache-Control': 'no-store' });
          res.end();
          return;
        }
      }
      res.writeHead(200, headers);
      res.end(JSON.stringify(tag ? { history_sessions: list, history_tag: tag } : { history_sessions: [] }));
      return;
    }

    if (pathname === NZ_CONTRACT.API.sessions_events && req.method === 'GET') {
      if (!checkAuth()) return;
      // Mirror EventLog.EntriesSince: `after` is strictly greater-than, so the
      // 1 s poll never re-delivers the watermark millisecond.
      const all = eventsData;
      const after = Number(url.searchParams.get('after') || 0);
      const before = Number(url.searchParams.get('before') || 0);
      const limit = Number(url.searchParams.get('limit') || 0);
      const headers = { 'Content-Type': 'application/json' };
      let out;
      if (after > 0) {
        out = all.filter(e => !e?.time || e.time > after);
        if (limit > 0 && out.length > limit) out = out.slice(-limit);
      } else if (before > 0) {
        // events.go `before` branch: strictly older, newest `limit` of them,
        // chronological, and whether the page left any of them out.
        const older = all.filter(e => e?.time && e.time < before);
        out = limit > 0 && older.length > limit ? older.slice(-limit) : older;
        headers['X-Events-Has-More'] = older.length > out.length ? '1' : '0';
      } else if (limit > 0) {
        // handlers.go initial-page branch: tail N + authoritative has-more header.
        out = all.slice(-limit);
        headers['X-Events-Has-More'] = all.length > out.length ? '1' : '0';
      } else {
        // handlers.go default branch: the in-memory ring only.
        out = all.slice(-eventsRingSize);
      }
      sendEntries(res, out, headers);
      return;
    }

    // Rename (user_label). Records the call and patches the snapshot like the
    // real handler; bumps stats.version so the next poll re-renders the card.
    if (pathname === NZ_CONTRACT.API.sessions_label && req.method === 'PATCH') {
      if (!checkAuth()) return;
      let body = '';
      req.on('data', c => (body += c));
      req.on('end', () => {
        try {
          const b = JSON.parse(body || '{}');
          const s = (sessionsData.sessions || []).find(x => x.key === b.key);
          if (s) s.user_label = b.label || '';
          if (sessionsData.stats && typeof sessionsData.stats.version === 'number') sessionsData.stats.version++;
        } catch (_) { /* malformed body: still ack like a lenient server */ }
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ ok: true }));
      });
      return;
    }

    // Git branch / worktree chip. Keyed by session key so a test can give
    // different sessions different checkouts (main tree vs linked worktree vs
    // non-repo). Unknown keys fall through to the is_repo:false shape, which
    // is what the real handler returns for a plain folder.
    if (pathname === NZ_CONTRACT.API.sessions_git && req.method === 'GET') {
      if (!checkAuth()) return;
      const key = url.searchParams.get('key') || '';
      const state = gitStates[key] || { is_repo: false };
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify(state));
      return;
    }

    // Full-size attachment fetch used by the lightbox (data-full URL).
    // Serves a tiny valid PNG so naturalWidth>0; ?path=missing.png returns
    // 404 to exercise the thumb-fallback path.
    if (pathname === NZ_CONTRACT.API.sessions_attachment && req.method === 'GET') {
      if (!checkAuth()) return;
      const p = url.searchParams.get('path') || '';
      if (p.includes('missing')) {
        res.writeHead(404);
        res.end();
        return;
      }
      const png = Buffer.from(
        'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==',
        'base64'
      );
      res.writeHead(200, { 'Content-Type': 'image/png' });
      res.end(png);
      return;
    }

    if (pathname === NZ_CONTRACT.API.sessions_send && req.method === 'POST') {
      if (!checkAuth()) return;
      const status = overrides.sendStatus || 200;
      let body = '';
      req.on('data', c => (body += c));
      req.on('end', () => {
        sendCalls.push(body);
        if (status === 429) {
          res.writeHead(429, { 'Content-Type': 'application/json' });
          res.end(JSON.stringify({ error: 'message queue full' }));
        } else if (status >= 400) {
          res.writeHead(status, { 'Content-Type': 'application/json' });
          res.end(JSON.stringify({ error: 'send failed' }));
        } else {
          res.writeHead(200, { 'Content-Type': 'application/json' });
          res.end(JSON.stringify({ ok: true }));
        }
      });
      return;
    }

    if (pathname === NZ_CONTRACT.API.sessions_bind && req.method === 'POST') {
      if (!checkAuth()) return;
      let body = '';
      req.on('data', c => (body += c));
      req.on('end', () => {
        bindCalls.push(body);
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ ok: true }));
      });
      return;
    }

    if (pathname === NZ_CONTRACT.API.sessions && req.method === 'DELETE') {
      if (!checkAuth()) return;
      // Like the backend, a 200 means the key is already gone from the list:
      // the client's post-DELETE re-sync must not bring the card back.
      let body = '';
      req.on('data', c => (body += c));
      req.on('end', () => {
        try {
          const b = JSON.parse(body || '{}');
          const list = sessionsData.sessions || [];
          const i = list.findIndex(x => x.key === b.key && (x.node || 'local') === (b.node || 'local'));
          if (i >= 0) list.splice(i, 1);
          if (sessionsData.stats && typeof sessionsData.stats.version === 'number') sessionsData.stats.version++;
        } catch (_) { /* malformed body: still ack like a lenient server */ }
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ ok: true }));
      });
      return;
    }

    if (pathname === NZ_CONTRACT.API.sessions_resume && req.method === 'POST') {
      if (!checkAuth()) return;
      let body = '';
      req.on('data', c => (body += c));
      req.on('end', () => {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ key: 'dashboard:direct:resumed:myproject' }));
      });
      return;
    }

    // Discovery routes
    if (pathname === NZ_CONTRACT.API.discovered && req.method === 'GET') {
      if (!checkAuth()) return;
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify([]));
      return;
    }

    if (pathname === NZ_CONTRACT.API.discovered_preview && req.method === 'GET') {
      if (!checkAuth()) return;
      sendEntries(res, []);
      return;
    }

    if (pathname === NZ_CONTRACT.API.discovered_close && req.method === 'POST') {
      if (!checkAuth()) return;
      let body = '';
      req.on('data', c => (body += c));
      req.on('end', () => {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ ok: true }));
      });
      return;
    }

    // Cron routes
    if (pathname === NZ_CONTRACT.API.cron && req.method === 'GET') {
      if (!checkAuth()) return;
      res.writeHead(200, { 'Content-Type': 'application/json' });
      // Dashboard expects { jobs: [...] } format (+ recent_runs_cap / timezone meta)
      res.end(JSON.stringify(Object.assign({ jobs: cronJobsData }, cronListMeta)));
      return;
    }

    if (pathname === NZ_CONTRACT.API.cron && req.method === 'POST') {
      if (!checkAuth()) return;
      let body = '';
      req.on('data', c => (body += c));
      req.on('end', () => {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ id: 'cron-new-001', ok: true }));
      });
      return;
    }

    if (pathname === NZ_CONTRACT.API.cron && req.method === 'PATCH') {
      if (!checkAuth()) return;
      let body = '';
      req.on('data', c => (body += c));
      req.on('end', () => {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ ok: true }));
      });
      return;
    }

    if (pathname === NZ_CONTRACT.API.cron && req.method === 'DELETE') {
      if (!checkAuth()) return;
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ ok: true }));
      return;
    }

    if (pathname === NZ_CONTRACT.API.cron_pause && req.method === 'POST') {
      if (!checkAuth()) return;
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ ok: true }));
      return;
    }

    if (pathname === NZ_CONTRACT.API.cron_resume && req.method === 'POST') {
      if (!checkAuth()) return;
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ ok: true }));
      return;
    }

    // /api/sessions/agent_events?key=&node=&task_id=&after=&limit=
    if (pathname === NZ_CONTRACT.API.sessions_agent_events && req.method === 'GET') {
      if (!checkAuth()) return;
      sendEntries(res, []);
      return;
    }

    // /api/sessions/workflow?key=&node=&task_id=[&rows=none | &since=&epoch=]
    if (pathname === NZ_CONTRACT.API.sessions_workflow && req.method === 'GET') {
      if (!checkAuth()) return;
      workflowCalls.push(Object.fromEntries(url.searchParams));
      const fx = workflows[url.searchParams.get('task_id') || ''];
      if (!fx) {
        res.writeHead(404, { 'Content-Type': 'text/plain' });
        res.end('unknown task');
        return;
      }
      const { status, body } = workflowReply(fx, url.searchParams);
      setTimeout(() => {
        res.writeHead(status, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify(body));
      }, workflowDelayMs);
      return;
    }

    // /api/cron/runs/<run_id>/snapshot?job_id=… — §7.3 输入快照面板。
    // 必须排在下面那条通配 /api/cron/runs/<id> 之前，否则 run_id 会被解析成
    // "<id>/snapshot" 而 404。
    if (pathname.startsWith('/api/cron/runs/') && pathname.endsWith('/snapshot') && req.method === 'GET') {
      if (!checkAuth()) return;
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ available: false }));
      return;
    }

    // /api/cron/runs/<run_id>?job_id=… — 单条 run 详情（PR-1 sheet 用）
    if (pathname.startsWith('/api/cron/runs/') && req.method === 'GET') {
      if (!checkAuth()) return;
      const runId = pathname.slice('/api/cron/runs/'.length);
      const jobId = url.searchParams.get('job_id') || '';
      const job = cronJobsData.find(j => j.id === jobId);
      const summary = job && Array.isArray(job.recent_runs)
        ? job.recent_runs.find(r => r.run_id === runId)
        : null;
      if (!summary) {
        res.writeHead(404, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ error: 'run not found' }));
        return;
      }
      // 构造详情：prompt + result + （失败时）error_msg
      const detail = {
        run_id: runId,
        job_id: jobId,
        state: summary.state,
        started_at: summary.started_at,
        ended_at: summary.ended_at,
        duration_ms: summary.duration_ms,
        trigger: summary.trigger,
        session_id: summary.session_id,
        prompt: job.prompt || '',
        work_dir: job.work_dir || '',
        fresh: false,
      };
      if (summary.state === 'succeeded') {
        detail.result = 'Task completed successfully.\nServer responded: 200 OK.\n(' + runId + ')';
        detail.result_bytes = detail.result.length;
      } else if (summary.state === 'failed') {
        detail.error_msg = 'connection refused: dial tcp 10.0.0.5:443: i/o timeout';
        detail.error_class = summary.error_class || 'network';
      } else if (summary.state === 'skipped') {
        detail.error_msg = 'previous run still in progress';
      }
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify(detail));
      return;
    }

    // /api/cron/runs?job_id=&limit=&before= — 翻页加载历史 runs (PR-1 加载更多)
    if (pathname === NZ_CONTRACT.API.cron_runs && req.method === 'GET') {
      if (!checkAuth()) return;
      const jobId = url.searchParams.get('job_id') || '';
      const job = cronJobsData.find(j => j.id === jobId);
      const runs = job && Array.isArray(job.recent_runs) ? job.recent_runs : [];
      res.writeHead(200, { 'Content-Type': 'application/json' });
      // 测试只用首页：next_before 缺失 → done
      res.end(JSON.stringify({ runs, next_before: 0 }));
      return;
    }

    // Project routes
    if (pathname === NZ_CONTRACT.API.projects && req.method === 'GET') {
      if (!checkAuth()) return;
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify(defaultProjects()));
      return;
    }
    if (pathname === NZ_CONTRACT.API.projects_favorite && req.method === 'POST') {
      if (!checkAuth()) return;
      const name = url.searchParams.get('name');
      const fav = url.searchParams.get('favorite') === 'true';
      favoriteCalls.push({ name, favorite: fav });
      // Update mock sessions so next poll reflects change.
      const projects = sessionsData.stats && sessionsData.stats.projects;
      if (Array.isArray(projects)) {
        for (const p of projects) {
          if (p.name === name) p.favorite = fav;
        }
        if (typeof sessionsData.stats.version === 'number') {
          sessionsData.stats.version++;
        }
      }
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ status: 'ok', favorite: fav }));
      return;
    }

    // Transcribe route
    if (pathname === NZ_CONTRACT.API.transcribe && req.method === 'POST') {
      if (!checkAuth()) return;
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ text: 'transcribed text' }));
      return;
    }

    // WebSocket — reject to force polling fallback
    if (pathname === '/ws') {
      res.writeHead(404);
      res.end();
      return;
    }

    // Default 404
    res.writeHead(404);
    res.end();
  });

  // Minimal in-process WebSocket server (opt-in via overrides.ws). Real
  // browsers send /ws as an HTTP Upgrade, which never reaches the request
  // handler above — without an 'upgrade' listener node just destroys the
  // socket and the dashboard falls back to polling (the default the whole
  // suite relies on). With ws:true we complete the RFC6455 handshake, answer
  // the dashboard's {type:'auth'} with auth_ok, record every client message,
  // and let the test push arbitrary frames — enough to cover the
  // connect → auth → onMessage dispatch path that polling tests never touch.
  const wsConnections = [];
  if (overrides.ws) {
    server.on('upgrade', (req, socket) => {
      if (new URL(req.url, 'http://x').pathname !== '/ws') { socket.destroy(); return; }
      const accept = crypto
        .createHash('sha1')
        .update(req.headers['sec-websocket-key'] + '258EAFA5-E914-47DA-95CA-C5AB0DC85B11')
        .digest('base64');
      socket.write(
        'HTTP/1.1 101 Switching Protocols\r\n' +
        'Upgrade: websocket\r\nConnection: Upgrade\r\n' +
        `Sec-WebSocket-Accept: ${accept}\r\n\r\n`
      );
      const conn = {
        messages: [],
        send(obj) {
          const payload = Buffer.from(JSON.stringify(obj));
          // RFC 6455 §5.2: a 7-bit length, or 126 + 16-bit, or 127 + 64-bit
          // (a 400-agent workflow frame passes 64 KiB).
          let head;
          if (payload.length < 126) {
            head = Buffer.from([0x81, payload.length]);
          } else if (payload.length < 0x10000) {
            head = Buffer.alloc(4);
            head.writeUInt16BE(payload.length, 2);
          } else {
            head = Buffer.alloc(10);
            head.writeBigUInt64BE(BigInt(payload.length), 2);
          }
          head[0] = 0x81;
          if (payload.length >= 126) head[1] = payload.length < 0x10000 ? 126 : 127;
          socket.write(Buffer.concat([head, payload]));
        },
        close() { socket.end(); },
      };
      wsConnections.push(conn);
      let buf = Buffer.alloc(0);
      socket.on('data', (chunk) => {
        buf = Buffer.concat([buf, chunk]);
        // Client frames are always masked; lengths in this suite stay < 64 KiB.
        for (;;) {
          if (buf.length < 2) return;
          const opcode = buf[0] & 0x0f;
          let len = buf[1] & 0x7f;
          let off = 2;
          if (len === 126) {
            if (buf.length < 4) return;
            len = buf.readUInt16BE(2);
            off = 4;
          }
          const masked = (buf[1] & 0x80) !== 0;
          const maskOff = off;
          if (masked) off += 4;
          if (buf.length < off + len) return;
          const payload = buf.subarray(off, off + len);
          if (masked) {
            for (let i = 0; i < payload.length; i++) payload[i] ^= buf[maskOff + (i % 4)];
          }
          buf = buf.subarray(off + len);
          if (opcode === 8) { socket.end(); return; }
          if (opcode !== 1) continue; // ignore ping/pong/binary
          let msg;
          try { msg = JSON.parse(payload.toString('utf8')); } catch { continue; }
          conn.messages.push(msg);
          if (msg.type === NZ_CONTRACT.WS.auth && !overrides.wsHoldAuth) {
            conn.send({ type: NZ_CONTRACT.WS.auth_ok });
          }
        }
      });
      socket.on('error', () => {});
    });
  }

  return new Promise((resolve) => {
    contractReady.then(() => server.listen(0, '127.0.0.1', () => {
      const { port } = server.address();
      resolve({
        server,
        port,
        url: `http://127.0.0.1:${port}`,
        get sendCalls() { return sendCalls; },
        get bindCalls() { return bindCalls; },
        get loginCalls() { return loginCalls; },
        get favoriteCalls() { return favoriteCalls; },
        get wsConnections() { return wsConnections; },
        get workflowCalls() { return workflowCalls; },
        resetCalls() { sendCalls = []; bindCalls = []; loginCalls = []; favoriteCalls = []; },
      });
    }));
  });
}

module.exports = { startMockServer, defaultSessions };
