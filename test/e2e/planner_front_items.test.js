// E2E: the open items under the Planner page's 工作线 group (planner RFC
// §15.23, D143). A line's card lists the things left undone on it, five
// until unfolded; each says where it came from, opens its source and can be
// struck; an opened card takes one more typed in. The items under no line
// are listed after the cards as 未归线, by workspace. A read from another
// machine offers neither the strike nor the input.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const PLANNER = 'planner:main';
const WS = 'ws_ab12cd34';
const TASK = 'task:9f3c2a71d0';
const day = 24 * 3600 * 1000;
const iso = (ms) => new Date(ms).toISOString();

function ledger(writable) {
  const now = Date.now();
  const item = (n, extra) => ({ id: 'i000000' + n, text: '第 ' + n + ' 件没做完的事', from: 'report', created: iso(now - (20 - n) * 60e3), ...extra });
  return {
    writable,
    fronts: [
      { id: 'gateway-refactor', name: '网关重构', state: '迁了一半', sessions: [TASK], created: iso(now - 2 * day), updated: iso(now - 3600e3) },
      { id: 'docs', name: '文档', created: iso(now - 2 * day), updated: iso(now - 2 * 3600e3) },
    ],
    items: [
      item(1, { front: 'gateway-refactor', source: TASK, workspace: WS }),
      item(2, { front: 'gateway-refactor', source: 'https://github.com/owner/repo/pull/12', from: 'patrol' }),
      item(3, { front: 'gateway-refactor', source: '11-22 用户说', from: 'user' }),
      item(4, { front: 'gateway-refactor', from: 'planner' }),
      item(5, { front: 'gateway-refactor' }),
      item(6, { front: 'gateway-refactor' }),
      item(7, { front: 'gateway-refactor' }),
      item(8, { workspace: WS, source: '#4025', from: 'planner' }),
      item(9, { from: 'user' }),
    ],
  };
}

async function open(page, fronts) {
  const mock = await startMockServer({
    ws: true,
    planner: { enabled: true, stopped: false, key: PLANNER, alive: true, workspaces: { [WS]: 'gateway' } },
    plannerEvents: [],
    fronts,
  });
  await page.goto(mock.url + '/dashboard');
  await waitForWs(page, 'CONNECTED');
  await page.click('#abnav-planner');
  return mock;
}

test('a line lists its open items, five until unfolded, each with where it came from and its source', async ({ page }) => {
  const mock = await open(page, ledger(true));
  try {
    const group = page.locator('#pl-front');
    await expect(group.locator('.pl-group-h')).toHaveText('工作线 2');
    const card = group.locator('.fr-card[data-id="gateway-refactor"]');
    const items = card.locator('.fr-item');
    await expect(items).toHaveCount(5);
    await expect(items.nth(0).locator('.fr-item-t')).toHaveText('第 1 件没做完的事');
    await expect(items.nth(0).locator('.fr-from')).toHaveText('汇报');
    await expect(items.nth(1).locator('.fr-from')).toHaveText('巡查');
    await expect(items.nth(2).locator('.fr-from')).toHaveText('你');
    await expect(items.nth(3).locator('.fr-from')).toHaveText('Planner');
    await expect(group.locator('.fr-card[data-id="docs"] .fr-item')).toHaveCount(0);

    // The source: a link opens in a new tab, a session in the middle
    // column, anything else is in the tooltip only.
    const link = items.nth(1).locator('a.fr-src');
    await expect(link).toHaveAttribute('href', 'https://github.com/owner/repo/pull/12');
    await expect(link).toHaveAttribute('target', '_blank');
    await expect(link).toHaveAttribute('rel', 'noopener noreferrer');
    await expect(items.nth(2).locator('.fr-src')).toHaveCount(0);
    await expect(items.nth(2).locator('.fr-item-t')).toHaveAttribute('title', '第 3 件没做完的事\n出处：11-22 用户说');
    await expect(items.nth(3).locator('.fr-src')).toHaveCount(0);

    // "另有 2 条" unfolds the rest, and folds them again; a repaint keeps it.
    const more = card.locator('[data-action="front-items-more"]');
    await expect(more).toHaveText('另有 2 条');
    await more.click();
    await expect(items).toHaveCount(7);
    await expect(more).toHaveText('收起');
    await card.locator('.tk-more').click();
    await expect(items).toHaveCount(7);
    await more.click();
    await expect(items).toHaveCount(5);

    await items.nth(0).locator('button.fr-src').click();
    await expect(page.locator('#pl-sess')).toBeVisible();
    await expect(page.locator('#pl-sess')).toHaveAttribute('data-key', TASK);
  } finally {
    mock.server.close();
  }
});

test('the items under no line are listed by workspace, named', async ({ page }) => {
  const mock = await open(page, ledger(true));
  try {
    const group = page.locator('#pl-front');
    await expect(group.locator('.fr-loose-h')).toHaveText('未归线 2');
    const blocks = group.locator('.fr-loose');
    await expect(blocks).toHaveCount(2);
    await expect(blocks.nth(0).locator('.fr-loose-ws')).toHaveText('gateway');
    await expect(blocks.nth(0).locator('.fr-item-t')).toHaveText('第 8 件没做完的事');
    // "#4025" names no repository the page knows: not a link.
    await expect(blocks.nth(0).locator('.fr-src')).toHaveCount(0);
    await expect(blocks.nth(1).locator('.fr-loose-ws')).toHaveText('没有工作区');
    await expect(blocks.nth(1).locator('.fr-item-t')).toHaveText('第 9 件没做完的事');

    // A line closed takes its items to 未归线: they are still left.
    const card = group.locator('.fr-card[data-id="gateway-refactor"]');
    await card.locator('.tk-more').click();
    await card.locator('[data-action="front-close"]').click();
    await expect(group.locator('.fr-card')).toHaveCount(1);
    await expect(group.locator('.fr-loose-h')).toHaveText('未归线 9');
  } finally {
    mock.server.close();
  }
});

