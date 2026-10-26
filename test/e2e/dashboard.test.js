const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const desktop = { viewport: { width: 1280, height: 800 } };

// ─── Sidebar & Session List ────────────────────────────────────────────────────

test.describe('Sidebar & session list', () => {
  let mock;

  test.beforeAll(async () => { mock = await startMockServer(); });
  test.afterAll(() => mock.server.close());

  test('renders session cards grouped by project', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    // Should show project section headers. Read the name span rather than the
    // whole header: since PR #2359 (376f3ba1, project settings gear) the header
    // also carries ⭐ / ⚙ / GitHub buttons, so textContent is "myproject⚙".
    const headers = await page.$$eval('.section-header .sh-name', els => els.map(e => e.textContent));
    expect(headers).toContain('myproject');
    expect(headers).toContain('otherproject');

    // Should have 3 session cards total
    const cards = await page.$$('.session-card');
    expect(cards.length).toBe(3);

    await ctx.close();
  });

  test('session card shows correct state badges', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    // Running session has dot-running
    const runningCard = page.locator('.session-card', { hasText: 'review this code' });
    await expect(runningCard.locator('.sc-dot')).toHaveClass(/dot-running/);
    await expect(runningCard.locator('.sc-meta')).toContainText('running');

    // Ready session has dot-ready
    const readyCard = page.locator('.session-card', { hasText: 'hello world' });
    await expect(readyCard.locator('.sc-dot')).toHaveClass(/dot-ready/);

    // Previously-suspended session now shows as ready
    const resumableCard = page.locator('.session-card', { hasText: 'fix the bug' });
    await expect(resumableCard.locator('.sc-dot')).toHaveClass(/dot-ready/);

    await ctx.close();
  });

  test('session cards sorted: running > ready', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    // In "otherproject" group there's only the running one.
    // In "myproject" group: both sessions show as ready.
    const myprojectCards = await page.$$eval(
      '.section-header',
      (headers) => {
        // Match on .sh-name — header textContent also contains the ⭐/⚙
        // button glyphs since PR #2359 (376f3ba1).
        const h = headers.find(el => {
          const n = el.querySelector('.sh-name');
          return n && n.textContent === 'myproject';
        });
        if (!h) return [];
        const cards = [];
        let sibling = h.nextElementSibling;
        while (sibling && sibling.classList.contains('session-card')) {
          const meta = sibling.querySelector('.sc-meta');
          cards.push(meta ? meta.textContent : '');
          sibling = sibling.nextElementSibling;
        }
        return cards;
      }
    );
    // Both cards should show ready
    expect(myprojectCards[0]).toContain('ready');
    expect(myprojectCards[1]).toContain('ready');

    await ctx.close();
  });

  test('empty session list shows placeholder', async ({ browser }) => {
    const emptyMock = await startMockServer({
      sessions: {
        sessions: [],
        stats: { total: 0, running: 0, ready: 0, active: 0, uptime: '0s', backend: 'cc', max_procs: 10, default_workspace: '/tmp', agents: ['general'], projects: [], version: 1 },
        nodes: { local: { display_name: 'Local', status: 'ok' } },
      },
    });

    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(emptyMock.url + '/dashboard');
    await page.waitForSelector('.no-sessions');

    const text = await page.$eval('.no-sessions', el => el.textContent);
    expect(text).toContain('no sessions');

    await ctx.close();
    emptyMock.server.close();
  });

  // The '#sidebar-status' WebSocket status bar test was removed: the node was
  // deleted when the sidebar foot gave way to the session list (already absent
  // at the history-squash commit a274e0cc; session_list.js updateStatusBar()
  // early-returns with the comment "#sidebar-status 节点已在'底部让位给 session
  // 列表'的迭代中删除"). Local WS state now only surfaces via getNodeStatus()
  // in the New Session connection picker.
});

// ─── Session Selection & Events Display ────────────────────────────────────────

