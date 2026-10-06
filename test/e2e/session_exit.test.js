// @ts-check
//
// A dead session tells the operator why it has no process. The backend sends
// state + death_reason; the dashboard shows it wherever the session appears:
//  - an abnormal exit (cli_exited, readloop_panic, …) is a warning chip;
//  - a reclaim done on purpose (idle_timeout, evicted, released) is a muted
//    note;
//  - a live session never shows one, even with a death_reason left over from
//    a timeout;
//  - the chip follows session_state pushes, and an optimistic send clears it;
//  - the tooltip says what the next send does (startup_failure): wait out
//    the startup breaker's pause, start a new conversation, need an
//    operator's fix, or just resume;
//  - the tooltip ends with death_detail, the stderr line naming the cause; a
//    push drops it and startup_failure, and the poll it triggers brings the
//    current ones to the card and the header alike.
//
// Run: cd test/e2e && npx playwright test session_exit.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const CRASHED = 'dashboard:direct:2026-01-01-120000-1:myproject';
const STALE = 'dashboard:direct:2026-01-01-120001-2:otherproject';
const RECLAIMED = 'dashboard:direct:2026-01-01-120002-3:myproject';
const DETAIL = 'No conversation found with session ID: abc';

function exitSessions() {
  const p = defaultSessions();
  const by = (/** @type {string} */ k) => p.sessions.find((s) => s.key === k);
  Object.assign(by(CRASHED), { state: 'dead', death_reason: 'cli_exited', death_detail: DETAIL });
  Object.assign(by(RECLAIMED), { state: 'dead', death_reason: 'idle_timeout' });
  // Alive, with the reason a no-output timeout left behind.
  Object.assign(by(STALE), { state: 'ready', death_reason: 'no_output_timeout' });
  return p;
}

/** @param {import('@playwright/test').Page} page @param {string} key */
const card = (page, key) => page.locator(`.session-card[data-key="${key}"]`);

