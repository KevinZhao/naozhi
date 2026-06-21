// release-gate.mjs — post-deploy screenshot gate for naozhi's dashboard.
//
// Drives a running naozhi instance with Playwright, captures screenshots of
// every top-level view, and asserts on three signals per view:
//   1. navigation succeeded (no thrown error / no HTTP failure)
//   2. a known load-bearing selector for that view is visible
//   3. zero console.error / pageerror accumulated up to that point
// Plus a /health probe that must report status:ok before any view runs.
//
// Unlike scripts/dashboard-screenshots.js (a best-effort capture tool that
// keeps going on per-step failure), THIS script is a gate: any failed
// assertion makes it exit non-zero so a release pipeline can block on it.
// Screenshots are still written for archival/visual inspection.
//
// Usage:
//   NAOZHI_DASHBOARD_TOKEN=... node scripts/release-gate.mjs
//
// Env vars:
//   NAOZHI_DASHBOARD_TOKEN  required — login token, same as config.yaml
//   NAOZHI_BASE_URL         optional — default http://127.0.0.1:8180
//   NAOZHI_SHOTS_DIR        optional — default tmp/release-gate (repo-relative)
//   NAOZHI_GATE_TIMEOUT_MS  optional — health-poll budget, default 30000
//   NAOZHI_GATE_ALLOW_CONSOLE_ERRORS  optional — "1" downgrades console.error
//                           assertions to warnings (still captured to file)
//
// Exit codes:
//   0 all views passed · 1 missing token · 2 health/login failure
//   3 playwright not installed / fatal launch error · 4 one or more views failed

import fs from 'node:fs';
import path from 'node:path';
import url from 'node:url';
import { createRequire } from 'node:module';

// Playwright lives in test/e2e/node_modules (that is where CI runs `npm install`),
// not at the repo root. ESM bare-specifier import ignores NODE_PATH and only
// walks up from this file, so resolve explicitly against the e2e package via
// createRequire. NAOZHI_PLAYWRIGHT_DIR overrides the base for unusual layouts.
const SCRIPT_DIR = path.dirname(url.fileURLToPath(import.meta.url));
const REPO_ROOT = path.resolve(SCRIPT_DIR, '..');
const PW_BASE = process.env.NAOZHI_PLAYWRIGHT_DIR
  ? path.join(process.env.NAOZHI_PLAYWRIGHT_DIR, 'package.json')
  : path.join(REPO_ROOT, 'test', 'e2e', 'package.json');

let chromium;
try {
  const requireFromE2E = createRequire(PW_BASE);
  let mod;
  try {
    mod = requireFromE2E('@playwright/test');
  } catch (_) {
    mod = requireFromE2E('playwright');
  }
  ({ chromium } = mod);
  if (!chromium) throw new Error('module loaded but chromium export missing');
} catch (e) {
  console.error('[fatal] playwright not resolvable from test/e2e. Run: (cd test/e2e && npm install)');
  console.error('        detail:', e.message);
  process.exit(3);
}

const BASE = (process.env.NAOZHI_BASE_URL || 'http://127.0.0.1:8180').replace(/\/$/, '');
const TOKEN = process.env.NAOZHI_DASHBOARD_TOKEN;
const OUT = path.resolve(process.env.NAOZHI_SHOTS_DIR || 'tmp/release-gate');
const HEALTH_BUDGET_MS = Number(process.env.NAOZHI_GATE_TIMEOUT_MS || 30000);
const ALLOW_CONSOLE_ERRORS = process.env.NAOZHI_GATE_ALLOW_CONSOLE_ERRORS === '1';

if (!TOKEN) {
  console.error('[fatal] NAOZHI_DASHBOARD_TOKEN not set.');
  process.exit(1);
}

fs.mkdirSync(OUT, { recursive: true });

const DESKTOP = { width: 1440, height: 900 };
const MOBILE = { width: 390, height: 844 };

// Console messages that are noise rather than real failures. Keep this list
// tight and documented — every entry weakens the gate.
// Console noise tolerated on EVERY view. Keep tight — each entry weakens the
// gate's ability to catch real console-error regressions.
const IGNORED_CONSOLE = [
  /Failed to load resource.*manifest\.json/i, // PWA manifest is optional in headless
  /\[HMR\]/i,
  // Browser doesn't recognize the deprecated `require-sri-for` CSP directive
  // naozhi still emits; it prints on every page load and is not a regression.
  /Unrecognized Content-Security-Policy directive 'require-sri-for'/i,
];