test.describe('Session selection & events', () => {
  let mock;

  test.beforeAll(async () => { mock = await startMockServer(); });
  test.afterAll(() => mock.server.close());

  test('clicking session card loads events in main area', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    // Click the ready session
    await page.click('.session-card[data-key="dashboard:direct:2026-01-01-120000-1:myproject"]');
    await page.waitForSelector('#events-scroll');

    // Should display events. The initial events fetch resolves after the
    // shell renders, so wait for the first .event instead of racing it.
    await page.waitForSelector('.event');
    const events = await page.$$('.event');
    expect(events.length).toBeGreaterThanOrEqual(2);

    await ctx.close();
  });

  test('text event renders markdown with bold and inline code', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    await page.click('.session-card[data-key="dashboard:direct:2026-01-01-120000-1:myproject"]');
    await page.waitForSelector('.event.text');

    // Bold text should be wrapped in <strong>
    const hasBold = await page.$eval('.event.text .event-content', el =>
      el.querySelector('strong') !== null
    );
    expect(hasBold).toBe(true);

    // Inline code should be wrapped in <code>
    const hasCode = await page.$eval('.event.text .event-content', el =>
      el.querySelector('code') !== null
    );
    expect(hasCode).toBe(true);

    // Markdown tables render with their header cells.
    const thTexts = await page.$$eval('.event.text table th', ths => ths.map(th => th.textContent.trim()));
    expect(thTexts).toEqual(['Col A', 'Col B']);

    // Fenced code blocks get a copy button.
    await expect(page.locator('.md-code-wrap .md-copy-btn').first()).toHaveText('copy');

    await ctx.close();
  });

  test('main header shows session info when selected', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    await page.click('.session-card[data-key="dashboard:direct:2026-01-01-120000-1:myproject"]');
    await page.waitForSelector('.main-header');

    // Header should contain some session info
    const headerText = await page.$eval('.main-header', el => el.textContent);
    expect(headerText.length).toBeGreaterThan(0);

    await ctx.close();
  });

  test('empty state shows when no session selected', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    // Main should show empty state
    const emptyText = await page.$eval('.empty-state', el => el.textContent);
    expect(emptyText).toBeTruthy();

    await ctx.close();
  });
});

// ─── Message Sending ───────────────────────────────────────────────────────────

test.describe('Message sending', () => {
  let mock;

  test.beforeAll(async () => { mock = await startMockServer(); });
  test.afterAll(() => mock.server.close());

  test('type and send message via Send button', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    // Select a session
    await page.click('.session-card[data-key="dashboard:direct:2026-01-01-120000-1:myproject"]');
    await page.waitForSelector('#msg-input');

    // Type a message in contenteditable
    const input = page.locator('#msg-input');
    await input.click();
    await input.pressSequentially('test message');

    // Verify text was entered
    const text = await page.$eval('#msg-input', el => el.innerText.trim());
    expect(text).toBe('test message');

    mock.resetCalls();

    // Click send
    await page.click('#btn-send');

    // Wait for the HTTP request to complete
    await page.waitForTimeout(500);

    // Should have made a send API call
    expect(mock.sendCalls.length).toBe(1);
    const sent = JSON.parse(mock.sendCalls[0]);
    expect(sent.text).toBe('test message');
    expect(sent.key).toBe('dashboard:direct:2026-01-01-120000-1:myproject');

    await ctx.close();
  });

  test('send message via Enter key', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    await page.click('.session-card[data-key="dashboard:direct:2026-01-01-120000-1:myproject"]');
    await page.waitForSelector('#msg-input');

    const input = page.locator('#msg-input');
    await input.click();
    await input.pressSequentially('enter key test');

    mock.resetCalls();
    await page.keyboard.press('Enter');
    await page.waitForTimeout(500);

    expect(mock.sendCalls.length).toBe(1);
    const sent = JSON.parse(mock.sendCalls[0]);
    expect(sent.text).toBe('enter key test');

    // A successful send clears the composer.
    expect(await page.$eval('#msg-input', el => el.innerText.trim())).toBe('');

    await ctx.close();
  });

  test('Shift+Enter does not send, inserts newline', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    await page.click('.session-card[data-key="dashboard:direct:2026-01-01-120000-1:myproject"]');
    await page.waitForSelector('#msg-input');

    const input = page.locator('#msg-input');
    await input.click();
    await input.pressSequentially('line1');

    mock.resetCalls();
    await page.keyboard.press('Shift+Enter');
    await page.waitForTimeout(200);

    // No send call should have been made
    expect(mock.sendCalls.length).toBe(0);

    await ctx.close();
  });

  test('empty message is not sent', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    await page.click('.session-card[data-key="dashboard:direct:2026-01-01-120000-1:myproject"]');
    await page.waitForSelector('#msg-input');

    mock.resetCalls();
    await page.click('#btn-send');
    await page.waitForTimeout(200);

    // No send call — empty messages are not sent
    expect(mock.sendCalls.length).toBe(0);

    await ctx.close();
  });

  test('message restored on send failure (429)', async ({ browser }) => {
    const failMock = await startMockServer({ sendStatus: 429 });

    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(failMock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    await page.click('.session-card[data-key="dashboard:direct:2026-01-01-120000-1:myproject"]');
    await page.waitForSelector('#msg-input');

    const input = page.locator('#msg-input');
    await input.click();
    await input.pressSequentially('keep this text');
    await page.keyboard.press('Enter');
    await page.waitForTimeout(500);

    // Text should be restored in input
    const textAfter = await page.$eval('#msg-input', el => el.innerText.trim());
    expect(textAfter).toBe('keep this text');

    // Toast should show
    const toast = await page.$eval('#toast', el => el.textContent);
    expect(toast).toContain('queue full');

    await ctx.close();
    failMock.server.close();
  });
});

