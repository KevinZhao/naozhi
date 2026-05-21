// @ts-check
//
// PR-1 (cron-history-redesign §6) — Run-Detail Sheet 共组件 e2e 实测。
//
// 目标：验证 sheet 桌面右滑 / 移动 bottom-sheet、五路关闭、↑↓ 切换、
// 选中态、ESC 优先级、focus 管理 等 §10 状态机定义的不变量。
//
// 跑法：
//   cd test/e2e && npx playwright test cron_run_sheet.test.js --project=desktop-chrome
//
// 移动端 case 用 chromium + iPhone 13 device emulation（webkit 在 Linux 服务器
// 缺系统依赖跑不起来）。整个 spec 仅在 desktop-chrome project 下运行；
// 通过 newContext({...mobile}) 切 viewport + UA 模拟 iOS Safari。
//
// 不依赖 NAOZHI_LIVE_E2E — 全部走 mock-server 的内存 cron-001 数据
// (5 条 recent_runs + /api/cron/runs/<id> 详情 endpoint)。

const { test, expect, devices } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

// 仅 desktop-chrome project 运行（移动 case 在内部用 device emulation）。
// mobile-safari project 上 webkit 依赖缺失会全炸。
test.beforeEach(({ browserName }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'cron_run_sheet 仅 desktop-chrome project 下跑');
  }
});

// PR-1 followup #4: detail-pane < 600 时 sheet 全占（防止挤出 ~180 timeline 中间态）。
// 测桌面双栏行为需要 detail-pane ≥ 600 — 1280 屏 - 320 sidebar = 960 main，
// drawer 列 ~360（list-pane）→ detail-pane ≈ 600，临界。提到 1600 避免抖动。
const desktop = { viewport: { width: 1600, height: 900 } };
// 窄桌面（detail < 600），用于测 followup #4 sheet 全占模式
const desktopNarrow = { viewport: { width: 1100, height: 800 } };
const mobile = devices['iPhone 13'];

// 进入 cron panel 并展开 cron-001 drawer 的公共序列
async function openCronDrawer(page) {
  await page.click('#btn-cron');
  await page.waitForSelector('.cron-detail');
  const row = page.locator('.cj-row[data-cron-id="cron-001"]');
  await expect(row).toBeVisible();
  await row.click();
  await page.waitForSelector('#cron-timeline-panel .ctr');
  // 至少 5 条（mock-server defaultCronRuns 现在生成 20 条，followup #1 实测需要）
  const count = await page.locator('#cron-timeline-panel .ctr').count();
  expect(count).toBeGreaterThanOrEqual(5);
}

// ─── Desktop ──────────────────────────────────────────────────────────────────