// Console noise tolerated ONLY on the named view. The server-rendered login
// page has one inline <style> its strict CSP (style-src 'sha256-...') blocks;
// scoping the allowance to the login view keeps a style-src violation on any
// OTHER view a hard failure.
const IGNORED_CONSOLE_PER_VIEW = {
  login: [/Applying inline style violates the following Content Security Policy directive 'style-src/i],
};

// Each view: screenshot file, viewport, the action that navigates to it, and
// the selector that proves the view actually rendered (not just a blank shell).
const VIEWS = [
  {
    name: 'login',
    file: '01-login.png',
    viewport: DESKTOP,
    auth: false,
    full: true,
    selector: '#token',
    act: async (page) => {
      await page.goto(BASE + '/dashboard', { waitUntil: 'domcontentloaded' });
    },
  },
  {
    name: 'chat-desktop',
    file: '02-chat-desktop.png',
    viewport: DESKTOP,
    full: false,
    selector: '#session-list',
    act: async (page) => {
      await page.goto(BASE + '/dashboard', { waitUntil: 'networkidle' });
      await page.waitForSelector('#session-list', { timeout: 8000 });
    },
  },
  {
    name: 'chat-mobile',
    file: '03-chat-mobile.png',
    viewport: MOBILE,
    full: false,
    selector: '#sidebar',
    act: async (page) => {
      await page.goto(BASE + '/dashboard', { waitUntil: 'networkidle' });
      await page.waitForSelector('#sidebar', { timeout: 8000 });
    },
  },
  {
    name: 'assets',
    file: '04-assets.png',
    viewport: DESKTOP,
    full: false,
    selector: '#asset-main',
    act: async (page) => {
      await page.goto(BASE + '/dashboard', { waitUntil: 'networkidle' });
      await page.locator('button#abnav-assets').click({ timeout: 5000 });
      await page.waitForTimeout(500);
    },
  },
  {
    name: 'cron',
    file: '05-cron.png',
    viewport: DESKTOP,
    full: false,
    // The <main> shells (#cron-main etc.) flip to display:flex the instant
    // their nav class lands, so asserting the container alone passes even if
    // JS render throws and leaves it empty. minChildren proves content
    // actually mounted.
    selector: '#cron-main',
    minChildren: 1,
    act: async (page) => {
      await page.goto(BASE + '/dashboard', { waitUntil: 'networkidle' });
      await page.locator('button#abnav-cron').click({ timeout: 5000 });
      await page.waitForSelector('#cron-main > *', { state: 'attached', timeout: 8000 });
    },
  },
  {
    name: 'system',
    file: '06-system.png',
    viewport: DESKTOP,
    full: false,
    selector: '#system-main',
    minChildren: 1,
    act: async (page) => {
      await page.goto(BASE + '/dashboard', { waitUntil: 'networkidle' });
      await page.locator('button#abnav-system').click({ timeout: 5000 });
      await page.waitForSelector('#system-main > *', { state: 'attached', timeout: 8000 });
    },
  },
  {
    name: 'settings',
    file: '07-settings.png',
    viewport: DESKTOP,
    full: false,
    selector: '#settings-main',
    minChildren: 1,
    act: async (page) => {
      await page.goto(BASE + '/dashboard', { waitUntil: 'networkidle' });
      await page.locator('button#abnav-settings').click({ timeout: 5000 });
      await page.waitForSelector('#settings-main > *', { state: 'attached', timeout: 8000 });
    },
  },
  {
    name: 'history',
    file: '08-history.png',
    viewport: DESKTOP,
    full: false,
    // Assert the popover toggleHistory() appends to <body>, NOT the #btn-history
    // trigger (which is always present and would make the assertion near-vacuous).
    selector: '.history-popover',
    act: async (page) => {
      await page.goto(BASE + '/dashboard', { waitUntil: 'networkidle' });
      await page.locator('button#btn-history').click({ timeout: 5000 });
      await page.waitForSelector('.history-popover', { state: 'visible', timeout: 8000 });
    },
  },
  {
    name: 'new-session',
    file: '09-new-session.png',
    viewport: DESKTOP,
    full: false,
    // createNewSession() appends a .modal-overlay (no projects) or opens a
    // .cmd-palette-overlay (project palette). The gate instance has no
    // projects, so the modal path renders — assert the produced dialog, not
    // the #btn-new-session trigger.
    selector: '.modal-overlay, .cmd-palette-overlay',
    act: async (page) => {
      await page.goto(BASE + '/dashboard', { waitUntil: 'networkidle' });
      await page.locator('button#btn-new-session').click({ timeout: 5000 });
      await page.waitForSelector('.modal-overlay, .cmd-palette-overlay', { state: 'visible', timeout: 8000 });
    },
  },
];

function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms));
}

// Poll /health until it reports status:ok or the budget is exhausted. A gate
// that screenshots a half-booted server produces misleading failures, so we
// refuse to start until the instance is demonstrably up.
async function waitForHealth(request) {
  const deadline = Date.now() + HEALTH_BUDGET_MS;
  let lastErr = 'never responded';
  while (Date.now() < deadline) {
    try {
      const resp = await request.get(BASE + '/health', { timeout: 5000 });
      if (resp.ok()) {
        const body = await resp.json().catch(() => ({}));
        if (body.status === 'ok') return body;
        lastErr = `status=${body.status}`;
      } else {
        lastErr = `http ${resp.status()}`;
      }
    } catch (e) {
      lastErr = e.message;
    }
    await sleep(1000);
  }
  throw new Error(`/health not ok within ${HEALTH_BUDGET_MS}ms (last: ${lastErr})`);
}

