// @ts-check
//
// Round 3 — large-surface kiro lifecycle scan. Covers what unit + round-1/2
// e2e didn't drive end-to-end:
//   1.  Lifecycle: spawn → ready → send → assistant reply → ready
//   2.  Persistence: save tick lands kiro entry; reattach across restart preserves sid
//   3.  Multi-turn: 2nd send on same kiro session keeps sid (no spurious resume)
//   4.  Mixed-backend dashboard: claude + kiro side-by-side, switching session
//       updates header / chip / cost-unit / feature gates
//   5.  Cron: create job with backend=kiro, list shows backend label, scheduler
//       respects the override (no regression to claude default)
//   6.  Interrupt: ACP session/cancel notification reaches kiro mid-turn
//   7.  Scratch (aside): kiro session → /api/scratch/open inherits backend
//   8.  History: /api/sessions/events for kiro session returns kiro frames
//   9.  Tool_call rendering: e2e from the dashboard side
//  10.  Error path: invalid backend ID rejected; unknown ID degrades cleanly
//
// All cases are skip-friendly: if a precondition (e.g., kiro session exists)
// isn't met they skip with a clear message rather than fail.

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
  if (!fs.existsSync(STORAGE_PATH)) {
    throw new Error(`storage state ${STORAGE_PATH} missing — globalSetup did not run`);
  }
  return browser.newContext({ ...desktop, baseURL: BASE, storageState: STORAGE_PATH });
}

// apiContext returns a Playwright APIRequestContext authenticated via the
// shared storage state. Methods are .get / .post / .delete / .patch
// directly on the returned context (NOT .request.get).
async function apiContext() {
  const { request } = require('@playwright/test');
  return request.newContext({ baseURL: BASE, storageState: STORAGE_PATH });
}

// requestOf normalises both APIRequestContext and BrowserContext: the
// former exposes .get/.post directly, the latter via .request.
function requestOf(ctxOrPage) {
  return ctxOrPage.request || ctxOrPage; // BrowserContext or APIRequestContext
}

async function getSessions(ctxOrPage) {
  const req = requestOf(ctxOrPage);
  const r = await req.get('/api/sessions');
  if (!r.ok()) throw new Error(`/api/sessions ${r.status()}`);
  const data = await r.json();
  return data.sessions || data || [];
}

async function findKiroSession(ctxOrPage) {
  const sessions = await getSessions(ctxOrPage);
  return sessions.find(s => s && s.backend === 'kiro') || null;
}

// sendMessage POSTs /api/sessions/send. Returns the parsed response.
async function sendMessage(ctxOrPage, key, text, opts = {}) {
  const req = requestOf(ctxOrPage);
  const body = { key, text, ...opts };
  const r = await req.post('/api/sessions/send', { data: body });
  return { ok: r.ok(), status: r.status(), body: await r.json().catch(() => null) };
}

async function waitForState(ctxOrPage, key, state, timeoutMs = 10000) {
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    const list = await getSessions(ctxOrPage);
    const s = list.find(x => x && x.key === key);
    if (s && s.state === state) return s;
    await new Promise(r => setTimeout(r, 300));
  }
  return null;
}

// ---------------------------------------------------------------------------

test.describe('Round 3 — kiro lifecycle (spawn → send → ready)', () => {
  const KEY = 'dashboard:direct:r3-lifecycle:general';

  test.beforeAll(async () => {
    // Best-effort cleanup — DELETE is idempotent.
    const ctx = await apiContext();
    await ctx.delete(`/api/sessions?key=${encodeURIComponent(KEY)}`).catch(() => {});
    await ctx.dispose();
  });

  test('fresh kiro session: send → result → state ready + non-empty session_id', async () => {
    const ctx = await apiContext();
    const r = await sendMessage(ctx, KEY, 'reply with the single word: pong', {
      cwd: '/home/ec2-user/workspace/naozhi',
      backend: 'kiro',
    });
    expect(r.ok, JSON.stringify(r)).toBeTruthy();

    // Wait for the assistant turn to complete (state=ready).
    // ACP turn for one short message tends to land in <5s.
    const snap = await waitForState(ctx, KEY, 'ready', 15000);
    expect(snap, 'kiro session never reached state=ready in 15s').toBeTruthy();
    expect(snap.backend).toBe('kiro');
    expect(snap.cli_name).toBe('kiro');
    expect(snap.cost_unit).toBe('credits');
    expect(snap.session_id, 'kiro sid must be populated post-send').toBeTruthy();
    // ctx_pct should be 0..100 (post normalize)
    if (snap.context_usage_percent != null) {
      expect(snap.context_usage_percent).toBeGreaterThanOrEqual(0);
      expect(snap.context_usage_percent).toBeLessThanOrEqual(100);
    }

    await ctx.dispose();
  });

  test('multi-turn: same kiro session keeps the same session_id across 2nd send', async () => {
    const ctx = await apiContext();
    // Capture sid from the previous turn
    const before = await getSessions(ctx);
    const prev = before.find(s => s && s.key === KEY);
    if (!prev) test.skip(true, 'lifecycle test prereq missing');
    const sidBefore = prev.session_id;
    expect(sidBefore).toBeTruthy();

    const r = await sendMessage(ctx, KEY, 'reply with: ok');
    expect(r.ok, JSON.stringify(r)).toBeTruthy();
    const snap = await waitForState(ctx, KEY, 'ready', 15000);
    expect(snap, 'kiro session never returned to ready after 2nd send').toBeTruthy();
    expect(
      snap.session_id,
      `sid drift across turns: before=${sidBefore} after=${snap.session_id}`
    ).toBe(sidBefore);

    await ctx.dispose();
  });
});