test.describe('Cron run-detail sheet — desktop', () => {
  /** @type {Awaited<ReturnType<typeof startMockServer>>} */
  let mock;
  test.beforeAll(async () => { mock = await startMockServer(); });
  test.afterAll(() => mock.server.close());

  test('点 timeline 行 sheet 滑出 + 显示状态/时长/详情', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await openCronDrawer(page);

    // sheet 默认 hidden
    const sheet = page.locator('#cron-run-sheet');
    await expect(sheet).toBeHidden();

    // 点第一条 run（最新一条 = run-aaaa1111 / 失败）
    await page.click('#cron-timeline-panel .ctr[data-run-id="run-aaaa1111"]');

    // sheet 滑出
    await expect(sheet).toBeVisible();
    await expect(sheet).toHaveClass(/is-open/);
    // 行进入选中态
    await expect(page.locator('#cron-timeline-panel .ctr[data-run-id="run-aaaa1111"]'))
      .toHaveClass(/is-selected/);

    // header 元数据
    await expect(page.locator('#crs-title')).toContainText('失败');
    await expect(page.locator('#crs-meta')).toContainText('31'); // 31s
    await expect(page.locator('#crs-meta')).toContainText('cron');

    // body 异步 fetch detail 后显示 prompt + error
    await expect(page.locator('#crs-body')).toContainText('check server status', { timeout: 5000 });
    await expect(page.locator('#crs-body')).toContainText('connection refused');

    await ctx.close();
  });

  test('桌面 sheet 浮在 detail-pane 右半，timeline 行左侧仍可点击（同行 toggle 关）', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await openCronDrawer(page);
    await page.click('#cron-timeline-panel .ctr[data-run-id="run-aaaa1111"]');
    const sheet = page.locator('#cron-run-sheet');
    await expect(sheet).toBeVisible();
    // 等动画完成
    await page.waitForTimeout(350);

    // 桌面 sheet 用 fixed + JS 同步坐标
    const pos = await sheet.evaluate(el => getComputedStyle(el).position);
    expect(pos).toBe('fixed');

    // 几何：sheet 必须**只占 detail-pane 右半**，左侧还能看到 timeline
    const geom = await page.evaluate(() => {
      const sb = document.getElementById('cron-run-sheet').getBoundingClientRect();
      const db = document.getElementById('cron-detail-pane').getBoundingClientRect();
      const rb = document.querySelector('#cron-timeline-panel .ctr[data-run-id="run-aaaa1111"]').getBoundingClientRect();
      return {
        sheetX: sb.x, sheetW: sb.width,
        detailX: db.x, detailW: db.width, detailRight: db.right,
        rowX: rb.x, rowW: rb.width,
      };
    });
    // sheet 右贴 detail 右
    expect(Math.abs(geom.sheetX + geom.sheetW - geom.detailRight)).toBeLessThan(2);
    // sheet 不超过 480px
    expect(geom.sheetW).toBeLessThanOrEqual(480);
    // sheet 不能完全遮 timeline 行（行左侧 ≥ 60px 应在 sheet 之外）
    expect(geom.sheetX - geom.rowX).toBeGreaterThan(60);

    // 列表 + drawer summary 都仍可见
    await expect(page.locator('.cj-row[data-cron-id="cron-001"]')).toBeVisible();
    await expect(page.locator('.cron-drawer-header')).toBeVisible();

    await ctx.close();
  });

  test('↑↓ 切上一条/下一条 run，prev/next disabled 同步 aria-disabled', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await openCronDrawer(page);

    // 从最新一条 (run-aaaa1111) 开始 — prev 应 disabled (top boundary)
    await page.click('#cron-timeline-panel .ctr[data-run-id="run-aaaa1111"]');
    await expect(page.locator('#cron-run-sheet')).toBeVisible();
    await expect(page.locator('#crs-prev')).toBeDisabled();
    await expect(page.locator('#crs-prev')).toHaveAttribute('aria-disabled', 'true');
    await expect(page.locator('#crs-next')).toBeEnabled();
    await expect(page.locator('#crs-next')).toHaveAttribute('aria-disabled', 'false');

    // ↓ 切到第 2 条 (run-bbbb2222 / succeeded)
    await page.keyboard.press('ArrowDown');
    await expect(page.locator('#cron-timeline-panel .ctr.is-selected'))
      .toHaveAttribute('data-run-id', 'run-bbbb2222');
    await expect(page.locator('#crs-title')).toContainText('成功');
    await expect(page.locator('#crs-prev')).toBeEnabled();
    await expect(page.locator('#crs-next')).toBeEnabled();

    // 一路 ↓ 到最后一条 — next 应 disabled。mock 现在 20 条 runs，
    // 用 last() locator 取末行避免硬编码 run_id。
    const totalRuns = await page.locator('#cron-timeline-panel .ctr').count();
    for (let i = 0; i < totalRuns - 2; i++) await page.keyboard.press('ArrowDown'); // 已在第 2 条
    const lastRow = page.locator('#cron-timeline-panel .ctr').last();
    const lastRunId = await lastRow.getAttribute('data-run-id');
    await expect(page.locator('#cron-timeline-panel .ctr.is-selected'))
      .toHaveAttribute('data-run-id', lastRunId || '');
    await expect(page.locator('#crs-next')).toBeDisabled();
    await expect(page.locator('#crs-next')).toHaveAttribute('aria-disabled', 'true');

    // ↑ 切回倒数第二条
    await page.keyboard.press('ArrowUp');
    const penult = await page.locator('#cron-timeline-panel .ctr').nth(totalRuns - 2).getAttribute('data-run-id');
    await expect(page.locator('#cron-timeline-panel .ctr.is-selected'))
      .toHaveAttribute('data-run-id', penult || '');

    await ctx.close();
  });

  test('五路关闭：ESC / ✕ / 同行二次点击 / 切 cron / 关 drawer', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await openCronDrawer(page);
    const sheet = page.locator('#cron-run-sheet');

    // (1) ESC 关 sheet（不关 drawer）
    await page.click('#cron-timeline-panel .ctr[data-run-id="run-aaaa1111"]');
    await expect(sheet).toBeVisible();
    await page.keyboard.press('Escape');
    await expect(sheet).not.toHaveClass(/is-open/);
    // drawer 还在
    await expect(page.locator('#cron-detail-pane.is-open')).toBeVisible();

    // (2) ✕ 关 sheet
    await page.click('#cron-timeline-panel .ctr[data-run-id="run-aaaa1111"]');
    await expect(sheet).toBeVisible();
    await page.click('#crs-close');
    await expect(sheet).not.toHaveClass(/is-open/);

    // (3) 同行二次点击 toggle 关 — 必须点行左侧（避开 sheet 浮层）
    await page.click('#cron-timeline-panel .ctr[data-run-id="run-aaaa1111"]', { position: { x: 30, y: 20 } });
    await expect(sheet).toHaveClass(/is-open/);
    await page.waitForTimeout(350); // 等 sheet 动画完成 + ResizeObserver 算坐标
    await page.click('#cron-timeline-panel .ctr[data-run-id="run-aaaa1111"]', { position: { x: 30, y: 20 } });
    await expect(sheet).not.toHaveClass(/is-open/);

    // (4) 切 cron 时 sheet 自动关
    await page.click('#cron-timeline-panel .ctr[data-run-id="run-aaaa1111"]');
    await expect(sheet).toBeVisible();
    await page.click('.cj-row[data-cron-id="cron-002"]');
    // cron-002 是 paused，没 recent_runs；sheet 应已关
    await expect(sheet).not.toHaveClass(/is-open/);

    // (5) 关 drawer 连带关 sheet — 先 ESC 关 sheet（必然路径，drawer ✕ 被 sheet 浮层挡），
    //     然后再 ESC 关 drawer。验证 closeCronDetail 调用时 sheet 也被清理（即使
    //     sheet 已关，state 一致性仍要检查）。
    await page.click('.cj-row[data-cron-id="cron-001"]');
    await page.waitForSelector('#cron-timeline-panel .ctr');
    await page.click('#cron-timeline-panel .ctr[data-run-id="run-aaaa1111"]');
    await expect(sheet).toBeVisible();
    await page.keyboard.press('Escape');         // 关 sheet
    await expect(sheet).not.toHaveClass(/is-open/);
    await page.locator('#cron-detail-pane .cdh-btn-icon[title^="关闭"]').click();
    await expect(page.locator('#cron-detail-pane.is-open')).toHaveCount(0);
    // sheet state 应彻底清理
    const sheetState = await page.evaluate(() => ({ open: window.cronRunSheetState && window.cronRunSheetState.open }));
    // cronRunSheetState 是 const 模块作用域，不挂 window。改用观察：sheet aria-hidden=true
    await expect(sheet).toHaveAttribute('aria-hidden', 'true');

    await ctx.close();
  });

  test('a11y: 焦点进 sheet 后落到 #crs-title (tabindex=-1)', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await openCronDrawer(page);
    await page.click('#cron-timeline-panel .ctr[data-run-id="run-aaaa1111"]');
    await expect(page.locator('#cron-run-sheet')).toBeVisible();
    // 焦点 50ms 后落到 crs-title — 用 wait 等到位
    await page.waitForFunction(() => document.activeElement && document.activeElement.id === 'crs-title');
    expect(await page.evaluate(() => document.activeElement.id)).toBe('crs-title');
    await ctx.close();
  });

  test('timeline 行不再 inline 展开 (ctr-detail 已删)', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await openCronDrawer(page);
    // 行下不应有 .ctr-detail 容器（v2 已移除）
    expect(await page.locator('#cron-timeline-panel .ctr-detail').count()).toBe(0);
    // 点行也不会 inject .ctr-detail
    await page.click('#cron-timeline-panel .ctr[data-run-id="run-aaaa1111"]');
    await expect(page.locator('#cron-run-sheet')).toBeVisible();
    expect(await page.locator('#cron-timeline-panel .ctr-detail').count()).toBe(0);
    await ctx.close();
  });

  // ── PR-1 followup ─────────────────────────────────────────────────────────

  test('followup #1: 首次打开 sheet 时选中行 scroll 到可视区（首/末行可能贴边）', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await openCronDrawer(page);
    // mock 20 条 runs。先点首行让 sheet 定位（避免 click 末行被 sheet 几何变化干扰）
    await page.click('#cron-timeline-panel .ctr[data-run-id="run-aaaa1111"]', { position: { x: 30, y: 20 } });
    await expect(page.locator('#cron-run-sheet')).toBeVisible();
    // 关 sheet
    await page.keyboard.press('Escape');
    await page.waitForTimeout(350);
    // 滚 timeline 到顶（保证末行在视野外）
    await page.evaluate(() => {
      const tl = document.querySelector('.cron-drawer-history');
      if (tl) tl.scrollTop = 0;
    });
    // 取末行 id 并点击它（点行左侧 30px 避开 sheet 区）
    const lastRow = page.locator('#cron-timeline-panel .ctr').last();
    const lastId = await lastRow.getAttribute('data-run-id');
    await lastRow.scrollIntoViewIfNeeded();
    await lastRow.click({ position: { x: 30, y: 20 } });
    await expect(page.locator('#cron-run-sheet')).toBeVisible();
    await page.waitForTimeout(150);
    // followup #1: 选中行 scroll 后应在 viewport 范围内（block:'center' 保证）
    const inView = await page.evaluate((id) => {
      const row = document.querySelector(`#cron-timeline-panel .ctr[data-run-id="${id}"]`);
      if (!row) return false;
      const r = row.getBoundingClientRect();
      return r.top >= 0 && r.bottom <= window.innerHeight;
    }, lastId);
    expect(inView).toBe(true);
    await ctx.close();
  });

  test('followup #2: sheet top 从 drawer-header 底部开始（drawer 头不被遮）', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await openCronDrawer(page);
    await page.click('#cron-timeline-panel .ctr[data-run-id="run-aaaa1111"]', { position: { x: 30, y: 20 } });
    await page.waitForTimeout(350);
    const m = await page.evaluate(() => {
      const sb = document.getElementById('cron-run-sheet').getBoundingClientRect();
      const hb = document.querySelector('.cron-drawer-header').getBoundingClientRect();
      return { sheetTop: sb.top, headerBottom: hb.bottom };
    });
    // sheet top 应 ≥ drawer header bottom（允许 1px 误差）
    expect(m.sheetTop).toBeGreaterThanOrEqual(m.headerBottom - 1);
  });

  test('followup #3: 选中行有强化视觉（左 4px 色条 + 蓝调背景 + 状态加粗）', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await openCronDrawer(page);
    await page.click('#cron-timeline-panel .ctr[data-run-id="run-aaaa1111"]', { position: { x: 30, y: 20 } });
    const styles = await page.evaluate(() => {
      const row = document.querySelector('#cron-timeline-panel .ctr.is-selected');
      const state = row.querySelector('.ctr-state');
      const cs = (el) => getComputedStyle(el);
      return {
        rowBg: cs(row).backgroundColor,
        rowShadow: cs(row).boxShadow,
        stateWeight: cs(state).fontWeight,
      };
    });
    // 蓝调背景（rgba(31,111,235,.12)）
    expect(styles.rowBg).toContain('rgba(31, 111, 235');
    // box-shadow inset 有 accent 色 + 4px 偏移（chrome 输出顺序：rgb 4px 0 0 0 inset）
    expect(styles.rowShadow).toMatch(/4px.*inset/);
    // state 字重 600
    expect(parseInt(styles.stateWeight, 10)).toBeGreaterThanOrEqual(600);
    await ctx.close();
  });

  test('followup #4: detail-pane < 600 时 sheet 全占（窄屏 fallback）', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktopNarrow });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await openCronDrawer(page);
    await page.click('#cron-timeline-panel .ctr[data-run-id="run-aaaa1111"]');
    await expect(page.locator('#cron-run-sheet')).toBeVisible();
    await page.waitForTimeout(350);
    const m = await page.evaluate(() => {
      const sb = document.getElementById('cron-run-sheet').getBoundingClientRect();
      const db = document.getElementById('cron-detail-pane').getBoundingClientRect();
      return { sheetW: sb.width, detailW: db.width, sheetX: sb.x, detailX: db.x };
    });
    // detail 应 < 600（fallback 触发条件）
    expect(m.detailW).toBeLessThan(600);
    // sheet 应填满 detail-pane 宽度（允许 1px 误差）
    expect(Math.abs(m.sheetW - m.detailW)).toBeLessThan(2);
    expect(Math.abs(m.sheetX - m.detailX)).toBeLessThan(2);
    await ctx.close();
  });
});

