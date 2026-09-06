#!/usr/bin/env node
// css-split.mjs — move dashboard.html's inline <style> block into per-view
// stylesheets under static/css/ (#2559 D6).
//
//   node scripts/css-split.mjs --dry-run   # plan only
//   node scripts/css-split.mjs             # write
//
// CSS is order-sensitive: later rules win at equal specificity, and the block
// carries 50 @media queries whose position relative to the base rules matters.
// So the split is strictly sequential — each output file is a contiguous slice
// of the original, and dashboard.html links them in the same order. That keeps
// the cascade byte-identical while making each view's rules reviewable next to
// its module (the JS side of the same view already lives in its own file after
// #2558).
//
// Slice boundaries come from the block's own region comments; SLICES below maps
// an output file to the comment that starts it. Everything before the first
// boundary is the tokens/base slice.

import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const STATIC_DIR = path.join(ROOT, 'internal', 'server', 'static');
const CSS_DIR = path.join(STATIC_DIR, 'css');
const HTML = path.join(STATIC_DIR, 'dashboard.html');
const dryRun = process.argv.includes('--dry-run');

// Output file -> the region comment that begins its slice (matched as a
// substring of the comment line). Order matters: it is the cascade order.
const SLICES = [
  ['tokens.css', null], // everything up to the first boundary below
  ['views.css', '===== Activity Bar (cc-asset-browser) ====='],
  ['split_view.css', '===== Split view (desktop) ====='],
  ['responsive.css', '===== Tablet / narrow desktop (≤1024px) ====='],
  ['cron.css', '===== Cron tab ====='],
  ['mobile_polish.css', '---- Track A: touch-target sizing on coarse pointers'],
];

const html = fs.readFileSync(HTML, 'utf8');
const m = /<style>\n([\s\S]*?)\n<\/style>/.exec(html);
if (!m) {
  console.error('css-split: no inline <style> block found in dashboard.html');
  process.exit(1);
}
const css = m[1];
const lines = css.split('\n');

// Resolve each boundary to a line index.
const bounds = [];
for (const [file, marker] of SLICES) {
  if (marker === null) {
    bounds.push({ file, start: 0 });
    continue;
  }
  const idx = lines.findIndex((l) => l.includes(marker));
  if (idx < 0) {
    console.error(`css-split: boundary comment not found: ${marker}`);
    process.exit(1);
  }
  bounds.push({ file, start: idx });
}
for (let i = 1; i < bounds.length; i++) {
  if (bounds[i].start <= bounds[i - 1].start) {
    console.error(`css-split: boundaries out of order at ${bounds[i].file}`);
    process.exit(1);
  }
}

const header = (file) => `/* ${file} — extracted verbatim from dashboard.html's inline <style> block
 * (#2559 D6). The split is sequential: each file is a contiguous slice of the
 * original block and dashboard.html links them in the same order, so the
 * cascade (later rule wins at equal specificity, @media position) is
 * unchanged. Edit a view's rules here, next to the module that renders it.
 */
`;

const out = [];
for (let i = 0; i < bounds.length; i++) {
  const start = bounds[i].start;
  const end = i + 1 < bounds.length ? bounds[i + 1].start : lines.length;
  out.push({ file: bounds[i].file, body: lines.slice(start, end).join('\n'), lines: end - start });
}

const total = out.reduce((n, o) => n + o.lines, 0);
console.log(`inline <style>: ${lines.length} lines -> ${out.length} files (${total} lines accounted)`);
for (const o of out) console.log(`  css/${o.file.padEnd(20)} ${String(o.lines).padStart(5)} lines`);
if (total !== lines.length) {
  console.error('css-split: slice line count does not match the source');
  process.exit(1);
}

const links = out.map((o) => `<link rel="stylesheet" href="/static/css/${o.file}">`).join('\n');
const newHtml = html.slice(0, m.index) + links + html.slice(m.index + m[0].length);

if (dryRun) {
  console.log(`\n(dry run) dashboard.html: ${html.split('\n').length} -> ${newHtml.split('\n').length} lines`);
  console.log('\nfollow-ups this tool does NOT do:');
  console.log('  - static_assets.go: embed css/*.css + asset table entries + handler');
  console.log('  - routes.go: GET /static/css/<file> routes (+ lint baseline)');
  console.log('  - routes_snapshot_test.go: handler names, then UPDATE_GOLDEN=1');
  console.log('  - the Go CSS contract tests read dashboard.html: point them at the new files');
  process.exit(0);
}

fs.mkdirSync(CSS_DIR, { recursive: true });
for (const o of out) fs.writeFileSync(path.join(CSS_DIR, o.file), header(o.file) + o.body + '\n');
fs.writeFileSync(HTML, newHtml);
console.log(`\nwrote ${out.length} files under internal/server/static/css/ and relinked dashboard.html`);
