// @ts-check
//
// Round 5 — verify the UI polish from PR #136 (7 改进点 R5-1..R5-7).
// Each describe block locks one design doc section.
//
// Round 5 design doc: docs/design/ui-round5-multibackend-polish.md

const { test, expect } = require('@playwright/test');
const fs = require('fs');
const path = require('path');

test.skip(
  process.env.NAOZHI_LIVE_E2E !== '1',
  'set NAOZHI_LIVE_E2E=1 to run live multi-backend e2e against 127.0.0.1:8180'
);

const BASE = process.env.NAOZHI_BASE || 'http://127.0.0.1:8180';
const STORAGE_PATH = path.join(__dirname, '.multibackend-storage.json');
const desktop = { viewport: { width: 1440, height: 900 } };

async function loginContext(browser) {
  return browser.newContext({ ...desktop, baseURL: BASE, storageState: STORAGE_PATH });
}

async function apiContext() {
  const { request } = require('@playwright/test');
  return request.newContext({ baseURL: BASE, storageState: STORAGE_PATH });
}

async function getSessions(req) {
  const r = await req.get('/api/sessions');
  if (!r.ok()) throw new Error(`/api/sessions ${r.status()}`);
  const d = await r.json();
  return d.sessions || d || [];
}

async function sendKiro(req, key, text, workspace = '/home/ec2-user/workspace/naozhi') {
  return req.post('/api/sessions/send', {
    data: { key, text, workspace, backend: 'kiro' },
  });
}

async function sendClaude(req, key, text, workspace = '/home/ec2-user/workspace/naozhi') {
  return req.post('/api/sessions/send', {
    data: { key, text, workspace, backend: 'claude' },
  });
}

async function waitForReady(req, key, timeoutMs = 45000) {
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    const list = await getSessions(req);
    const s = list.find(x => x && x.key === key);
    if (s && s.state === 'ready') return s;
    await new Promise(r => setTimeout(r, 300));
  }
  return null;
}

// ---------------------------------------------------------------------------

test.describe('Round 5 R5-1: kiro official ghost icon', () => {
  test('kiro session card cli-icon SVG contains ghost mark (purple bg + white body)', async ({ browser }) => {
    const ctx = await loginContext(browser);
    const page = await ctx.newPage();
    await page.goto('/dashboard');
    await page.waitForSelector('.session-card', { timeout: 8000 });

    // Find a kiro card; if none, create one via API first
    let kiroCard = await page.$('.session-card .sc-cli-icon[viewbox*="1200"]').catch(() => null);
    if (!kiroCard) {
      const req = await apiContext();
      const KEY = 'dashboard:direct:r5-icon-' + Date.now() + ':general';
      await sendKiro(req, KEY, 'hi');
      await waitForReady(req, KEY, 15000);
      await page.reload();
      await page.waitForSelector('.session-card', { timeout: 5000 });
      await req.dispose();
    }

    // The new ghost SVG has viewBox="0 0 1200 1200" and fill="#9046FF"
    // — the previous occupier was the hexagonal placeholder with
    // viewBox="0 0 16 16". This test pins the migration: at least one
    // sc-cli-icon on the page must be the ghost shape.
    const stats = await page.evaluate(() => {
      const icons = document.querySelectorAll('.session-card .sc-cli-icon');
      let ghost = 0, hex = 0;
      for (const ic of icons) {
        const vb = ic.getAttribute('viewBox') || '';
        if (vb.includes('1200')) ghost++;
        else if (vb.includes('16')) hex++;
      }
      return { ghost, hex, total: icons.length };
    });
    expect(stats.ghost, `expected at least 1 ghost icon, got ${JSON.stringify(stats)}`).toBeGreaterThan(0);

    await ctx.close();
  });
});

test.describe('Round 5 R5-2: backend chip removed from sidebar + header', () => {
  test('no .sc-backend-chip in any session-card or main-header', async ({ browser }) => {
    const ctx = await loginContext(browser);
    const page = await ctx.newPage();
    await page.goto('/dashboard');
    await page.waitForSelector('.session-card', { timeout: 8000 });

    // Pick any kiro session — header chip used to live there
    const kiroCard = page.locator('.session-card', {
      has: page.locator('.sc-cli-icon[viewBox*="1200"]'),
    }).first();
    if ((await kiroCard.count()) > 0) {
      await kiroCard.click();
      await page.waitForTimeout(400);
    }

    const counts = await page.evaluate(() => ({
      sidebarChips: document.querySelectorAll('.session-card .sc-backend-chip').length,
      headerChips: document.querySelectorAll('.main-header .sc-backend-chip').length,
      // doctor panel may still use chip-style spans, but those carry
      // class .doctor-feat / .doctor-row-proto — never .sc-backend-chip.
    }));
    expect(counts.sidebarChips, 'sidebar still renders backend chips').toBe(0);
    expect(counts.headerChips, 'header still renders backend chip').toBe(0);

    await ctx.close();
  });
});