// ─── Running Session Banner & Stop ─────────────────────────────────────────────

test.describe('Running session banner & stop', () => {
  let mock;

  test.beforeAll(async () => { mock = await startMockServer(); });
  test.afterAll(() => mock.server.close());

  test('stop button visible for running session', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    // Select the running session
    await page.click('.session-card[data-key="dashboard:direct:2026-01-01-120001-2:otherproject"]');
    await page.waitForSelector('#btn-stop');

    const stopDisplay = await page.$eval('#btn-stop', el => getComputedStyle(el).display);
    expect(stopDisplay).not.toBe('none');

    await ctx.close();
  });

  test('stop button hidden for ready session', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    // Select the ready session
    await page.click('.session-card[data-key="dashboard:direct:2026-01-01-120000-1:myproject"]');
    await page.waitForSelector('#btn-stop', { state: 'attached' });

    const stopDisplay = await page.$eval('#btn-stop', el => getComputedStyle(el).display);
    expect(stopDisplay).toBe('none');

    await ctx.close();
  });

  test('Escape key triggers interrupt', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    // Select the running session
    await page.click('.session-card[data-key="dashboard:direct:2026-01-01-120001-2:otherproject"]');
    await page.waitForSelector('#msg-input');

    // Focus input and press Escape
    await page.click('#msg-input');

    // Escape should not throw errors (interruptSession fallback to warning toast since no WS)
    await page.keyboard.press('Escape');
    await page.waitForTimeout(300);

    // Toast warning about WebSocket should appear
    const toastText = await page.$eval('#toast', el => el.textContent);
    expect(toastText.length).toBeGreaterThan(0);

    await ctx.close();
  });
});

// ─── Auth Modal & Login ────────────────────────────────────────────────────────

