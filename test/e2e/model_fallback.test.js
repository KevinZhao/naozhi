// E2E: a model_fallback entry names the model the CLI switched to and why
// (event_render.js modelFallbackHtml); the CLI's explanation is the title. An
// entry logged before the frame was parsed (a bare system row) still reads
// 模型回退 rather than the subtype.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');
const { openSessionCard } = require('./session_nav');

const A = 'dashboard:direct:2026-01-01-120000-1:myproject';
const now = Date.now();

test('a model fallback shows both models and why', async ({ page }) => {
  const mock = await startMockServer({ ws: true });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await openSessionCard(page, page.locator(`.session-card[data-key="${A}"]`));
    const conn = mock.wsConnections[mock.wsConnections.length - 1];
    await expect.poll(() => conn.messages.filter((m) => m.type === 'subscribe' && m.key === A).length).toBe(1);
    conn.send({ type: 'subscribed', key: A, state: 'ready' });
    conn.send({ type: 'history', key: A, initial: true, events: [
      { type: 'user', detail: '你好', time: now - 60000, uuid: 'u1' },
      { type: 'system', summary: 'model_fallback', time: now - 50000, uuid: 's1' },
      { type: 'model_fallback', summary: 'claude-sonnet-5-5 → global.anthropic.claude-sonnet-5-5', reason: 'last_resort',
        detail: 'Switched to Sonnet 5.5 because Sonnet 5.5 returned an error', time: now - 40000, uuid: 'f1' },
      { type: 'text', detail: '你好！', time: now - 30000, uuid: 't1' },
    ] });

    const row = page.locator('#events-scroll .event.model_fallback');
    await expect(row).toHaveCount(1);
    await expect(row).toContainText('模型回退 claude-sonnet-5-5 → global.anthropic.claude-sonnet-5-5（不可重试的错误）');
    await expect(row.locator('[title]').first()).toHaveAttribute('title', /returned an error/);
    await expect(page.locator('#events-scroll .event.system')).toContainText('模型回退');
    await expect(page.locator('#events-scroll')).not.toContainText('model_fallback');
  } finally {
    mock.server.close();
  }
});
