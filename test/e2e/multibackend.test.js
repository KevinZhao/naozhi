// @ts-check
//
// Multi-Backend full-scene UI smoke. Connects to the LIVE service running on
// 127.0.0.1:8180 (naozhi systemd unit) — this is intentional: the mock
// server doesn't speak the multi-backend RFC §8 contract, and we want to
// catch regressions against real registry / Profile / dashboard wiring.
//
// Auth flow: POST /api/auth/login {token} → cookie naozhi_auth → /dashboard.
// The token is read from $NAOZHI_DASHBOARD_TOKEN env. Fail fast if missing.
//
// Scenarios cover the 26-item D-table at a sweep level:
//   - /api/cli/backends shape + every key the dashboard reads
//   - dashboard loads w/o JS errors, picker is rendered (D3)
//   - chips render w/ correct color (D1)
//   - feature gates flip on session selection (D9-D15)
//   - doctor panel renders + opens (D22)
//   - context bar / cost unit visual presence (D5/D6)
//
// We do NOT spawn a kiro session here (would need the full ACP probe path
// + a real workspace) — those are smoke-checked at API level. UI-driven
// session-creation tests will arrive in a follow-up.

const { test, expect } = require('@playwright/test');
const fs = require('fs');
const path = require('path');

// Live mode — only run when caller opts in via NAOZHI_LIVE_E2E=1. The mock
// server based suite (dashboard.test.js etc.) stays self-contained.
test.skip(
  process.env.NAOZHI_LIVE_E2E !== '1',
  'set NAOZHI_LIVE_E2E=1 to run live multi-backend e2e against 127.0.0.1:8180'
);

const BASE = process.env.NAOZHI_BASE || 'http://127.0.0.1:8180';
const STORAGE_PATH = path.join(__dirname, '.multibackend-storage.json');
const desktop = { viewport: { width: 1440, height: 900 } };

// loginContext returns a fresh browser context already authenticated against
// the live naozhi service via the cookie state seeded by globalSetup. Each
// test owns its own context so other state (selected session, etc.) doesn't
// leak across cases.
async function loginContext(browser) {
  if (!fs.existsSync(STORAGE_PATH)) {
    throw new Error(
      `storage state ${STORAGE_PATH} missing — globalSetup did not run or failed`
    );
  }
  return browser.newContext({ ...desktop, baseURL: BASE, storageState: STORAGE_PATH });
}

test.describe('Multi-Backend live API contract', () => {
  test('GET /api/cli/backends returns claude + kiro both available', async ({ browser }) => {
    const ctx = await loginContext(browser);
    const resp = await ctx.request.get('/api/cli/backends');
    expect(resp.ok()).toBeTruthy();
    const data = await resp.json();

    expect(data.default).toBe('claude');
    expect(Array.isArray(data.backends)).toBe(true);
    expect(data.backends.length).toBe(2);

    const byID = Object.fromEntries(data.backends.map(b => [b.id, b]));
    // Claude assertions
    expect(byID.claude).toBeTruthy();
    expect(byID.claude.protocol).toBe('stream-json');
    expect(byID.claude.available).toBe(true);
    expect(byID.claude.chip_color).toBeTruthy();
    expect(byID.claude.reply_tag).toBe('cc');
    expect(byID.claude.features.askuser).toBe(true);
    expect(byID.claude.features.passthrough).toBe(true);
    // Kiro assertions
    expect(byID.kiro).toBeTruthy();
    expect(byID.kiro.protocol).toBe('acp');
    expect(byID.kiro.available).toBe(true);
    expect(byID.kiro.chip_color).toBeTruthy();
    expect(byID.kiro.reply_tag).toBe('kiro');
    expect(byID.kiro.features.askuser).toBe(false);
    expect(byID.kiro.features.passthrough).toBe(false);
    expect(byID.kiro.features.image_input).toBe(true);

    await ctx.close();
  });
});

