// E2E: the session view's UX review of 11-18.
//   1. A question card the backend sends again (a poll's or a resubscribe's
//      watermark millisecond) is deduped by its uuid, as every other event
//      is: one card, not one more per poll.
//   2. Interrupting refills the composer only with the interrupted turn's
//      text: a message sent while the turn ran waits in the queue and still
//      runs, so it does not come back to be sent twice.
//   3. On a phone the header's 托管 / 观察 chips keep their width; the
//      model and access profile give way, so on / off stays readable.
//   4. The ▲▼ pill sits above the events pane, not over the running
//      banner's timer, and the column keeps clear of it (a pending
//      question's 提交).
//   5. The tuning popover's options, a sub-agent row and an upload's ↻ work
//      from the keyboard; Esc closes the popover (and only it) with focus
//      back on its chip.
//   6. Dropping a file that is neither an image nor a PDF says so.
//   7. The session view's wording is Chinese and says what happened: a WS
//      ack's error is not an HTTP 500, an attachment is not always a picture,
//      a sub-agent's status reads 运行中.
const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { waitForShim, waitForWs } = require('./shim_wait');

const A = 'dashboard:direct:2026-01-01-120000-1:myproject';
const B = 'dashboard:direct:2026-01-01-120001-2:otherproject';
const PLANNER = 'planner:main';
const DESKTOP = { viewport: { width: 1440, height: 900 } };
const PHONE = { viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true };
const now = Date.now();

const lastConn = (mock) => mock.wsConnections[mock.wsConnections.length - 1];

async function openSession(page, mock, key, events, state = 'ready') {
  await page.evaluate((k) => window.nz.test.selectSession(k, 'local'), key);
  const conn = lastConn(mock);
  await expect.poll(() => conn.messages.some((m) => m.type === 'subscribe' && m.key === key)).toBe(true);
  conn.send({ type: 'subscribed', key, state });
  conn.send({ type: 'history', key, initial: true, events });
  return conn;
}

function ask(id, t, question) {
  return { type: 'ask_question', time: t, uuid: 'aq-' + id,
    ask_question: { tool_use_id: id, items: [{ header: '格式', question, options: [{ label: 'CSV' }, { label: 'JSON' }] }] } };
}

const inputText = (page) => page.locator('#msg-input').evaluate((el) => el.innerText.replace(/\n$/, ''));

// rectsOverlap reports whether two client rects share any area.
const rectsOverlap = (a, b) => a.x < b.x + b.width && b.x < a.x + a.width && a.y < b.y + b.height && b.y < a.y + a.height;

