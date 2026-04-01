// @ts-check
const { test, expect, devices } = require('@playwright/test');
const http = require('http');
const fs = require('fs');
const path = require('path');

// Minimal mock server: serves dashboard.html + stubs all API calls
function startMockServer() {
  const htmlPath = path.join(__dirname, 'internal/server/static/dashboard.html');
  const manifestPath = path.join(__dirname, 'internal/server/static/manifest.json');
  const html = fs.readFileSync(htmlPath, 'utf8');
  const manifest = fs.readFileSync(manifestPath, 'utf8');

  const mockSessions = {
    sessions: [
      {
        key: 'dashboard:direct:2026-01-01-120000-1:myproject',
        state: 'ready',
        platform: 'dashboard',
        agent: 'general',
        workspace: '/home/ec2-user/workspace/myproject',
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
        workspace: '/home/ec2-user/workspace/otherproject',
        last_active: Date.now() - 30000,
        last_prompt: 'review this',
        last_activity: 'reviewing code',
        node: 'local',
        project: 'otherproject',
      },
    ],
    stats: {
      total: 2,
      running: 1,
      ready: 1,
      active: 2,
      uptime: '1h00m00s',
      backend: 'cc',
      max_procs: 10,
      default_workspace: '/home/ec2-user/workspace',
      agents: ['general', 'reviewer'],
      projects: [],
    },
    nodes: { local: { display_name: 'Local', status: 'ok' } },
  };

  const server = http.createServer((req, res) => {
    if (req.url === '/dashboard') {
      res.writeHead(200, { 'Content-Type': 'text/html' });
      res.end(html);
    } else if (req.url === '/manifest.json') {
      res.writeHead(200, { 'Content-Type': 'application/manifest+json' });
      res.end(manifest);
    } else if (req.url === '/api/sessions') {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify(mockSessions));
    } else if (req.url && req.url.startsWith('/api/sessions/events')) {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify([
        { type: 'user', detail: 'hello world', time: Date.now() - 5000 },
        { type: 'text', detail: 'hi there!', time: Date.now() - 3000 },
      ]));
    } else if (req.url === '/api/discovered') {
      res.writeHead(200, { 'Content-Type': 'application/json' });
      res.end('[]');
    } else if (req.url === '/ws' || (req.url && req.url.startsWith('/ws'))) {
      // Reject WebSocket upgrade — dashboard falls back to polling
      res.writeHead(404);
      res.end();
    } else {
      res.writeHead(404);
      res.end();
    }
  });

  return new Promise((resolve) => {
    server.listen(0, '127.0.0.1', () => {
      const { port } = server.address();
      resolve({ server, port });
    });
  });
}

const iPhone = devices['iPhone 13'];
const desktop = { viewport: { width: 1280, height: 800 } };