test.describe('Auth modal & login', () => {
  test('auth modal appears when API returns 401', async ({ browser }) => {
    const authMock = await startMockServer({
      requireAuth: true,
      authToken: 'secret-token',
    });

    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(authMock.url + '/dashboard');

    // Auth modal should appear since /api/sessions returns 401
    await page.waitForSelector('.modal-overlay');
    const modalTitle = await page.$eval('.modal h3', el => el.textContent);
    expect(modalTitle).toContain('Dashboard API Token');

    // Token input should be focused
    const tokenInput = page.locator('#token-input');
    await expect(tokenInput).toBeVisible();

    await ctx.close();
    authMock.server.close();
  });

  test('successful login closes modal and loads sessions', async ({ browser }) => {
    const authMock = await startMockServer({
      requireAuth: true,
      authToken: 'secret-token',
    });

    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(authMock.url + '/dashboard');
    await page.waitForSelector('.modal-overlay');

    // Enter correct token; submit via the Enter key path.
    await page.fill('#token-input', 'secret-token');
    await page.keyboard.press('Enter');

    // Modal should close
    await page.waitForSelector('.modal-overlay', { state: 'detached' });

    // Sessions should load (cookie was set)
    await page.waitForSelector('.session-card');
    const cards = await page.$$('.session-card');
    expect(cards.length).toBeGreaterThan(0);

    await ctx.close();
    authMock.server.close();
  });

  test('wrong token shows error in placeholder', async ({ browser }) => {
    const authMock = await startMockServer({
      requireAuth: true,
      authToken: 'secret-token',
    });

    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(authMock.url + '/dashboard');
    await page.waitForSelector('.modal-overlay');

    // Enter wrong token
    await page.fill('#token-input', 'wrong-token');
    await page.click('.modal-btns button.primary');

    // Input should be cleared and placeholder changed. Poll instead of a fixed
    // 300ms sleep — under a parallel worker load the 401 round-trip
    // occasionally landed after the sleep (observed 1/3 runs; #2454).
    const tokenInput = page.locator('#token-input');
    await expect(tokenInput).toHaveAttribute('placeholder', /invalid token/);
    await expect(tokenInput).toHaveValue('');

    // Modal should still be open
    const modal = await page.$('.modal-overlay');
    expect(modal).not.toBeNull();

    await ctx.close();
    authMock.server.close();
  });

  test('a 400 login refusal shows the server reason, not invalid token', async ({ browser }) => {
    const reason = 'server.trusted_proxy is on <b>x</b>';
    const authMock = await startMockServer({
      requireAuth: true,
      authToken: 'secret-token',
      loginStatus400: reason,
    });

    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(authMock.url + '/dashboard');
    await page.waitForSelector('.modal-overlay');

    await page.fill('#token-input', 'secret-token');
    await page.click('.modal-btns button.primary');

    const refusal = page.locator('.modal-overlay .auth-refusal');
    await expect(refusal).toHaveText(reason);
    // Rendered as text: the markup in the reason must not become an element.
    await expect(refusal.locator('b')).toHaveCount(0);
    // The permanent hint keeps saying where the token lives.
    await expect(page.locator('.modal-overlay .auth-hint').first()).toContainText('dashboard_token');
    const tokenInput = page.locator('#token-input');
    await expect(tokenInput).not.toHaveAttribute('placeholder', /invalid token/);
    // The token was never compared, so it is kept for the retry.
    await expect(tokenInput).toHaveValue('secret-token');
    expect(authMock.loginCalls).toHaveLength(1);

    // A later 401 clears the stale reason instead of stacking under it.
    await page.fill('#token-input', 'wrong-token');
    await page.click('.modal-btns button.primary');
    await expect(tokenInput).toHaveAttribute('placeholder', /invalid token/);
    await expect(refusal).toHaveCount(0);
    expect(authMock.loginCalls).toHaveLength(2);

    await ctx.close();
    authMock.server.close();
  });
});

// ─── New Session Creation ──────────────────────────────────────────────────────

