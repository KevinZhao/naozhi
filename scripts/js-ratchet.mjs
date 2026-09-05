#!/usr/bin/env node
// js-ratchet.mjs — growth ratchet for the dashboard's classic scripts
// (internal/server/static/*.js). Per file, four numbers may only go down:
//
//   lines             total line count
//   maxFunctionLines  longest column-0 `function` (to its column-0 `}`)
//   functionsOver100  count of such functions longer than 100 lines
//   topLevelLetVar    column-0 `let` / `var` declarations (mutable globals)
//
// typeof-guard counts are ratcheted by scripts/js-deps-freeze.mjs already and
// are not duplicated here.
//
//   node scripts/js-ratchet.mjs --check   # CI gate
//   node scripts/js-ratchet.mjs --write   # ratchet the baseline down
//
// --check fails on any metric above its baseline (growth) AND on any metric
// below it (the improving PR must ship the tightened baseline — run --write).
// --write refuses to raise a value: deliberate growth requires hand-editing
// scripts/js-ratchet.baseline.json where the diff is visible to reviewers.

import fs from 'node:fs';
import path from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const STATIC_DIR = path.join(ROOT, 'internal', 'server', 'static');
const BASELINE_PATH = path.join(ROOT, 'scripts', 'js-ratchet.baseline.json');

// sw.js is a 27-line service worker with its own scope; not worth ratcheting.
const EXCLUDE = new Set(['sw.js']);

function measure(file) {
  const src = fs.readFileSync(path.join(STATIC_DIR, file), 'utf8');
  const lines = src.split('\n');
  const total = lines.length - (src.endsWith('\n') ? 1 : 0);

  let maxFn = 0;
  let over100 = 0;
  let letVar = 0;
  let fnStart = -1;
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    if (/^(?:async\s+)?function\s/.test(line)) {
      fnStart = i;
    } else if (fnStart >= 0 && /^\}/.test(line)) {
      const len = i - fnStart + 1;
      if (len > maxFn) maxFn = len;
      if (len > 100) over100++;
      fnStart = -1;
    }
    if (/^(?:let|var)\s/.test(line)) letVar++;
  }
  return {
    lines: total,
    maxFunctionLines: maxFn,
    functionsOver100: over100,
    topLevelLetVar: letVar,
  };
}

function snapshot() {
  const out = {};
  const files = fs
    .readdirSync(STATIC_DIR)
    .filter((f) => f.endsWith('.js') && !EXCLUDE.has(f))
    .sort();
  for (const f of files) out[f] = measure(f);
  return out;
}

function check(current) {
  if (!fs.existsSync(BASELINE_PATH)) {
    console.error(`missing baseline ${path.relative(ROOT, BASELINE_PATH)}; run --write first`);
    return 1;
  }
  const baseline = JSON.parse(fs.readFileSync(BASELINE_PATH, 'utf8'));
  let failures = 0;
  for (const [file, metrics] of Object.entries(current)) {
    const base = baseline[file];
    if (!base) {
      console.error(`${file}: not in baseline — run --write to start tracking it`);
      failures++;
      continue;
    }
    for (const [metric, cur] of Object.entries(metrics)) {
      const b = base[metric];
      if (cur > b) {
        console.error(`${file}: ${metric} grew ${b} -> ${cur} (ratchet: may only shrink)`);
        failures++;
      } else if (cur < b) {
        console.error(`${file}: ${metric} improved ${b} -> ${cur} — ship the tightened baseline (run scripts/js-ratchet.mjs --write)`);
        failures++;
      }
    }
  }
  for (const file of Object.keys(baseline)) {
    if (!current[file]) {
      console.error(`${file}: in baseline but gone from static/ — run --write`);
      failures++;
    }
  }
  if (failures) {
    console.error(`js-ratchet: ${failures} metric(s) out of step with the baseline`);
    return 1;
  }
  console.log('js-ratchet: OK');
  return 0;
}

function write(current) {
  if (fs.existsSync(BASELINE_PATH)) {
    const baseline = JSON.parse(fs.readFileSync(BASELINE_PATH, 'utf8'));
    for (const [file, metrics] of Object.entries(current)) {
      for (const [metric, cur] of Object.entries(metrics)) {
        const b = baseline[file]?.[metric];
        if (b !== undefined && cur > b) {
          console.error(`refusing to raise ${file} ${metric} ${b} -> ${cur}; growth requires hand-editing ${path.relative(ROOT, BASELINE_PATH)}`);
          return 1;
        }
      }
    }
  }
  fs.writeFileSync(BASELINE_PATH, JSON.stringify(current, null, 2) + '\n');
  console.log(`wrote ${path.relative(ROOT, BASELINE_PATH)}`);
  return 0;
}

const current = snapshot();
const mode = process.argv[2] ?? '--check';
if (mode === '--check') process.exit(check(current));
else if (mode === '--write') process.exit(write(current));
else {
  console.error(`unknown mode ${mode}; use --check | --write`);
  process.exit(2);
}