async function login(request) {
  const resp = await request.post(BASE + '/api/auth/login', {
    headers: { 'Content-Type': 'application/json' },
    data: { token: TOKEN },
    timeout: 5000,
  });
  if (!resp.ok()) {
    throw new Error(`login failed: ${resp.status()} ${await resp.text()}`);
  }
}

function isIgnored(text, viewName) {
  if (IGNORED_CONSOLE.some((re) => re.test(text))) return true;
  const perView = IGNORED_CONSOLE_PER_VIEW[viewName] || [];
  return perView.some((re) => re.test(text));
}

(async () => {
  let browser;
  try {
    browser = await chromium.launch({ headless: true });
  } catch (e) {
    console.error('[fatal] chromium launch failed:', e.message);
    process.exit(3);
  }

  const ctx = await browser.newContext({ viewport: DESKTOP });

  // Health + login are hard preconditions: fail them and the gate cannot
  // produce a meaningful verdict, so exit 2 (distinct from a view failure).
  try {
    const health = await waitForHealth(ctx.request);
    console.log(`[health] ok · uptime=${health.uptime || '?'} version=${health.version || '?'}`);
    await login(ctx.request);
    console.log('[auth] login ok');
  } catch (e) {
    console.error('[fatal]', e.message);
    await browser.close();
    process.exit(2);
  }

  const page = await ctx.newPage();
  // Surface unhandled promise rejections as pageerror — Playwright does NOT
  // report them otherwise, so an async handler (e.g. createNewSession's
  // fetchCLIBackends().then chain) could reject silently and leave a broken
  // view that still "looks" fine. Routing them here makes them gate failures.
  await page.addInitScript(() => {
    window.addEventListener('unhandledrejection', (ev) => {
      const r = ev && ev.reason;
      const msg = r && r.message ? r.message : String(r);
      // Re-thrown on the microtask queue so Playwright's pageerror sees it.
      setTimeout(() => { throw new Error('unhandledrejection: ' + msg); });
    });
  });

  let currentView = null;
  const consoleErrors = [];
  const allLogs = [];
  page.on('console', (m) => {
    const line = `[${m.type()}] ${m.text()}`;
    allLogs.push(line);
    if (m.type() === 'error' && !isIgnored(m.text(), currentView)) consoleErrors.push(line);
  });
  page.on('pageerror', (e) => {
    const line = `[pageerror] ${e.message}`;
    allLogs.push(line);
    if (!isIgnored(e.message, currentView)) consoleErrors.push(line);
  });

  const failures = [];
  for (const view of VIEWS) {
    currentView = view.name;
    const errBefore = consoleErrors.length;
    await page.setViewportSize(view.viewport);
    try {
      // The login view must be unauthenticated; clear cookies for it and
      // restore them right after so later views stay logged in.
      if (view.auth === false) await ctx.clearCookies();
      await view.act(page);
      if (view.auth === false) await login(ctx.request);

      const visible = await page
        .locator(view.selector)
        .first()
        .isVisible()
        .catch(() => false);
      if (!visible) {
        failures.push(`${view.name}: selector "${view.selector}" not visible`);
      } else if (view.minChildren) {
        // Container is visible — confirm it actually mounted content rather
        // than being an empty shell that flipped display:flex.
        const childCount = await page
          .locator(view.selector)
          .first()
          .evaluate((el) => el.childElementCount)
          .catch(() => 0);
        if (childCount < view.minChildren) {
          failures.push(`${view.name}: "${view.selector}" has ${childCount} children (want ≥${view.minChildren}) — empty shell`);
        }
      }

      // Let any microtask-queued unhandledrejection surface before we read.
      await page.waitForTimeout(50);
      const newErrors = consoleErrors.slice(errBefore);
      if (newErrors.length && !ALLOW_CONSOLE_ERRORS) {
        failures.push(`${view.name}: ${newErrors.length} console error(s): ${newErrors[0]}`);
      } else if (newErrors.length) {
        console.warn(`[warn] ${view.name}: ${newErrors.length} console error(s) (ignored by flag)`);
      }

      await page.screenshot({ path: path.join(OUT, view.file), fullPage: !!view.full });
      const status = failures.length && failures[failures.length - 1].startsWith(view.name) ? 'FAIL' : 'ok';
      console.log(`[view] ${view.name} ${status} → ${view.file}`);
    } catch (e) {
      failures.push(`${view.name}: ${e.message}`);
      console.log(`[view] ${view.name} FAIL → ${e.message}`);
      // Still attempt a screenshot for post-mortem.
      await page.screenshot({ path: path.join(OUT, view.file), fullPage: !!view.full }).catch(() => {});
    }
  }

  fs.writeFileSync(path.join(OUT, 'console.log'), allLogs.join('\n'));
  await browser.close();

  console.log('');
  if (failures.length) {
    console.error(`[gate] FAILED · ${failures.length} issue(s):`);
    for (const f of failures) console.error('  ✗ ' + f);
    console.error(`[gate] screenshots + console.log in ${OUT}`);
    process.exit(4);
  }
  console.log(`[gate] PASSED · ${VIEWS.length} views · screenshots in ${OUT}`);
})().catch((e) => {
  console.error('[fatal]', e);
  process.exit(3);
});
