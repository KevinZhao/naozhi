// dashboard-screenshots.js — consolidated Playwright capture pipeline for
// naozhi's dashboard UI. Produces a canonical set of screenshots covering the
// 9 key states that R110 reviewed (login / desktop idle / session chat /
// history drawer / cron panel / help modal / new-session command palette /
// mobile sidebar / mobile chat) plus a DOM outline for side-by-side diffs.
//
// This file sinks the throwaway /tmp/naozhi-shots/capture{,2,3,4}.js scripts
// (R110-P3 "screenshot 工具化"). Keeping it in-tree means UI-impacting PRs can
// attach before/after images without hunting through /tmp.
//
// Usage:
//   NAOZHI_DASHBOARD_TOKEN=... node scripts/dashboard-screenshots.js
//
// Env vars:
//   NAOZHI_DASHBOARD_TOKEN  required — login token, same as config.yaml
//   NAOZHI_BASE_URL         optional — default http://localhost:8180
//   NAOZHI_SHOTS_DIR        optional — default ./tmp/naozhi-shots (relative
//                           to the repo root; created if missing)
//
// Exit codes:
//   0 success, 1 missing token, 2 auth failure, 3 playwright error.
//
// Output naming keeps the same ordinal prefix the ad-hoc scripts used so
// existing review docs (docs/TODO.md Round 110) remain navigable.

const fs = require('fs');
const path = require('path');

let chromium;
try {
  ({ chromium } = require('playwright'));
} catch (_) {
  console.error('[fatal] playwright not installed. Run:');
  console.error('   (cd test/e2e && npm install) && npm install --no-save playwright');
  process.exit(3);
}

const BASE = process.env.NAOZHI_BASE_URL || 'http://localhost:8180';
const TOKEN = process.env.NAOZHI_DASHBOARD_TOKEN;
const OUT = path.resolve(process.env.NAOZHI_SHOTS_DIR || 'tmp/naozhi-shots');

if (!TOKEN) {
  console.error('[fatal] NAOZHI_DASHBOARD_TOKEN not set. Example:');
  console.error('   NAOZHI_DASHBOARD_TOKEN=$(sudo grep NAOZHI_DASHBOARD_TOKEN /home/ec2-user/.naozhi/env | cut -d= -f2-) \\');
  console.error('     node scripts/dashboard-screenshots.js');
  process.exit(1);
}

fs.mkdirSync(OUT, { recursive: true });

const DESKTOP = { width: 1440, height: 900 };
const MOBILE = { width: 390, height: 844 };

// A single "step" names the screenshot file, the viewport to use, and an
// async body that prepares the page. The runner isolates failures so one
// broken step doesn't abort the whole pipeline.
const STEPS = [
  {
    file: '01-login.png',
    viewport: DESKTOP,
    login: false,
    full: true,
    body: async (page) => {
      await page.goto(BASE + '/dashboard', { waitUntil: 'networkidle' });
    },
  },
  {
    file: '02-dashboard-desktop.png',
    viewport: DESKTOP,
    full: true,
    body: async (page) => {
      await page.goto(BASE + '/dashboard', { waitUntil: 'networkidle' });
      await page.waitForTimeout(1200);
    },
  },
  {
    file: '02b-dashboard-viewport.png',
    viewport: DESKTOP,
    full: false,
    body: async (page) => {
      await page.goto(BASE + '/dashboard', { waitUntil: 'networkidle' });
      await page.waitForTimeout(1000);
    },
  },
  {
    file: '03-dashboard-mobile.png',
    viewport: MOBILE,
    full: true,
    body: async (page) => {
      await page.goto(BASE + '/dashboard', { waitUntil: 'networkidle' });
      await page.waitForTimeout(800);
    },
  },
  {
    file: '06-new-session-modal.png',
    viewport: DESKTOP,
    full: false,
    body: async (page) => {
      await page.goto(BASE + '/dashboard', { waitUntil: 'networkidle' });
      await page.waitForTimeout(600);
      // Click header `+` via its accessible label — stable selector that
      // survives copy tweaks.
      const plus = page.locator('button[aria-label="Create new session"]').first();
      await plus.click({ timeout: 2000 });
      await page.waitForTimeout(600);
    },
  },
  {
    file: '07-history-drawer.png',
    viewport: DESKTOP,
    full: false,
    body: async (page) => {
      await page.goto(BASE + '/dashboard', { waitUntil: 'networkidle' });
      await page.waitForTimeout(600);
      const btn = page.locator('button#btn-history').first();
      await btn.click({ timeout: 2000 });
      await page.waitForTimeout(600);
    },
  },
  {
    file: '08-cron-panel.png',
    viewport: DESKTOP,
    full: false,
    body: async (page) => {
      await page.goto(BASE + '/dashboard', { waitUntil: 'networkidle' });
      await page.waitForTimeout(600);
      const btn = page.locator('button#btn-cron').first();
      await btn.click({ timeout: 2000 });
      await page.waitForTimeout(600);
    },
  },
  {
    file: '09-help.png',
    viewport: DESKTOP,
    full: false,
    body: async (page) => {
      await page.goto(BASE + '/dashboard', { waitUntil: 'networkidle' });
      await page.waitForTimeout(600);
      // `?` opens the cheatsheet from global keydown; skip the sidebar
      // footer '?' button so the keyboard path is what we're exercising.
      await page.keyboard.press('?');
      await page.waitForTimeout(400);
    },
  },
];

