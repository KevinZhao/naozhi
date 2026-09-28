// @ts-check
//
// Opening a session the way the layout in front of the test allows. On a
// phone the chat view replaces the list, so a second card is off screen until
// the in-app back button returns to the list; on desktop both stay visible.

const { expect } = require('@playwright/test');

/**
 * @param {import('@playwright/test').Page} page
 * @param {import('@playwright/test').Locator} card
 */
async function openSessionCard(page, card) {
  const inChat = await page.evaluate(() => document.body.classList.contains('mobile-chat-view'));
  if (inChat) {
    await page.locator('.main .btn-mobile-back').click();
    await expect(page.locator('body')).not.toHaveClass(/mobile-chat-view/);
  }
  await card.click();
}

module.exports = { openSessionCard };