test('an item is struck at once, and comes back when the ledger refuses', async ({ page }) => {
  const fronts = ledger(true);
  const mock = await open(page, fronts);
  try {
    const card = page.locator('#pl-front .fr-card[data-id="gateway-refactor"]');
    const items = card.locator('.fr-item');
    await expect(items).toHaveCount(5);
    await items.nth(1).locator('.fr-item-x').click();
    await expect(card.locator('.fr-item[data-id="i0000002"]')).toHaveCount(0);
    await expect(card.locator('[data-action="front-items-more"]')).toHaveText('另有 1 条');
    await expect.poll(() => mock.frontsCalls).toEqual([{ item_done: 'i0000002' }]);
    expect(fronts.items.some((it) => it.id === 'i0000002')).toBe(false);

    // The ledger fails: the item is back where it stood, with why.
    fronts.itemsFail = true;
    await items.nth(0).locator('.fr-item-x').click();
    await expect(page.locator('#toast')).toContainText('没有划掉：台账写不进去');
    await expect(items.nth(0)).toHaveAttribute('data-id', 'i0000001');
    await expect(card.locator('[data-action="front-items-more"]')).toHaveText('另有 1 条');
    expect(mock.frontsCalls[1]).toEqual({ item_done: 'i0000001' });

    // One the ledger no longer has is struck already.
    fronts.itemsFail = false;
    fronts.items = fronts.items.filter((it) => it.id !== 'i0000001');
    await items.nth(0).locator('.fr-item-x').click();
    await expect(card.locator('.fr-item[data-id="i0000001"]')).toHaveCount(0);
    await expect(items.nth(0)).toHaveAttribute('data-id', 'i0000003');
  } finally {
    mock.server.close();
  }
});

test('an item typed under a line or a workspace is added as yours', async ({ page }) => {
  const fronts = ledger(true);
  const mock = await open(page, fronts);
  try {
    const group = page.locator('#pl-front');
    const card = group.locator('.fr-card[data-id="docs"]');
    await expect(card.locator('.fr-add')).toHaveCount(0);
    await card.locator('.tk-more').click();
    const input = card.locator('.fr-add input');
    await input.fill('  给 README 补上   安装步骤 ');
    await input.press('Enter');
    await expect(card.locator('.fr-item-t')).toHaveText('给 README 补上 安装步骤');
    await expect(card.locator('.fr-from')).toHaveText('你');
    await expect(input).toHaveValue('');
    await expect.poll(() => mock.frontsCalls).toEqual([{ item: { text: '给 README 补上 安装步骤', front: 'docs' } }]);
    // The ledger's item took the typed one's place: it can be struck.
    await expect(card.locator('.fr-item-x')).toHaveCount(1);
    await expect(card.locator('.fr-item')).toHaveAttribute('data-id', /^i\d{7}$/);

    // Among a workspace's loose ones.
    const loose = group.locator('.fr-loose[data-ws="' + WS + '"]');
    await loose.locator('.fr-add input').fill('清掉旧的 worktree');
    await loose.locator('.fr-add input').press('Enter');
    await expect(loose.locator('.fr-item')).toHaveCount(2);
    await expect.poll(() => mock.frontsCalls[1]).toEqual({ item: { text: '清掉旧的 worktree', workspace: WS } });

    // The ledger fails: the item goes, the words are back in the input.
    fronts.itemsFail = true;
    await input.fill('写不进去的一条');
    await input.press('Enter');
    await expect(page.locator('#toast')).toContainText('没有记下：台账写不进去');
    await expect(card.locator('.fr-item')).toHaveCount(1);
    await expect(input).toHaveValue('写不进去的一条');
  } finally {
    mock.server.close();
  }
});

test('a read from another machine offers no strike and no input; items alone still show', async ({ page }) => {
  let mock = await open(page, ledger(false));
  try {
    const group = page.locator('#pl-front');
    const card = group.locator('.fr-card[data-id="gateway-refactor"]');
    await expect(card.locator('.fr-item')).toHaveCount(5);
    await card.locator('.tk-more').click();
    await expect(card.locator('[data-action="front-continue"]')).toBeVisible();
    await expect(group.locator('.fr-item-x')).toHaveCount(0);
    await expect(group.locator('.fr-add')).toHaveCount(0);
  } finally {
    mock.server.close();
  }
  // No line, but something left: the group shows it.
  const only = ledger(true);
  only.fronts = [];
  mock = await open(page, only);
  try {
    const group = page.locator('#pl-front');
    await expect(group.locator('.pl-group-h')).toHaveText('工作线');
    await expect(group.locator('.fr-loose-h')).toHaveText('未归线 9');
    await expect(group.locator('.fr-loose')).toHaveCount(2);
  } finally {
    mock.server.close();
  }
});