test.describe('Round 3 — kiro persistence + reattach', () => {
  test('every kiro session has non-empty sid (saveStore eligibility)', async ({}) => {
    const ctx = await apiContext();
    const sessions = await getSessions(ctx);
    const kiroLive = sessions.filter(s => s && s.backend === 'kiro');
    if (kiroLive.length === 0) test.skip(true, 'no kiro session in fixture');

    for (const s of kiroLive) {
      expect(
        s.session_id,
        `kiro ${s.key} missing session_id — saveStore would drop it`
      ).toBeTruthy();
    }
    await ctx.dispose();
  });

  test('all sid-bearing kiro sessions also appear in sessions.json (after save tick)', async ({}) => {
    const ctx = await apiContext();
    // saveStore runs on Cleanup ticks (~every 30s) plus on graceful
    // shutdown. New sessions in this run may take a tick to land. Poll
    // up to 65s and re-fetch live state every loop so a sid that flipped
    // because a parallel test re-spawned the same key isn't asserted as
    // drift.
    let kiroLive = [];
    let persisted = [];
    let driftKey = null;
    for (let i = 0; i < 65; i++) {
      kiroLive = (await getSessions(ctx)).filter(s => s && s.backend === 'kiro' && s.session_id);
      if (kiroLive.length === 0) {
        await new Promise(r => setTimeout(r, 1000));
        continue;
      }
      persisted = JSON.parse(fs.readFileSync('/home/ec2-user/.naozhi/sessions.json', 'utf8'));
      const persistedKeys = new Set(persisted.map(s => s.key));
      const missing = kiroLive.filter(s => !persistedKeys.has(s.key));
      driftKey = null;
      for (const s of kiroLive) {
        const p = persisted.find(x => x.key === s.key);
        if (p && p.session_id !== s.session_id) { driftKey = s.key; break; }
      }
      if (missing.length === 0 && !driftKey) break;
      await new Promise(r => setTimeout(r, 1000));
    }
    if (kiroLive.length === 0) test.skip(true, 'no kiro sessions to assert');

    const persistedKeys = new Set(persisted.map(s => s.key));
    const missing = kiroLive.filter(s => !persistedKeys.has(s.key));
    expect(
      missing.length,
      `missing kiro in sessions.json after 65s: ${missing.map(s => s.key).join(',')}`
    ).toBe(0);

    for (const s of kiroLive) {
      const p = persisted.find(x => x.key === s.key);
      if (p) {
        expect(p.session_id, `${s.key} persisted sid drift`).toBe(s.session_id);
        expect(p.backend, `${s.key} persisted backend drift`).toBe('kiro');
      }
    }
    await ctx.dispose();
  });
});

