// @ts-check
//
// Previewing a discovered (external) CLI session, discovery.js
// previewDiscovered: clicking the card paints the read-only panel (header
// with the cwd basename and the type chip, the events pane, a composer whose
// first send takes the session over), loads the transcript tail, and a 2s
// poll appends what the CLI writes afterwards.
//
// 跑法：cd test/e2e && npx playwright test discovered_preview.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'viewport-independent; desktop-chrome only');
  }
});

test('a discovered preview paints the panel, loads the tail and appends what the poll finds', async ({ browser }) => {
  const now = Date.now();
  const preview = [
    { type: 'user', summary: 'first question', detail: 'first question', time: now - 60000, uuid: 'dp-1' },
    { type: 'text', summary: 'first answer', detail: 'first answer', time: now - 50000, uuid: 'dp-2' },
  ];
  const mock = await startMockServer({
    discovered: [
      { pid: 777, session_id: 'disc-preview', cwd: '/home/user/workspace/terminalproj', proc_start_time: 1,
        node: 'local', cli_name: 'claude-code', type_label: 'Claude Terminal', state: 'ready', started_at: now - 70000 },
    ],
    discoveredPreview: preview,
  });
  const ctx = await browser.newContext({ viewport: { width: 1400, height: 900 } });
  const page = await ctx.newPage();
  try {
    await page.goto(mock.url + '/dashboard');
    await page.locator('.session-card[data-key^="_discovered:777"]').click();

    const main = page.locator('#main');
    await expect(main.locator('.main-header h2')).toHaveText('terminalproj');
    await expect(main.locator('.main-header .detail')).toContainText('Claude Terminal');
    await expect(main.locator('#msg-input')).toHaveAttribute('data-placeholder', 'send a message to take over...');
    await expect(main.locator('#nav-pill')).toHaveCount(1);

    const events = page.locator('#events-scroll');
    await expect(events).toContainText('first answer');
    await expect(events).not.toContainText('later answer');

    // The CLI writes more; the next poll tick appends it below what is shown.
    preview.push({ type: 'text', summary: 'later answer', detail: 'later answer', time: now - 1000, uuid: 'dp-3' });
    await expect(events).toContainText('later answer', { timeout: 6000 });
    const text = await events.innerText();
    expect(text.indexOf('first answer')).toBeLessThan(text.indexOf('later answer'));
    expect(text.split('first answer').length - 1, 'the poll appends only the new tail').toBe(1);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
