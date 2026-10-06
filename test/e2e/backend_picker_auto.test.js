// @ts-check
//
// The backend picker's 自动 option (#3418). Untouched, every picker sends no
// backend, so the server's own order (agents[].backend, the access profile's
// default_backend, the router default) decides; only an explicit pick is
// sent. The 自动 label names the backend that order would pick from the
// access profile in view.
//
//   - New session (palette and custom-workspace modal): 自动 selected and
//     labelled for the default profile; the send carries no backend unless
//     one was picked; switching profile relabels 自动 without changing it.
//   - Project settings: a project with no backend opens on 自动 and saves
//     backend "" instead of pinning the router default.
//   - Cron: a create on 自动 sends no backend, and saving an edit of a job
//     with none does not PATCH one in.
//   - A session opened but not sent yet shows and gates on the backend it
//     will spawn on: the pick, else what 自动 resolves to (sidebar icon,
//     header label, image button, tuning model list). Once sent it keeps
//     showing that backend until the server lists the key, and no later send
//     carries the pick again; a sent key this browser did not create shows
//     the router default.
//
// /api/cli/backends, /api/access-profiles and /api/projects/config are
// answered by page.route (the mock serves none of them).
//
// 跑法：cd test/e2e && npx playwright test backend_picker_auto.test.js --project=desktop-chrome

const { test, expect } = require('@playwright/test');
const { startMockServer, defaultSessions } = require('./mock-server');
const { waitForWs } = require('./shim_wait');

const MANIFEST = {
  backends: [
    { id: 'claude', display_name: 'claude-code', protocol: 'stream-json', available: true,
      features: { image_input: true }, models: [{ id: 'claude-opus' }] },
    { id: 'kiro', display_name: 'kiro', protocol: 'acp', available: true,
      features: { image_input: false }, models: [{ id: 'kiro-auto' }] },
  ],
  default: 'claude',
  detected: [],
};

const PROFILES = {
  profiles: [
    { id: 'team', display_name: 'Team', default_backend: 'kiro', secret_ok: true },
    { id: 'solo', display_name: 'Solo', secret_ok: true },
  ],
  default: 'team',
};

const NOW = Date.now();
const CRON_JOB = {
  id: 'cron-auto-1', schedule: '0 9 * * *', prompt: 'no backend job', backend: '',
  work_dir: '/home/user/workspace/myproject', paused: false, created_at: NOW - 86400000,
  next_run: NOW + 3600000, recent_runs: [], stats: { total: 0, succeeded: 0 },
};

/** @param {object} body */
function json(body) {
  return { status: 200, contentType: 'application/json', body: JSON.stringify(body) };
}

test.use({ viewport: { width: 1600, height: 900 } });

test.beforeEach(({ }, testInfo) => {
  if (testInfo.project.name !== 'desktop-chrome') {
    testInfo.skip(true, 'viewport-independent; desktop-chrome only');
  }
});

/** @type {Awaited<ReturnType<typeof startMockServer>>} */
let mock;
test.beforeAll(async () => { mock = await startMockServer({ cronJobs: [CRON_JOB] }); });
test.afterAll(async () => { await new Promise(r => mock.server.close(r)); });

test.beforeEach(async ({ page }) => {
  mock.resetCalls();
  await page.route(url => url.pathname === '/api/cli/backends', route => route.fulfill(json(MANIFEST)));
  await page.route(url => url.pathname === '/api/access-profiles', route => route.fulfill(json(PROFILES)));
});

/** @param {import('@playwright/test').Page} page */
async function openPalette(page) {
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click('.hdr-btn[title="New Session"]');
  await page.waitForSelector('.cmd-palette-item');
}

/**
 * Opens myproject from the palette, sends one message and returns its body.
 * @param {import('@playwright/test').Page} page
 */
async function sendFromProject(page) {
  await page.locator('.cmd-palette-item', { hasText: 'myproject' }).first().click();
  const input = page.locator('#msg-input');
  await input.click();
  await input.pressSequentially('hello');
  await page.keyboard.press('Enter');
  await expect.poll(() => mock.sendCalls.length).toBe(1);
  return JSON.parse(mock.sendCalls[0]);
}

