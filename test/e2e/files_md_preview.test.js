// E2E: the files view renders a .md file with the chat renderer (renderRich)
// and the header toggle flips to the raw source and back.
const { test, expect } = require('@playwright/test');
const { startMockServer } = require('./mock-server');

const README = [
  '# Title',
  '',
  '| a | b |',
  '|---|---|',
  '| 1 | 2 |',
  '',
  'Inline $x^2$ math.',
  '',
  '```mermaid',
  'graph TD; A-->B',
  '```',
].join('\n');

let mock;

test.beforeAll(async () => {
  mock = await startMockServer();
});

test.afterAll(async () => {
  await new Promise(r => mock.server.close(r));
});

test('markdown file renders, toggles to raw and back', async ({ page }) => {
  await page.route('**/api/projects/files/list?*', route => route.fulfill({
    contentType: 'application/json',
    body: JSON.stringify({ entries: [{ name: 'README.md', size: README.length }, { name: 'notes.txt', size: 5 }] }),
  }));
  await page.route('**/api/projects/file?*', route => {
    const p = new URL(route.request().url()).searchParams.get('path');
    const content = p === 'README.md' ? README : 'plain';
    route.fulfill({ contentType: 'application/json', body: JSON.stringify({ content, size: content.length, mime: 'text/plain' }) });
  });
  await page.goto(`${mock.url}/dashboard`);
  await page.click('#abnav-files');
  await page.locator('.files-row[data-name="README.md"]').click();

  const body = page.locator('#files-main-body');
  const toggle = page.locator('#files-md-toggle');
  await expect(body.locator('.fv-rich .md-h1')).toHaveText('Title');
  await expect(body.locator('.md-table')).toHaveCount(1);
  // Math and diagrams render once their vendor bundles load; their slots
  // being emitted is what proves the chat renderer is wired in.
  await expect(body.locator('.katex, .katex-pending')).toHaveCount(1);
  await expect(body.locator('.mermaid-wrap')).toHaveCount(1);
  await expect(toggle).toBeVisible();
  await expect(toggle).toHaveText('原文');

  await toggle.click();
  await expect(body.locator('pre.files-raw')).toHaveText(README);
  await expect(toggle).toHaveText('渲染');
  await toggle.click();
  await expect(body.locator('.fv-rich .md-h1')).toHaveText('Title');

  // A non-markdown file hides the toggle and stays raw.
  await page.locator('.files-row[data-name="notes.txt"]').click();
  await expect(body.locator('pre.files-raw')).toHaveText('plain');
  await expect(toggle).toBeHidden();
});