test.describe('dead session exit chip', () => {
  test.beforeEach(({ }, testInfo) => {
    if (testInfo.project.name !== 'desktop-chrome') testInfo.skip(true, 'desktop-chrome only');
  });

  test('sidebar cards tell an abnormal exit from a reclaim', async ({ page }) => {
    const mock = await startMockServer({ sessions: exitSessions() });
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector(`.session-card[data-key="${CRASHED}"]`);

    const crashed = card(page, CRASHED).locator('.sc-exit');
    await expect(crashed).toHaveClass(/sc-exit-crashed/);
    await expect(crashed).toHaveText('⚠ 异常退出');
    await expect(crashed).toHaveAttribute('title', 'CLI 进程退出，下次发送时自动恢复\n' + DETAIL);
    // The state stays "ready": sending resumes it.
    await expect(card(page, CRASHED).locator('.sc-meta')).toContainText('ready');

    const reclaimed = card(page, RECLAIMED).locator('.sc-exit');
    await expect(reclaimed).toHaveClass(/sc-exit-reclaimed/);
    await expect(reclaimed).toHaveText('已回收');
    await expect(reclaimed).toHaveAttribute('title', '空闲超时，进程已回收，下次发送时自动恢复');

    await expect(card(page, STALE).locator('.sc-exit')).toHaveCount(0);
    mock.server.close();
  });

  test('the exit wording covers every reason, including unknown ones', async ({ page }) => {
    const mock = await startMockServer();
    await page.goto(mock.url + '/dashboard');
    const got = await page.evaluate(async () => {
      const { sessionExit } = await import('/static/nz_util.js');
      return {
        unknown: sessionExit('dead', 'weird_reason'),
        empty: sessionExit('dead', ''),
        evicted: sessionExit('dead', 'evicted'),
        released: sessionExit('dead', 'released'),
        alive: sessionExit('ready', 'cli_exited'),
        proto: sessionExit('dead', 'toString'),
        detail: sessionExit('dead', 'cli_exited_code_1', 'Error: Invalid API key'),
        killed: sessionExit('dead', 'cli_exited_code_-1'),
        signal: sessionExit('dead', 'cli_exited_signal_SIGKILL'),
        notCode: sessionExit('dead', 'cli_exited_code_x'),
        bareSignal: sessionExit('dead', 'cli_exited_signal_'),
      };
    });
    expect(got.unknown).toEqual({ crashed: true, text: '进程已退出（weird_reason）', title: '进程已退出（weird_reason），下次发送时自动恢复' });
    expect(got.empty.text).toBe('进程已退出');
    expect(got.evicted.crashed).toBe(false);
    // A cron run's process closed after the run is a reclaim, not a crash.
    expect(got.released).toEqual({ crashed: false, text: '本次执行结束，进程已释放', title: '本次执行结束，进程已释放，下次发送时自动恢复' });
    expect(got.alive).toBeNull();
    // An inherited Object property is not a known reason.
    expect(got.proto.text).toBe('进程已退出（toString）');
    // A cli_exited reason names the exit code or the signal; the shim reports
    // a CLI a signal killed as exit code -1.
    expect(got.detail).toEqual({ crashed: true, text: 'CLI 进程异常退出（退出码 1）', title: 'CLI 进程异常退出（退出码 1），下次发送时自动恢复\nError: Invalid API key' });
    expect(got.killed).toEqual({ crashed: true, text: 'CLI 进程被信号终止', title: 'CLI 进程被信号终止，下次发送时自动恢复' });
    expect(got.signal.text).toBe('CLI 进程被信号 SIGKILL 终止');
    // A suffix that is neither shape stays the raw value.
    expect(got.notCode.text).toBe('进程已退出（cli_exited_code_x）');
    expect(got.bareSignal.text).toBe('进程已退出（cli_exited_signal_）');
    mock.server.close();
  });

  test('the tooltip says what the next send does about startup failures', async ({ page }) => {
    const mock = await startMockServer();
    await page.goto(mock.url + '/dashboard');
    const got = await page.evaluate(async () => {
      const { sessionExit, patchCardExitChip } = await import('/static/nz_util.js');
      const { NZ_CONTRACT } = await import('/static/contract.js');
      const card = document.createElement('div');
      card.innerHTML = '<div class="sc-meta"><span></span><span>ready</span></div>';
      patchCardExitChip(card, 'dead', 'cli_exited', '', { class: 'mcp_config', streak: 1 });
      const title = (/** @type {any} */ f) => sessionExit('dead', 'cli_exited', '', f).title;
      const retryAt = Date.now() + 90_000;
      return {
        clock: new Date(retryAt).toLocaleTimeString('en-GB', { hour12: false }),
        paused: title({ class: 'auth', streak: 3, retry_at: retryAt, new_session: true }),
        pauseOver: title({ class: 'unknown', streak: 2, retry_at: Date.now() - 1000, new_session: true }),
        pauseOverAuth: title({ class: 'auth', streak: 2, retry_at: Date.now() - 1000 }),
        newSession: title({ class: 'auth', streak: 1, new_session: true }),
        onlyNewSession: title({ new_session: true }),
        byClass: Object.fromEntries(NZ_CONTRACT.ENUMS.STARTUP_FAILURE_CLASS.map((c) => [c, title({ class: c, streak: 1 })])),
        proto: title({ class: 'toString', streak: 1 }),
        none: title(undefined),
        withDetail: sessionExit('dead', 'cli_exited_code_1', 'Error: Invalid API key', { class: 'auth', streak: 1 }).title,
        patched: card.querySelector('.sc-exit')?.getAttribute('title'),
      };
    });
    expect(got.paused).toBe('CLI 进程退出，CLI 连续启动失败（3 次），已暂停自动重试；' + got.clock + ' 后可重试，或发送 /new 立即重试');
    // A pause that is over leaves what the next send then does.
    expect(got.pauseOver).toBe('CLI 进程退出，下次发送将开启新会话（上次会话无法恢复）');
    expect(got.pauseOverAuth).toBe('CLI 进程退出，后端认证失败，需管理员修复后重试');
    // A refused resume can come with an operator's class: the new session wins.
    expect(got.newSession).toBe('CLI 进程退出，下次发送将开启新会话（上次会话无法恢复）');
    expect(got.onlyNewSession).toBe(got.newSession);
    expect(got.byClass).toEqual({
      unknown: 'CLI 进程退出，下次发送时自动恢复',
      resume_not_found: 'CLI 进程退出，下次发送时自动恢复',
      auth: 'CLI 进程退出，后端认证失败，需管理员修复后重试',
      mcp_config: 'CLI 进程退出，CLI 配置错误，需管理员修复后重试',
      missing_runtime: 'CLI 进程退出，CLI 运行环境缺失，需管理员修复后重试',
    });
    expect(got.proto).toBe('CLI 进程退出，下次发送时自动恢复');
    expect(got.none).toBe('CLI 进程退出，下次发送时自动恢复');
    expect(got.patched).toBe('CLI 进程退出，CLI 配置错误，需管理员修复后重试');
    expect(got.withDetail).toBe('CLI 进程异常退出（退出码 1），后端认证失败，需管理员修复后重试\nError: Invalid API key');
    mock.server.close();
  });

  test('a paused key shows on the card and the header until a push replaces it', async ({ page }) => {
    const data = exitSessions();
    const crashed = data.sessions.find((x) => x.key === CRASHED);
    const retryAt = Date.now() + 10 * 60_000;
    crashed.startup_failure = { class: 'unknown', streak: 2, retry_at: retryAt, new_session: true };
    const mock = await startMockServer({ sessions: data, ws: true });
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector(`.session-card[data-key="${CRASHED}"]`);
    // @ts-ignore — wsm / WS_STATES are mirrored onto window by the e2e shim.
    await waitForWs(page);
    const clock = await page.evaluate((t) => new Date(t).toLocaleTimeString('en-GB', { hour12: false }), retryAt);
    const paused = 'CLI 进程退出，CLI 连续启动失败（2 次），已暂停自动重试；' + clock + ' 后可重试，或发送 /new 立即重试\n' + DETAIL;
    await expect(card(page, CRASHED).locator('.sc-exit')).toHaveAttribute('title', paused);
    await expect(page.locator(`#recent-sessions-panel .recent-row[data-key="${CRASHED}"] .sc-exit`)).toHaveAttribute('title', paused);
    await card(page, CRASHED).click();
    const header = page.locator('#header-exit .sc-exit');
    await expect(header).toHaveAttribute('title', paused);

    await expect.poll(() => mock.wsConnections.length).toBeGreaterThan(0);
    const conn = mock.wsConnections[mock.wsConnections.length - 1];
    // A poll's repaint of the header keeps the pause.
    crashed.death_detail = 'Error: Invalid API key';
    data.stats.version++;
    conn.send({ type: 'sessions_update' });
    await expect(header).toHaveAttribute('title', paused.replace(DETAIL, crashed.death_detail));
    Object.assign(crashed, { death_reason: 'readloop_panic' });
    delete crashed.startup_failure;
    delete crashed.death_detail;
    conn.send({ type: 'session_state', key: CRASHED, state: 'dead', reason: 'readloop_panic' });
    // The push repaints before its debounced poll; the first paint must not
    // keep the previous death's pause.
    const first = await page.waitForFunction(() => {
      const el = document.querySelector('#header-exit .sc-exit');
      return el && el.getAttribute('title').startsWith('读取循环崩溃') ? el.getAttribute('title') : false;
    });
    expect(await first.jsonValue()).toBe('读取循环崩溃，下次发送时自动恢复');
    await expect(card(page, CRASHED).locator('.sc-exit')).toHaveAttribute('title', '读取循环崩溃，下次发送时自动恢复');
    mock.server.close();
  });

  test('the header chip follows session_state pushes', async ({ page }) => {
    // The 5 s /api/sessions poll must agree with each push, as the real
    // backend's would; a mock still answering 'dead' after a 'running' push
    // repaints the chip the push just cleared.
    const data = exitSessions();
    const mock = await startMockServer({ sessions: data, ws: true });
    const setState = (state, reason) => {
      const s = data.sessions.find((x) => x.key === CRASHED);
      s.state = state;
      s.death_reason = reason || '';
      delete s.death_detail;
    };
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector(`.session-card[data-key="${CRASHED}"]`);
    // @ts-ignore — wsm / WS_STATES are mirrored onto window by the e2e shim.
    await waitForWs(page);
    await card(page, CRASHED).click();
    const header = page.locator('#header-exit .sc-exit');
    await expect(header).toHaveClass(/sc-exit-crashed/);
    await expect(header).toHaveAttribute('title', 'CLI 进程退出，下次发送时自动恢复\n' + DETAIL);

    await expect.poll(() => mock.wsConnections.length).toBeGreaterThan(0);
    const conn = mock.wsConnections[mock.wsConnections.length - 1];
    setState('running');
    conn.send({ type: 'session_state', key: CRASHED, state: 'running' });
    await expect(header).toHaveCount(0);
    await expect(card(page, CRASHED).locator('.sc-exit')).toHaveCount(0);

    setState('dead', 'readloop_panic');
    conn.send({ type: 'session_state', key: CRASHED, state: 'dead', reason: 'readloop_panic' });
    // The push repaints before its debounced poll; the first paint must not
    // carry the previous death's detail.
    const first = await page.waitForFunction(() => {
      const el = document.querySelector('#header-exit .sc-exit');
      return el && el.getAttribute('title').startsWith('读取循环崩溃') ? el.getAttribute('title') : false;
    });
    expect(await first.jsonValue()).toBe('读取循环崩溃，下次发送时自动恢复');
    await expect(header).toHaveAttribute('title', '读取循环崩溃，下次发送时自动恢复');
    await expect(card(page, CRASHED).locator('.sc-exit')).toHaveAttribute('title', '读取循环崩溃，下次发送时自动恢复');
    mock.server.close();
  });

  test('the header chip takes the detail the poll brings after a dead push', async ({ page }) => {
    const data = exitSessions();
    const mock = await startMockServer({ sessions: data, ws: true });
    const crashed = data.sessions.find((x) => x.key === CRASHED);
    await page.goto(mock.url + '/dashboard');
    await page.waitForSelector(`.session-card[data-key="${CRASHED}"]`);
    // @ts-ignore — wsm / WS_STATES are mirrored onto window by the e2e shim.
    await waitForWs(page);
    await card(page, CRASHED).click();
    const header = page.locator('#header-exit .sc-exit');
    await expect(header).toHaveAttribute('title', 'CLI 进程退出，下次发送时自动恢复\n' + DETAIL);

    await expect.poll(() => mock.wsConnections.length).toBeGreaterThan(0);
    const conn = mock.wsConnections[mock.wsConnections.length - 1];
    Object.assign(crashed, { state: 'running', death_reason: '' });
    delete crashed.death_detail;
    conn.send({ type: 'session_state', key: CRASHED, state: 'running' });
    await expect(header).toHaveCount(0);

    // A push carries no detail; the poll it triggers does, and the header,
    // already showing 'dead', must still repaint with it like the card does.
    const detail2 = 'Error: Invalid API key';
    Object.assign(crashed, { state: 'dead', death_reason: 'cli_exited', death_detail: detail2 });
    conn.send({ type: 'session_state', key: CRASHED, state: 'dead', reason: 'cli_exited' });
    const want = 'CLI 进程退出，下次发送时自动恢复\n' + detail2;
    await expect(card(page, CRASHED).locator('.sc-exit')).toHaveAttribute('title', want);
    await expect(header).toHaveAttribute('title', want);
    mock.server.close();
  });

  test('an optimistic send clears the card chip', async ({ page }) => {
    const mock = await startMockServer({ sessions: exitSessions() });
    await page.goto(mock.url + '/dashboard');
    await expect(card(page, CRASHED).locator('.sc-exit')).toHaveCount(1);
    await page.evaluate((k) => /** @type {any} */ (window).markSessionOptimisticRunning(k, 'local'), CRASHED);
    await expect(card(page, CRASHED).locator('.sc-exit')).toHaveCount(0);
    mock.server.close();
  });

  test('the home list shows the chip too', async ({ page }) => {
    const mock = await startMockServer({ sessions: exitSessions() });
    await page.goto(mock.url + '/dashboard');
    const row = page.locator(`#recent-sessions-panel .recent-row[data-key="${CRASHED}"]`);
    await expect(row.locator('.sc-exit-crashed')).toHaveCount(1);
    await expect(page.locator(`#recent-sessions-panel .recent-row[data-key="${STALE}"] .sc-exit`)).toHaveCount(0);
    mock.server.close();
  });
});

// Every project: the chip must fit a phone header.
test('at 320px the header with an exit chip stays inside the viewport', async ({ page }) => {
  const mock = await startMockServer({ sessions: exitSessions() });
  await page.setViewportSize({ width: 320, height: 640 });
  await page.goto(mock.url + '/dashboard');
  await card(page, CRASHED).click();
  await expect(page.locator('#header-exit .sc-exit')).toBeVisible();
  const geom = await page.evaluate(() => {
    const row = /** @type {HTMLElement} */ (document.querySelector('.main-header .detail'));
    return {
      innerWidth: window.innerWidth,
      scroll: row.scrollWidth,
      client: row.clientWidth,
      rights: [...row.children].filter((c) => getComputedStyle(c).display !== 'none').map((c) => c.getBoundingClientRect().right),
    };
  });
  expect(geom.scroll).toBeLessThanOrEqual(geom.client + 1);
  for (const r of geom.rights) expect(r).toBeLessThanOrEqual(geom.innerWidth);
  mock.server.close();
});