test.describe('Round 5 R5-3: model display all backends', () => {
  test('claude session has model populated from system/init', async ({}) => {
    const req = await apiContext();
    const KEY = 'dashboard:direct:r5-cc-model-' + Date.now() + ':general';
    // Use a non-trivial prompt — "reply: hi" sometimes returns instantly
    // before the system/init event we depend on lands. A multi-token
    // request keeps the CLI alive long enough that init definitely fires.
    await sendClaude(req, KEY, 'list 3 short reasons to use Go, one per line');
    // Poll for BOTH state==ready AND model populated. claude-cli has a
    // brief window where state flips ready before system/init has been
    // captured by the snapshot path; tolerate that with explicit retry.
    let snap = null;
    for (let i = 0; i < 60; i++) {
      const list = await getSessions(req);
      const s = list.find(x => x && x.key === KEY);
      if (s && s.state === 'ready' && s.model) { snap = s; break; }
      await new Promise(r => setTimeout(r, 500));
    }
    expect(snap, 'claude session not ready with model in 30s').toBeTruthy();
    expect(snap.model, `unexpected model shape: ${snap.model}`).toMatch(/claude-/);
    await req.delete(`/api/sessions?key=${encodeURIComponent(KEY)}`).catch(() => {});
    await req.dispose();
  });

  test('kiro session has model populated from cli.backends[].model', async ({}) => {
    const req = await apiContext();
    const KEY = 'dashboard:direct:r5-kiro-model-' + Date.now() + ':general';
    await sendKiro(req, KEY, 'reply: hi');
    const snap = await waitForReady(req, KEY, 45000);
    expect(snap, 'kiro session not ready').toBeTruthy();
    expect(snap.model, `kiro session has empty model: ${JSON.stringify(snap)}`).toBeTruthy();
    // config.yaml pins kiro to claude-opus-4.7
    expect(snap.model).toBe('claude-opus-4.7');
    await req.delete(`/api/sessions?key=${encodeURIComponent(KEY)}`).catch(() => {});
    await req.dispose();
  });

  test('dashboard header shows compact model label', async ({ browser }) => {
    const ctx = await loginContext(browser);
    const page = await ctx.newPage();
    await page.goto('/dashboard');
    await page.waitForSelector('.session-card', { timeout: 8000 });
    // Find a session with a model AND that is rendered as
    // .session-card[data-key=...] in the DOM. cron jobs render via
    // .cron-card on a different code path so they don't accept the
    // data-key click target.
    const req = await apiContext();
    const sessions = await getSessions(req);
    const renderedKeys = new Set(
      await page.$$eval('.session-card[data-key]', els => els.map(e => e.getAttribute('data-key')))
    );
    const candidate = sessions.find(
      s => s && s.model && s.state === 'ready' && renderedKeys.has(s.key)
    );
    if (!candidate) test.skip(true, 'no rendered session-card with populated model');
    await page.click(`.session-card[data-key="${candidate.key}"]`);
    await page.waitForTimeout(800);

    const probe = await page.evaluate(() => {
      const lbl = document.querySelector('.main-header .model-label');
      if (!lbl) return { present: false };
      return {
        present: true,
        text: (lbl.textContent || '').trim(),
        title: lbl.getAttribute('title') || '',
        unsetClass: lbl.classList.contains('model-label-unset'),
      };
    });
    expect(probe.present, '.model-label not in DOM').toBeTruthy();
    expect(probe.unsetClass, `model-label is unset: ${JSON.stringify(probe)}`).toBeFalsy();
    expect(probe.text.length, `model-label empty: ${JSON.stringify(probe)}`).toBeGreaterThan(0);
    await req.dispose();
    await ctx.close();
  });
});

test.describe('Round 5 R5-4: kiro credits accumulate across turns', () => {
  test('3 kiro turns: total_cost is monotonically non-decreasing + ends > 0', async ({}) => {
    const req = await apiContext();
    const KEY = 'dashboard:direct:r5-credits-acc-' + Date.now() + ':general';

    // Use a multi-line prompt — single-token replies sometimes don't
    // trigger metering on kiro 2.3.0; longer prompts always do.
    const longPrompt = 'list integers 1 to 5, one per line, just the numbers';

    await sendKiro(req, KEY, longPrompt);
    let snap = await waitForReady(req, KEY, 45000);
    expect(snap).toBeTruthy();
    const cost1 = snap.total_cost || 0;

    await req.post('/api/sessions/send', { data: { key: KEY, text: longPrompt } });
    snap = await waitForReady(req, KEY, 45000);
    const cost2 = snap.total_cost || 0;
    expect(cost2, `t2 cost (${cost2}) should be ≥ t1 cost (${cost1})`).toBeGreaterThanOrEqual(cost1);

    await req.post('/api/sessions/send', { data: { key: KEY, text: longPrompt } });
    snap = await waitForReady(req, KEY, 45000);
    const cost3 = snap.total_cost || 0;
    expect(cost3, `t3 cost (${cost3}) should be ≥ t2 cost (${cost2})`).toBeGreaterThanOrEqual(cost2);
    // After 3 longish turns, SOMETHING should have been billed.
    expect(cost3, `total cost still 0 after 3 long turns: ${cost3}`).toBeGreaterThan(0);

    await req.delete(`/api/sessions?key=${encodeURIComponent(KEY)}`).catch(() => {});
    await req.dispose();
  });
});

