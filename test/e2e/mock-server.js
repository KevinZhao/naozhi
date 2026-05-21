// @ts-check
// Shared mock server for naozhi dashboard E2E tests.
// Serves dashboard.html and stubs all API endpoints with configurable responses.
const http = require('http');
const fs = require('fs');
const path = require('path');

const STATIC_DIR = path.join(__dirname, '..', '..', 'internal', 'server', 'static');

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
        active_tools: ['Read', 'Grep'],
        active_agents: [{ name: 'code-reviewer', activity: 'reading files' }],
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
        { name: 'myproject', path: '/home/user/workspace/myproject', favorite: false, github: true, git_remote_url: 'https://github.com/acme/myproject.git' },
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
    { type: 'system', summary: 'session started', time: Date.now() - 10000 },
    { type: 'user', detail: 'hello world', time: Date.now() - 8000 },
    {
      type: 'text',
      detail: 'Hi! Here is some **bold** and `inline code`.\n\n```javascript\nconsole.log("hello");\n```\n\nAnd a table:\n\n| Col A | Col B |\n|-------|-------|\n| 1     | 2     |',
      time: Date.now() - 5000,
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

/**
 * Start a mock HTTP server.
 * @param {object} [overrides] - Override specific route handlers.
 * @param {object} [overrides.sessions] - Custom sessions response.
 * @param {object[]} [overrides.events] - Custom events response.
 * @param {object[]} [overrides.cronJobs] - Custom cron jobs response.
 * @param {boolean} [overrides.requireAuth] - If true, require bearer token.
 * @param {string} [overrides.authToken] - Expected token value.
 * @param {Function} [overrides.onSend] - Callback when POST /api/sessions/send is called.
 * @param {Function} [overrides.onCronCreate] - Callback when POST /api/cron is called.
 * @param {number} [overrides.sendStatus] - Status code for POST /api/sessions/send.
 * @returns {Promise<{server: http.Server, port: number, url: string}>}
 */
function startMockServer(overrides = {}) {
  const html = fs.readFileSync(path.join(STATIC_DIR, 'dashboard.html'), 'utf8');
  const manifest = fs.readFileSync(path.join(STATIC_DIR, 'manifest.json'), 'utf8');

  const sessionsData = overrides.sessions || defaultSessions();
  const eventsData = overrides.events || defaultEvents();
  const cronJobsData = overrides.cronJobs || defaultCronJobs();
  const requireAuth = overrides.requireAuth || false;
  const authToken = overrides.authToken || 'test-token-123';

  let sendCalls = [];
  let cronCreateCalls = [];
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

    // Static routes
    if (pathname === '/dashboard') {
      res.writeHead(200, { 'Content-Type': 'text/html' });
      res.end(html);
      return;
    }
    if (pathname === '/manifest.json') {
      res.writeHead(200, { 'Content-Type': 'application/manifest+json' });
      res.end(manifest);
      return;
    }
    if (pathname === '/static/dashboard.js') {
      try {
        const js = fs.readFileSync(path.join(STATIC_DIR, 'dashboard.js'));
        res.writeHead(200, { 'Content-Type': 'application/javascript' });
        res.end(js);
      } catch {
        res.writeHead(404);
        res.end();
      }
      return;
    }
    // dashboard.html 末尾还引用 agent_view.js — 不伺服会触发 global-error
    // toast "页面遇到异常: [object Object]" 干扰 e2e 视觉截图。
    if (pathname === '/static/agent_view.js') {
      try {
        const js = fs.readFileSync(path.join(STATIC_DIR, 'agent_view.js'));
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
    if (pathname === '/api/auth/login' && req.method === 'POST') {
      let body = '';
      req.on('data', c => (body += c));
      req.on('end', () => {
        loginCalls.push(body);
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

    if (pathname === '/api/auth/logout' && req.method === 'POST') {
      res.writeHead(200, {
        'Content-Type': 'application/json',
        'Set-Cookie': 'naozhi_auth=; Path=/; HttpOnly; Max-Age=0',
      });
      res.end(JSON.stringify({ ok: true }));
      return;
    }

    // Session routes
    if (pathname === '/api/sessions' && req.method === 'GET') {
      if (!checkAuth()) return;
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify(sessionsData));
      return;
    }

    if (pathname === '/api/sessions/events' && req.method === 'GET') {
      if (!checkAuth()) return;
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify(eventsData));
      return;
    }

    if (pathname === '/api/sessions/send' && req.method === 'POST') {
      if (!checkAuth()) return;
      const status = overrides.sendStatus || 200;
      let body = '';
      req.on('data', c => (body += c));
      req.on('end', () => {
        sendCalls.push(body);
        if (overrides.onSend) overrides.onSend(body);
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

    if (pathname === '/api/sessions' && req.method === 'DELETE') {
      if (!checkAuth()) return;
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ ok: true }));
      return;
    }

    if (pathname === '/api/sessions/resume' && req.method === 'POST') {
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
    if (pathname === '/api/discovered' && req.method === 'GET') {
      if (!checkAuth()) return;
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end('[]');
      return;
    }

    // Cron routes
    if (pathname === '/api/cron' && req.method === 'GET') {
      if (!checkAuth()) return;
      res.writeHead(200, { 'Content-Type': 'application/json' });
      // Dashboard expects { jobs: [...] } format
      res.end(JSON.stringify({ jobs: cronJobsData }));
      return;
    }

    if (pathname === '/api/cron' && req.method === 'POST') {
      if (!checkAuth()) return;
      let body = '';
      req.on('data', c => (body += c));
      req.on('end', () => {
        cronCreateCalls.push(body);
        if (overrides.onCronCreate) overrides.onCronCreate(body);
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ id: 'cron-new-001', ok: true }));
      });
      return;
    }

    if (pathname === '/api/cron' && req.method === 'DELETE') {
      if (!checkAuth()) return;
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ ok: true }));
      return;
    }

    if (pathname === '/api/cron/pause' && req.method === 'POST') {
      if (!checkAuth()) return;
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ ok: true }));
      return;
    }

    if (pathname === '/api/cron/resume' && req.method === 'POST') {
      if (!checkAuth()) return;
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ ok: true }));
      return;
    }

    if (pathname === '/api/cron/preview' && req.method === 'GET') {
      if (!checkAuth()) return;
      const schedule = url.searchParams.get('schedule') || '';
      const count = Math.min(Math.max(parseInt(url.searchParams.get('count') || '1', 10), 1), 10);
      if (schedule) {
        const base = Date.now() + 3600000;
        const runs = Array.from({ length: count }, (_, i) => base + i * 3600000);
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ valid: true, next_run: runs[0], next_runs: runs, timezone: 'UTC', timezone_label: 'UTC (UTC+00:00)' }));
      } else {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ valid: false, error: 'empty schedule' }));
      }
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
    if (pathname === '/api/cron/runs' && req.method === 'GET') {
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
    if (pathname === '/api/projects' && req.method === 'GET') {
      if (!checkAuth()) return;
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify(defaultProjects()));
      return;
    }
    if (pathname === '/api/projects/favorite' && req.method === 'POST') {
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
    if (pathname === '/api/transcribe' && req.method === 'POST') {
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

  return new Promise((resolve) => {
    server.listen(0, '127.0.0.1', () => {
      const { port } = server.address();
      resolve({
        server,
        port,
        url: `http://127.0.0.1:${port}`,
        get sendCalls() { return sendCalls; },
        get cronCreateCalls() { return cronCreateCalls; },
        get loginCalls() { return loginCalls; },
        get favoriteCalls() { return favoriteCalls; },
        resetCalls() { sendCalls = []; cronCreateCalls = []; loginCalls = []; favoriteCalls = []; },
      });
    });
  });
}

module.exports = { startMockServer, defaultSessions, defaultEvents, defaultCronJobs };
