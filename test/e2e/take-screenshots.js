// Quick script to capture dashboard screenshots for UI/UX review
const { chromium, devices } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const path = require('path');

const SHOTS_DIR = path.join(__dirname, 'screenshots');
const iPhone = devices['iPhone 13'];

async function main() {
  const mock = await startMockServer();
  const browser = await chromium.launch();

  // ── Desktop screenshots ──
  const dCtx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const dp = await dCtx.newPage();

  // 1. Initial load - sidebar with sessions, empty main
  await dp.goto(mock.url + '/dashboard');
  await dp.waitForSelector('.session-card');
  await dp.screenshot({ path: path.join(SHOTS_DIR, '01-desktop-initial.png') });

  // 2. Session selected - events displayed
  await dp.click('.session-card[data-key="dashboard:direct:2026-01-01-120000-1:myproject"]');
  await dp.waitForSelector('.event');
  await dp.waitForTimeout(300);
  await dp.screenshot({ path: path.join(SHOTS_DIR, '02-desktop-session-selected.png') });

  // 3. Running session with stop button
  await dp.click('.session-card[data-key="dashboard:direct:2026-01-01-120001-2:otherproject"]');
  await dp.waitForSelector('#btn-stop');
  await dp.waitForTimeout(300);
  await dp.screenshot({ path: path.join(SHOTS_DIR, '03-desktop-running-session.png') });

  // 4. New session modal (project picker)
  await dp.click('.hdr-btn[title="New Session"]');
  await dp.waitForSelector('.modal-overlay');
  await dp.waitForTimeout(200);
  await dp.screenshot({ path: path.join(SHOTS_DIR, '04-desktop-new-session-modal.png') });
  await dp.click('.modal-btns button:not(.primary)');
  await dp.waitForTimeout(200);

  // 5. Cron panel
  await dp.click('#btn-cron');
  await dp.waitForSelector('.cron-detail');
  await dp.waitForTimeout(500);
  await dp.screenshot({ path: path.join(SHOTS_DIR, '05-desktop-cron-panel.png') });

  // 6. History popover
  await dp.click('.session-card[data-key="dashboard:direct:2026-01-01-120000-1:myproject"]');
  await dp.waitForSelector('.event');
  await dp.waitForTimeout(200);
  await dp.click('#btn-history');
  await dp.waitForSelector('.history-popover');
  await dp.waitForTimeout(200);
  await dp.screenshot({ path: path.join(SHOTS_DIR, '06-desktop-history-popover.png') });
  await dp.click('#btn-history');
  await dp.waitForTimeout(200);

  // 7. Voice mode
  await dp.click('#btn-mic');
  await dp.waitForTimeout(200);
  await dp.screenshot({ path: path.join(SHOTS_DIR, '07-desktop-voice-mode.png') });
  await dp.click('#btn-mic');

  // 8. Toast notification
  await dp.evaluate(() => showToast('Example error notification', 'error'));
  await dp.waitForTimeout(100);
  await dp.screenshot({ path: path.join(SHOTS_DIR, '08-desktop-toast.png') });

  await dCtx.close();

  // ── Auth modal ──
  const authMock = await startMockServer({ requireAuth: true, authToken: 'secret' });
  const aCtx = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  const ap = await aCtx.newPage();
  await ap.goto(authMock.url + '/dashboard');
  await ap.waitForSelector('.modal-overlay');
  await ap.waitForTimeout(200);
  await ap.screenshot({ path: path.join(SHOTS_DIR, '09-desktop-auth-modal.png') });
  await aCtx.close();
  authMock.server.close();

  // ── Mobile screenshots ──
  const mCtx = await browser.newContext({ ...iPhone });
  const mp = await mCtx.newPage();

  // 10. Mobile list view
  await mp.goto(mock.url + '/dashboard');
  await mp.waitForSelector('.session-card');
  await mp.screenshot({ path: path.join(SHOTS_DIR, '10-mobile-list-view.png') });

  // 11. Mobile chat view
  await mp.click('.session-card[data-key="dashboard:direct:2026-01-01-120000-1:myproject"]');
  await mp.waitForSelector('.event');
  await mp.waitForTimeout(400);
  await mp.screenshot({ path: path.join(SHOTS_DIR, '11-mobile-chat-view.png') });

  // 12. Mobile new session modal
  await mp.click('.btn-mobile-back');
  await mp.waitForTimeout(300);
  await mp.click('.hdr-btn[title="New Session"]');
  await mp.waitForSelector('.modal-overlay');
  await mp.waitForTimeout(200);
  await mp.screenshot({ path: path.join(SHOTS_DIR, '12-mobile-new-session-modal.png') });

  await mCtx.close();
  await browser.close();
  mock.server.close();
  console.log('Done! Screenshots saved to', SHOTS_DIR);
}

main().catch(e => { console.error(e); process.exit(1); });