test.describe('Round 5 R5-5: alert dot moved past chevron', () => {
  test('ns-alert sits AFTER ns-chev in DOM order', async ({ browser }) => {
    const ctx = await loginContext(browser);
    const page = await ctx.newPage();
    await page.goto('/dashboard');
    await page.waitForSelector('#ns-trigger', { timeout: 5000 }).catch(() => {});

    // Multi-node hidden if no remotes; skip cleanly
    const ns = await page.$('#node-selector:not([hidden])').catch(() => null);
    if (!ns) test.skip(true, 'no multi-node selector visible');

    const order = await page.evaluate(() => {
      const trigger = document.getElementById('ns-trigger');
      if (!trigger) return null;
      const kids = Array.from(trigger.children);
      return kids.map(k => k.id || k.classList.value || k.tagName).join(' / ');
    });
    expect(order, 'ns-alert should come after ns-chev').toMatch(/ns-chev[^/]*\/ ns-trigger-alert/);
    await ctx.close();
  });
});

test.describe('Round 5 R5-7: ctx-bar removed from header', () => {
  test('main-header has no .ctx-bar element', async ({ browser }) => {
    const ctx = await loginContext(browser);
    const page = await ctx.newPage();
    await page.goto('/dashboard');
    await page.waitForSelector('.session-card', { timeout: 8000 });
    // Click into any session
    await page.click('.session-card');
    await page.waitForTimeout(400);

    const count = await page.locator('.main-header .ctx-bar').count();
    expect(count, '.ctx-bar still rendered in header').toBe(0);

    // Server-side field still exists (doctor / future compact mode use it)
    const req = await apiContext();
    const sessions = await getSessions(req);
    const someRunning = sessions.find(
      s => s && typeof s.context_usage_percent === 'number' && s.context_usage_percent > 0
    );
    if (someRunning) {
      // Ensure field IS in API response — only the UI render is gone.
      expect(someRunning.context_usage_percent).toBeGreaterThan(0);
    }
    await req.dispose();
    await ctx.close();
  });
});

test.describe('Round 5 R5-3 deep: model survives restart via persistence', () => {
  // This test is observational: it inspects sessions.json AFTER a fresh
  // send round-trip. Restart itself isn't triggered (slow + stateful);
  // instead we verify the persisted entry carries model so a restart
  // would survive.
  test('sessions.json contains model field for kiro entries', async ({}) => {
    const req = await apiContext();
    // Trigger a fresh kiro send to ensure save tick has model on it.
    const KEY = 'dashboard:direct:r5-persist-' + Date.now() + ':general';
    await sendKiro(req, KEY, 'reply: persist test');
    await waitForReady(req, KEY, 45000);

    // Wait for a save tick. saveStore runs every ~30s but also fires
    // when storeDirty is set on session lifecycle events. Poll up to 65s.
    let entry = null;
    for (let i = 0; i < 65; i++) {
      const items = JSON.parse(fs.readFileSync('/home/ec2-user/.naozhi/sessions.json', 'utf8'));
      entry = items.find(s => s.key === KEY);
      if (entry && entry.model) break;
      await new Promise(r => setTimeout(r, 1000));
    }
    expect(entry, 'session not persisted').toBeTruthy();
    expect(entry.model, 'persisted entry missing model field').toBe('claude-opus-4.7');
    expect(entry.backend).toBe('kiro');

    await req.delete(`/api/sessions?key=${encodeURIComponent(KEY)}`).catch(() => {});
    await req.dispose();
  });
});

test.describe('Round 5 R5-6: WS push slack 80px', () => {
  test('scrollSlackPx is 80 (vs old 30)', async ({ browser }) => {
    const ctx = await loginContext(browser);
    const page = await ctx.newPage();
    await page.goto('/dashboard');
    await page.waitForSelector('.session-card', { timeout: 8000 });

    // Read window.scrollSlackPx if exposed; otherwise probe via the
    // wasBottom logic. Simpler approach: parse dashboard.js source via
    // public asset — verify the constant is wired.
    const slack = await page.evaluate(() => {
      // The const is module-local; expose via a tiny probe — check the
      // event handler's behavior by injecting a synthetic scroll state.
      // Easier: find the const definition by inspecting Function.prototype
      // representations? Hard. Instead, check the HTML source served by
      // /dashboard/dashboard.js for the 80 literal in expected context.
      return null;
    });
    // Fall back to fetching the JS asset and grep.
    const r = await ctx.request.get('/dashboard/dashboard.js');
    if (r.ok()) {
      const src = await r.text();
      expect(src, 'scrollSlackPx const not present in served JS').toMatch(/scrollSlackPx\s*=\s*80/);
    }
    await ctx.close();
  });
});
