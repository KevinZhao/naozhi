// E2E: the chat composer across a session switch. The view has one composer
// and one attachment list, rebuilt for whichever session is selected, so each
// way off a session (another card, a new session, the cron view, another view)
// must take its draft and attachments with it and give them back on return,
// and a send that is still out when the user switches must finish on its own
// session.
//
// 跑法：cd test/e2e && npx playwright test composer_switch.test.js --project=desktop-chrome
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const desktop = { viewport: { width: 1280, height: 800 } };
const A = 'dashboard:direct:2026-01-01-120000-1:myproject';
const B = 'dashboard:direct:2026-01-01-120002-3:myproject';
const PNG_1X1 = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=';

const card = (page, key) => page.locator('.session-card[data-key="' + key + '"]');
const inputText = (page) => page.locator('#msg-input').evaluate((el) => el.innerText.trim());

async function open(page, key) {
  await card(page, key).click();
  await expect(card(page, key)).toHaveClass(/\bactive\b/);
  await expect(page.locator('#msg-input')).toBeVisible();
}

test('an attachment stays with its session: not shown in, nor sent with, another session', async ({ browser }) => {
  const ctx = await browser.newContext(desktop);
  const page = await ctx.newPage();
  const mock = await startMockServer({ uploads: true });
  try {
    await page.goto(mock.url + '/dashboard');
    await open(page, A);
    await page.locator('#file-input').setInputFiles({ name: 'a.png', mimeType: 'image/png', buffer: Buffer.from(PNG_1X1, 'base64') });
    await expect(page.locator('#file-preview .file-thumb.ready')).toHaveCount(1);

    await open(page, B);
    await expect(page.locator('#file-preview .file-thumb')).toHaveCount(0);
    mock.resetCalls();
    await page.locator('#msg-input').click();
    await page.keyboard.type('only text for B');
    await page.keyboard.press('Enter');
    await expect.poll(() => mock.sendCalls.length).toBe(1);
    const sentB = JSON.parse(mock.sendCalls[0]);
    expect(sentB.key).toBe(B);
    expect(sentB.file_ids).toBeUndefined();

    await open(page, A);
    await expect(page.locator('#file-preview .file-thumb.ready')).toHaveCount(1);
    await page.locator('#msg-input').click();
    await page.keyboard.press('Enter');
    await expect.poll(() => mock.sendCalls.length).toBe(2);
    const sentA = JSON.parse(mock.sendCalls[1]);
    expect(sentA.key).toBe(A);
    expect(sentA.file_ids).toEqual([mock.uploadCalls[0].id]);
    await expect(page.locator('#file-preview .file-thumb')).toHaveCount(0);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('a new session keeps the draft of the one left', async ({ browser }) => {
  const ctx = await browser.newContext(desktop);
  const page = await ctx.newPage();
  const mock = await startMockServer();
  try {
    await page.goto(mock.url + '/dashboard');
    await open(page, A);
    await page.locator('#msg-input').click();
    await page.keyboard.type('draft of A');
    await page.click('.hdr-btn[title="New Session"]');
    await page.locator('.cmd-palette-item', { hasText: 'myproject' }).first().click();
    await page.waitForSelector('.cmd-palette-overlay', { state: 'detached' });
    await expect.poll(() => page.evaluate(() => nz.test.selectedKey)).not.toBe(A);
    expect(await inputText(page)).toBe('');

    await open(page, A);
    expect(await inputText(page)).toBe('draft of A');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('back from the cron view, the session left is not shown as if still selected; its draft comes back with it', async ({ browser }) => {
  const ctx = await browser.newContext(desktop);
  const page = await ctx.newPage();
  const mock = await startMockServer();
  try {
    await page.goto(mock.url + '/dashboard');
    await open(page, A);
    await page.locator('#msg-input').click();
    await page.keyboard.type('draft before cron');
    await page.locator('#abnav-cron').click();
    await expect(page.locator('body')).toHaveClass(/nz-view-cron/);
    await page.locator('#abnav-chat').click();
    await expect(page.locator('body')).not.toHaveClass(/nz-view-cron/);
    // No session is selected, so no composer that would send nothing.
    await expect(page.locator('#main #msg-input')).toHaveCount(0);
    await expect(page.locator('#quick-ask-form')).toBeVisible();

    await open(page, A);
    expect(await inputText(page)).toBe('draft before cron');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('a multi-line draft keeps its lines when a session is opened from another view', async ({ browser }) => {
  const ctx = await browser.newContext(desktop);
  const page = await ctx.newPage();
  const mock = await startMockServer();
  try {
    await page.goto(mock.url + '/dashboard');
    await open(page, A);
    // A multi-line paste (execCommand insertText) leaves the lines as blocks.
    await page.locator('#msg-input').evaluate((el) => { el.innerHTML = 'line one<div>line two</div>'; });
    await page.locator('#abnav-settings').click();
    await expect(page.locator('body')).toHaveClass(/nz-view-settings/);
    // As the system view's health table and the Planner page open a session.
    await page.evaluate((k) => nz.test.selectSession(k, 'local'), B);
    await expect(card(page, B)).toHaveClass(/\bactive\b/);

    await open(page, A);
    expect(await inputText(page)).toBe('line one\nline two');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});
