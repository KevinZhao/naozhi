// @ts-check
// Negative CSP probes (#1980 PR-3). The mock server mirrors the production
// Content-Security-Policy header (drift-guarded by
// TestDashboardCSP_MockServerHeaderInSync), so these tests prove in a real
// browser that the policy actually blocks the injection shapes the
// unsafe-inline removal is meant to kill: inline event-handler attributes
// and inline <script> elements. Both assertions are doubled with
// securitypolicyviolation events so a silently missing header cannot show
// up as a false green.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };

test.describe('CSP negative probes', () => {
  let mock;

  test.beforeAll(async () => { mock = await startMockServer(); });
  test.afterAll(() => mock.server.close());

  test('inline handler attributes and inline scripts are blocked', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    const result = await page.evaluate(async () => {
      const violations = [];
      document.addEventListener('securitypolicyviolation', (e) => {
        violations.push(e.effectiveDirective);
      });
      // Injected inline handler attribute (the pre-#1980 XSS shape).
      const btn = document.createElement('button');
      btn.setAttribute('onclick', 'window.__pwn1 = 1');
      document.body.appendChild(btn);
      btn.click();
      // Injected inline <script> element.
      const s = document.createElement('script');
      s.textContent = 'window.__pwn2 = 1';
      document.body.appendChild(s);
      await new Promise((r) => setTimeout(r, 100));
      return {
        pwn1: window.__pwn1,
        pwn2: window.__pwn2,
        violations,
      };
    });

    expect(result.pwn1).toBeUndefined();
    expect(result.pwn2).toBeUndefined();
    expect(result.violations).toContain('script-src-attr');
    expect(result.violations).toContain('script-src-elem');

    await ctx.close();
  });

  test('the theme bootstrap inline script still runs (hash allowlisted)', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    // The bootstrap sets data-theme before first paint; if the CSP hash ever
    // drifts from the block, the attribute stays unset and this fails.
    const theme = await page.getAttribute('html', 'data-theme');
    expect(['auto', 'light', 'dark']).toContain(theme);
    await ctx.close();
  });
});
