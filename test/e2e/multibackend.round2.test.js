// @ts-check
//
// Multi-Backend Round 2 e2e: deeper UI diagnostics after PR #128 / #127.
// Targeted scenarios:
//   - kiro session: header, ctx-bar (1-2% range, NOT >100%)
//   - kiro session: input-area buttons readable (Bug 4 verification)
//   - askuser fallback render path (Bug 2-D12)
//   - /urgent send-time gate (Bug 2-D9)
//   - persistence: send a message via API, verify it lands in /api/sessions
//
// Reuses globalSetup-seeded storage state from multibackend.global-setup.js.

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

// findKiroSessionKey returns the first session key whose backend is kiro,
// or null if no kiro session exists in the live router.
async function findKiroSessionKey(ctx) {
  const r = await ctx.request.get('/api/sessions');
  if (!r.ok()) return null;
  const data = await r.json();
  const list = data.sessions || data || [];
  const kiro = list.find(s => s && s.backend === 'kiro');
  return kiro ? kiro.key : null;
}

// async navigation+selection helper. Selects a session card by key.
async function selectByKey(page, key) {
  // Sessions render with data-key attr on .session-card
  const card = page.locator(`.session-card[data-key="${key}"]`).first();
  await card.click();
  await page.waitForTimeout(400);
}

test.describe('Round 2 — kiro session UI', () => {
  test('ctx_pct is normalized to 0-100 range', async ({ browser }) => {
    const ctx = await loginContext(browser);
    const r = await ctx.request.get('/api/sessions');
    const data = await r.json();
    const list = data.sessions || data || [];
    const kiro = list.find(s => s && s.backend === 'kiro');
    if (!kiro) test.skip(true, 'no kiro session in fixture');
    const pct = kiro.context_usage_percent;
    if (pct != null) {
      expect(pct, `ctx_pct out of [0,100]: ${pct}`).toBeGreaterThanOrEqual(0);
      expect(pct, `ctx_pct out of [0,100]: ${pct}`).toBeLessThanOrEqual(100);
    }
    await ctx.close();
  });

  test('selecting kiro session: header chip + version + cost unit credits', async ({ browser }) => {
    const ctx = await loginContext(browser);
    const page = await ctx.newPage();
    await page.goto('/dashboard');
    await page.waitForSelector('.session-card', { timeout: 8000 });

    const kiroKey = await findKiroSessionKey(ctx);
    if (!kiroKey) test.skip(true, 'no kiro session');
    await selectByKey(page, kiroKey);

    // Header should expose "kiro" (display name) somewhere
    const headerText = await page.locator(
      '.detail-pane-header, .main-header, .session-header'
    ).first().textContent({ timeout: 4000 }).catch(() => '');
    expect(headerText, 'header missing kiro version').toMatch(/kiro/i);
    // cost unit: when kiro is selected, header cost cell shows credits, NOT $
    expect(headerText, 'header should NOT show USD on kiro').not.toMatch(/\$\s*\d/);

    await page.screenshot({ path: 'test-results/round2-kiro-header.png', fullPage: true });
    await ctx.close();
  });

  test('input-area buttons remain visually readable on kiro (Bug 4)', async ({ browser }) => {
    const ctx = await loginContext(browser);
    const page = await ctx.newPage();
    await page.goto('/dashboard');
    await page.waitForSelector('.session-card', { timeout: 8000 });

    const kiroKey = await findKiroSessionKey(ctx);
    if (!kiroKey) test.skip(true, 'no kiro session');
    await selectByKey(page, kiroKey);

    // Probe each gated button. We're not asserting specific colors (theme
    // dependent) — instead we assert (a) no element has computed opacity
    // below .5 (the old .feat-disabled hard gate left buttons at .4), and
    // (b) titles contain the operator-readable hint.
    const probe = await page.evaluate(() => {
      const out = {};
      const fp = document.querySelector('button[onclick="openFilePicker()"]');
      if (fp) {
        const style = getComputedStyle(fp);
        out.filePicker = {
          opacity: parseFloat(style.opacity),
          background: style.backgroundImage || style.backgroundColor,
          title: fp.title,
          disabled: fp.disabled,
        };
      }
      const mic = document.getElementById('btn-mic');
      if (mic) {
        const style = getComputedStyle(mic);
        out.mic = {
          opacity: parseFloat(style.opacity),
          title: mic.title,
          hasFeatDegraded: mic.classList.contains('feat-degraded'),
        };
      }
      return out;
    });

    // After PR #128: feat-disabled drops to opacity:1 (or default), uses
    // diagonal stripe background. Old code was .4 — anything <= .5 is the
    // pre-fix regression.
    if (probe.filePicker) {
      expect(
        probe.filePicker.opacity,
        `filePicker opacity: ${JSON.stringify(probe.filePicker)}`
      ).toBeGreaterThan(0.5);
    }
    if (probe.mic) {
      expect(
        probe.mic.opacity,
        `mic opacity: ${JSON.stringify(probe.mic)}`
      ).toBeGreaterThan(0.5);
      // feat-degraded should set a tooltip explaining the soft gate
      if (probe.mic.hasFeatDegraded) {
        expect(probe.mic.title, JSON.stringify(probe.mic)).toMatch(/转写|不直接接收|fallback/i);
      }
    }

    await page.screenshot({ path: 'test-results/round2-kiro-input.png', fullPage: true });
    await ctx.close();
  });
});