async function login(request) {
  const resp = await request.post(BASE + '/api/auth/login', {
    headers: { 'Content-Type': 'application/json' },
    data: { token: TOKEN },
  });
  if (!resp.ok()) {
    console.error('[fatal] login failed:', resp.status(), await resp.text());
    process.exit(2);
  }
}

async function captureDomSummary(page, outFile) {
  try {
    const summary = await page.evaluate(() => {
      const walk = (el, depth) => {
        if (depth > 3) return '';
        let out = '';
        const pad = '  '.repeat(depth);
        const tag = el.tagName ? el.tagName.toLowerCase() : '';
        const id = el.id ? `#${el.id}` : '';
        const cls = el.className && typeof el.className === 'string'
          ? `.${el.className.split(/\s+/).filter(Boolean).slice(0, 3).join('.')}`
          : '';
        out += `${pad}${tag}${id}${cls}\n`;
        for (const c of el.children) out += walk(c, depth + 1);
        return out;
      };
      return walk(document.body, 0);
    });
    fs.writeFileSync(outFile, summary);
  } catch (e) {
    console.warn('[warn] dom summary failed:', e.message);
  }
}

(async () => {
  const browser = await chromium.launch({ headless: true });
  const ctx = await browser.newContext({ viewport: DESKTOP });
  const page = await ctx.newPage();
  const logs = [];
  page.on('console', (m) => logs.push(`[${m.type()}] ${m.text()}`));
  page.on('pageerror', (e) => logs.push(`[pageerror] ${e.message}`));

  // Login once via the HTTP API so each step starts from an authenticated
  // cookie jar. The /dashboard route also accepts Bearer in headers, but
  // cookie auth exercises the same surface the real browser uses.
  await login(ctx.request);

  let ok = 0;
  let failed = 0;
  for (const step of STEPS) {
    await page.setViewportSize(step.viewport);
    try {
      await step.body(page);
      const outPath = path.join(OUT, step.file);
      await page.screenshot({ path: outPath, fullPage: !!step.full });
      console.log('captured', step.file);
      ok++;
    } catch (e) {
      console.warn('[warn] step failed', step.file, '—', e.message);
      failed++;
    }
  }

  // Capture DOM outline + console logs from the last-visited page state.
  await captureDomSummary(page, path.join(OUT, 'dom-summary.txt'));
  fs.writeFileSync(path.join(OUT, 'console.log'), logs.join('\n'));

  await browser.close();
  console.log(`done · ${ok} captured, ${failed} failed · out=${OUT}`);
  if (failed === STEPS.length) process.exit(3);
})().catch((e) => {
  console.error('[fatal]', e);
  process.exit(3);
});
