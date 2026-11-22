// E2E: the user's say on what the project patrol found (planner RFC §15.23
// ④, D142). An open item the patrol filed opens with "⋯": a deep check's
// finding shows its evidence as links, what it costs and how to fix it,
// with 做 / 不做 / 这类别再提; every patrol item has 这个工作区别巡查了.
// A choice shows at once and goes to POST /api/planner/patrol/decide; a
// failure puts the list back. A read from another machine sees the finding
// and no choice. A notice's card keeps the last choice behind its own "⋯",
// answered through the inbox like any card.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const PLANNER = 'planner:main';
const WS = 'ws_ab12cd34';
const OTHER = 'ws_ef56ab78';
const now = Date.now();
const iso = (ms) => new Date(ms).toISOString();
const ref = (id, ws = WS) => ws + '/' + id;
const BLOB = 'https://github.com/owner/repo/blob/0123456789abcdef0123456789abcdef01234567/internal/router.go#L42';
const PR = 'https://github.com/owner/repo/pull/31';

function ledger(writable) {
  let n = 0;
  const item = (text, extra) => ({ id: 'i000000' + (++n), text, from: 'patrol', workspace: WS, created: iso(now - (20 - n) * 60e3), ...extra });
  const finding = (extra) => ({
    kind: 'bug', size: 'S', confidence: 'high', state: 'item', impact: '重连即崩', fix: '先判空再取会话',
    evidence: [{ text: 'internal/router.go:42', link: BLOB }], ...extra,
  });
  return {
    writable,
    fronts: [],
    items: [
      item('重连时会话为空会崩', { source: 'patrol:' + ref('bug-router-nil') }),
      item('退避有两份实现', { source: 'patrol:' + ref('debt-dup-backoff') }),
      item('watchdog 的旧开关没人用了', { source: 'patrol:' + ref('debt-old-flag') }),
      item('已备好 PR #31：去掉重复的超时常量', { source: 'patrol:' + ref('debt-dup-timeout') }),
      item('2 处改动没提交，最早的在 9 天前', { source: 'check:dirty:/srv/gateway' }),
      item('例行任务「夜间构建」连续失败 3 次', { source: 'check:routine:job1' }),
      item('文档里的 debt', { source: 'patrol:' + ref('debt-docs', OTHER), workspace: OTHER }),
      item('手工记的一条', { source: '11-22 用户说', from: 'user' }),
    ],
    findings: {
      [ref('bug-router-nil')]: finding({ evidence: [{ text: 'internal/router.go:42', link: BLOB }, { text: 'docs/notes.md:3' }, { text: 'bad', link: 'javascript:alert(1)' }] }),
      [ref('debt-dup-backoff')]: finding({ kind: 'debt', size: 'M', confidence: 'medium' }),
      [ref('debt-old-flag')]: finding({ kind: 'debt' }),
      [ref('debt-dup-timeout')]: finding({ kind: 'debt', state: 'pr', pr: PR, ci: '通过' }),
      [ref('debt-docs', OTHER)]: finding({ kind: 'debt' }),
    },
  };
}

async function open(page, extra) {
  const mock = await startMockServer({
    ws: true,
    planner: { enabled: true, stopped: false, key: PLANNER, alive: true, workspaces: { [WS]: 'gateway', [OTHER]: 'docs' } },
    plannerEvents: [],
    ...extra,
  });
  await page.goto(mock.url + '/dashboard');
  await waitForWs(page, 'CONNECTED');
  await page.click('#abnav-planner');
  if (extra.fronts) {
    // The workspace's items are past the five a list shows folded.
    await page.locator('#pl-front [data-action="front-items-more"]').click();
    await expect(page.locator('#pl-front .fr-item')).toHaveCount(extra.fronts.items.length);
  }
  return mock;
}

const itemOf = (page, text) => page.locator('#pl-front .fr-item', { hasText: text });
const texts = (page) => page.locator('#pl-front .fr-item-t').allTextContents();