test('palette: 自动 is preselected, labelled for the profile, and sends no backend', async ({ page }) => {
  await openPalette(page);
  const backend = page.locator('#new-backend');
  await expect(backend).toHaveValue('');
  await expect(backend.locator('option')).toHaveText(['自动（kiro）', 'claude-code', 'kiro']);

  const body = await sendFromProject(page);
  expect('backend' in body, 'an untouched picker must leave the backend to the server').toBe(false);
  expect(body.access_profile).toBe('team');
});

test('palette: an explicit pick of the router default is still sent', async ({ page }) => {
  await openPalette(page);
  await page.selectOption('#new-backend', 'claude');
  const body = await sendFromProject(page);
  expect(body.backend).toBe('claude');
});

test('palette: switching access profile relabels 自动 and keeps it selected', async ({ page }) => {
  await openPalette(page);
  await page.selectOption('#new-access-profile', 'solo');
  const auto = page.locator('#new-backend option[value=""]');
  await expect(auto).toHaveText('自动（claude-code）');
  await expect(page.locator('#new-backend')).toHaveValue('');
  // An explicit pick survives the repaint a profile switch triggers.
  await page.selectOption('#new-backend', 'kiro');
  await page.selectOption('#new-access-profile', 'team');
  await expect(auto).toHaveText('自动（kiro）');
  await expect(page.locator('#new-backend')).toHaveValue('kiro');
});

test('custom workspace: 自动 and the profile carry over from the palette', async ({ page }) => {
  await openPalette(page);
  await page.selectOption('#new-access-profile', 'solo');
  await page.locator('.cmd-palette-item', { hasText: '打开自定义工作目录' }).click();
  await expect(page.locator('#new-access-profile')).toHaveValue('solo');
  const backend = page.locator('#new-backend');
  await expect(backend).toHaveValue('');
  await expect(backend.locator('option[value=""]')).toHaveText('自动（claude-code）');

  await page.fill('#new-workspace', '/tmp/elsewhere');
  await page.click('.modal-overlay .modal-btns button.primary');
  const input = page.locator('#msg-input');
  await input.click();
  await input.pressSequentially('hello');
  await page.keyboard.press('Enter');
  await expect.poll(() => mock.sendCalls.length).toBe(1);
  const body = JSON.parse(mock.sendCalls[0]);
  expect('backend' in body).toBe(false);
  expect(body.access_profile).toBe('solo');
});

test('project settings: no saved backend opens on 自动 and saves backend ""', async ({ page }) => {
  /** @type {any[]} */
  const puts = [];
  await page.route(url => url.pathname === '/api/projects/config', route => {
    if (route.request().method() === 'PUT') {
      puts.push(JSON.parse(route.request().postData() || '{}'));
      return route.fulfill(json({ ok: true }));
    }
    return route.fulfill(json({ access_profile: 'solo' }));
  });
  await page.goto(`${mock.url}/dashboard`);
  await page.locator('[data-action="project-settings"][data-name="myproject"]').click();

  const backend = page.locator('#ps-backend');
  await expect(backend).toHaveValue('');
  const auto = backend.locator('option[value=""]');
  await expect(auto).toHaveText('自动（claude-code）');
  await page.selectOption('#ps-access-profile', 'team');
  await expect(auto).toHaveText('自动（kiro）');

  await page.click('[data-action="ps-save"]');
  await expect.poll(() => puts.length).toBe(1);
  expect(puts[0].backend).toBe('');
  expect(puts[0].access_profile).toBe('team');
});

/** @param {import('@playwright/test').Page} page */
async function openCron(page) {
  await page.goto(mock.url + '/dashboard');
  await page.waitForSelector('.session-card');
  await page.click('#abnav-cron');
  await page.waitForSelector('.cj-row[data-cron-id="cron-auto-1"]');
}

