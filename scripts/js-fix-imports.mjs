#!/usr/bin/env node
// js-fix-imports.mjs — after a #2558 D4 extraction, repoint every
// `import … from './<module>.js'` at the module that actually owns each name,
// and report names nobody exports (or that two modules export).
//
//   node scripts/js-fix-imports.mjs            # rewrite
//   node scripts/js-fix-imports.mjs --dry-run  # report only
//
// A region move can relocate a name a THIRD file imports (cron_view imported
// `processEventsForDisplay`, which left dashboard for file_refs). The browser
// only surfaces the first such break per load, so fixing them one at a time
// costs a full page-load round trip each; this resolves the whole graph at once
// and fails loudly on a name with no owner.

import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const STATIC_DIR = path.join(ROOT, 'internal', 'server', 'static');
const dryRun = process.argv.includes('--dry-run');

// sw.js has its own worker scope; contract.js is generated UMD-lite.
const SKIP = new Set(['sw.js', 'contract.js']);
const files = fs.readdirSync(STATIC_DIR).filter((f) => f.endsWith('.js') && !SKIP.has(f));

// exportsOf collects a module's exported names in both shapes the codebase
// uses: the trailing `export { … };` block and inline `export function/const`.
function exportsOf(src) {
  const names = new Set();
  for (const m of src.matchAll(/^export \{\n([\s\S]*?)\};$/gm)) {
    for (const line of m[1].split('\n')) {
      const n = line.trim().replace(/,$/, '');
      if (/^[A-Za-z_$][\w$]*$/.test(n)) names.add(n);
    }
  }
  for (const m of src.matchAll(/^export (?:async )?function ([A-Za-z_$][\w$]*)/gm)) names.add(m[1]);
  for (const m of src.matchAll(/^export (?:const|let|var) ([A-Za-z_$][\w$]*)/gm)) names.add(m[1]);
  return names;
}

const owners = new Map();
const sources = new Map();
let problems = 0;
for (const f of files) {
  const src = fs.readFileSync(path.join(STATIC_DIR, f), 'utf8');
  sources.set(f, src);
  for (const n of exportsOf(src)) {
    if (owners.has(n) && owners.get(n) !== f) {
      // Two modules exporting one name means an extraction left the original
      // behind — the duplicate definition, not just a stale export line.
      console.log(`! ${n} exported by both ${owners.get(n)} and ${f}`);
      problems++;
    } else {
      owners.set(n, f);
    }
  }
}

let changed = 0;
for (const f of files) {
  let src = sources.get(f);
  // Both import shapes: the multi-line block the extraction tool emits, and a
  // hand-written single-line `import { a, b } from './x.js';`. Missing the
  // latter would drop it when the blocks are coalesced.
  const blocks = [
    ...src.matchAll(/import \{\n((?:  [A-Za-z_$][\w$]*,\n)+)\} from '\.\/([\w.]+)';/g),
    ...src.matchAll(/import \{ ([A-Za-z_$][\w$]*(?:, [A-Za-z_$][\w$]*)*) \} from '\.\/([\w.]+)';/g),
  ].sort((a, b) => a.index - b.index);
  if (blocks.length === 0) continue;

  const wanted = new Map(); // name -> module it is currently imported from
  for (const b of blocks) {
    const names = b[1].includes('\n')
      ? b[1].trimEnd().split('\n').map((l) => l.trim().replace(/,$/, ''))
      : b[1].split(',').map((n) => n.trim());
    for (const n of names) wanted.set(n, b[2]);
  }

  const byOwner = new Map();
  let needsRewrite = false;
  for (const [name, declared] of wanted) {
    const owner = owners.get(name);
    if (!owner) {
      console.log(`! ${f}: nothing exports ${name} (declared from ${declared})`);
      problems++;
      continue;
    }
    if (owner === f) {
      console.log(`! ${f}: imports ${name} from itself — drop the import`);
      problems++;
      continue;
    }
    if (owner !== declared) {
      needsRewrite = true;
      console.log(`${f}: ${name} ${declared} -> ${owner}`);
    }
    if (!byOwner.has(owner)) byOwner.set(owner, []);
    byOwner.get(owner).push(name);
  }
  if (!needsRewrite) continue;

  // Rewrite every import block of this file as one contiguous, owner-grouped
  // run — never patch individual names in place (that is what corrupted the
  // blocks on the first D4-4 attempt).
  const rebuilt = [...byOwner.entries()]
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([mod, names]) => `import {\n${names.sort().map((n) => `  ${n},`).join('\n')}\n} from './${mod}';`)
    .join('\n');
  const first = blocks[0];
  const last = blocks[blocks.length - 1];
  src = src.slice(0, first.index) + rebuilt + src.slice(last.index + last[0].length);
  if (!dryRun) fs.writeFileSync(path.join(STATIC_DIR, f), src);
  changed++;
}

console.log(`\n${changed} file(s) ${dryRun ? 'would be ' : ''}rewritten, ${problems} problem(s)`);
if (problems) process.exit(1);
