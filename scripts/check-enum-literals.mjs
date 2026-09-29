#!/usr/bin/env node
// check-enum-literals.mjs — the death_reason wire vocabulary is generated
// into contract.js (internal/contractjs, #2909 G5 PR2); this checks the
// dashboard actually reads it from there instead of restating it.
//
// Two checks:
//   1. nz_util.js's DEATH_REASONS table has exactly the keys
//      NZ_CONTRACT.ENUMS.DEATH_REASON lists — neither a reason the backend
//      declares and the table forgot, nor one the table still has after the
//      backend dropped it.
//   2. No OTHER static/*.js file hardcodes one of those reason strings: the
//      literal that matters is quoted whole (`'idle_timeout'`), not a
//      substring, so this cannot flag prose that merely mentions a reason.
//      SESSION_STATE (ready/running/dead) is not checked here — those are
//      also common English words compared throughout the dashboard, and
//      moving ~90 existing comparisons onto the generated list is a separate,
//      larger change (tracked in #2909's follow-up), not a measurement gate.
//
//   node scripts/check-enum-literals.mjs
import fs from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';
import { stripComments } from './js-deps-freeze.mjs';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const STATIC_DIR = path.join(ROOT, 'internal', 'server', 'static');

// deathReasonKeys extracts the DEATH_REASONS object's own top-level keys from
// nz_util.js's source (not evaluated: the file is a browser ES module).
export function deathReasonKeys(nzUtilSrc) {
  const start = nzUtilSrc.indexOf('const DEATH_REASONS = {');
  if (start < 0) return null;
  const open = nzUtilSrc.indexOf('{', start);
  let depth = 0, end = open;
  for (; end < nzUtilSrc.length; end++) {
    if (nzUtilSrc[end] === '{') depth++;
    else if (nzUtilSrc[end] === '}') { depth--; if (depth === 0) break; }
  }
  const body = nzUtilSrc.slice(open + 1, end);
  return [...body.matchAll(/^\s*([a-z_]+):/gm)].map((m) => m[1]);
}

// literalHits finds every static/*.js file (besides nz_util.js and
// contract.js) that quotes one of reasons as a whole string literal, mapped
// to the reasons it hits.
export function literalHits(files, reasons) {
  const want = new Set(reasons);
  const out = [];
  for (const [name, src] of Object.entries(files)) {
    if (name === 'nz_util.js' || name === 'contract.js') continue;
    const stripped = stripComments(src);
    const hits = new Set();
    for (const m of stripped.matchAll(/(['"])([a-z_]+)\1/g)) {
      if (want.has(m[2])) hits.add(m[2]);
    }
    if (hits.size) out.push({ file: name, reasons: [...hits].sort() });
  }
  return out;
}

export function run(files, contractReasons) {
  const problems = [];
  const keys = deathReasonKeys(files['nz_util.js']);
  if (!keys) {
    problems.push('nz_util.js: no `const DEATH_REASONS = {...}` found — the golden check has gone blind');
  } else {
    const want = new Set(contractReasons);
    const got = new Set(keys);
    for (const k of got) if (!want.has(k)) problems.push(`nz_util.js: DEATH_REASONS has ${JSON.stringify(k)}, which NZ_CONTRACT.ENUMS.DEATH_REASON does not list`);
    for (const w of want) if (!got.has(w)) problems.push(`nz_util.js: DEATH_REASONS is missing ${JSON.stringify(w)}, which NZ_CONTRACT.ENUMS.DEATH_REASON lists`);
  }
  for (const hit of literalHits(files, contractReasons)) {
    problems.push(`${hit.file}: hardcodes death_reason literal(s) ${hit.reasons.join(', ')} instead of reading NZ_CONTRACT.ENUMS.DEATH_REASON`);
  }
  return problems;
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  // contract.js is the classic (module.exports || root.NZ_CONTRACT) script
  // the browser also loads via <script defer>; require() picks the
  // module.exports branch, same as mock-server.js does.
  const contract = createRequire(import.meta.url)(path.join(STATIC_DIR, 'contract.js'));
  const files = {};
  for (const f of fs.readdirSync(STATIC_DIR).filter((f) => f.endsWith('.js') && f !== 'sw.js')) {
    files[f] = fs.readFileSync(path.join(STATIC_DIR, f), 'utf8');
  }
  const problems = run(files, contract.ENUMS.DEATH_REASON);
  if (problems.length) {
    for (const p of problems) console.error(p);
    console.error(`check-enum-literals: ${problems.length} problem(s)`);
    process.exit(1);
  }
  console.log(`check-enum-literals: OK (${contract.ENUMS.DEATH_REASON.length} death reasons, ${Object.keys(files).length - 2} files checked)`);
}
