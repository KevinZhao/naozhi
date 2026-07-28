// @ts-check
// E2E: session-header git branch / worktree chip.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

// Sidebar order is derived (created_at / last_active), not source order, so
// address cards by key rather than by index.
const MAIN_TREE_KEY = 'dashboard:direct:2026-01-01-120000-1:myproject';
const WORKTREE_KEY = 'dashboard:direct:2026-01-01-120001-2:otherproject';
const NON_REPO_KEY = 'dashboard:direct:2026-01-01-120002-3:myproject';

let mock;

test.beforeAll(async () => {
  mock = await startMockServer();
});

test.afterAll(async () => {
  await new Promise(r => mock.server.close(r));
});

test.beforeEach(async ({ page }) => {
  mock.resetCalls();
  await page.goto(`${mock.url}/dashboard`);
  await page.waitForSelector('.session-card', { timeout: 5000 });
});

function card(page, key) {
  return page.locator(`.session-card[data-key="${key}"]`);
}

test('main-tree session shows a plain branch chip', async ({ page }) => {
  await card(page, MAIN_TREE_KEY).click();
  const chip = page.locator('#header-git .git-chip');
  await expect(chip).toHaveCount(1);
  await expect(chip.locator('.git-chip-text')).toHaveText('master');
  // A main tree gets the neutral treatment, not the worktree accent.
  await expect(chip).not.toHaveClass(/git-chip-worktree/);
  await expect(chip).toHaveAttribute('title', /分支: master/);
  await expect(chip).toHaveAttribute('title', /仓库: myproject/);
});

test('linked-worktree session shows worktree · branch with accent', async ({ page }) => {
  await card(page, WORKTREE_KEY).click();
  const chip = page.locator('#header-git .git-chip');
  await expect(chip).toHaveCount(1);
  await expect(chip).toHaveClass(/git-chip-worktree/);
  // The worktree name leads — it is the stronger "which checkout" signal.
  await expect(chip.locator('.git-chip-text')).toContainText('feat-x');
  await expect(chip.locator('.git-chip-text')).toContainText('worktree-feat-x');
  await expect(chip).toHaveAttribute('title', /worktree: feat-x/);
});

// This key has no entry in the mock's gitStates map → is_repo:false.
test('non-repo workspace renders no chip and collapses the slot', async ({ page }) => {
  const settled = page.waitForResponse(r => r.url().includes('/api/sessions/git'));
  await card(page, NON_REPO_KEY).click();
  await settled;
  await expect(page.locator('#header-git .git-chip')).toHaveCount(0);
  // :empty collapses the wrapper so a plain folder leaves no gap in the header.
  await expect(page.locator('#header-git')).toBeHidden();
});

test('switching sessions replaces the chip rather than stacking', async ({ page }) => {
  await card(page, MAIN_TREE_KEY).click();
  await expect(page.locator('#header-git .git-chip-text')).toHaveText('master');

  await card(page, WORKTREE_KEY).click();
  const chip = page.locator('#header-git .git-chip');
  await expect(chip).toHaveCount(1);
  await expect(chip.locator('.git-chip-text')).toContainText('feat-x');
});

test('chip survives a header rebuild (rename)', async ({ page }) => {
  await card(page, MAIN_TREE_KEY).click();
  await expect(page.locator('#header-git .git-chip-text')).toHaveText('master');
  // renderMainShell rebuilds the whole header, emptying #header-git; the
  // cached repaint must put the chip back rather than leave it blank (a blank
  // chip reads as "not a repo", which is a wrong answer, not a missing one).
  await page.evaluate(() => renderMainShell());
  await expect(page.locator('#header-git .git-chip-text')).toHaveText('master');
});