// ─── Mobile ───────────────────────────────────────────────────────────────────

test.describe('Cron run-detail sheet — mobile (iPhone 13)', () => {
  /** @type {Awaited<ReturnType<typeof startMockServer>>} */
  let mock;
  test.beforeAll(async () => { mock = await startMockServer(); });
  test.afterAll(() => mock.server.close());

  test('移动端 sheet 用 position:fixed 从底部滑入 + drag handle 可见', async ({ browser }) => {
    const ctx = await browser.newContext({ ...mobile });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await openCronDrawer(page);
    await page.click('#cron-timeline-panel .ctr[data-run-id="run-aaaa1111"]');

    const sheet = page.locator('#cron-run-sheet');
    await expect(sheet).toBeVisible();
    await expect(sheet).toHaveClass(/is-open/);

    // 移动断点下 position:fixed
    const pos = await sheet.evaluate(el => getComputedStyle(el).position);
    expect(pos).toBe('fixed');

    // sheet 从底部 (bottom:0)
    const box = await sheet.boundingBox();
    const viewport = page.viewportSize();
    expect(viewport).not.toBeNull();
    expect(box).not.toBeNull();
    if (box && viewport) {
      // sheet 底部贴 viewport 底（允许 1px 误差）
      expect(box.y + box.height).toBeGreaterThan(viewport.height - 2);
      // sheet 高度不超过 75vh
      expect(box.height).toBeLessThanOrEqual(viewport.height * 0.76);
    }

    // backdrop 可见
    await expect(page.locator('#cron-run-sheet-backdrop')).toBeVisible();
    await expect(page.locator('#cron-run-sheet-backdrop')).toHaveClass(/is-open/);

    await ctx.close();
  });

  test('点 backdrop 关 sheet', async ({ browser }) => {
    const ctx = await browser.newContext({ ...mobile });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await openCronDrawer(page);
    await page.click('#cron-timeline-panel .ctr[data-run-id="run-aaaa1111"]');
    const sheet = page.locator('#cron-run-sheet');
    await expect(sheet).toBeVisible();

    // 点 backdrop（位置在 sheet 之上的空白区域）
    await page.click('#cron-run-sheet-backdrop', { position: { x: 50, y: 50 } });
    await expect(sheet).not.toHaveClass(/is-open/);

    await ctx.close();
  });

  test('header 区域下滑 ≥ 80px 关 sheet', async ({ browser }) => {
    const ctx = await browser.newContext({ ...mobile, hasTouch: true });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await openCronDrawer(page);
    await page.click('#cron-timeline-panel .ctr[data-run-id="run-aaaa1111"]');
    const sheet = page.locator('#cron-run-sheet');
    await expect(sheet).toBeVisible();

    // 用 dispatchTouch 模拟 swipe down on header
    const header = page.locator('.crs-header');
    const box = await header.boundingBox();
    if (!box) throw new Error('crs-header bounding box null');
    const startX = box.x + box.width / 2;
    const startY = box.y + box.height / 2;
    // 模拟 touchstart → touchmove (dy=120) → touchend
    await page.touchscreen.tap(startX, startY); // 给个起点接触
    await page.evaluate(({ sx, sy }) => {
      const target = document.querySelector('.crs-header');
      if (!target) return;
      const fire = (type, y) => {
        const t = new Touch({ identifier: 1, target, clientX: sx, clientY: y, pageX: sx, pageY: y });
        const ev = new TouchEvent(type, {
          bubbles: true, cancelable: true,
          touches: type === 'touchend' ? [] : [t],
          targetTouches: type === 'touchend' ? [] : [t],
          changedTouches: [t],
        });
        target.dispatchEvent(ev);
      };
      fire('touchstart', sy);
      fire('touchmove', sy + 120);
      fire('touchend', sy + 120);
    }, { sx: startX, sy: startY });

    // 等动画 + state 切
    await page.waitForFunction(() => {
      const s = document.getElementById('cron-run-sheet');
      return s && !s.classList.contains('is-open');
    }, { timeout: 2000 });

    await expect(sheet).not.toHaveClass(/is-open/);
    await ctx.close();
  });
});