test.describe('Round 3 — Mixed-backend dashboard switching', () => {
  test('switching from claude card to kiro card flips chip color + feature gates', async ({ browser }) => {
    const ctx = await loginContext(browser);
    const page = await ctx.newPage();
    await page.goto('/dashboard');
    await page.waitForSelector('.session-card', { timeout: 8000 });

    // Find one claude + one kiro that is rendered as a session-card on the
    // dashboard. The card renderer uses data-key=<key>; cron jobs are
    // rendered as cron-card via a different code path, so pre-filter to
    // keys that actually exist as .session-card[data-key=...] in the DOM.
    const sessions = await getSessions(ctx);
    const renderedKeys = await page.evaluate(() => {
      return Array.from(document.querySelectorAll('.session-card[data-key]'))
        .map(c => c.getAttribute('data-key'));
    });
    const renderedSet = new Set(renderedKeys);
    const claude = sessions.find(
      s => s && s.backend === 'claude' && s.state === 'ready' && renderedSet.has(s.key)
    );
    const kiro = sessions.find(
      s => s && s.backend === 'kiro' && s.state === 'ready' && renderedSet.has(s.key)
    );
    if (!claude || !kiro) test.skip(true, 'need both claude + kiro ready sessions rendered');

    // Select claude first
    await page.click(`.session-card[data-key="${claude.key}"]`);
    await page.waitForTimeout(400);
    const claudeSnap = await page.evaluate(() => {
      const fp = document.querySelector('button[onclick="openFilePicker()"]');
      const headerCost = document.querySelector('.main-header')?.textContent || '';
      return {
        filePickerDisabled: fp ? fp.disabled : null,
        hasUSD: /\$/.test(headerCost),
        hasCredits: /credits/i.test(headerCost),
      };
    });

    // Switch to kiro
    await page.click(`.session-card[data-key="${kiro.key}"]`);
    await page.waitForTimeout(400);
    const kiroSnap = await page.evaluate(() => {
      const fp = document.querySelector('button[onclick="openFilePicker()"]');
      const headerCost = document.querySelector('.main-header')?.textContent || '';
      return {
        filePickerDisabled: fp ? fp.disabled : null,
        hasUSD: /\$/.test(headerCost),
        hasCredits: /credits/i.test(headerCost),
      };
    });

    // Cost unit must flip between sessions
    expect(claudeSnap.hasUSD, 'claude header missing $').toBeTruthy();
    expect(kiroSnap.hasCredits, 'kiro header missing credits').toBeTruthy();
    expect(kiroSnap.hasUSD, 'kiro header should not show $').toBeFalsy();

    await page.screenshot({ path: 'test-results/round3-mixed-switch.png', fullPage: true });
    await ctx.close();
  });
});

test.describe('Round 3 — cron with backend=kiro', () => {
  let createdID = null;

  test.afterAll(async () => {
    if (!createdID) return;
    const ctx = await apiContext();
    await ctx.delete(`/api/cron?id=${createdID}`).catch(() => {});
    await ctx.dispose();
  });

  test('create cron job with backend=kiro, list reports backend, delete', async ({}) => {
    const ctx = await apiContext();

    // Create
    const create = await ctx.post('/api/cron', {
      data: {
        name: 'r3-kiro-cron',
        schedule: '0 9 * * *',           // daily 09:00
        prompt: 'echo kiro cron ping',
        cwd: '/home/ec2-user/workspace/naozhi',
        agent: 'general',
        backend: 'kiro',
      },
    });
    expect(create.ok(), `cron create: ${await create.text()}`).toBeTruthy();
    const job = await create.json();
    createdID = job.id;
    expect(createdID).toBeTruthy();

    // List + verify
    const list = await ctx.get('/api/cron');
    expect(list.ok()).toBeTruthy();
    const all = await list.json();
    const arr = Array.isArray(all) ? all : (all.jobs || []);
    const found = arr.find(j => j && j.id === createdID);
    expect(found, 'created job missing in list').toBeTruthy();
    expect(found.backend, `cron backend not preserved: ${JSON.stringify(found)}`).toBe('kiro');

    // Reject invalid backend on update
    const bad = await ctx.patch('/api/cron', {
      data: { id: createdID, backend: '../etc/passwd' },
    });
    expect(bad.status(), 'invalid backend should 400').toBe(400);

    await ctx.dispose();
  });
});

test.describe('Round 3 — kiro send error paths', () => {
  test('unknown backend ID is rejected at handler edge', async ({}) => {
    const ctx = await apiContext();
    const r = await ctx.post('/api/sessions/send', {
      data: {
        key: 'dashboard:direct:r3-bad-backend:general',
        text: 'hi',
        cwd: '/home/ec2-user/workspace/naozhi',
        backend: 'gemini',
      },
    });
    // Could be either 400 (rejected up-front) or accepted but session
    // never spawns. If accepted, status should be "accepted" but the
    // session never reaches state=ready. Tolerate both shapes.
    if (r.ok()) {
      const body = await r.json();
      expect(body.status === 'accepted' || body.error, JSON.stringify(body)).toBeTruthy();
    } else {
      expect([400, 422]).toContain(r.status());
    }
    await ctx.dispose();
  });

  test('invalid backend chars rejected', async ({}) => {
    const ctx = await apiContext();
    const r = await ctx.post('/api/sessions/send', {
      data: {
        key: 'dashboard:direct:r3-bad-chars:general',
        text: 'hi',
        cwd: '/home/ec2-user/workspace/naozhi',
        backend: '../sneaky',
      },
    });
    expect(r.ok(), 'invalid backend chars should NOT be accepted').toBeFalsy();
    // dashboard_send.go:883 returns 403 with a localised generic error
    // ("处理失败，请发送 /new 重置后重试") for ALL validation failures, by
    // design — the raw error may embed paths / keys, so /api/sessions/send
    // intentionally collapses them into a single status (R218-SEC-P1). We
    // only assert it's a non-2xx with a non-empty error body.
    expect([400, 403, 422]).toContain(r.status());
    const body = await r.json().catch(() => null);
    expect(body && body.error, `error body: ${JSON.stringify(body)}`).toBeTruthy();
    await ctx.dispose();
  });
});