test.describe('session view UX review', () => {
  test('a redelivered question card is deduped by its uuid (WS backfill)', async ({ browser }) => {
    const ctx = await browser.newContext(PHONE);
    const page = await ctx.newPage();
    const mock = await startMockServer({ ws: true });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page);
      const q = ask('t1', now - 5000, '导出成什么格式？');
      const conn = await openSession(page, mock, A, [{ type: 'user', detail: 'iso 导出', time: now - 6000, uuid: 'u1' }, q]);
      const cards = page.locator('#events-scroll .event.ask_question');
      await expect(cards).toHaveCount(1);
      // A resubscribe re-sends the watermark millisecond, the card again;
      // the later text proves the frames were painted.
      conn.send({ type: 'history', key: A, events: [q] });
      conn.send({ type: 'history', key: A, events: [q] });
      conn.send({ type: 'history', key: A, events: [q, { type: 'text', detail: 'iso 等你选', time: now - 4000, uuid: 'x1' }] });
      await expect(page.locator('#events-scroll')).toContainText('iso 等你选');
      await expect(cards).toHaveCount(1);
      await expect(cards).toHaveAttribute('data-uuid', 'aq-t1');
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });

  test('a polled question card is not appended again on each poll', async ({ browser }) => {
    const ctx = await browser.newContext(DESKTOP);
    const page = await ctx.newPage();
    const mock = await startMockServer({
      keyEvents: { [A]: [{ type: 'user', detail: 'iso 导出', time: now - 6000, uuid: 'u1' }, ask('t1', now - 5000, '导出成什么格式？')] },
    });
    try {
      let polls = 0;
      page.on('request', (r) => {
        const u = new URL(r.url());
        if (u.pathname.endsWith('/api/sessions/events') && u.searchParams.get('key') === A && u.searchParams.get('after')) polls++;
      });
      await page.goto(mock.url + '/dashboard');
      await waitForShim(page);
      await page.evaluate((k) => window.nz.test.selectSession(k, 'local'), A);
      const cards = page.locator('#events-scroll .event.ask_question');
      await expect(cards).toHaveCount(1);
      await expect.poll(() => polls, { timeout: 15000 }).toBeGreaterThanOrEqual(3);
      await expect(cards).toHaveCount(1);
      await page.screenshot({ path: test.info().outputPath('ask-poll.png') });
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });

  test('interrupting does not refill a message that waits in the queue', async ({ browser }) => {
    const ctx = await browser.newContext(DESKTOP);
    const page = await ctx.newPage();
    const mock = await startMockServer({ ws: true });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page);
      // A running turn this tab did not start; a message joins its queue.
      const conn = await openSession(page, mock, B, [{ type: 'user', detail: 'iso review', time: now - 9000, uuid: 'u1' }], 'running');
      const text = '顺便检查一下 cancel 路径';
      await page.locator('#msg-input').click();
      await page.keyboard.type(text);
      await page.keyboard.press('Enter');
      await expect.poll(() => conn.messages.filter((m) => m.type === 'send' && m.text === text).length).toBe(1);
      const sent = conn.messages.find((m) => m.type === 'send' && m.text === text);
      conn.send({ type: 'send_ack', id: sent.id, key: B, status: 'queued' });
      await expect(page.locator('#events-scroll .msg-queued-chip')).toHaveText('排队中…');
      await page.locator('#btn-stop').click();
      await expect.poll(() => conn.messages.filter((m) => m.type === 'interrupt' && m.key === B).length).toBe(1);
      expect(await inputText(page)).toBe('');
      await page.screenshot({ path: test.info().outputPath('interrupt-queued.png') });

      // Once the queued message's own turn starts (its echo arrives), it is
      // the running turn's text again: interrupting that turn refills it.
      conn.send({ type: 'event', key: B, event: { type: 'result', detail: 'iso 好了', time: now - 3000, uuid: 'r1' } });
      conn.send({ type: 'event', key: B, event: { type: 'user', detail: text, time: now - 2000, uuid: 'u-x' } });
      conn.send({ type: 'session_state', key: B, node: 'local', state: 'running' });
      await expect(page.locator('#events-scroll .event.user.optimistic-msg')).toHaveCount(0);
      await page.locator('#btn-stop').click();
      await expect.poll(() => inputText(page)).toBe(text);
      await page.locator('#msg-input').evaluate((el) => { el.textContent = ''; });

      // The turn this tab started is still refilled.
      const conn2 = await openSession(page, mock, A, [{ type: 'user', detail: 'iso hello', time: now - 9000, uuid: 'u2' }]);
      const mine = '写一个快速排序';
      await page.locator('#msg-input').click();
      await page.keyboard.type(mine);
      await page.keyboard.press('Enter');
      await expect.poll(() => conn2.messages.filter((m) => m.type === 'send' && m.text === mine).length).toBe(1);
      const sent2 = conn2.messages.find((m) => m.type === 'send' && m.text === mine);
      conn2.send({ type: 'send_ack', id: sent2.id, key: A, status: 'accepted' });
      conn2.send({ type: 'session_state', key: A, node: 'local', state: 'running' });
      await page.locator('#btn-stop').click();
      await expect.poll(() => inputText(page)).toBe(mine);
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });

  for (const theme of ['light', 'dark']) {
    test(`a phone header keeps 托管 / 观察 readable (${theme})`, async ({ browser }) => {
      const ctx = await browser.newContext({ ...PHONE, colorScheme: theme });
      const page = await ctx.newPage();
      const sessions = defaultSessions();
      Object.assign(sessions.sessions[0], { access_profile: 'isengard', effort: 'xhigh', turn_duration_ms: 123400, model: 'claude-opus-5-5[1m]' });
      const mock = await startMockServer({
        ws: true, sessions, planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
        custody: { paused: false, sessions: [{ key: A, on: true, state: 'on', sent: 0, max: 8 }] },
      });
      await page.route((u) => u.pathname === '/api/access-profiles', (r) => r.fulfill({ status: 200, contentType: 'application/json',
        body: JSON.stringify({ profiles: [{ id: 'isengard', display_name: 'Isengard · 788668107894', chip_color: 'var(--nz-amber)', secret_ok: true }], default: '' }) }));
      try {
        await page.goto(mock.url + '/dashboard');
        await waitForWs(page);
        await openSession(page, mock, A, [{ type: 'text', detail: 'iso 跑着', time: now - 20000, uuid: 't1' }]);
        await expect(page.locator('#header-custody > button')).toHaveText(['托管开', '观察开']);
        await expect(page.locator('#header-access-profile')).toBeVisible();
        await expect(page.locator('#header-effort .effort-tag')).toHaveText('xhigh');
        const m = await page.evaluate(() => {
          const r = (el) => { const b = el.getBoundingClientRect(); return { x: b.x, y: b.y, width: b.width, height: b.height }; };
          const chips = [...document.querySelectorAll('#header-custody > button')];
          return {
            fit: chips.map((c) => c.scrollWidth <= c.clientWidth),
            chips: chips.map(r),
            effort: r(document.querySelector('#header-effort .effort-tag')),
            rowRight: document.querySelector('.main-header .detail').getBoundingClientRect().right,
            left: r(document.querySelector('.main-header .detail-left')),
            model: r(document.getElementById('header-model')),
            profile: r(document.getElementById('header-access-profile')),
            profileStyle: (() => { const cs = getComputedStyle(document.getElementById('header-access-profile')); return cs.display + ' ' + cs.textOverflow; })(),
          };
        });
        // Each chip's text fits its box, and nothing sits on another.
        expect(m.fit).toEqual([true, true]);
        expect(rectsOverlap(m.chips[0], m.chips[1])).toBe(false);
        expect(rectsOverlap(m.chips[1], m.effort)).toBe(false);
        expect(m.effort.x + m.effort.width).toBeLessThanOrEqual(m.rowRight + 1);
        // The model and the access profile shrink to ellipses of their own:
        // both keep some width, and the chip ends inside the group, its
        // border whole, rather than being cut at the group's edge.
        expect(m.model.width).toBeGreaterThan(20);
        expect(m.profile.width).toBeGreaterThan(30);
        expect(m.profile.x + m.profile.width).toBeLessThanOrEqual(m.left.x + m.left.width + 1);
        expect(m.profileStyle).toBe('block ellipsis'); // a flex item's inline-block is blockified
        await page.locator('.main-header').screenshot({ path: test.info().outputPath('header-390-' + theme + '.png') });
      } finally {
        await ctx.close();
        mock.server.close();
      }
    });
  }

  test('the ▲▼ pill stays off the running banner and a question\'s 提交', async ({ browser }) => {
    const ctx = await browser.newContext({ viewport: { width: 1024, height: 768 } });
    const page = await ctx.newPage();
    const mock = await startMockServer({ ws: true });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page);
      const events = [];
      for (let i = 0; i < 4; i++) {
        events.push({ type: 'user', detail: 'iso 第 ' + (i + 1) + ' 条', time: now - 90000 + i * 10000, uuid: 'u' + i });
        events.push({ type: 'text', detail: 'iso 回答 ' + (i + 1), time: now - 85000 + i * 10000, uuid: 'x' + i });
      }
      events.push(ask('t1', now - 30000, '导出成什么格式？'));
      await openSession(page, mock, B, events, 'running');
      const pill = page.locator('#nav-pill');
      await expect(pill).toHaveClass(/visible/);
      await expect(page.locator('#running-banner')).toBeVisible();
      await page.locator('#rb-elapsed').evaluate((el) => { el.textContent = '0:43'; });
      await page.locator('#events-scroll').evaluate((el) => { el.scrollTop = el.scrollHeight; });
      const [p, banner, submit] = await Promise.all([pill.boundingBox(), page.locator('#running-banner').boundingBox(), page.locator('.event.ask_question .ask-submit').boundingBox()]);
      expect(p.y + p.height).toBeLessThanOrEqual(banner.y + 1);
      expect(rectsOverlap(p, submit)).toBe(false);
      await page.screenshot({ path: test.info().outputPath('pill-1024.png') });

      // Only the pill's side gives way in a pane narrower than the column; a
      // wide pane's column keeps its place whether the pill shows or not.
      const pad = () => page.locator('#events-scroll').evaluate((el) => {
        const cs = getComputedStyle(el);
        return { left: parseFloat(cs.paddingLeft), right: parseFloat(cs.paddingRight) };
      });
      expect(await pad()).toEqual({ left: 0, right: 46 });
      await page.locator('.main-header .btn-workbench').click();
      await expect(page.locator('#fv-drawer')).toBeVisible();
      expect(await pad()).toEqual({ left: 0, right: 46 });
      await page.locator('#fv-btn-close').click();
      await expect(page.locator('#fv-drawer')).toBeHidden();
      await page.setViewportSize({ width: 1440, height: 900 });
      await expect(pill).toHaveClass(/visible/);
      const colX = () => page.locator('#events-scroll > .event.text').first().evaluate((el) => el.getBoundingClientRect().x);
      const withPill = await colX();
      await pill.evaluate((el) => el.classList.remove('visible'));
      expect(Math.abs((await colX()) - withPill)).toBeLessThanOrEqual(1);
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });

  test('the popover, a sub-agent row and ↻ work from the keyboard', async ({ browser }) => {
    const ctx = await browser.newContext(DESKTOP);
    const page = await ctx.newPage();
    const sessions = defaultSessions();
    Object.assign(sessions.sessions[1], { model: 'claude-opus-5-5[1m]', effort: 'high' });
    const mock = await startMockServer({ ws: true, sessions });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page);
      const conn = await openSession(page, mock, B, [{ type: 'user', detail: 'iso 调研', time: now - 9000, uuid: 'u1' }], 'running');

      // Enter opens the model popover with an option focused; Esc closes it.
      await page.locator('#header-model').focus();
      await page.keyboard.press('Enter');
      const pop = page.locator('#tuning-popover');
      await expect(pop).toBeVisible();
      await expect(pop.locator('.nz-tuning-opt').first()).toHaveAttribute('tabindex', '0');
      expect(await page.evaluate(() => !!document.activeElement.closest('#tuning-popover'))).toBe(true);
      await page.keyboard.press('Escape');
      await expect(pop).toHaveCount(0);
      await expect(page.locator('#header-model')).toBeFocused();
      // Space on the effort tag opens its popover; Esc closes that one too.
      await page.locator('#header-effort .effort-tag').focus();
      await page.keyboard.press(' ');
      await expect(pop).toBeVisible();
      await page.keyboard.press('Escape');
      await expect(pop).toHaveCount(0);
      // Focus back on the tag itself, not on its (unfocusable) wrapper.
      await expect(page.locator('#header-effort .effort-tag')).toBeFocused();
      // With the workbench docked, Esc closes the popover above it, not the pane.
      await page.locator('.main-header .btn-workbench').click();
      await expect(page.locator('#fv-drawer')).toBeVisible();
      await page.locator('#header-model').focus();
      await page.keyboard.press('Enter');
      await expect(pop).toBeVisible();
      await page.keyboard.press('Escape');
      await expect(pop).toHaveCount(0);
      await expect(page.locator('#fv-drawer')).toBeVisible();
      await expect(page.locator('#header-model')).toBeFocused();
      await page.keyboard.press('Escape');
      await expect(page.locator('#fv-drawer')).toBeHidden();

      // A sub-agent row opens on Enter, its status in Chinese.
      conn.send({ type: 'event', key: B, event: { type: 'agent', tool_use_id: 'tu1', subagent: 'Explore', summary: 'iso 查调用方', time: now - 3000 } });
      conn.send({ type: 'event', key: B, event: { type: 'task_start', tool_use_id: 'tu1', task_id: 'task-1', time: now - 2900 } });
      const row = page.locator('.rb-agent-row[data-task="task-1"]');
      await expect(row).toBeVisible();
      await row.focus();
      await page.keyboard.press('Enter');
      await expect(page.locator('#bc-agent-name')).toHaveText('Explore');
      await expect(page.locator('#bc-agent-stat')).toHaveText('运行中');
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });

  test('an upload\'s ↻ is reachable and retries on Enter', async ({ browser }) => {
    const ctx = await browser.newContext(DESKTOP);
    const page = await ctx.newPage();
    const mock = await startMockServer({ ws: true });
    try {
      let uploads = 0;
      await page.route('**/api/sessions/upload', (r) => { uploads++; r.fulfill({ status: 500, contentType: 'application/json', body: '{"error":"disk full"}' }); });
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page);
      await openSession(page, mock, A, [{ type: 'user', detail: 'iso hi', time: now - 9000, uuid: 'u1' }]);
      await page.locator('#file-input').setInputFiles({ name: 'report.pdf', mimeType: 'application/pdf', buffer: Buffer.from('%PDF-1.4\n') });
      const retry = page.locator('#file-preview .upload-status.error');
      await expect(retry).toBeVisible();
      await expect(retry).toHaveAttribute('tabindex', '0');
      await expect(retry).toHaveAttribute('aria-label', '重试上传');
      await retry.focus();
      await page.keyboard.press('Enter');
      await expect.poll(() => uploads).toBe(2);
      // Sending with it still failed names an attachment, not a picture.
      await page.locator('#msg-input').click();
      await page.keyboard.type('iso 看看这份');
      await page.keyboard.press('Enter');
      await expect(page.locator('#toast')).toContainText('附件上传失败（disk full）');
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });

  test('dropping a file that is neither an image nor a PDF says so', async ({ browser }) => {
    const ctx = await browser.newContext(DESKTOP);
    const page = await ctx.newPage();
    const mock = await startMockServer({ ws: true, uploads: true });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page);
      await openSession(page, mock, A, [{ type: 'user', detail: 'iso hi', time: now - 9000, uuid: 'u1' }]);
      await page.locator('#input-area').evaluate((el) => {
        const dt = new DataTransfer();
        dt.items.add(new File(['notes'], 'notes.txt', { type: 'text/plain' }));
        dt.items.add(new File(['package main'], 'main.go'));
        el.dispatchEvent(new DragEvent('drop', { dataTransfer: dt, bubbles: true, cancelable: true }));
      });
      await expect(page.locator('#toast')).toContainText('只支持图片和 PDF，已跳过 2 个文件');
      await expect(page.locator('#file-preview .file-thumb')).toHaveCount(0);
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });

  test('a binary or cut preview says so in Chinese', async ({ browser }) => {
    const ctx = await browser.newContext(DESKTOP);
    const page = await ctx.newPage();
    const mock = await startMockServer({ ws: true });
    try {
      await page.route('**/api/projects/file?*', (r) => {
        const p = new URL(r.request().url()).searchParams.get('path');
        const body = p === 'bin/app' ? { binary: true, mime: 'application/octet-stream', size: 2048 }
          : { content: 'x'.repeat(64), truncated: true, size: 3 * 1024 * 1024, mime: 'text/plain' };
        r.fulfill({ contentType: 'application/json', body: JSON.stringify(body) });
      });
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page);
      await openSession(page, mock, A, [{ type: 'user', detail: 'iso hi', time: now - 9000, uuid: 'u1' }]);
      const open = (path) => page.evaluate(async (p) => {
        const m = await import('/static/file_refs.js');
        await m.openFilePreview({ dataset: { project: 'myproject', node: 'local', path: p, mime: '' } });
      }, path);
      await open('bin/app');
      await expect(page.locator('.fv-binary')).toContainText('二进制文件，请下载查看');
      await open('big.log');
      await expect(page.locator('.fv-truncated')).toHaveText('已截断：只显示前 1.0 MB（共 3.0 MB），完整内容请下载');
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });

  test('a WS send error is not called an HTTP 500', async ({ browser }) => {
    const ctx = await browser.newContext(DESKTOP);
    const page = await ctx.newPage();
    const mock = await startMockServer({ ws: true });
    try {
      await page.goto(mock.url + '/dashboard');
      await waitForWs(page);
      const conn = await openSession(page, mock, A, [{ type: 'user', detail: 'iso hi', time: now - 9000, uuid: 'u1' }]);
      await page.locator('#msg-input').click();
      await page.keyboard.type('iso 再试');
      await page.keyboard.press('Enter');
      await expect.poll(() => conn.messages.some((m) => m.type === 'send' && m.text === 'iso 再试')).toBe(true);
      const sent = conn.messages.find((m) => m.type === 'send' && m.text === 'iso 再试');
      conn.send({ type: 'send_ack', id: sent.id, key: A, status: 'error', error: '模拟失败' });
      await expect(page.locator('#toast')).toHaveText('发送消息失败：模拟失败');
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
});