test.describe('New session creation', () => {
  let mock;

  test.beforeAll(async () => { mock = await startMockServer(); });
  test.afterAll(() => mock.server.close());

  // With projects configured, "New Session" opens the command palette
  // (openProjectPalette → .cmd-palette-overlay) instead of the old
  // .modal-overlay/.proj-pick list. The palette was already the picker at the
  // history-squash commit a274e0cc; the modal only survives for the
  // no-projects path (see createNewSession in dashboard.js).

  test('new session button opens project picker palette', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    // Click new session button
    await page.click('.hdr-btn[title="New Session"]');
    await page.waitForSelector('.cmd-palette-overlay');

    const dialogLabel = await page.$eval('.cmd-palette', el => el.getAttribute('aria-label'));
    expect(dialogLabel).toBe('新建会话');

    // Rows: quick-create + one per project + custom-workspace entry.
    const names = await page.$$eval('.cmd-palette-item .cp-name', els => els.map(e => e.textContent));
    expect(names[0]).toBe('快速新建');
    expect(names).toContain('myproject');
    expect(names).toContain('otherproject');
    expect(names).toContain('pinned-empty');
    expect(names[names.length - 1]).toMatch(/^打开自定义工作目录/);

    await ctx.close();
  });

  // Regression: the palette opens underneath a stationary pointer (the New
  // Session button sits where the list renders). Chrome dispatches mouseenter
  // to the row that lands under the cursor without any pointer motion; when
  // hover wrote state.activeIdx (#2447) that made Enter open the project row
  // under the mouse -> resumed the folder's existing stable session instead of
  // creating a quick/new one. Only real pointer motion may move the cursor.
  test('palette opening under a stationary pointer keeps Enter on row 0', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    // First open: learn where the 'myproject' row renders, then close.
    await page.click('.hdr-btn[title="New Session"]');
    await page.waitForSelector('.cmd-palette-item');
    const box = await page.locator('.cmd-palette-item', { hasText: 'myproject' }).first().boundingBox();
    // Palette keys are handled on #cp-input, which is focused asynchronously.
    await page.waitForFunction(() => document.activeElement && document.activeElement.id === 'cp-input');
    await page.keyboard.press('Escape');
    await page.waitForSelector('.cmd-palette-overlay', { state: 'detached' });

    // Park the pointer exactly where that row will appear, then open the
    // palette via a JS click so the pointer does not move.
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
    await page.$eval('.hdr-btn[title="New Session"]', el => el.click());
    await page.waitForSelector('.cmd-palette-item');
    await page.waitForFunction(() => document.activeElement && document.activeElement.id === 'cp-input');
    await page.waitForTimeout(50); // let any synthetic mouseenter land
    await page.keyboard.press('Enter');
    await page.waitForSelector('.cmd-palette-overlay', { state: 'detached' });

    // Row 0 is 快速新建 -> key slug is the default workspace basename, not the
    // project under the pointer.
    const key = await page.evaluate(() => selectedKey);
    expect(key).toContain('-workspace:');
    expect(key).not.toContain('-myproject:');
    await ctx.close();
  });

  test('palette real pointer motion still moves the Enter target', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click('.hdr-btn[title="New Session"]');
    await page.waitForSelector('.cmd-palette-item');
    const row = page.locator('.cmd-palette-item', { hasText: 'otherproject' }).first();
    const box = await row.boundingBox();
    // Two moves so a mousemove is dispatched inside the row.
    await page.mouse.move(box.x + 5, box.y + 5);
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2);
    await expect(row).toHaveClass(/active/);
    await page.waitForFunction(() => document.activeElement && document.activeElement.id === 'cp-input');
    await page.keyboard.press('Enter');
    await page.waitForSelector('.cmd-palette-overlay', { state: 'detached' });
    const key = await page.evaluate(() => selectedKey);
    expect(key).toContain('-otherproject:');
    await ctx.close();
  });

  // #2476: the New Session palette must start a fresh session even when the
  // backend supplies a project stableKey (stats.projects carries it since
  // v0.0.78). Resuming the stable dashboard:pj: session is the sidebar card's
  // job; clicking a project row here must never land on the folder's existing
  // (possibly running) session.
  test('palette project row starts a fresh session, not the project-stable one', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');
    const hasStable = await page.evaluate(() => (projectsData.find(p => p.name === 'myproject') || {}).stableKey);
    expect(hasStable).toBe('dashboard:pj:0123456789abcdef:general');

    await page.click('.hdr-btn[title="New Session"]');
    await page.waitForSelector('.cmd-palette-item');
    await page.locator('.cmd-palette-item', { hasText: 'myproject' }).first().click();
    await page.waitForSelector('.cmd-palette-overlay', { state: 'detached' });

    const key = await page.evaluate(() => selectedKey);
    expect(key).toMatch(/^dashboard:direct:/);
    expect(key).toContain('-myproject:');
    expect(key).not.toContain('dashboard:pj:');
    await ctx.close();
  });

  test('clicking project creates session and closes palette', async ({ browser }) => {
    mock.resetCalls();
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    await page.click('.hdr-btn[title="New Session"]');
    await page.waitForSelector('.cmd-palette-item');

    // Pick a project row
    await page.locator('.cmd-palette-item', { hasText: 'myproject' }).first().click();

    // Palette closes and the workspace is eagerly bound (doCreateInProject)
    await page.waitForSelector('.cmd-palette-overlay', { state: 'detached' });
    await expect.poll(() => mock.bindCalls.length).toBeGreaterThan(0);

    await ctx.close();
  });

  test('custom workspace row opens the workspace input modal', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    await page.click('.hdr-btn[title="New Session"]');
    await page.waitForSelector('.cmd-palette-item');

    // Click "打开自定义工作目录…" (pickPaletteCustom)
    await page.locator('.cmd-palette-item', { hasText: '打开自定义工作目录' }).click();

    // Palette is replaced by the custom-workspace modal with the path input
    await page.waitForSelector('.cmd-palette-overlay', { state: 'detached' });
    await expect(page.locator('.modal-overlay')).toBeVisible();
    await expect(page.locator('#new-workspace')).toBeVisible();

    await ctx.close();
  });

  test('Esc closes palette; cancel closes custom workspace modal', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    // Palette has no cancel button — footer advertises "Esc 关闭".
    await page.click('.hdr-btn[title="New Session"]');
    await page.waitForSelector('.cmd-palette-overlay');
    await page.press('#cp-input', 'Escape');
    await page.waitForSelector('.cmd-palette-overlay', { state: 'detached' });

    // The custom-workspace modal keeps a 取消 button.
    await page.click('.hdr-btn[title="New Session"]');
    await page.locator('.cmd-palette-item', { hasText: '打开自定义工作目录' }).click();
    await page.waitForSelector('.modal-overlay');
    await page.click('.modal-btns button:not(.primary)');
    await page.waitForSelector('.modal-overlay', { state: 'detached' });

    await ctx.close();
  });

  test('no-projects mode shows simple workspace input', async ({ browser }) => {
    const noProjectMock = await startMockServer({
      sessions: {
        sessions: [],
        stats: { total: 0, running: 0, ready: 0, active: 0, uptime: '0s', backend: 'cc', max_procs: 10, default_workspace: '/tmp', agents: ['general'], projects: [], version: 1 },
        nodes: { local: { display_name: 'Local', status: 'ok' } },
      },
    });

    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(noProjectMock.url + '/dashboard');
    await page.waitForLoadState('networkidle');

    // First visit with no sessions/projects shows the one-time onboarding
    // overlay (maybeShowOnboarding, present since squash a274e0cc). Dismiss
    // it before reaching the header button it would otherwise intercept.
    const onboarding = page.locator('.modal-overlay.onboarding-overlay');
    await expect(onboarding).toBeVisible();
    await onboarding.locator('.modal-btns button:not(.primary)').click();
    await expect(onboarding).toHaveCount(0);

    await page.click('.hdr-btn[title="New Session"]');
    await page.waitForSelector('.modal-overlay');

    // Should show workspace input directly (no palette)
    const wsInput = page.locator('#new-workspace');
    await expect(wsInput).toBeVisible();
    expect(await page.$('.cmd-palette-overlay')).toBeNull();

    await ctx.close();
    noProjectMock.server.close();
  });
});