test('a finding opens with its evidence as links and the choices about it', async ({ page }) => {
  const mock = await open(page, { fronts: ledger(true) });
  try {
    const bug = itemOf(page, '重连时会话为空会崩');
    await expect(bug.locator('.fr-from')).toHaveText('巡查');
    // A finding is not struck: it is done, turned down, or left.
    await expect(bug.locator('[data-action="front-item-done"]')).toHaveCount(0);
    await expect(bug.locator('.fr-detail')).toHaveCount(0);
    const more = bug.locator('[data-action="front-item-more"]');
    await expect(more).toHaveAttribute('aria-expanded', 'false');
    await more.click();
    await expect(more).toHaveAttribute('aria-expanded', 'true');
    const detail = bug.locator('.fr-detail');
    await expect(detail.locator('.fr-detail-meta')).toHaveText('bug · S · 把握大');
    const ev = detail.locator('.fr-ev li');
    await expect(ev).toHaveCount(3);
    const link = ev.nth(0).locator('a');
    await expect(link).toHaveText('internal/router.go:42');
    await expect(link).toHaveAttribute('href', BLOB);
    await expect(link).toHaveAttribute('target', '_blank');
    await expect(link).toHaveAttribute('rel', 'noopener noreferrer');
    // Evidence that opens nowhere, or not over http(s), is text.
    await expect(ev.nth(1)).toHaveText('docs/notes.md:3');
    await expect(ev.nth(1).locator('a')).toHaveCount(0);
    await expect(ev.nth(2).locator('a')).toHaveCount(0);
    await expect(detail).toContainText('影响重连即崩');
    await expect(detail).toContainText('改法先判空再取会话');
    await expect(detail.locator('[data-action="front-item-decide"]')).toHaveText(['做', '不做', '这类别再提', '这个工作区别巡查了']);
    // No inline handlers (CSP): the buttons carry data-action only.
    expect(await detail.locator('[onclick]').count()).toBe(0);

    // With its pull request ready "do" is merging it, on GitHub.
    await itemOf(page, '已备好 PR #31').locator('[data-action="front-item-more"]').click();
    const ready = itemOf(page, '已备好 PR #31').locator('.fr-detail');
    await expect(bug.locator('.fr-detail')).toHaveCount(0); // one open at a time
    await expect(ready.locator('a[href="' + PR + '"]')).toHaveText('owner/repo/pull/31');
    await expect(ready).toContainText('CI 通过');
    await expect(ready.locator('[data-action="front-item-decide"]')).toHaveText(['不做', '这类别再提', '这个工作区别巡查了']);

    // A check's hit keeps its strike and offers only the workspace's choice.
    const dirty = itemOf(page, '2 处改动没提交');
    await expect(dirty.locator('[data-action="front-item-done"]')).toHaveCount(1);
    await dirty.locator('[data-action="front-item-more"]').click();
    await expect(dirty.locator('.fr-ev')).toHaveCount(0);
    await expect(dirty.locator('[data-action="front-item-decide"]')).toHaveText(['这个工作区别巡查了']);
    // Not the patrol's: nothing to open.
    await expect(itemOf(page, '手工记的一条').locator('[data-action="front-item-more"]')).toHaveCount(0);
    expect(mock.patrolCalls).toEqual([]);
  } finally {
    await mock.server.close();
  }
});

test('不做 takes the finding off the list; 做 hands it to the Planner', async ({ page }) => {
  const mock = await open(page, { fronts: ledger(true) });
  try {
    const bug = itemOf(page, '重连时会话为空会崩');
    await bug.locator('[data-action="front-item-more"]').click();
    await bug.locator('[data-choice="skip"]').click();
    await expect(bug).toHaveCount(0);
    await expect.poll(() => mock.patrolCalls).toEqual([{ ref: ref('bug-router-nil'), choice: 'skip' }]);
    await expect(page.locator('#pl-front .fr-item')).toHaveCount(7);

    const debt = itemOf(page, '退避有两份实现');
    await debt.locator('[data-action="front-item-more"]').click();
    await expect(debt.locator('.fr-detail-meta')).toHaveText('debt · M · 把握中等');
    await debt.locator('[data-choice="do"]').click();
    await expect(debt.locator('.fr-detail-meta')).toHaveText('debt · M · 把握中等 · 已交给 Planner 派任务');
    await expect(debt.locator('[data-action="front-item-decide"]')).toHaveText(['不做', '这类别再提', '这个工作区别巡查了']);
    await expect.poll(() => mock.patrolCalls[1]).toEqual({ ref: ref('debt-dup-backoff'), choice: 'do' });
    await expect(page.locator('#pl-front .fr-item')).toHaveCount(7);
  } finally {
    await mock.server.close();
  }
});

test('这类别再提 takes the kind\'s other findings in that workspace along, but for one with its pull request', async ({ page }) => {
  const mock = await open(page, { fronts: ledger(true) });
  try {
    const debt = itemOf(page, '退避有两份实现');
    await debt.locator('[data-action="front-item-more"]').click();
    await debt.locator('[data-choice="mute_kind"]').click();
    await expect.poll(() => texts(page)).toEqual(['重连时会话为空会崩', '已备好 PR #31：去掉重复的超时常量', '2 处改动没提交，最早的在 9 天前',
      '例行任务「夜间构建」连续失败 3 次', '手工记的一条', '文档里的 debt']);
    await expect.poll(() => mock.patrolCalls).toEqual([{ ref: ref('debt-dup-backoff'), choice: 'mute_kind' }]);
    await expect(page.locator('#toast')).toContainText('「gateway」的 debt 类不再报');
    await expect(page.locator('#toast')).toContainText('planner/patrol/declined.json');
  } finally {
    await mock.server.close();
  }
});

