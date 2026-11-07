// Meeting recording in the dashboard (meetings.js, docs/rfc/meeting-notes.md §6–§7): the red
// "录音中" strip follows meeting_state frames on every view and its 停止 posts
// /api/meetings/stop; 设置 shows the two macOS grants and brings up their prompts; the 成品库's
// "会议纪要" row opens the meetings panel, whose meeting shows its summary and transcript.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const ID = '2026-11-09-a1b2c3';
const started = () => new Date(Date.now() - 65000).toISOString();

function meetingsFixture(auth) {
  return {
    status: { available: true, auth },
    meetings: [{ id: ID, title: '客户 A 周会', app: 'Teams', started: '2026-11-09T02:00:00Z', state: 'done', seconds: 1830, audio_expires: '2026-12-09T03:00:00Z' }],
    details: {
      [ID]: {
        meeting: { id: ID, title: '客户 A 周会', app: 'Teams', started: '2026-11-09T02:00:00Z', state: 'done', seconds: 1830, engine: 'qwen3-asr-1.7b-mlx', audio_expires: '2026-12-09T03:00:00Z' },
        summary: '# 客户 A 周会 · 纪要\n\n## 结论\n上线定在下周。',
        transcript: [
          { start: 3, end: 6, speaker: '你', text: '我们先看报价。' },
          { start: 7, end: 12, speaker: '说话人 A', text: 'OK, the price looks fine.' },
        ],
        audio: ['mic', 'remote'],
      },
    },
  };
}

for (const [name, viewport] of [['desktop', { width: 1280, height: 800 }], ['mobile', { width: 390, height: 844 }]]) {
  test.describe(`meeting recording (${name})`, () => {
    test('the red strip follows meeting_state and 停止 stops the recording', async ({ browser }) => {
      const mock = await startMockServer({ ws: true, meetings: meetingsFixture({ microphone: 'authorized', audio_capture: 'authorized' }) });
      const ctx = await browser.newContext({ viewport });
      const page = await ctx.newPage();
      try {
        await page.goto(mock.url + '/dashboard');
        await waitForWs(page);
        const strip = page.locator('#rec-strip');
        await expect(strip).toBeHidden();
        const conn = mock.wsConnections[mock.wsConnections.length - 1];
        const st = { available: true, auth: { microphone: 'authorized', audio_capture: 'authorized' } };
        conn.send({ type: 'meeting_state', state: { ...st, recording: { id: ID, title: '客户 A 周会', app: 'Teams', started: started(), state: 'recording', seconds: 60 } } });
        await expect(strip).toBeVisible();
        await expect(strip.locator('#rec-open')).toHaveText('录音中 · 客户 A 周会');
        await expect(strip.locator('#rec-time')).toHaveText(/^01:0\d$/);
        await strip.locator('#rec-stop').click();
        await expect.poll(() => mock.meetingCalls.map((c) => c.path)).toContain('/api/meetings/stop');
        conn.send({ type: 'meeting_state', state: st });
        await expect(strip).toBeHidden();
      } finally {
        await ctx.close();
        await new Promise((r) => mock.server.close(r));
      }
    });

    test('设置 shows the grants and 授权 brings up the prompts', async ({ browser }) => {
      test.skip(name === 'mobile', 'the rail is desktop only; 设置 renders the same');
      const mock = await startMockServer({ meetings: meetingsFixture({ microphone: 'authorized', audio_capture: 'not_determined' }) });
      const ctx = await browser.newContext({ viewport });
      const page = await ctx.newPage();
      try {
        await page.goto(mock.url + '/dashboard');
        await page.click('#abnav-settings');
        const sec = page.locator('#settings-meeting');
        await expect(sec).toBeVisible();
        await expect(sec).toContainText('麦克风已授权');
        await expect(sec).toContainText('屏幕与系统录音未授权');
        await sec.locator('#mtg-authorize').click();
        await expect.poll(() => mock.meetingCalls.map((c) => c.path)).toContain('/api/meetings/authorize');
      } finally {
        await ctx.close();
        await new Promise((r) => mock.server.close(r));
      }
    });

    test('a meeting shows its summary and transcript', async ({ browser }) => {
      const mock = await startMockServer({ meetings: meetingsFixture({ microphone: 'authorized', audio_capture: 'authorized' }) });
      const ctx = await browser.newContext({ viewport });
      const page = await ctx.newPage();
      try {
        await page.goto(mock.url + '/dashboard');
        await page.waitForFunction(() => !!document.getElementById('rec-strip'));
        await page.evaluate(async () => { const m = await import('/static/meetings.js'); m.openMeetings(); });
        const panel = page.locator('#mtg-panel');
        await expect(panel.locator('.mtg-row')).toHaveCount(1);
        await panel.locator('.mtg-row').click();
        await expect(panel.locator('#mtg-h')).toHaveText('客户 A 周会');
        await expect(panel.locator('.mtg-summary')).toContainText('上线定在下周');
        await expect(panel.locator('.mtg-line')).toHaveCount(2);
        await expect(panel.locator('.mtg-line').nth(1)).toContainText('说话人 A');
        await panel.locator('[data-keep="1"]').click();
        await expect.poll(() => mock.meetingCalls.map((c) => c.path + ' ' + JSON.stringify(c.body))).toContain(`/api/meetings/${ID}/keep {"keep":true}`);
      } finally {
        await ctx.close();
        await new Promise((r) => mock.server.close(r));
      }
    });
  });
}