test('cron create on 自动 sends no backend', async ({ page }) => {
  await openCron(page);
  await page.click('.cron-new-btn');
  await page.waitForSelector('.cron-modal');
  await expect(page.locator('#cron-backend')).toHaveValue('');
  await page.evaluate(() => {
    const el = /** @type {HTMLInputElement|null} */ (document.getElementById('freq-advanced-input'));
    if (el) el.value = '0 9 * * *';
  });
  await page.fill('#cron-prompt', 'auto job');
  await page.click('[data-action="cron-create-save"]');
  await expect.poll(() => mock.cronCreateCalls.length).toBe(1);
  expect('backend' in JSON.parse(mock.cronCreateCalls[0])).toBe(false);
});

test('cron edit of a job with no backend does not PATCH one in', async ({ page }) => {
  await openCron(page);
  await page.click('.cj-row[data-cron-id="cron-auto-1"] .cj-schedule');
  await page.waitForSelector('[data-action="cron-edit-save"]');
  await expect(page.locator('#edit-cron-backend')).toHaveValue('');
  await page.fill('#edit-cron-prompt', 'edited prompt');
  await page.click('[data-action="cron-edit-save"]');
  await expect.poll(() => mock.cronPatchCalls.length).toBe(1);
  const body = JSON.parse(mock.cronPatchCalls[0].body);
  expect(body.prompt).toBe('edited prompt');
  expect('backend' in body, 'saving a job with no backend must not pin the router default').toBe(false);
});

/**
 * What a pending session shows for its backend: the header label from both
 * of its painters, whether the sidebar card wears the kiro mark, both image
 * gates and the tuning popover's model ids.
 * @param {import('@playwright/test').Page} page
 */
async function pendingBackendView(page) {
  await expect(page.locator('.session-card.new-card')).toHaveCount(1);
  const header = await page.evaluate(() => {
    const t = /** @type {any} */ (window).nz.test;
    t.renderMainShell();
    const fromShell = document.getElementById('header-cli')?.textContent;
    const el = /** @type {HTMLElement} */ (document.getElementById('header-cli'));
    el.textContent = 'stale';
    t.updateHeaderCLI();
    return [fromShell, el.textContent];
  });
  // featureForCurrent, the gate behind a click that slips past the button.
  const imageRefused = await page.evaluate(() => {
    const toast = /** @type {HTMLElement} */ (document.getElementById('toast'));
    const input = /** @type {HTMLInputElement} */ (document.getElementById('file-input'));
    toast.textContent = '';
    input.click = () => {};
    /** @type {any} */ (window).nz.test.openFilePicker();
    return toast.textContent === '当前后端不支持图片上传';
  });
  const kiroMark = await page.locator('.session-card.new-card .sc-cli-icon rect[fill="#9046FF"]').count();
  const imageDisabled = await page.locator('button[data-action="file-picker"]').isDisabled();
  await page.click('#header-model');
  const models = await page.locator('#tuning-popover .tuning-opt[data-value]:not([data-value=""])')
    .evaluateAll(els => els.map(e => e.getAttribute('data-value')));
  return { header, kiroMark, imageDisabled, imageRefused, models };
}

test('pending session on 自动 under a kiro-default profile shows and gates on kiro', async ({ page }) => {
  await openPalette(page);
  await page.locator('.cmd-palette-item', { hasText: 'myproject' }).first().click();
  expect(await pendingBackendView(page)).toEqual({
    header: ['kiro', 'kiro'], kiroMark: 1, imageDisabled: true, imageRefused: true, models: ['kiro-auto'],
  });
});

test('pending session on 自动 under a profile without default_backend stays on the router default', async ({ page }) => {
  await openPalette(page);
  await page.selectOption('#new-access-profile', 'solo');
  await page.locator('.cmd-palette-item', { hasText: 'myproject' }).first().click();
  expect(await pendingBackendView(page)).toEqual({
    header: ['claude-code', 'claude-code'], kiroMark: 0, imageDisabled: false, imageRefused: false, models: ['claude-opus'],
  });
});