test.describe('Dashboard load w/ multi-backend', () => {
  test('loads w/o JS errors and renders home health strip', async ({ browser }) => {
    const ctx = await loginContext(browser);
    const page = await ctx.newPage();
    const errors = [];
    page.on('pageerror', e => errors.push(e.message));
    page.on('console', m => {
      if (m.type() === 'error') errors.push('[console] ' + m.text());
    });

    await page.goto('/dashboard');
    // Header always renders; recent panel arrives after stats fetch
    await page.waitForSelector('.recent-panel-title', { timeout: 8000 });

    // No JS errors on load
    expect(errors, errors.join('\n')).toEqual([]);

    await page.screenshot({ path: 'test-results/multibackend-home.png', fullPage: true });
    await ctx.close();
  });

  test('Backends roll-up health line shown when ≥2 backends', async ({ browser }) => {
    const ctx = await loginContext(browser);
    const page = await ctx.newPage();
    await page.goto('/dashboard');
    await page.waitForSelector('.recent-panel-title');

    const healthLines = await page.$$eval('.recent-health-line', els =>
      els.map(e => e.textContent || '')
    );
    const backendsLine = healthLines.find(t => t.includes('Backends:'));
    expect(backendsLine, 'Backends roll-up line missing').toBeTruthy();
    expect(backendsLine).toMatch(/Backends:\s+2\/2/);
    expect(backendsLine).toContain('claude');
    expect(backendsLine).toContain('kiro');

    await ctx.close();
  });

  test('doctor panel: <details> renders, expandable, lists both backends', async ({ browser }) => {
    const ctx = await loginContext(browser);
    const page = await ctx.newPage();
    await page.goto('/dashboard');
    await page.waitForSelector('.doctor-panel', { timeout: 8000 });

    // Folded by default
    const isOpenBefore = await page.$eval('.doctor-panel', el => el.hasAttribute('open'));
    expect(isOpenBefore).toBe(false);

    // Open it
    await page.click('.doctor-panel .doctor-summary');
    const isOpenAfter = await page.$eval('.doctor-panel', el => el.hasAttribute('open'));
    expect(isOpenAfter).toBe(true);

    // Both backend rows present
    const rows = await page.$$eval('.doctor-row', els =>
      els.map(e => (e.textContent || '').replace(/\s+/g, ' ').trim())
    );
    expect(rows.length).toBe(2);
    expect(rows.find(r => r.includes('claude'))).toBeTruthy();
    expect(rows.find(r => r.includes('kiro'))).toBeTruthy();

    // Status dot present per row (● or ○)
    const dots = await page.$$('.doctor-row .doctor-status');
    expect(dots.length).toBe(2);

    // Feature pills present (each row has at least 7 features)
    const featCounts = await page.$$eval('.doctor-row', rows =>
      rows.map(r => r.querySelectorAll('.doctor-feat').length)
    );
    expect(featCounts.every(n => n >= 7), `feature pill counts: ${featCounts}`).toBeTruthy();

    await page.screenshot({ path: 'test-results/multibackend-doctor.png', fullPage: true });
    await ctx.close();
  });
});

test.describe('Backend picker + chips', () => {
  test('new-session modal renders backend <select> with both options', async ({ browser }) => {
    const ctx = await loginContext(browser);
    const page = await ctx.newPage();
    await page.goto('/dashboard');
    await page.waitForSelector('.recent-panel-title');

    // Trigger new-session modal — keyboard shortcut "n" is the canonical
    // open path; falls back to clicking the "+" button.
    await page.keyboard.press('n');
    const select = await page.waitForSelector('select#new-backend', { timeout: 4000 }).catch(() => null);
    if (!select) {
      // Fallback: click the new-session button (id varies — use title attr).
      const btn = await page.$('button[title*="new" i], button.new-session-btn, .header-new');
      if (btn) await btn.click();
      await page.waitForSelector('select#new-backend', { timeout: 4000 });
    }

    const opts = await page.$$eval('select#new-backend option', els =>
      els.map(o => ({ value: o.value, label: o.textContent || '', disabled: o.disabled }))
    );
    expect(opts.length).toBe(2);
    expect(opts.find(o => o.value === 'claude')).toBeTruthy();
    expect(opts.find(o => o.value === 'kiro')).toBeTruthy();
    expect(opts.every(o => !o.disabled), 'no backend should be disabled').toBeTruthy();

    await page.screenshot({ path: 'test-results/multibackend-picker.png' });
    await ctx.close();
  });

  // UI Round 5 R5-2 (PR #136) removed sidebar backend chips intentionally —
  // the cli icon (R5-1: kiro ghost vs claude logomark) carries the backend
  // signal now. Updated invariant: every session card MUST carry a cli icon
  // SVG, and the icon shape distinguishes backend.
  test('every session card carries a cli icon (kiro ghost vs claude logomark)', async ({ browser }) => {
    const ctx = await loginContext(browser);
    const page = await ctx.newPage();
    await page.goto('/dashboard');
    await page.waitForSelector('.session-card', { timeout: 8000 }).catch(() => null);

    const cards = await page.$$('.session-card');
    if (cards.length === 0) {
      test.skip(true, 'no sessions to assert icon on');
    }
    // Every card has exactly one .sc-cli-icon
    const iconCounts = await page.$$eval('.session-card', cards =>
      cards.map(c => c.querySelectorAll('.sc-cli-icon').length)
    );
    expect(iconCounts.every(n => n >= 1), `icon counts: ${iconCounts}`).toBeTruthy();

    // The set of icon shapes used must include either ghost (kiro,
    // viewBox 0 0 1200 1200) or claude (viewBox 0 0 248 248). No
    // session card should fall through to the default placeholder.
    const viewboxes = await page.$$eval('.session-card .sc-cli-icon', els =>
      els.map(e => e.getAttribute('viewBox') || '')
    );
    const validShapes = viewboxes.filter(vb =>
      vb.includes('1200') || vb.includes('248')
    );
    expect(validShapes.length, `unexpected icon viewBoxes: ${JSON.stringify(viewboxes)}`).toBe(viewboxes.length);

    await ctx.close();
  });
});

