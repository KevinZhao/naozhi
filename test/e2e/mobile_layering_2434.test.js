// @ts-check
// E2E: #2434 item 7 — mobile layering / visualViewport regressions.
//
//  a. .aside-drawer / .fv-drawer must follow --vv-top / --vv-height on phones
//     (same mechanism .main / .sidebar use) so the iOS soft keyboard does not
//     cover the drawer composer.
//  b. --nz-z-toast must be the topmost tier (above overlay AND lightbox): a
//     toast is a pointer-events:none notification and must never be hidden
//     behind the voice overlay or the image viewer — and, being topmost, it
//     must stay click-through while shown so it cannot swallow taps meant
//     for the lightbox toolbar underneath.
//  c. #split-resizer must paint above the .nz-split-front drawer, otherwise the
//     3px of the seam that overlap the front pane are swallowed and the drag
//     hit-area is halved.
//  d. A mobile .modal without a .modal-body must still let the user reach its
//     .modal-btns (outer scroll fallback) when the content is taller than the
//     85dvh cap.
const { test, expect, devices } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const iPhone = devices['iPhone 13'];
const desktop = { viewport: { width: 1280, height: 800 } };

let mock;
test.beforeAll(async () => { mock = await startMockServer(); });
// Not awaited: a failed test may leave a keep-alive socket open, and
// server.close() only resolves once every connection has drained.
test.afterAll(() => { mock.server.close(); });

async function openDashboard(browser, ctxOpts) {
  const ctx = await browser.newContext(ctxOpts);
  const page = await ctx.newPage();
  await page.goto(`${mock.url}/dashboard`);
  await page.waitForSelector('.session-card', { timeout: 5000 });
  return { ctx, page };
}

test('a. phone: drawers follow --vv-top/--vv-height like .main', async ({ browser }) => {
  const { ctx, page } = await openDashboard(browser, { ...iPhone });
  const rects = await page.evaluate(() => {
    const root = document.documentElement;
    root.style.setProperty('--vv-top', '40px');
    root.style.setProperty('--vv-height', '500px');
    const aside = /** @type {HTMLElement} */ (document.getElementById('aside-drawer'));
    const fv = /** @type {HTMLElement} */ (document.getElementById('fv-drawer'));
    aside.classList.add('visible');
    fv.classList.remove('hidden');
    fv.classList.add('fv-open');
    const pick = (el) => { const r = el.getBoundingClientRect(); return { top: r.top, height: r.height }; };
    return {
      main: pick(document.querySelector('.main')),
      aside: pick(aside),
      fv: pick(fv),
    };
  });
  expect(rects.main).toEqual({ top: 40, height: 500 });
  expect(rects.aside).toEqual(rects.main);
  expect(rects.fv).toEqual(rects.main);
  await ctx.close();
});

test('a2. landscape phone (769–950px, coarse pointer): drawers follow --vv-* like .main', async ({ browser }) => {
  // iPhone 13 rotated: 844px wide sits above the ≤768 breakpoint, so only the
  // landscape+coarse block can supply the vv rules.
  const { ctx, page } = await openDashboard(browser, { ...iPhone, viewport: { width: 844, height: 390 } });
  const r = await page.evaluate(() => {
    const mq = matchMedia('(max-width: 950px) and (orientation: landscape) and (pointer: coarse)').matches;
    const root = document.documentElement;
    root.style.setProperty('--vv-top', '30px');
    root.style.setProperty('--vv-height', '200px');
    const aside = /** @type {HTMLElement} */ (document.getElementById('aside-drawer'));
    const fv = /** @type {HTMLElement} */ (document.getElementById('fv-drawer'));
    aside.classList.add('visible');
    fv.classList.remove('hidden');
    fv.classList.add('fv-open');
    const pick = (el) => { const b = el.getBoundingClientRect(); return { top: b.top, height: b.height }; };
    return { mq, main: pick(document.querySelector('.main')), aside: pick(aside), fv: pick(fv) };
  });
  expect(r.mq).toBe(true);
  expect(r.main).toEqual({ top: 30, height: 200 });
  expect(r.aside).toEqual(r.main);
  expect(r.fv).toEqual(r.main);
  await ctx.close();
});

