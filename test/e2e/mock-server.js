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
        state: 'suspended',
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
        { name: 'myproject', path: '/home/user/workspace/myproject' },
        { name: 'otherproject', path: '/home/user/workspace/otherproject' },
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
      if (schedule) {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ valid: true, next_run: Date.now() + 3600000 }));
      } else {
        res.writeHead(200, { 'Content-Type': 'application/json' });
        res.end(JSON.stringify({ valid: false, error: 'empty schedule' }));
      }
      return;
    }

    // Project routes
    if (pathname === '/api/projects' && req.method === 'GET') {
      if (!checkAuth()) return;
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify(defaultProjects()));
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
        resetCalls() { sendCalls = []; cronCreateCalls = []; loginCalls = []; },
      });
    });
  });
}

module.exports = { startMockServer, defaultSessions, defaultEvents, defaultCronJobs };