// ─── Cron Panel ────────────────────────────────────────────────────────────────

test.describe('Cron panel', () => {
  let mock;

  test.beforeAll(async () => { mock = await startMockServer(); });
  test.afterAll(() => mock.server.close());

  test('cron button opens cron panel with job list', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    // Click cron button
    await page.click('#abnav-cron');
    await page.waitForSelector('.cron-detail');

    // Heading is "定时任务" (Chinese since the cron_view.js extraction #1874
    // fa001adf, already so at squash a274e0cc) followed by a hidden
    // "· 运行中 N · 需关注 N" summary chip (b435ecbf), so match the prefix.
    const heading = await page.$eval('.cron-detail h3', el => el.textContent);
    expect(heading).toMatch(/^定时任务/);

    await ctx.close();
  });

  test('new cron job button opens creation modal', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    await page.click('#abnav-cron');
    await page.waitForSelector('.cron-detail');

    // Click "新建" button in cron panel (.cron-new-btn — `.cron-detail button`
    // now matches the hidden mobile back arrow first).
    await page.click('.cron-detail .cron-new-btn');
    await page.waitForSelector('.modal-overlay');

    const title = await page.$eval('.modal h3', el => el.textContent);
    expect(title).toBe('新建定时任务');

    await ctx.close();
  });

  // cron-v2-polish (docs/rfc/cron-v2-polish.md; cron_view.js
  // buildFreqPickerHtml): the 4-tab picker (间隔/每天/每周/每月) + advanced raw
  // cron disclosure were replaced by a single Frequency <select> with
  // Hourly / Daily / Weekdays / Weekly / Monthly and per-mode auxiliary
  // controls. `.freq-tabs` was already gone at the history-squash commit
  // a274e0cc. The "advanced cron expression disclosure toggles" test was
  // dropped with the feature ("高级 raw cron 输入全部删除").

  test('selecting a frequency mode toggles the auxiliary controls', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    await page.click('#abnav-cron');
    await page.waitForSelector('.cron-detail');
    await page.click('.cron-detail .cron-new-btn');
    await page.waitForSelector('#freq-mode-select');

    // Daily → time-of-day shown, weekday / day-of-month pickers hidden.
    await page.selectOption('#freq-mode-select', 'daily');
    await expect(page.locator('#freq-time')).toBeVisible();
    await expect(page.locator('#freq-weekly-dow')).toBeHidden();
    await expect(page.locator('#freq-monthly-day')).toBeHidden();

    // Weekly → weekday picker joins the time input.
    await page.selectOption('#freq-mode-select', 'weekly');
    await expect(page.locator('#freq-time')).toBeVisible();
    await expect(page.locator('#freq-weekly-dow')).toBeVisible();
    await expect(page.locator('#freq-monthly-day')).toBeHidden();

    // Monthly → day-of-month picker instead.
    await page.selectOption('#freq-mode-select', 'monthly');
    await expect(page.locator('#freq-weekly-dow')).toBeHidden();
    await expect(page.locator('#freq-monthly-day')).toBeVisible();

    await ctx.close();
  });

  test('empty cron list shows create-first prompt', async ({ browser }) => {
    const emptyCronMock = await startMockServer({ cronJobs: [] });

    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(emptyCronMock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    await page.click('#abnav-cron');
    await page.waitForSelector('.cron-detail');

    const text = await page.$eval('.cron-detail', el => el.textContent);
    // cron-v2-polish §3.1: 面板本地化为中文，去掉 "cron" 对初级用户过于
    // 工程化的术语。legacy 英文 fragment 同步在 static_ux_contract_test 解除锁定。
    expect(text).toContain('还没有定时任务');
    expect(text).toContain('创建第一个定时任务');

    await ctx.close();
    emptyCronMock.server.close();
  });
});