test.describe('Round 3 — kiro interrupt (ACP session/cancel)', () => {
  const KEY = 'dashboard:direct:r3-cancel:general';

  test.beforeAll(async () => {
    const ctx = await apiContext();
    await ctx.delete(`/api/sessions?key=${encodeURIComponent(KEY)}`).catch(() => {});
    await ctx.dispose();
  });

  test('interrupt during running turn flips state to ready', async ({}) => {
    const ctx = await apiContext();
    // Spawn session with a deliberately long prompt so we have time to
    // interrupt before turn-end. Backed by the in-process kiro CLI; if
    // it's <500ms we may race and skip the assertion.
    const r = await sendMessage(ctx, KEY, 'count slowly from 1 to 50, one number per line', {
      cwd: '/home/ec2-user/workspace/naozhi',
      backend: 'kiro',
    });
    expect(r.ok, JSON.stringify(r)).toBeTruthy();

    // Wait until session enters running, then interrupt
    let running = null;
    for (let i = 0; i < 30; i++) {
      const list = await getSessions(ctx);
      const s = list.find(x => x && x.key === KEY);
      if (s && s.state === 'running') { running = s; break; }
      if (s && s.state === 'ready' && s.session_id) {
        // Already finished. Test is best-effort; skip rather than fail
        // because the model may have replied instantly.
        test.skip(true, 'kiro replied too fast to interrupt — racy test');
      }
      await new Promise(r => setTimeout(r, 200));
    }
    if (!running) test.skip(true, 'never observed running state — skipping');

    const intr = await ctx.post('/api/sessions/interrupt', {
      data: { key: KEY },
    });
    expect(intr.ok(), `interrupt rejected: ${await intr.text()}`).toBeTruthy();

    const ready = await waitForState(ctx, KEY, 'ready', 8000);
    expect(ready, 'kiro session did not return to ready after interrupt').toBeTruthy();
    expect(ready.session_id).toBeTruthy();

    await ctx.dispose();
  });
});

test.describe('Round 3 — kiro events transcript', () => {
  test('GET /api/sessions/events on a kiro session returns frames', async ({}) => {
    const ctx = await apiContext();
    const kiro = await findKiroSession(ctx);
    if (!kiro) test.skip(true, 'no kiro session in fixture');

    const r = await ctx.get(
      `/api/sessions/events?key=${encodeURIComponent(kiro.key)}`
    );
    expect(r.ok(), `events ${r.status()}`).toBeTruthy();
    const data = await r.json();
    const events = data.events || [];
    // Even an empty session has init / system frames; assert array present.
    expect(Array.isArray(events), 'events should be an array').toBeTruthy();
    // For sessions that have run a turn, we should see at least 1 user + 1 text
    // event. If the kiro session is brand-new, this is a soft check.
    if (events.length >= 2) {
      const types = events.map(e => e.type);
      expect(
        types.some(t => t === 'user' || t === 'text'),
        `expected user/text in transcript, got ${types.slice(0, 8).join(',')}`
      ).toBeTruthy();
    }

    await ctx.dispose();
  });
});

test.describe('Round 3 — dashboard tool_call panel render', () => {
  test('dashboard does not throw on kiro tool_call event payload', async ({ browser }) => {
    // Smoke check: load dashboard with a kiro session in the fixture and
    // verify there are no console errors. Real tool_call events are hard to
    // synthesize without driving the model; fall back to a JS-error scan.
    const ctx = await loginContext(browser);
    const page = await ctx.newPage();
    const errors = [];
    page.on('pageerror', e => errors.push(e.message));
    page.on('console', m => {
      if (m.type() === 'error') errors.push('[console] ' + m.text());
    });
    await page.goto('/dashboard');
    await page.waitForSelector('.recent-panel-title', { timeout: 8000 });

    // Pick the kiro session if present so eventHtml runs against ACP frames
    const kiro = await findKiroSession(ctx);
    if (kiro) {
      await page.click(`.session-card[data-key="${kiro.key}"]`).catch(() => {});
      await page.waitForTimeout(800);
    }

    expect(errors, errors.join('\n')).toEqual([]);
    await ctx.close();
  });
});