test('a workspace change (/cd) re-resolves the chip', async ({ page }) => {
  await card(page, MAIN_TREE_KEY).click();
  await expect(page.locator('#header-git .git-chip-text')).toHaveText('master');

  // Simulate /cd: the session's workspace moves to a different checkout, which
  // the next /api/sessions poll reports. The cached chip must not stick.
  mock.setGitState(MAIN_TREE_KEY, {
    is_repo: true, repo: 'myproject', branch: 'worktree-hotfix', worktree: 'hotfix',
    root: '/home/user/workspace/myproject/.claude/worktrees/hotfix',
  });
  mock.setSessionWorkspace(MAIN_TREE_KEY, '/home/user/workspace/myproject/.claude/worktrees/hotfix');

  await expect(page.locator('#header-git .git-chip-text')).toContainText('hotfix', { timeout: 10000 });
  await expect(page.locator('#header-git .git-chip')).toHaveClass(/git-chip-worktree/);

  // The mock server is shared across this file's tests, so put it back.
  mock.setGitState(MAIN_TREE_KEY, {
    is_repo: true, repo: 'myproject', branch: 'master',
    root: '/home/user/workspace/myproject', workspace: '/home/user/workspace/myproject',
  });
  mock.setSessionWorkspace(MAIN_TREE_KEY, '/home/user/workspace/myproject');
});

test('a branch switch inside a turn re-resolves the chip (workspace unchanged)', async ({ page }) => {
  await card(page, MAIN_TREE_KEY).click();
  await expect(page.locator('#header-git .git-chip-text')).toHaveText('master');

  // The agent runs `git checkout` mid-turn: the branch changes but the
  // workspace PATH does not, so the /cd-oriented invalidation above never
  // fires. Only the turn-boundary refresh can catch this.
  mock.setGitState(MAIN_TREE_KEY, {
    is_repo: true, repo: 'myproject', branch: 'feat/mid-turn',
    root: '/home/user/workspace/myproject', workspace: '/home/user/workspace/myproject',
  });

  // Drive the real running→ready edge through the WS message handler.
  await page.evaluate((key) => {
    // eslint-disable-next-line no-undef
    sessionsData[sid(key, 'local')].state = 'running';
    // eslint-disable-next-line no-undef
    wsm.onSessionState({ type: 'session_state', key, node: 'local', state: 'ready' });
  }, MAIN_TREE_KEY);

  await expect(page.locator('#header-git .git-chip-text')).toHaveText('feat/mid-turn', { timeout: 10000 });

  // The mock server is shared across this file's tests, so put it back.
  mock.setGitState(MAIN_TREE_KEY, {
    is_repo: true, repo: 'myproject', branch: 'master',
    root: '/home/user/workspace/myproject', workspace: '/home/user/workspace/myproject',
  });
});

test('detached HEAD shows the abbreviated sha with the detached tint', async ({ page }) => {
  await page.route('**/api/sessions/git**', route => route.fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify({
      is_repo: true, repo: 'myproject', root: '/home/user/workspace/myproject',
      detached: true, head_sha: 'a1b2c3d',
    }),
  }));
  await card(page, MAIN_TREE_KEY).click();

  const chip = page.locator('#header-git .git-chip');
  await expect(chip).toHaveClass(/git-chip-detached/);
  await expect(chip.locator('.git-chip-text')).toHaveText('a1b2c3d');
  await expect(chip).toHaveAttribute('title', /分离 HEAD: a1b2c3d/);
});

test('a hostile branch name is escaped, not executed', async ({ page }) => {
  await page.route('**/api/sessions/git**', route => route.fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify({
      is_repo: true, repo: 'x', branch: '<img src=x onerror=window.__pwned=1>',
    }),
  }));
  await card(page, MAIN_TREE_KEY).click();

  const chip = page.locator('#header-git .git-chip');
  await expect(chip).toHaveCount(1);
  // Rendered as text, so no <img> element was created and no handler fired.
  await expect(page.locator('#header-git img')).toHaveCount(0);
  expect(await page.evaluate(() => window.__pwned)).toBeUndefined();
  await expect(chip.locator('.git-chip-text')).toContainText('onerror=');
});

test('a failed git fetch leaves the header usable', async ({ page }) => {
  await page.route('**/api/sessions/git**', route => route.fulfill({ status: 500, body: 'boom' }));
  await card(page, MAIN_TREE_KEY).click();

  await expect(page.locator('#header-git .git-chip')).toHaveCount(0);
  // The conversation surface must not be blocked on the chip.
  await expect(page.locator('#msg-input')).toBeVisible();
  await expect(page.locator('.main-header h2')).toBeVisible();
});
