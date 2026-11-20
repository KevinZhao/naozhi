// E2E: the Planner page's 工作线 group (planner RFC §15.19, D134). The lines
// of work the Planner keeps are listed in the left column; "⋯" opens what
// was decided and the controls: 接着做 writes into the Planner's input box,
// 编辑 and 结束 change the ledger. A read from another machine offers no
// edit; with no line the group takes no room.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const PLANNER = 'planner:main';
const day = 24 * 3600 * 1000;
const iso = (ms) => new Date(ms).toISOString();

function fronts(writable) {
  const now = Date.now();
  return {
    writable,
    fronts: [
      { id: 'jd-tgw', name: 'JD TGW 迁移', state: 'EC2 VXLAN 方案被否', next: '改迁移路线图', waiting: '等客户确认 2468 的配置',
        decisions: [{ at: iso(now - 3600e3), text: '先把 prefix VPC 的资源迁回 JDOS VPC' }], created: iso(now - 2 * day), updated: iso(now - 3600e3) },
      { id: 'blog', name: 'trn2 blog', state: '草稿完成', created: iso(now - 9 * day), updated: iso(now - 5 * day) },
    ],
  };
}

async function open(page, overrides) {
  const mock = await startMockServer({
    ws: true,
    planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
    plannerEvents: [],
    ...overrides,
  });
  await page.goto(mock.url + '/dashboard');
  await waitForWs(page, 'CONNECTED');
  await page.click('#abnav-planner');
  return mock;
}

test('the lines of work are listed, continued, edited and closed', async ({ page }) => {
  const mock = await open(page, { fronts: fronts(true) });
  try {
    const group = page.locator('#pl-front');
    await expect(group.locator('.pl-group-h')).toHaveText('工作线 2');
    const cards = group.locator('.fr-card');
    await expect(cards).toHaveCount(2);
    await expect(cards.nth(0).locator('.tk-title-t')).toHaveText('JD TGW 迁移');
    await expect(cards.nth(0).locator('.fr-state')).toHaveText('EC2 VXLAN 方案被否');
    await expect(cards.nth(0).locator('.fr-meta')).toHaveText('→ 改迁移路线图 · 等 客户确认 2468 的配置');
    await expect(cards.nth(1).locator('.fr-meta')).toHaveText('5 天没动');

    // "⋯": what was decided, and the controls.
    await cards.nth(0).locator('.tk-more').click();
    await expect(cards.nth(0).locator('.fr-decisions li')).toHaveText('定了：先把 prefix VPC 的资源迁回 JDOS VPC');
    await cards.nth(0).locator('[data-action="front-continue"]').click();
    await expect(page.locator('#pl-input')).toHaveValue('继续「JD TGW 迁移」');
    await expect(page.locator('#pl-input')).toBeFocused();

    // 编辑: the three fields, 等 stripped from who it waits on; a field
    // emptied is cleared.
    await cards.nth(0).locator('.tk-more').click();
    await cards.nth(0).locator('.tk-more').click();
    await cards.nth(0).locator('[data-action="front-edit"]').click();
    const form = cards.nth(0).locator('.fr-edit');
    await expect(form.locator('textarea[data-field="waiting"]')).toHaveValue('客户确认 2468 的配置');
    await form.locator('textarea[data-field="state"]').fill('路线图已改');
    await form.locator('textarea[data-field="waiting"]').fill('');
    await form.locator('[type="submit"]').click();
    await expect(cards.nth(0).locator('.fr-state')).toHaveText('路线图已改');
    await expect(cards.nth(0).locator('.fr-meta')).toHaveText('→ 改迁移路线图');
    expect(mock.frontsCalls).toEqual([{ id: 'jd-tgw', state: '路线图已改', next: '改迁移路线图', waiting: '' }]);

    // 结束 takes the line off the list.
    await cards.nth(1).locator('.tk-more').click();
    await cards.nth(1).locator('[data-action="front-close"]').click();
    await expect(cards).toHaveCount(1);
    await expect(group.locator('.pl-group-h')).toHaveText('工作线 1');
    expect(mock.frontsCalls[1]).toEqual({ id: 'blog', close: true });
  } finally {
    mock.server.close();
  }
});

test('a read from another machine offers no edit; no line, no group', async ({ page }) => {
  let mock = await open(page, { fronts: fronts(false) });
  try {
    const card = page.locator('#pl-front .fr-card').first();
    await card.locator('.tk-more').click();
    await expect(card.locator('[data-action="front-continue"]')).toBeVisible();
    await expect(card.locator('[data-action="front-edit"]')).toHaveCount(0);
    await expect(card.locator('[data-action="front-close"]')).toHaveCount(0);
  } finally {
    mock.server.close();
  }
  mock = await open(page, { fronts: { fronts: [], writable: true } });
  try {
    await expect(page.locator('#pl-task .pl-group-h')).toBeVisible();
    await expect(page.locator('#pl-front').locator('xpath=ancestor::section')).toBeHidden();
  } finally {
    mock.server.close();
  }
});
