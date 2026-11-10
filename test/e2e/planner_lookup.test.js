// E2E: the Planner's web lookup (planner RFC §15.12 ⑨, D107). The lookup
// block is one line, 查一下 · the question; the answer the gateway hands back
// (<naozhi-lookup q run state>) is not your bubble but the query card: the
// question and the answer's first sentence, the source sites as links, the
// whole answer and each fact's quote in the fold. A failed lookup says why.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const PLANNER = 'planner:main';
const now = Date.now();
const block = (a) => '```naozhi-action\n' + JSON.stringify(a) + '\n```';
const RUN = '20261110T093000-ab12';
const BAD = '20261110T094500-cd34';
const result = {
  answer: 'M4 Max 的内存带宽是 546GB/s。16 核 CPU 版才有这个带宽。',
  facts: [
    { claim: 'M4 Max 内存带宽 546GB/s', url: 'https://www.apple.com/macbook-pro/specs/', quote: 'up to 546GB/s memory bandwidth' },
    { claim: '14 核版为 410GB/s', url: 'https://support.apple.com/kb/SP1', quote: '410GB/s memory bandwidth' },
  ],
  confidence: 'high',
};

test('lookup line and query card', async ({ page }) => {
  const mock = await startMockServer({
    ws: true,
    planner: { enabled: true, stopped: false, key: PLANNER, alive: true },
    plannerLookups: {
      [RUN]: { id: RUN, q: 'M4 Max 内存带宽', state: 'ok', result },
      [BAD]: { id: BAD, q: 'trn3 价格', state: 'failed', error: '查询结果不合格：answer 是空的' },
    },
    plannerEvents: [
      { type: 'text', time: now - 9000, uuid: 'lk-t1', detail: '凭记忆是 546GB/s，我查一下核实。\n\n' +
        block({ type: 'lookup', q: 'M4 Max 内存带宽', why: '用户问规格' }) },
      { type: 'user', time: now - 6000, uuid: 'lk-u1', detail: '<naozhi-lookup q="M4 Max 内存带宽" run="' + RUN + '" state="ok">\n以下是……\n' +
        JSON.stringify(result) + '\n</naozhi-lookup>\n\n<naozhi-snapshot unchanged since="09:30" at="x"/>' },
      { type: 'text', time: now - 5000, uuid: 'lk-t2', detail: '核实了，是 546GB/s。' },
      { type: 'user', time: now - 3000, uuid: 'lk-u2', detail: '<naozhi-lookup q="trn3 价格" run="' + BAD + '" state="failed">\n没查成\n</naozhi-lookup>' },
    ],
  });
  try {
    await page.goto(mock.url + '/dashboard');
    await waitForWs(page, 'CONNECTED');
    await page.click('#abnav-planner');
    const stream = page.locator('#pl-stream');
    await expect(stream.locator('.pl-act-lookup')).toHaveText('查一下 · M4 Max 内存带宽');
    // The answer is no bubble of yours, nor raw JSON.
    await expect(stream.locator('.event.user')).toHaveCount(0);
    await expect(stream).not.toContainText('naozhi-lookup');
    const card = stream.locator('details.pl-lookup');
    await expect(card.locator('summary')).toHaveText('网页查询 · M4 Max 内存带宽M4 Max 的内存带宽是 546GB/s。可信度高');
    await expect(card.locator('.pl-lookup-one')).toBeVisible();
    const src = stream.locator('.pl-lookup-src a');
    await expect(src).toHaveText(['apple.com', 'support.apple.com']);
    await expect(src.first()).toHaveAttribute('href', 'https://www.apple.com/macbook-pro/specs/');
    await expect(card.locator('.pl-lookup-quote').first()).toBeHidden();
    await card.locator('summary').click();
    await expect(card.locator('.pl-lookup-quote')).toHaveText(['up to 546GB/s memory bandwidth', '410GB/s memory bandwidth']);
    await expect(card.locator('.pl-lookup-ans')).toContainText('16 核 CPU 版');
    const failed = stream.locator('.pl-lookup-failed');
    await expect(failed).toContainText('网页查询 · trn3 价格');
    await expect(failed).toContainText('没查成：查询结果不合格');
  } finally {
    mock.server.close();
  }
});