test('pending session with an explicit kiro pick gates image upload on kiro', async ({ page }) => {
  await openPalette(page);
  await page.selectOption('#new-access-profile', 'solo');
  await page.selectOption('#new-backend', 'kiro');
  await page.locator('.cmd-palette-item', { hasText: 'myproject' }).first().click();
  expect(await pendingBackendView(page)).toEqual({
    header: ['kiro', 'kiro'], kiroMark: 1, imageDisabled: true, imageRefused: true, models: ['kiro-auto'],
  });
});

/**
 * A mock whose stats name the router default's CLI, so the header's no-guess
 * fallback is visible.
 * @param {(sessions: any) => void} [edit]
 * @param {object} [extra] more startMockServer overrides
 */
async function mockWithCLIName(edit, extra = {}) {
  const sessions = /** @type {any} */ (defaultSessions());
  Object.assign(sessions.stats, { cli_name: 'claude-live', cli_version: '9.9.9' });
  if (edit) edit(sessions);
  return startMockServer({ sessions, ...extra });
}

test('pending session on a remote node takes no guess from this node\'s profiles', async ({ page }) => {
  const own = await mockWithCLIName(sessions => {
    sessions.nodes.mac = { display_name: 'Mac', status: 'ok' };
    sessions.stats.projects.push({ name: 'macproj', path: '/Users/m/macproj', node: 'mac' });
  });
  try {
    await page.goto(own.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click('.hdr-btn[title="New Session"]');
    await page.selectOption('#new-node', 'mac');
    await page.locator('.cmd-palette-item', { hasText: 'macproj' }).first().click();
    await expect(page.locator('.session-card.new-card')).toHaveCount(1);
    await expect(page.locator('.main-header .detail-left #header-cli')).toHaveText('claude-live');
  } finally { own.server.close(); }
});

test('pending session with a single backend keeps the server-reported CLI name', async ({ page }) => {
  await page.route(url => url.pathname === '/api/cli/backends', route => route.fulfill(json({ backends: [MANIFEST.backends[0]], default: 'claude', detected: [] })));
  const own = await mockWithCLIName();
  try {
    await page.goto(own.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click('.hdr-btn[title="New Session"]');
    await page.locator('.cmd-palette-item', { hasText: 'myproject' }).first().click();
    await expect(page.locator('.session-card.new-card')).toHaveCount(1);
    await expect(page.locator('.main-header .detail-left #header-cli')).toHaveText('claude-live');
  } finally { own.server.close(); }
});

/**
 * Opens myproject from the palette on profile/backend, sends text (or one
 * image when text is '') and waits for the send to leave. Returns the header
 * labels painted before the send.
 * @param {import('@playwright/test').Page} page
 * @param {Awaited<ReturnType<typeof startMockServer>>} own
 * @param {{ profile: string, backend: string, ws: boolean, text: string }} c
 */
async function createAndSend(page, own, c) {
  await page.goto(own.url + '/dashboard');
  await page.waitForSelector('.session-card');
  if (c.ws) await waitForWs(page);
  await page.click('.hdr-btn[title="New Session"]');
  await page.selectOption('#new-access-profile', c.profile);
  await page.selectOption('#new-backend', c.backend);
  await page.locator('.cmd-palette-item', { hasText: 'myproject' }).first().click();
  await expect(page.locator('.session-card.new-card')).toHaveCount(1);
  const before = await headerLabels(page);
  await sendNow(page, own, c.text, 1);
  return before;
}

/**
 * Sends text (or one image when text is '') on the selected key and waits
 * until n sends have left in total.
 * @param {import('@playwright/test').Page} page
 * @param {Awaited<ReturnType<typeof startMockServer>>} own
 * @param {string} text
 * @param {number} n
 */
async function sendNow(page, own, text, n) {
  await page.evaluate((text) => {
    const t = /** @type {any} */ (window).nz.test;
    t.setMsgValue(document.getElementById('msg-input'), text);
    if (!text) t.pendingFiles.push({ id: 'file-1', kind: 'image', status: 'ready', normalizedSize: 16, file: new File([new Uint8Array(16)], 'p.png', { type: 'image/png' }) });
    t.sendMessage();
  }, text);
  await expect.poll(() => own.sendCalls.length + wsSendBodies(own).length).toBe(n);
}

/** @param {Awaited<ReturnType<typeof startMockServer>>} own */
function wsSendBodies(own) {
  return own.wsConnections.flatMap(conn => conn.messages).filter(m => m.type === 'send');
}

/**
 * The header label from both of its painters.
 * @param {import('@playwright/test').Page} page
 */
async function headerLabels(page) {
  return page.evaluate(() => {
    const t = /** @type {any} */ (window).nz.test;
    t.renderMainShell();
    const el = /** @type {HTMLElement} */ (document.getElementById('header-cli'));
    const fromShell = el.textContent;
    el.textContent = 'stale';
    t.updateHeaderCLI();
    return [fromShell, el.textContent];
  });
}

// After the first send the pick and profile are consumed, but until the
// server lists the key the header keeps the backend shown before the send
// (claude-code), neither the router default's stats name (claude-live) nor a
// guess from the default profile (kiro). The three sends leave different
// marks: a WS send only lastSent, an image-only HTTP send only
// httpSendPending, a text HTTP send both.
const SENT_CASES = [
  { name: 'an explicit claude pick sent over WS', profile: 'team', backend: 'claude', ws: true, text: 'hello' },
  { name: '自动 under solo sent over HTTP', profile: 'solo', backend: '', ws: false, text: 'hello' },
  { name: '自动 under solo sent image-only over HTTP', profile: 'solo', backend: '', ws: false, text: '' },
];
for (const c of SENT_CASES) {
  test(`a sent, not yet listed session (${c.name}) keeps its pre-send backend`, async ({ page }) => {
    const own = await mockWithCLIName(undefined, { ws: c.ws });
    try {
      expect(await createAndSend(page, own, c)).toEqual(['claude-code', 'claude-code']);
      expect(wsSendBodies(own).length).toBe(c.ws ? 1 : 0);
      expect(await headerLabels(page)).toEqual(['claude-code', 'claude-code']);
    } finally { own.server.close(); }
  });
}

test('a sent key this browser did not create takes no default-profile guess', async ({ page }) => {
  const own = await mockWithCLIName();
  try {
    await page.goto(own.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.evaluate(() => {
      const t = /** @type {any} */ (window).nz.test;
      t.selectedKey = 'dashboard:direct:2026-02-02-000000-9:myproject';
      t.selectedNode = 'local';
      t.renderMainShell();
    });
    await sendNow(page, own, 'hello', 1);
    expect(await headerLabels(page)).toEqual(['claude-live', 'claude-live']);
  } finally { own.server.close(); }
});

test('a sent kiro pick keeps gating images and the clawd icon off until listed', async ({ page }) => {
  const own = await mockWithCLIName(undefined, { ws: true });
  try {
    expect(await createAndSend(page, own, { profile: 'team', backend: 'kiro', ws: true, text: 'hello' })).toEqual(['kiro', 'kiro']);
    expect(wsSendBodies(own)[0].backend).toBe('kiro');
    expect(await headerLabels(page)).toEqual(['kiro', 'kiro']);
    await page.evaluate(() => /** @type {any} */ (window).nz.test.applyFeatureGates());
    await expect(page.locator('button[data-action="file-picker"]')).toBeDisabled();
    const clawd = await page.evaluate(() => /** @type {any} */ (window).nz.test.eventHtml({ type: 'text', detail: 'hi', time: 1 }).includes('cc-clawd'));
    expect(clawd, 'a kiro turn must not wear the claude mascot').toBe(false);
  } finally { own.server.close(); }
});

for (const ws of [false, true]) {
  test(`a second send before listing carries no backend or access profile (${ws ? 'WS' : 'HTTP'})`, async ({ page }) => {
    const own = await mockWithCLIName(undefined, { ws });
    try {
      await createAndSend(page, own, { profile: 'team', backend: 'kiro', ws, text: 'one' });
      await sendNow(page, own, 'two', 2);
      const bodies = ws ? wsSendBodies(own) : own.sendCalls.map(b => JSON.parse(b));
      expect(bodies.length).toBe(2);
      expect([bodies[0].backend, bodies[0].access_profile]).toEqual(['kiro', 'team']);
      expect(['backend', 'access_profile', 'workspace'].filter(f => f in bodies[1]), 'spawn-time fields ride only the first send').toEqual([]);
      // The display copy outlives the consumed pick.
      expect(await headerLabels(page)).toEqual(['kiro', 'kiro']);
    } finally { own.server.close(); }
  });
}

test('once the server lists a sent key, its listing replaces the kept pick', async ({ page }) => {
  /** @type {any} */
  let sessions;
  const own = await mockWithCLIName(s => { sessions = s; });
  try {
    await createAndSend(page, own, { profile: 'team', backend: 'kiro', ws: false, text: 'hello' });
    expect(await headerLabels(page)).toEqual(['kiro', 'kiro']);
    // Listed with no backend or cli_name, so only a kept pick could still
    // name kiro; the header must fall back to the stats name.
    const key = await page.evaluate(() => /** @type {any} */ (window).nz.test.selectedKey);
    sessions.sessions.push({ key, state: 'ready', platform: 'dashboard', agent: 'general', workspace: '/home/user/workspace/myproject', last_active: Date.now(), node: 'local', project: 'myproject' });
    sessions.stats.version++;
    await page.evaluate(() => /** @type {any} */ (window).nz.test.fetchSessions());
    await expect.poll(() => page.evaluate(k => {
      const t = /** @type {any} */ (window).nz.test;
      return !!t.sessionsData[t.sid(k, 'local')];
    }, key)).toBe(true);
    expect(await headerLabels(page)).toEqual(['claude-live', 'claude-live']);
  } finally { own.server.close(); }
});

test('re-creating a sent, not yet listed key starts from its new picks', async ({ page }) => {
  const own = await mockWithCLIName();
  try {
    await page.goto(own.url + '/dashboard');
    await page.waitForSelector('.session-card');
    /**
     * @param {string} backend
     * @param {string} profile
     */
    const create = (backend, profile) => page.evaluate(([b, p]) => /** @type {any} */ (window).nz.test.doCreateInProject(
      '/home/user/workspace/myproject', 'myproject', 'local', b, 'general',
      { mode: 'continue', stableKey: 'dashboard:pj:abc:general', accessProfile: p }), [backend, profile]);
    await create('kiro', 'team');
    await sendNow(page, own, 'hello', 1);
    expect(await headerLabels(page)).toEqual(['kiro', 'kiro']);
    // The same continued key on 自动 under solo must not inherit the kiro
    // pick the first send left behind. The key was already sent from here, so
    // with no display copy it shows the router default's stats name.
    await create('', 'solo');
    expect(await headerLabels(page)).toEqual(['claude-live', 'claude-live']);
  } finally { own.server.close(); }
});

test('a listed session with no backend field is not re-guessed from the access profile', async ({ page }) => {
  const own = await mockWithCLIName();
  try {
    await page.goto(own.url + '/dashboard');
    await page.waitForSelector('.session-card');
    await page.click('.hdr-btn[title="New Session"]');
    // The 自动 label proves the manifest and the profiles are both cached.
    await expect(page.locator('#new-backend option[value=""]')).toHaveText('自动（kiro）');
    await page.locator('#cp-input').press('Escape');
    await expect(page.locator('.cmd-palette-overlay')).toHaveCount(0);
    await page.click('.session-card[data-key="dashboard:direct:2026-01-01-120000-1:myproject"]');
    await page.waitForSelector('#msg-input');
    await page.evaluate(() => /** @type {any} */ (window).nz.test.applyFeatureGates());
    await expect(page.locator('button[data-action="file-picker"]')).toBeEnabled();
    await page.click('#header-model');
    await expect(page.locator('#tuning-popover .tuning-opt[data-value="claude-opus"]')).toHaveCount(1);
  } finally { own.server.close(); }
});