test('这个工作区别巡查了 takes the patrol\'s items of that workspace off the list', async ({ page }) => {
  const mock = await open(page, { fronts: ledger(true) });
  try {
    const dirty = itemOf(page, '2 处改动没提交');
    await dirty.locator('[data-action="front-item-more"]').click();
    await dirty.locator('[data-choice="mute_ws"]').click();
    // A standing task's failures, the other workspace's and the user's stay.
    await expect.poll(() => texts(page)).toEqual(['例行任务「夜间构建」连续失败 3 次', '手工记的一条', '文档里的 debt']);
    await expect.poll(() => mock.patrolCalls).toEqual([{ workspace: WS, choice: 'mute_ws' }]);
    await expect(page.locator('#toast')).toContainText('不再巡查「gateway」');
  } finally {
    await mock.server.close();
  }
});

test('a choice the gateway refuses puts the list back and says why', async ({ page }) => {
  const fronts = ledger(true);
  fronts.decideFail = '现在联系不上 Planner（没开或已停用），这条发现没有交出去';
  const mock = await open(page, { fronts });
  try {
    const before = await texts(page);
    const debt = itemOf(page, '退避有两份实现');
    await debt.locator('[data-action="front-item-more"]').click();
    await debt.locator('[data-choice="do"]').click();
    await expect(page.locator('#toast')).toContainText('没有生效：现在联系不上 Planner');
    await expect(debt.locator('.fr-detail-meta')).toHaveText('debt · M · 把握中等');
    await expect(debt.locator('[data-choice="do"]')).toHaveCount(1);

    await debt.locator('[data-choice="mute_kind"]').click();
    await expect.poll(() => mock.patrolCalls.length).toBe(2);
    await expect.poll(() => texts(page)).toEqual(before);
  } finally {
    await mock.server.close();
  }
});

test('a read from another machine shows the finding and offers no choice', async ({ page }) => {
  const mock = await open(page, { fronts: ledger(false) });
  try {
    const bug = itemOf(page, '重连时会话为空会崩');
    await bug.locator('[data-action="front-item-more"]').click();
    await expect(bug.locator('.fr-ev a')).toHaveCount(1);
    await expect(bug.locator('[data-action="front-item-decide"]')).toHaveCount(0);
    // A check's hit has nothing behind it there.
    await expect(itemOf(page, '2 处改动没提交').locator('[data-action="front-item-more"]')).toHaveCount(0);
    await expect(page.locator('#pl-front [data-action="front-item-done"]')).toHaveCount(0);
  } finally {
    await mock.server.close();
  }
});

test('a notice\'s card keeps 这个工作区别巡查了 behind ⋯ and answers through the inbox', async ({ page }) => {
  const card = (id, decision) => ({ id, kind: 'notice', level: 'timely', unit: PLANNER, title: 'iso ' + id, why: 'CI 通过', link: PR,
    created: iso(now - 50000), state: 'open', source: 'planner:' + id, decision });
  const inbox = {
    open: [
      card('in_pr', { handler: 'patrol', payload: { ref: ref('debt-dup-timeout') }, actions: ['不做', '这类别再提'], more: ['这个工作区别巡查了'] }),
      card('in_plain', { handler: 'ask', actions: ['好', '先不'] }),
    ],
    closed: [],
    counts: { open: 2, badge: 0 },
  };
  const mock = await open(page, { inbox });
  try {
    const pr = page.locator('#pl-cards .pl-card[data-id="in_pr"]');
    await expect(pr.locator('.pl-card-acts > button')).toHaveText(['不做', '这类别再提']);
    // Turning it down is offered, not put forward: the link is what to open.
    await expect(pr.locator('.pl-card-acts .pri')).toHaveCount(0);
    await expect(pr.locator('a.pl-link')).toHaveAttribute('href', PR);
    const more = pr.locator('details.pl-card-more');
    await expect(more.locator('button')).toBeHidden();
    // A card with nothing kept back is as it was.
    const plain = page.locator('#pl-cards .pl-card[data-id="in_plain"]');
    await expect(plain.locator('.pl-card-acts > button')).toHaveText(['好', '先不']);
    await expect(plain.locator('details')).toHaveCount(0);
    expect(await plain.locator('.pl-card-acts').innerHTML()).toBe(
      '<button type="button" class="pl-btn pri" data-action="planner-inbox-decide" data-id="in_plain" data-act="好">好</button>' +
      '<button type="button" class="pl-btn" data-action="planner-inbox-decide" data-id="in_plain" data-act="先不">先不</button>');

    await more.locator('summary').click();
    await more.locator('button', { hasText: '这个工作区别巡查了' }).click();
    await expect.poll(() => mock.inboxCalls.filter((c) => c.path === '/api/inbox/decide').map((c) => c.body))
      .toEqual([{ id: 'in_pr', action: '这个工作区别巡查了' }]);
    await expect(pr).toHaveCount(0);
  } finally {
    await mock.server.close();
  }
});
