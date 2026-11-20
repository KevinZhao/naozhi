// E2E: a done task is not over for the user until they looked at its result
// (planner RFC D136). One not looked at is listed first, under 做完了，等你看;
// read, it stays there until the page is opened again, then it is in the
// 已完成 fold. A done task's session that was put away still opens on its
// record, saying it is put away and how to get it back; a record that
// cannot be read says so rather than load forever.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const PLANNER = 'planner:main';
const now = Date.now();
const M = 60000;
const iso = (ms) => new Date(ms).toISOString();
const UNREAD = 'task:unr000000001';
const AWAY = 'task:awy000000001';
const LOST = 'task:lst000000001';
const card = (page, id) => page.locator('#pl-task .tk-card[data-id="' + id + '"]');

function tasks() {
  return {
    tasks: [
      { id: UNREAD, kind: 'session', title: 'iso 查容量', state: 'done', by: 'planner', why: '它汇报查完了', at: iso(now - 30 * M), created: iso(now - 90 * M),
        report: { state: 'done', summary: 'FRA i8g 空闲 681 台。', at: iso(now - 31 * M) }, digest: true, result_at: iso(now - 30 * M), new: true },
      { id: AWAY, kind: 'session', title: 'iso 清理 worktree', state: 'done', by: 'planner', why: '清完了', at: iso(now - 300 * M), created: iso(now - 400 * M),
        result_at: iso(now - 300 * M), seen_at: iso(now - 290 * M), retired: iso(now - 180 * M), reap: { at: iso(now - 180 * M), by: 'planner', worktrees: [] } },
      { id: LOST, kind: 'session', title: 'iso 旧任务', state: 'done', by: 'planner', why: '', at: iso(now - 600 * M), created: iso(now - 700 * M),
        result_at: iso(now - 600 * M), seen_at: iso(now - 500 * M), retired: iso(now - 480 * M), reap: { at: iso(now - 480 * M), by: 'planner', worktrees: [] } },
    ],
  };
}

async function open(page, data) {
  const mock = await startMockServer({
    ws: true, tasks: data, plannerEvents: [], inbox: { open: [], closed: [], counts: { open: 0, badge: 0 } },
    planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
    digests: { [UNREAD]: '# iso 查容量\n\n## 最新汇报\n\nFRA i8g 空闲 681 台。\n' },
    keyEvents: { [AWAY]: [
      { type: 'user', detail: '清理 naozhi 的本地 worktree', time: now - 320 * M, uuid: 'aw-u1' },
      { type: 'text', detail: '清完了：删了 5 个已合并的 worktree。', time: now - 301 * M, uuid: 'aw-t1' },
    ] },
    eventsMissing: [LOST],
  });
  await page.goto(mock.url + '/dashboard');
  await waitForWs(page, 'CONNECTED');
  await page.click('#abnav-planner');
  return mock;
}

test('a done task not looked at is listed first, and joins 已完成 only after it was read', async ({ page }) => {
  const mock = await open(page, tasks());
  try {
    const fresh = page.locator('#pl-task .tk-sec-fresh');
    await expect(fresh.locator('.tk-sec-h')).toHaveText('做完了，等你看 1');
    await expect(fresh.locator('.tk-card')).toHaveAttribute('data-id', UNREAD);
    await expect(card(page, UNREAD).locator('.tk-report')).toContainText('FRA i8g 空闲 681 台。');
    // The fold holds only what was looked at.
    const fold = page.locator('#pl-task .tk-sec-done');
    await expect(fold.locator('summary')).toHaveText('已完成 2');
    await fold.locator('summary').click();
    await expect(fold.locator('.tk-card')).toHaveCount(2);
    await expect(fold.locator('.tk-card[data-id="' + UNREAD + '"]')).toHaveCount(0);

    // Reading it (the digest) marks it read; the card stays where it is.
    await card(page, UNREAD).locator('.tk-expand').click();
    await expect.poll(() => mock.tasksSeenCalls).toContainEqual({ ids: [UNREAD] });
    await expect(card(page, UNREAD).locator('.tk-new')).toHaveCount(0);
    await expect(fresh.locator('.tk-card[data-id="' + UNREAD + '"]')).toHaveCount(1);

    // The page opened again: it is in 已完成 now.
    await page.click('#abnav-chat');
    await page.click('#abnav-planner');
    await expect(page.locator('#pl-task .tk-sec-fresh')).toHaveCount(0);
    await expect(page.locator('#pl-task .tk-sec-done summary')).toHaveText('已完成 3');
  } finally {
    mock.server.close();
  }
});

test('a done task put away opens on its record; one whose record is gone says so', async ({ page }) => {
  const mock = await open(page, tasks());
  try {
    await page.locator('#pl-task .tk-sec-done summary').click();
    await card(page, AWAY).locator('.tk-open').click();
    const pane = page.locator('#pl-sess');
    await expect(pane.locator('#pl-sess-stream')).toContainText('清完了：删了 5 个已合并的 worktree。');
    await expect(pane.locator('.pl-sess-away')).toHaveText('已收起');
    const reopen = pane.locator('#pl-sess-line [data-action="task-reopen"]');
    await expect(reopen).toHaveText('重新打开');
    await expect(page.locator('#pl-input')).toBeDisabled();
    await expect(page.locator('#pl-target-note')).toContainText('已收起，记录还在');
    await reopen.click();
    await expect.poll(() => mock.tasksCalls).toContainEqual({ id: AWAY, state: 'doing' });

    // A record that is not there: said, not "正在加载…" for ever.
    await page.click('[data-action="planner-close-session"]');
    await card(page, LOST).locator('.tk-open').click();
    await expect(pane.locator('#pl-sess-stream .pl-empty')).toContainText('读不到这个会话的记录');
  } finally {
    mock.server.close();
  }
});