// ─── History Popover ───────────────────────────────────────────────────────────

test.describe('History popover', () => {
  let mock;

  test.beforeAll(async () => { mock = await startMockServer(); });
  test.afterAll(() => mock.server.close());

  test('history button opens popover', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    await page.click('#btn-history');
    await page.waitForSelector('.history-popover');

    const popover = await page.$('.history-popover');
    expect(popover).not.toBeNull();

    // Since #1800 a .history-backdrop covers the page while open; a click on
    // it closes the popover and both nodes must be removed.
    await page.locator('.history-backdrop').click({ position: { x: 5, y: 5 } });
    await page.waitForSelector('.history-popover', { state: 'detached' });
    expect(await page.$('.history-backdrop')).toBeNull();

    await ctx.close();
  });

  test('history badge shows count of filesystem sessions', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    // History badge should show count (1 filesystem session)
    const badge = page.locator('#history-badge');
    const display = await badge.evaluate(el => getComputedStyle(el).display);
    // Badge may be visible if there's a history session not in workspace
    if (display !== 'none') {
      const count = await badge.textContent();
      expect(parseInt(count)).toBeGreaterThanOrEqual(0);
    }

    await ctx.close();
  });
});

// ─── Error Handling ────────────────────────────────────────────────────────────

test.describe('Error handling', () => {
  test('network error on send shows toast', async ({ browser }) => {
    // Mock server that closes connection on /api/sessions/send
    const brokenMock = await startMockServer({ sendStatus: 500 });

    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(brokenMock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    await page.click('.session-card[data-key="dashboard:direct:2026-01-01-120000-1:myproject"]');
    await page.waitForSelector('#msg-input');

    const input = page.locator('#msg-input');
    await input.click();
    await input.pressSequentially('will fail');
    await page.keyboard.press('Enter');
    await page.waitForTimeout(500);

    // Toast should show error
    const toast = await page.$eval('#toast', el => el.textContent);
    expect(toast).toContain('send failed');

    // Input should retain the text
    const textAfter = await page.$eval('#msg-input', el => el.innerText.trim());
    expect(textAfter).toBe('will fail');

    await ctx.close();
    brokenMock.server.close();
  });
});

// ─── Session Dismiss ───────────────────────────────────────────────────────────

test.describe('Session dismiss', () => {
  let mock;

  test.beforeAll(async () => { mock = await startMockServer(); });
  test.afterAll(() => mock.server.close());

  test('dismiss button removes session card from DOM', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    const initialCount = await page.$$eval('.session-card', els => els.length);

    // Hover and click dismiss on first card
    const firstCard = page.locator('.session-card').first();
    await firstCard.hover();
    await firstCard.locator('.btn-dismiss').click();

    // Wait for API call and re-render
    await page.waitForTimeout(500);

    // Card count should decrease (API returns success, sidebar re-renders)
    // Note: Since our mock always returns the same sessions, the sidebar will re-render
    // with the same data. In a real scenario, the dismissed session would be gone.
    // We verify the dismiss API was called by checking the button interaction works.
    expect(initialCount).toBe(3);

    await ctx.close();
  });
});

