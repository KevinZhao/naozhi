// @ts-check
//
// User-reported bug: "kiro session 不能正常返回结果"
// 这个 spec 模拟操作员路径 — 打开 dashboard → 选 kiro session →
// 在输入框敲消息 → 看 assistant reply 是否在事件流里出现。
// API 层验证过 kiro 正常返回（result event 有 'pong'），所以问题
// 几乎肯定在 dashboard UI 渲染或 WS 事件分发。

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

test('user reproduces: send msg via dashboard UI → assistant reply appears', async ({ browser }) => {
  const ctx = await loginContext(browser);
  const page = await ctx.newPage();

  // Capture all console + page errors
  const consoleMsgs = [];
  page.on('console', m => consoleMsgs.push({ type: m.type(), text: m.text() }));
  const pageErrors = [];
  page.on('pageerror', e => pageErrors.push(e.message));
  // Capture network failures (esp. /api/sessions/send + WS frames)
  const networkFailures = [];
  page.on('requestfailed', r => networkFailures.push(`${r.method()} ${r.url()}: ${r.failure()?.errorText}`));

  await page.goto('/dashboard');
  await page.waitForSelector('.session-card', { timeout: 8000 });

  // Pick the first kiro session card
  const kiroCard = page.locator('.session-card', {
    has: page.locator('.sc-backend-chip', { hasText: 'kiro' }),
  }).first();
  if ((await kiroCard.count()) === 0) {
    test.skip(true, 'no kiro session in fixture — create one via API first');
  }
  await kiroCard.click();
  await page.waitForTimeout(500);

  // Snapshot baseline events
  const baselineEvents = await page.locator('#events-scroll .event').count();

  // Type + send
  const input = await page.waitForSelector('#msg-input', { timeout: 4000 });
  await input.click();
  // Use unique text so we can find the reply unambiguously
  const tag = 'r3-ui-' + Date.now();
  await page.keyboard.type(`reply with the single word: ${tag}`);
  await page.click('#btn-send');

  // The send happens via /api/sessions/send. Let's track:
  // 1. user bubble appears within 2s (optimistic)
  // 2. assistant reply text appears within 15s
  // 3. state returns to ready

  // Wait for user bubble
  const userOk = await page.waitForFunction(
    (t) => {
      const els = document.querySelectorAll('#events-scroll .event.user');
      return Array.from(els).some(e => (e.textContent || '').includes(t));
    },
    `reply with the single word: ${tag}`,
    { timeout: 5000 }
  ).catch(() => null);
  expect(userOk, 'user bubble never appeared in 5s').toBeTruthy();

  // Wait for assistant reply with tag echo. Most ACP turns come back in
  // 1-3s but allow 20s slack for cold start.
  const assistantOk = await page.waitForFunction(
    (t) => {
      const els = document.querySelectorAll('#events-scroll .event.text, #events-scroll .event[class*="assistant"]');
      return Array.from(els).some(e => (e.textContent || '').includes(t));
    },
    tag,
    { timeout: 20000 }
  ).catch(() => null);

  // Capture state irrespective of pass/fail for diagnosis
  const finalEvents = await page.evaluate(() => {
    const els = document.querySelectorAll('#events-scroll .event');
    return Array.from(els).slice(-6).map(e => ({
      cls: e.className,
      text: (e.textContent || '').slice(0, 120),
    }));
  });
  console.log('[kiro-ui finalEvents]', JSON.stringify(finalEvents, null, 2));
  console.log('[kiro-ui pageErrors]', pageErrors.length, pageErrors.slice(0, 3));
  console.log('[kiro-ui consoleErrors]', consoleMsgs.filter(m => m.type === 'error').slice(0, 5));
  console.log('[kiro-ui networkFailures]', networkFailures.slice(0, 5));
  console.log('[kiro-ui baseline → final]', baselineEvents, '→', await page.locator('#events-scroll .event').count());

  await page.screenshot({ path: 'test-results/kiro-ui-after-send.png', fullPage: true });

  expect(assistantOk, `assistant reply with tag ${tag} never appeared in 20s`).toBeTruthy();

  await ctx.close();
});