test.describe('Mobile dashboard', () => {
  let mockServer;
  let baseURL;

  test.beforeAll(async () => {
    const { server, port } = await startMockServer();
    mockServer = server;
    baseURL = `http://127.0.0.1:${port}`;
  });

  test.afterAll(() => {
    mockServer.close();
  });

  test('on mobile: sidebar starts visible (list view)', async ({ browser }) => {
    const ctx = await browser.newContext({ ...iPhone });
    const page = await ctx.newPage();
    await page.goto(baseURL + '/dashboard');
    await page.waitForLoadState('networkidle');

    // body should have mobile-list-view class
    const bodyClass = await page.evaluate(() => document.body.className);
    expect(bodyClass).toContain('mobile-list-view');
    expect(bodyClass).not.toContain('mobile-chat-view');

    // Sidebar should be visible (translateX(0))
    const transform = await page.$eval('.sidebar', el =>
      getComputedStyle(el).transform
    );
    // translateX(0) = matrix(1, 0, 0, 1, 0, 0)
    expect(transform).toBe('matrix(1, 0, 0, 1, 0, 0)');

    await ctx.close();
  });

  test('on mobile: selecting a session switches to chat view', async ({ browser }) => {
    const ctx = await browser.newContext({ ...iPhone });
    const page = await ctx.newPage();
    await page.goto(baseURL + '/dashboard');
    await page.waitForLoadState('networkidle');
    // Wait for session cards to render
    await page.waitForSelector('.session-card');

    // Click the first session card
    await page.click('.session-card');
    // Wait for CSS transition (sidebar slides out over .25s)
    await page.waitForTimeout(350);

    // body should now have mobile-chat-view
    const bodyClass = await page.evaluate(() => document.body.className);
    expect(bodyClass).toContain('mobile-chat-view');
    expect(bodyClass).not.toContain('mobile-list-view');

    // Sidebar should be off-screen (translateX(-100%))
    const transform = await page.$eval('.sidebar', el =>
      getComputedStyle(el).transform
    );
    // translateX(-100%) renders as matrix(1,0,0,1,-<width>,0)
    expect(transform).not.toBe('matrix(1, 0, 0, 1, 0, 0)');

    // Back button should be visible in the main header
    const backVisible = await page.$eval('.btn-mobile-back', el =>
      getComputedStyle(el).display !== 'none'
    );
    expect(backVisible).toBe(true);

    await ctx.close();
  });

  test('on mobile: back button returns to session list', async ({ browser }) => {
    const ctx = await browser.newContext({ ...iPhone });
    const page = await ctx.newPage();
    await page.goto(baseURL + '/dashboard');
    await page.waitForLoadState('networkidle');
    await page.waitForSelector('.session-card');

    // Enter chat view
    await page.click('.session-card');
    await expect(page.locator('.btn-mobile-back')).toBeVisible();

    // Click back
    await page.click('.btn-mobile-back');

    const bodyClass = await page.evaluate(() => document.body.className);
    expect(bodyClass).toContain('mobile-list-view');
    expect(bodyClass).not.toContain('mobile-chat-view');

    await ctx.close();
  });

  test('on mobile: browser back (popstate) returns to session list', async ({ browser }) => {
    const ctx = await browser.newContext({ ...iPhone });
    const page = await ctx.newPage();
    await page.goto(baseURL + '/dashboard');
    await page.waitForLoadState('networkidle');
    await page.waitForSelector('.session-card');

    await page.click('.session-card');
    let bodyClass = await page.evaluate(() => document.body.className);
    expect(bodyClass).toContain('mobile-chat-view');

    // Simulate browser back button
    await page.goBack();

    bodyClass = await page.evaluate(() => document.body.className);
    expect(bodyClass).toContain('mobile-list-view');
    expect(bodyClass).not.toContain('mobile-chat-view');

    await ctx.close();
  });

  test('on mobile: dismiss button is visible without hover', async ({ browser }) => {
    const ctx = await browser.newContext({ ...iPhone });
    const page = await ctx.newPage();
    await page.goto(baseURL + '/dashboard');
    await page.waitForLoadState('networkidle');
    await page.waitForSelector('.session-card');

    const opacity = await page.$eval('.session-card .btn-dismiss', el =>
      parseFloat(getComputedStyle(el).opacity)
    );
    expect(opacity).toBeGreaterThan(0);

    await ctx.close();
  });

  test('on mobile: modal fits within screen width', async ({ browser }) => {
    const ctx = await browser.newContext({ ...iPhone });
    const page = await ctx.newPage();
    await page.goto(baseURL + '/dashboard');
    await page.waitForLoadState('networkidle');

    // Open new session modal
    await page.click('.new-session-btn');
    await page.waitForSelector('.modal');

    const modalWidth = await page.$eval('.modal', el => el.getBoundingClientRect().width);
    const viewportWidth = iPhone.viewport.width;
    expect(modalWidth).toBeLessThanOrEqual(viewportWidth);

    await ctx.close();
  });

  test('on mobile: toast appears at top of screen', async ({ browser }) => {
    const ctx = await browser.newContext({ ...iPhone });
    const page = await ctx.newPage();
    await page.goto(baseURL + '/dashboard');
    await page.waitForLoadState('networkidle');

    // Trigger a toast
    await page.evaluate(() => {
      const el = document.getElementById('toast');
      el.textContent = 'test message';
      el.classList.add('show');
    });

    const toastTop = await page.$eval('#toast', el => el.getBoundingClientRect().top);
    const toastBottom = await page.$eval('#toast', el => el.getBoundingClientRect().bottom);
    const viewportHeight = iPhone.viewport.height;

    // Toast should be in the top half of the screen
    expect(toastTop).toBeGreaterThanOrEqual(0);
    expect(toastBottom).toBeLessThan(viewportHeight / 2);

    await ctx.close();
  });

  test('on desktop: no mobile classes, sidebar is always visible', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(baseURL + '/dashboard');
    await page.waitForLoadState('networkidle');

    // No mobile classes on desktop
    const bodyClass = await page.evaluate(() => document.body.className);
    expect(bodyClass).not.toContain('mobile-list-view');
    expect(bodyClass).not.toContain('mobile-chat-view');

    // Select a session to make renderMainShell create the back button
    await page.waitForSelector('.session-card');
    await page.click('.session-card');
    // Wait for the element to exist in DOM (it's hidden on desktop, so use state:attached)
    await page.waitForSelector('.btn-mobile-back', { state: 'attached' });

    // Back button should not be visible on desktop (display:none)
    const backDisplay = await page.$eval('.btn-mobile-back', el =>
      getComputedStyle(el).display
    );
    expect(backDisplay).toBe('none');

    // No mobile classes even after selecting a session
    const bodyClassAfter = await page.evaluate(() => document.body.className);
    expect(bodyClassAfter).not.toContain('mobile-list-view');
    expect(bodyClassAfter).not.toContain('mobile-chat-view');

    await ctx.close();
  });

  test('manifest.json is served correctly', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    const response = await page.request.get(baseURL + '/manifest.json');
    expect(response.status()).toBe(200);
    const body = await response.json();
    expect(body.name).toBe('naozhi dashboard');
    expect(body.display).toBe('standalone');
    expect(body.start_url).toBe('/dashboard');
    await ctx.close();
  });
});