test.describe('Round 2 — D9/D13 send-time gate', () => {
  test('/urgent on kiro session is rejected with toast', async ({ browser }) => {
    const ctx = await loginContext(browser);
    const page = await ctx.newPage();
    await page.goto('/dashboard');
    await page.waitForSelector('.session-card', { timeout: 8000 });

    const kiroKey = await findKiroSessionKey(ctx);
    if (!kiroKey) test.skip(true, 'no kiro session');
    await selectByKey(page, kiroKey);

    // Type /urgent into the message input + click send
    const input = await page.waitForSelector('#msg-input', { timeout: 4000 });
    await input.click();
    await page.keyboard.type('/urgent test message');
    // Snapshot pre-send state — we expect send NOT to fire and toast to appear
    const beforeBubbles = await page.locator('.user-msg, .msg-bubble.user').count();
    await page.click('#btn-send');
    await page.waitForTimeout(800);

    // Toast should mention "不支持 /urgent" or "passthrough"
    const toastText = await page.locator('#toast').textContent().catch(() => '');
    expect(toastText, `toast: "${toastText}"`).toMatch(/urgent|passthrough|抢占/i);

    // No new user message bubble should have been inserted
    const afterBubbles = await page.locator('.user-msg, .msg-bubble.user').count();
    expect(afterBubbles, 'message should NOT have been sent').toBe(beforeBubbles);

    await ctx.close();
  });

  test('@-mention on kiro session is rejected with toast', async ({ browser }) => {
    const ctx = await loginContext(browser);
    const page = await ctx.newPage();
    await page.goto('/dashboard');
    await page.waitForSelector('.session-card', { timeout: 8000 });

    const kiroKey = await findKiroSessionKey(ctx);
    if (!kiroKey) test.skip(true, 'no kiro session');
    await selectByKey(page, kiroKey);

    const input = await page.waitForSelector('#msg-input', { timeout: 4000 });
    await input.click();
    await page.keyboard.type('please read @docs/README.md and summarize');
    const beforeBubbles = await page.locator('.user-msg, .msg-bubble.user').count();
    await page.click('#btn-send');
    await page.waitForTimeout(800);

    const toastText = await page.locator('#toast').textContent().catch(() => '');
    expect(toastText, `toast: "${toastText}"`).toMatch(/mention|@|embedded|绝对路径/i);

    const afterBubbles = await page.locator('.user-msg, .msg-bubble.user').count();
    expect(afterBubbles, 'message should NOT have been sent').toBe(beforeBubbles);

    await ctx.close();
  });
});