test('b. toast tier sits above overlay and lightbox', async ({ browser }) => {
  const { ctx, page } = await openDashboard(browser, { ...iPhone });
  const z = await page.evaluate(() => {
    const cs = getComputedStyle(document.documentElement);
    const n = (name) => parseInt(cs.getPropertyValue(name), 10);
    return {
      toast: n('--nz-z-toast'),
      menu: n('--nz-z-menu'),
      overlay: n('--nz-z-overlay'),
      lightbox: n('--nz-z-lightbox'),
      toastEl: parseInt(getComputedStyle(document.getElementById('toast')).zIndex, 10),
      voiceEl: parseInt(getComputedStyle(document.getElementById('voice-overlay')).zIndex, 10),
    };
  });
  expect(Number.isFinite(z.toast)).toBe(true);
  expect(z.toast).toBeGreaterThan(z.overlay);
  expect(z.toast).toBeGreaterThan(z.lightbox);
  expect(z.toastEl).toBeGreaterThan(z.voiceEl);
  // Topmost ⇒ must be click-through even while shown, or it would swallow
  // taps on the lightbox toolbar (top-right, same band as the phone toast).
  const shownPE = await page.evaluate(() => {
    const el = /** @type {HTMLElement} */ (document.getElementById('toast'));
    el.className = 'toast show error';
    const pe = getComputedStyle(el).pointerEvents;
    el.className = 'toast';
    return pe;
  });
  expect(shownPE).toBe('none');
  // The context menu keeps painting above its own click-away overlay.
  const ctxMenu = await page.evaluate(() => {
    const ov = document.createElement('div'); ov.className = 'ctx-menu-overlay';
    const m = document.createElement('div'); m.className = 'ctx-menu';
    document.body.append(ov, m);
    const r = {
      overlay: parseInt(getComputedStyle(ov).zIndex, 10),
      menu: parseInt(getComputedStyle(m).zIndex, 10),
    };
    ov.remove(); m.remove();
    return r;
  });
  expect(ctxMenu.menu).toBeGreaterThan(ctxMenu.overlay);
  expect(ctxMenu.menu).toBe(z.menu);
  await ctx.close();
});

test('c. desktop split: #split-resizer paints above the .nz-split-front drawer', async ({ browser }) => {
  const { ctx, page } = await openDashboard(browser, desktop);
  const z = await page.evaluate(() => {
    document.body.classList.add('nz-split-open');
    const fv = /** @type {HTMLElement} */ (document.getElementById('fv-drawer'));
    fv.classList.remove('hidden');
    fv.classList.add('fv-open', 'nz-split-front');
    const rs = /** @type {HTMLElement} */ (document.getElementById('split-resizer'));
    return {
      resizerDisplay: getComputedStyle(rs).display,
      resizer: parseInt(getComputedStyle(rs).zIndex, 10),
      front: parseInt(getComputedStyle(fv).zIndex, 10),
      drawerTier: parseInt(getComputedStyle(document.documentElement).getPropertyValue('--nz-z-drawer'), 10),
    };
  });
  expect(z.resizerDisplay).toBe('block');
  expect(z.front).toBeGreaterThan(z.drawerTier);
  expect(z.resizer).toBeGreaterThan(z.front);
  await ctx.close();
});

test('d. phone: tall .modal without .modal-body keeps .modal-btns reachable', async ({ browser }) => {
  const { ctx, page } = await openDashboard(browser, { ...iPhone });
  const r = await page.evaluate(() => {
    const overlay = document.createElement('div');
    overlay.className = 'modal-overlay';
    let body = '<h3>兜底 modal</h3>';
    for (let i = 0; i < 60; i++) body += '<p style="margin:0;height:30px">row ' + i + '</p>';
    overlay.innerHTML = '<div class="modal" role="dialog">' + body +
      '<div class="modal-btns"><button>取消</button><button class="primary" id="t2434-ok">确定</button></div></div>';
    document.body.appendChild(overlay);
    const modal = /** @type {HTMLElement} */ (overlay.querySelector('.modal'));
    // The phone bottom-sheet slides in via translateY(100%) → 0; kill the
    // animation so the rects below measure the settled layout.
    modal.style.animation = 'none';
    const btn = /** @type {HTMLElement} */ (document.getElementById('t2434-ok'));
    const before = btn.getBoundingClientRect();
    btn.scrollIntoView({ block: 'nearest' });
    const after = btn.getBoundingClientRect();
    const out = {
      vh: window.innerHeight,
      beforeBottom: before.bottom,
      afterTop: after.top,
      afterBottom: after.bottom,
      modalBottom: modal.getBoundingClientRect().bottom,
    };
    overlay.remove();
    return out;
  });
  // Sanity: content really overflows the 85dvh cap before scrolling.
  expect(r.beforeBottom).toBeGreaterThan(r.vh);
  // The modal box itself stays inside the viewport…
  expect(r.modalBottom).toBeLessThanOrEqual(r.vh + 1);
  // …and the confirm button can be scrolled into it.
  expect(r.afterTop).toBeGreaterThanOrEqual(0);
  expect(r.afterBottom).toBeLessThanOrEqual(r.vh + 1);
  await ctx.close();
});
