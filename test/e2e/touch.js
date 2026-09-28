// @ts-check
//
// Synthetic touch events that work in both engines the suite runs on.
// Chromium builds a Touch with `new Touch({...})` and takes plain arrays in
// TouchEventInit; WebKit rejects `new Touch` ("Illegal constructor") and needs
// document.createTouch + document.createTouchList. installTouch defines
// window.__nzTouch on the current page so a spec's page.evaluate can build
// events without caring which engine it is on.

/** @param {import('@playwright/test').Page} page */
async function installTouch(page) {
  await page.evaluate(() => {
    const w = /** @type {any} */ (window);
    if (w.__nzTouch) return;
    const d = /** @type {any} */ (document);
    w.__nzTouch = {
      /** @param {EventTarget} target @param {number} id @param {number} x @param {number} y */
      touch(target, id, x, y) {
        if (typeof d.createTouch === 'function') return d.createTouch(window, target, id, x, y, x, y);
        return new Touch({ identifier: id, target, clientX: x, clientY: y, pageX: x, pageY: y });
      },
      /** @param {Touch[]} touches */
      list(touches) {
        return typeof d.createTouchList === 'function' ? d.createTouchList(...touches) : touches;
      },
      /** @param {string} type @param {{touches?: Touch[], changedTouches?: Touch[], targetTouches?: Touch[]}} init */
      event(type, init) {
        const touches = init.touches || [];
        return new TouchEvent(type, {
          bubbles: true,
          cancelable: true,
          touches: this.list(touches),
          changedTouches: this.list(init.changedTouches || []),
          targetTouches: this.list(init.targetTouches || touches),
        });
      },
    };
  });
}

module.exports = { installTouch };
