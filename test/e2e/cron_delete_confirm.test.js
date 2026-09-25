// @ts-check
//
// Deleting a cron job goes through a confirm dialog whose copy depends on
// the job (RFC §7.5):
//
//   - with history: says how many runs go and that the CLI's JSONL stays on
//     disk, reachable with `claude --resume`. The command is framed as 在终端用
//     so operators who never open a shell can skip past it;
//   - running: says the in-flight run keeps going to completion and its
//     result is recorded nowhere;
//   - never run: a short line with no history hint.
//
// The title names the task in 「」 when it has one. The message keeps its
// paragraph break (white-space: pre-wrap). The 删除 button stays disabled
// through a 3s countdown, driven here with page.clock. Under
// prefers-reduced-motion the disabled button does not pulse.
//
// 跑法：cd test/e2e && npx playwright test cron_delete_confirm.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'viewport-independent; desktop-chrome only');
  }
});

function jobs() {
  const now = Date.now();
  const base = { schedule: '0 6 * * *', prompt: 'do the thing', work_dir: '/home/user/workspace/myproject', paused: false, created_at: now - 86400000, next_run: now + 3600000, recent_runs: [] };
  return [
    { ...base, id: 'cron-del-hist', title: 'nightly report', stats: { total: 5, succeeded: 5 } },
    { ...base, id: 'cron-del-run', title: 'long job', stats: { total: 2, succeeded: 2 }, current_run: { run_id: 'r1', started_at: now - 65000, phase: 'sending', trigger: 'cron' } },
    { ...base, id: 'cron-del-new', stats: { total: 0, succeeded: 0 } },
  ];
}

/**
 * @param {import('@playwright/test').Browser} browser
 * @param {{ reducedMotion?: 'reduce' }} [opts]
 */
async function openPanel(browser, opts = {}) {
  const mock = await startMockServer({ cronJobs: jobs() });
  const ctx = await browser.newContext({ viewport: { width: 1600, height: 900 }, reducedMotion: opts.reducedMotion });
  const page = await ctx.newPage();
  await page.clock.install();
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click('#abnav-cron');
  await expect(page.locator('.cj-row')).toHaveCount(3);
  return { mock, ctx, page };
}

/** @param {import('@playwright/test').Page} page @param {string} id */
async function askDelete(page, id) {
  await page.locator(`.cj-row[data-cron-id="${id}"] [data-action="cron-menu-toggle"]`).click();
  await page.locator('.cj-menu-item[data-menu-action="delete"]').click();
  const dlg = page.locator('.confirm-dialog');
  await expect(dlg).toBeVisible();
  return dlg;
}

test('with history: names the task, counts runs, points at JSONL via the terminal', async ({ browser }) => {
  const { mock, ctx, page } = await openPanel(browser);
  try {
    const dlg = await askDelete(page, 'cron-del-hist');
    await expect(dlg.locator('#confirm-title')).toHaveText('删除「nightly report」？');
    const msg = dlg.locator('.confirm-msg');
    await expect(msg).toContainText('5 次执行记录');
    await expect(msg).toContainText('JSONL');
    await expect(msg).toContainText('在终端用 claude --resume');
    expect(await msg.evaluate((el) => getComputedStyle(el).whiteSpace)).toBe('pre-wrap');
    expect(await msg.innerText(), 'the paragraph break must render').toContain('\n');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('running: the in-flight run continues and is not recorded', async ({ browser }) => {
  const { mock, ctx, page } = await openPanel(browser);
  try {
    const dlg = await askDelete(page, 'cron-del-run');
    await expect(dlg.locator('#confirm-title')).toHaveText('删除「long job」？');
    const msg = dlg.locator('.confirm-msg');
    await expect(msg).toContainText('正在执行');
    await expect(msg).toContainText('继续运行直到完成');
    await expect(msg).toContainText('不会被记录');
    await expect(msg).not.toContainText('claude --resume');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('never run: short copy, no history hint, untitled heading', async ({ browser }) => {
  const { mock, ctx, page } = await openPanel(browser);
  try {
    const dlg = await askDelete(page, 'cron-del-new');
    await expect(dlg.locator('#confirm-title')).toHaveText('删除定时任务？');
    const msg = dlg.locator('.confirm-msg');
    await expect(msg).toContainText('尚未执行过');
    await expect(msg).not.toContainText('JSONL');
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

test('the 删除 button counts down 3s before it can be used, then deletes', async ({ browser }) => {
  const { mock, ctx, page } = await openPanel(browser);
  try {
    await page.clock.pauseAt(Date.now() + 1000);
    const dlg = await askDelete(page, 'cron-del-hist');
    const ok = dlg.locator('.confirm-ok');
    await expect(ok).toBeDisabled();
    await expect(ok).toHaveText('删除 (3)');
    await ok.click({ force: true });
    expect(mock.cronDeleteCalls, 'a click during the countdown must not delete').toEqual([]);

    await page.clock.runFor(1000);
    await expect(ok).toHaveText('删除 (2)');
    await page.clock.runFor(2000);
    await expect(ok).toBeEnabled();
    await expect(ok).toHaveText('删除');
    await ok.click();
    await expect.poll(() => mock.cronDeleteCalls).toEqual(['cron-del-hist']);
  } finally {
    await ctx.close();
    mock.server.close();
  }
});

for (const [label, reducedMotion, pulses] of /** @type {const} */ ([
  ['control: the disabled 删除 pulses by default', undefined, true],
  ['reduced motion: the disabled 删除 does not pulse', 'reduce', false],
])) {
  test(label, async ({ browser }) => {
    const { mock, ctx, page } = await openPanel(browser, { reducedMotion });
    try {
      const dlg = await askDelete(page, 'cron-del-new');
      const anim = await dlg.locator('.confirm-ok').evaluate((el) => getComputedStyle(el).animationName);
      if (pulses) expect(anim).not.toBe('none');
      else expect(anim).toBe('none');
    } finally {
      await ctx.close();
      mock.server.close();
    }
  });
}