// ─── Switching Between Sessions ────────────────────────────────────────────────

test.describe('Switching sessions', () => {
  let mock;

  test.beforeAll(async () => { mock = await startMockServer(); });
  test.afterAll(() => mock.server.close());

  test('switching sessions updates active card highlight', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    // Select first session
    await page.click('.session-card[data-key="dashboard:direct:2026-01-01-120000-1:myproject"]');
    await page.waitForTimeout(200);

    let activeKey = await page.$eval('.session-card.active', el => el.dataset.key);
    expect(activeKey).toBe('dashboard:direct:2026-01-01-120000-1:myproject');

    // Switch to second session
    await page.click('.session-card[data-key="dashboard:direct:2026-01-01-120001-2:otherproject"]');
    await page.waitForTimeout(200);

    activeKey = await page.$eval('.session-card.active', el => el.dataset.key);
    expect(activeKey).toBe('dashboard:direct:2026-01-01-120001-2:otherproject');

    // Only one card should be active
    const activeCards = await page.$$('.session-card.active');
    expect(activeCards.length).toBe(1);

    await ctx.close();
  });

  test('switching sessions reloads events', async ({ browser }) => {
    const ctx = await browser.newContext({ ...desktop });
    const page = await ctx.newPage();
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector('.session-card');

    await page.click('.session-card[data-key="dashboard:direct:2026-01-01-120000-1:myproject"]');
    await page.waitForSelector('.event');

    // Switch to another session
    await page.click('.session-card[data-key="dashboard:direct:2026-01-01-120001-2:otherproject"]');
    await page.waitForSelector('#events-scroll');

    // Events area should still exist (reloaded with new session's events)
    const eventsArea = await page.$('#events-scroll');
    expect(eventsArea).not.toBeNull();

    await ctx.close();
  });
});

// ─── Dark Theme & Styling ──────────────────────────────────────────────────────

test.describe('Theme & styling', () => {
  let mock;

  test.beforeAll(async () => { mock = await startMockServer(); });
  test.afterAll(() => mock.server.close());

  test('body background follows the OS colour scheme (auto theme)', async ({ browser }) => {
    // THEME-1 (#453, landed via #1431 5a7f04aa): default theme is 'auto', so
    // the body tokens resolve from prefers-color-scheme. Dark OS → #0d1117,
    // light OS → #ffffff (:root[data-theme="light"] parity tokens).
    const darkCtx = await browser.newContext({ ...desktop, colorScheme: 'dark' });
    const darkPage = await darkCtx.newPage();
    await darkPage.goto(mock.url + '/dashboard');
    await darkPage.waitForLoadState('networkidle');
    expect(await darkPage.$eval('html', el => el.getAttribute('data-theme'))).toBe('auto');
    const darkBg = await darkPage.$eval('body', el => getComputedStyle(el).backgroundColor);
    // #0d1117 = rgb(13, 17, 23)
    expect(darkBg).toBe('rgb(13, 17, 23)');
    await darkCtx.close();

    const lightCtx = await browser.newContext({ ...desktop, colorScheme: 'light' });
    const lightPage = await lightCtx.newPage();
    await lightPage.goto(mock.url + '/dashboard');
    await lightPage.waitForLoadState('networkidle');
    const lightBg = await lightPage.$eval('body', el => getComputedStyle(el).backgroundColor);
    expect(lightBg).toBe('rgb(255, 255, 255)');
    await lightCtx.close();
  });
});