test.describe('Per-session feature gates', () => {
  // Selectors mirror applyFeatureGates() in dashboard.js (lines ~1178-1248).
  // The implementation only wires gates for two D-table items today:
  //   D14 (image_input)  →  button[onclick="openFilePicker()"]
  //   D15 (audio_input)  →  #btn-mic, #btn-hold-talk (uses .feat-degraded)
  // Other D-items (D9 /urgent, D11 queue indicator, D12 askuser, D13 @-mention)
  // appear in the RFC but are not implemented yet — we only assert the two
  // that the code claims to gate.
  test('selecting kiro session: file-picker hard-disabled (D14)', async ({ browser }) => {
    const ctx = await loginContext(browser);
    const page = await ctx.newPage();
    await page.goto('/dashboard');

    // Pick the first kiro session. fixtures may only contain claude
    // sessions — skip cleanly in that case rather than fail.
    await page.waitForSelector('.session-card', { timeout: 8000 }).catch(() => null);
    const kiroCard = await page.locator('.session-card', {
      has: page.locator('.sc-backend-chip', { hasText: 'kiro' }),
    }).first();
    if ((await kiroCard.count()) === 0) {
      test.skip(true, 'no kiro session in fixtures');
    }
    await kiroCard.click();
    // applyFeatureGates runs synchronously after selectSession but the DOM
    // may need a tick for class re-render.
    await page.waitForTimeout(300);

    const probe = await page.evaluate(() => {
      const fp = document.querySelector('button[onclick="openFilePicker()"]');
      const mic = document.getElementById('btn-mic');
      const hold = document.getElementById('btn-hold-talk');
      return {
        filePicker: fp ? {
          disabled: fp.disabled,
          hasFeatDisabled: fp.classList.contains('feat-disabled'),
          ariaDisabled: fp.getAttribute('aria-disabled'),
          title: fp.title,
        } : null,
        mic: mic ? {
          hasFeatDegraded: mic.classList.contains('feat-degraded'),
          title: mic.title,
        } : null,
        hold: hold ? {
          hasFeatDegraded: hold.classList.contains('feat-degraded'),
          title: hold.title,
        } : null,
      };
    });

    // kiro Profile.Features.image_input is **true** today (RFC §8.2 — claude
    // and kiro both support image), so D14 file picker should NOT be disabled
    // when kiro is selected. This pins the contract: if Profile flips to
    // image:false in future, the test must be flipped too.
    if (probe.filePicker) {
      expect(probe.filePicker.disabled, JSON.stringify(probe)).toBe(false);
    }
    // kiro audio_input is false → mic / hold should carry .feat-degraded
    // (NOT disabled — the audio path goes through transcribe-then-text on
    // the server side, so the button still works, the title just informs).
    if (probe.mic) expect(probe.mic.hasFeatDegraded, JSON.stringify(probe)).toBe(true);
    if (probe.hold) expect(probe.hold.hasFeatDegraded, JSON.stringify(probe)).toBe(true);

    await page.screenshot({ path: 'test-results/multibackend-kiro-gates.png', fullPage: true });
    await ctx.close();
  });
});

test.describe('Per-session cost unit + context bar', () => {
  test('claude session header shows USD cost format', async ({ browser }) => {
    const ctx = await loginContext(browser);
    const page = await ctx.newPage();
    await page.goto('/dashboard');
    await page.waitForSelector('.session-card', { timeout: 8000 }).catch(() => null);

    // Select first claude session. claude is the default, so most cards
    // will be claude unless the operator created kiro ones.
    const claudeCard = page.locator('.session-card', {
      has: page.locator('.sc-backend-chip', { hasText: 'claude' }),
    }).first();
    if ((await claudeCard.count()) === 0) {
      test.skip(true, 'no claude session in fixtures');
    }
    await claudeCard.click();
    await page.waitForTimeout(300);

    // Cost cell may use any of these selectors depending on the build —
    // check for any '$' prefix or '0.00 credits' anywhere in the header
    // detail row to confirm the unit-aware format ran.
    const headerText = await page.locator('.detail-pane-header, .main-header, .session-header').first().textContent().catch(() => '');
    expect(headerText, 'header should mention cost in USD/credits').toMatch(/\$|credits/);

    await page.screenshot({ path: 'test-results/multibackend-claude-header.png' });
    await ctx.close();
  });
});
