// Synthetic touch events that work in both engines the suite runs on.
// Chromium builds a Touch with `new Touch({...})` and takes plain arrays in
// TouchEventInit; WebKit rejects `new Touch` ("Illegal constructor") and needs
// document.createTouch + document.createTouchList. installTouch defines
// window.__nzTouch on the current page so a spec's page.evaluate can build
// events without caring which engine it is on.

async function installTouch(page) {
  await page.evaluate(() => {
    const w = window;
    if (w.__nzTouch) return;
    const d = document;
    w.__nzTouch = {
      touch(target, id, x, y) {
        if (typeof d.createTouch === 'function') return d.createTouch(window, target, id, x, y, x, y);
        return new Touch({ identifier: id, target, clientX: x, clientY: y, pageX: x, pageY: y });
      },
      list(touches) {
        return typeof d.createTouchList === 'function' ? d.createTouchList(...touches) : touches;
      },
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