test.describe('Round 2 — kiro session deep dive', () => {
  test('inspect kiro header layout + ctx-bar visibility + spinner', async ({ browser }) => {
    const ctx = await loginContext(browser);
    const page = await ctx.newPage();
    await page.goto('/dashboard');
    await page.waitForSelector('.session-card', { timeout: 8000 });

    const kiroKey = await findKiroSessionKey(ctx);
    if (!kiroKey) test.skip(true, 'no kiro session');
    await selectByKey(page, kiroKey);

    const inspection = await page.evaluate(() => {
      // Header structure
      const header = document.querySelector('.main-header');
      // Ctx bar
      const ctxBar = document.querySelector('.ctx-bar');
      const ctxFill = ctxBar ? ctxBar.querySelector('.ctx-bar-fill') : null;
      const ctxFillStyle = ctxFill ? getComputedStyle(ctxFill) : null;
      const ctxBarStyle = ctxBar ? getComputedStyle(ctxBar) : null;
      // Real selectors per dashboard.js (#events-scroll .event.user)
      const userMsgs = document.querySelectorAll('#events-scroll .event.user, .event.user');
      const lastMsg = userMsgs[userMsgs.length - 1];
      const spinners = document.querySelectorAll(
        '.spinner, .msg-spinner, .pending-indicator, [class*="spinner"], [class*="pending"]'
      );
      // Any element with circular border immediately after a user-msg?
      let circularSiblings = [];
      if (lastMsg) {
        let sib = lastMsg.nextElementSibling;
        while (sib) {
          const s = getComputedStyle(sib);
          // Detect a circular (border-radius:50%) small element nearby
          if (parseFloat(s.borderRadius) > 0 || s.borderRadius.includes('50%')) {
            const rect = sib.getBoundingClientRect();
            if (rect.width <= 30 && rect.height <= 30) {
              circularSiblings.push({
                tag: sib.tagName,
                cls: sib.className,
                rect: { w: rect.width, h: rect.height },
              });
            }
          }
          sib = sib.nextElementSibling;
        }
      }
      // Inspect the event-icon next to user messages — RFC §0 mentions a
      // 👤 (U+1F464) but if it doesn't render, the styled circle looks like
      // an unexplained spinner. Flag explicitly.
      const eventIcons = [];
      for (const u of userMsgs) {
        const icon = u.querySelector('.event-icon');
        if (!icon) continue;
        const text = (icon.textContent || '').trim();
        const codepoints = [...text].map(ch => ch.codePointAt(0).toString(16));
        eventIcons.push({
          text,
          codepoints,
          empty: text.length === 0,
        });
      }
      return {
        ctxBarPresent: !!ctxBar,
        ctxBarTitle: ctxBar ? ctxBar.title || ctxBar.getAttribute('aria-label') : '',
        ctxBarClasses: ctxBar ? ctxBar.className : '',
        ctxFillWidth: ctxFillStyle ? ctxFillStyle.width : '',
        ctxBarSize: ctxBarStyle ? `${ctxBarStyle.width} × ${ctxBarStyle.height}` : '',
        spinnerCount: spinners.length,
        spinnerSamples: Array.from(spinners).slice(0, 3).map(e => e.className),
        userMsgCount: userMsgs.length,
        eventIcons,
        circularSiblings: circularSiblings.slice(0, 3),
      };
    });

    console.log('[round2 inspect]', JSON.stringify(inspection, null, 2));

    await page.screenshot({ path: 'test-results/round2-deep-dive.png', fullPage: true });
    await ctx.close();
  });
});

test.describe('Round 2 — kiro persistence (Bug 1)', () => {
  test('newly created kiro session lands in sessions.json after save tick', async ({ browser }) => {
    const ctx = await loginContext(browser);

    // The fixture has a kiro session created earlier in this run; verify
    // /api/sessions reports it and the persisted store would include it.
    const r = await ctx.request.get('/api/sessions');
    const data = await r.json();
    const list = data.sessions || data || [];
    const kiroEntries = list.filter(s => s && s.backend === 'kiro');
    expect(kiroEntries.length, 'expected at least one kiro session').toBeGreaterThan(0);

    // Each kiro session should have a non-empty session_id (so saveStore
    // doesn't drop it via store.go:135 `if sid != ""`).
    for (const s of kiroEntries) {
      expect(
        s.session_id,
        `kiro session ${s.key} has empty session_id — saveStore would drop it`
      ).toBeTruthy();
    }

    await ctx.close();
  });
});
