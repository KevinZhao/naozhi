#!/usr/bin/env node
// check-enum-literals.mjs — wire vocabularies generated into contract.js
// (internal/contractjs) that the dashboard must read from there instead of
// restating them.
//
// death_reason (#2909 G5 PR2):
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
// EventEntry kinds (S13b-4, #3021), from the AST (espree), contract.js aside:
//   a. ENUMS.EVENT_TYPE, EVENT_TYPE_INTERNAL and EVENT_TYPE_MD_IGNORE are
//      non-empty, and the last two are subsets of the first.
//   b. No array literal holds two or more kind literals — counted, so
//      `new Set([...X, 'tool_use', 'result'])` is a restated set too — no
//      object literal is a kind set (two or more kind keys, every value
//      `true` or `1`), and every key of an object literal looked up by
//      `[x.type]` is a kind.
//   c. A string compared with a type expression (===, !==, ==, !=, a switch
//      case, or an element of an inline array / `new Set([...])`, or of a
//      const bound to one, passed to .includes/.has/.indexOf) is a WS frame
//      type, a kind, or listed for its file in OTHER_TYPES; an OTHER_TYPES
//      entry no comparison uses is dead and fails. A type expression is
//      `x.type`, `x?.type`, `x['type']`, or a name a const binds to one
//      (`const t = x.type`, `const t = x && x.type`, `const t = x.type || ''`,
//      `const { type: t } = x`).
//   Blind guards: a file that does not parse fails, and every KIND_SENTINELS
//   file must compare `.type` with a kind at least once.
//
//   Limits, so nobody reads this as full coverage: aliases are by name per
//   file, not by scope (a const alias makes every same-named identifier in
//   the file a type expression — that can only add findings, not hide one);
//   a type value passed through a function argument, a `let`, or a property
//   is not followed; and a kind-keyed table whose values are real (the icons
//   table) is allowed by (b) unless it is looked up by `[x.type]` with a
//   non-kind key. The kind sets themselves come from contract.js, so those
//   paths would have to restate a kind on purpose to slip past.
//
//   node scripts/check-enum-literals.mjs
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { createRequire } from 'node:module';
import { stripComments } from './js-deps-freeze.mjs';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const STATIC_DIR = path.join(ROOT, 'internal', 'server', 'static');
const espree = createRequire(path.join(ROOT, 'test', 'e2e', 'package.json'))('espree');

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

// OTHER_TYPES: per file, the `.type` values that are neither a WS frame type
// nor a kind. Each must be compared at least once in its file (c).
export const OTHER_TYPES = {
  'auth_modal.js': ['quick', 'project'], // the auth dialog's mode
  'composer_files.js': ['application/pdf'], // File.type
  'cron_view.js': ['keydown'], // DOM events
  'dashboard.js': ['keydown', 'unhandledrejection'],
};

// KIND_SENTINELS: files that must compare `.type` with a kind, so a scan that
// stops seeing them fails instead of passing empty.
export const KIND_SENTINELS = ['dashboard.js', 'running_banner.js'];

export const KIND_ENUMS = ['EVENT_TYPE', 'EVENT_TYPE_INTERNAL', 'EVENT_TYPE_MD_IGNORE'];

// contractKindProblems is check (a) over NZ_CONTRACT.ENUMS.
export function contractKindProblems(enums) {
  const problems = [];
  for (const k of KIND_ENUMS) {
    if (!Array.isArray(enums?.[k]) || enums[k].length === 0) problems.push(`contract.js: ENUMS.${k} is missing or empty`);
  }
  const all = new Set(enums?.EVENT_TYPE || []);
  for (const k of KIND_ENUMS.slice(1)) {
    for (const v of enums?.[k] || []) if (!all.has(v)) problems.push(`contract.js: ENUMS.${k} has ${JSON.stringify(v)}, which ENUMS.EVENT_TYPE does not list`);
  }
  return problems;
}

const walk = (n, fn) => {
  if (!n || typeof n.type !== 'string') return;
  fn(n);
  for (const [k, v] of Object.entries(n)) {
    if (k === 'loc') continue;
    if (Array.isArray(v)) v.forEach((c) => walk(c, fn));
    else if (v && typeof v === 'object') walk(v, fn);
  }
};
const unchain = (n) => (n?.type === 'ChainExpression' ? n.expression : n);
// isTypeMember: x.type, x?.type or x['type'].
const isTypeMember = (n) => {
  const m = unchain(n);
  return m?.type === 'MemberExpression' && (m.computed ? str(m.property) === 'type' : m.property.name === 'type');
};
// str: the value of a string literal or of a template with no expressions.
const str = (n) => {
  if (n?.type === 'Literal' && typeof n.value === 'string') return n.value;
  if (n?.type === 'TemplateLiteral' && n.expressions.length === 0) return n.quasis[0].value.cooked;
  return null;
};
const keyName = (p) => (p.type !== 'Property' || p.computed ? null : p.key.type === 'Identifier' ? p.key.name : str(p.key));

// kindProblems is checks (b) and (c) plus the blind guards over files
// (name → source); counts reports what was seen.
export function kindProblems(files, contract, other = OTHER_TYPES, sentinels = KIND_SENTINELS) {
  const problems = [];
  const kinds = new Set(contract.ENUMS?.EVENT_TYPE || []);
  const ws = new Set(Object.values(contract.WS || {}));
  const otherHits = new Map(Object.entries(other).map(([f, vs]) => [f, new Map(vs.map((v) => [v, 0]))]));
  const kindCmp = new Map();
  const counts = { kindComparisons: 0, otherComparisons: 0, lookups: 0 };
  for (const [file, src] of Object.entries(files)) {
    if (file === 'contract.js') continue;
    let ast;
    try {
      ast = espree.parse(src, { ecmaVersion: 'latest', sourceType: 'module', loc: true });
    } catch (err) {
      problems.push(`${file}: does not parse (${err.message}) — the kind scan cannot see it`);
      continue;
    }
    const at = (n) => `${file}:${n.loc.start.line}`;
    const objects = new Map();
    const arrays = new Map(); // const name → the ArrayExpressions bound to it (directly or via new Set([...]))
    const aliases = new Set(); // const names bound to a type expression
    const typeValued = (n) =>
      isTypeMember(n) ||
      (n?.type === 'LogicalExpression' && (n.operator === '&&' ? isTypeMember(n.right) : isTypeMember(n.left)));
    const isType = (n) => isTypeMember(n) || (n?.type === 'Identifier' && aliases.has(n.name));
    const arrayOf = (n) =>
      n?.type === 'ArrayExpression' ? n : n?.type === 'NewExpression' && n.callee.name === 'Set' && n.arguments[0]?.type === 'ArrayExpression' ? n.arguments[0] : null;
    const push = (m, k, v) => m.set(k, [...(m.get(k) || []), v]);
    walk(ast, (n) => {
      if (n.type === 'VariableDeclarator' && n.id.type === 'Identifier' && n.init?.type === 'ObjectExpression') push(objects, n.id.name, n.init);
      if (n.type !== 'VariableDeclaration' || n.kind !== 'const') return;
      for (const d of n.declarations) {
        if (d.id.type === 'Identifier' && arrayOf(d.init)) push(arrays, d.id.name, arrayOf(d.init));
        if (d.id.type === 'Identifier' && typeValued(d.init)) aliases.add(d.id.name);
        if (d.id.type === 'ObjectPattern') {
          for (const p of d.id.properties) if (keyName(p) === 'type' && p.value.type === 'Identifier') aliases.add(p.value.name);
        }
      }
    });
    const compared = (node, v) => {
      if (kinds.has(v)) {
        counts.kindComparisons++;
        kindCmp.set(file, (kindCmp.get(file) || 0) + 1);
      } else if (otherHits.get(file)?.has(v)) {
        counts.otherComparisons++;
        otherHits.get(file).set(v, otherHits.get(file).get(v) + 1);
      } else if (!ws.has(v)) {
        problems.push(`${at(node)}: .type compared with ${JSON.stringify(v)}, which is not a WS frame type, an ENUMS.EVENT_TYPE kind, or listed for ${file} in OTHER_TYPES`);
      }
    };
    walk(ast, (n) => {
      if (n.type === 'ArrayExpression') {
        const hit = n.elements.map(str).filter((v) => v !== null && kinds.has(v));
        if (hit.length >= 2) problems.push(`${at(n)}: array restates ${hit.length} kinds (${hit.join(', ')}) — read NZ_CONTRACT.ENUMS.EVENT_TYPE*`);
      }
      if (n.type === 'ObjectExpression') {
        const hit = n.properties.map(keyName).filter((k) => k !== null && kinds.has(k));
        const setLike = n.properties.every((p) => p.type === 'Property' && p.value.type === 'Literal' && (p.value.value === true || p.value.value === 1));
        if (hit.length >= 2 && setLike) problems.push(`${at(n)}: object restates ${hit.length} kinds as a set (${hit.join(', ')}) — read NZ_CONTRACT.ENUMS.EVENT_TYPE*`);
      }
      if (n.type === 'MemberExpression' && n.computed && isType(n.property)) {
        const tables = n.object.type === 'ObjectExpression' ? [n.object] : n.object.type === 'Identifier' ? objects.get(n.object.name) || [] : [];
        for (const t of tables) {
          counts.lookups++;
          for (const p of t.properties) {
            const k = keyName(p);
            if (k === null) problems.push(`${at(p)}: a table looked up by [.type] has a key that is not a static name`);
            else if (!kinds.has(k)) problems.push(`${at(p)}: a table looked up by [.type] has key ${JSON.stringify(k)}, which ENUMS.EVENT_TYPE does not list`);
          }
        }
      }
      if (n.type === 'BinaryExpression' && /^[!=]==?$/.test(n.operator)) {
        for (const [a, b] of [[n.left, n.right], [n.right, n.left]]) {
          if (isType(a) && str(b) !== null) compared(n, str(b));
        }
      }
      if (n.type === 'SwitchStatement' && isType(n.discriminant)) {
        for (const c of n.cases) if (str(c.test) !== null) compared(c, str(c.test));
      }
      const callee = unchain(n.type === 'CallExpression' ? n.callee : null);
      if (callee?.type === 'MemberExpression' && !callee.computed && ['includes', 'has', 'indexOf'].includes(callee.property.name) && isType(n.arguments[0])) {
        const lists = arrayOf(callee.object) ? [arrayOf(callee.object)] : callee.object.type === 'Identifier' ? arrays.get(callee.object.name) || [] : [];
        for (const l of lists) for (const el of l.elements) if (str(el) !== null) compared(el, str(el));
      }
    });
  }
  for (const [file, vs] of otherHits) {
    for (const [v, hits] of vs) if (!hits) problems.push(`OTHER_TYPES['${file}'] lists ${JSON.stringify(v)}, which no .type comparison in ${file} uses — drop the dead entry`);
  }
  for (const f of sentinels) {
    if (!kindCmp.get(f)) problems.push(`${f}: no .type comparison with a kind — the kind scan has gone blind`);
  }
  return { problems, counts };
}

// checkAll runs every check above over one tree (file name → source); the CLI
// prints what it returns.
export function checkAll(files, contract, other = OTHER_TYPES, sentinels = KIND_SENTINELS) {
  const kind = kindProblems(files, contract, other, sentinels);
  const problems = [...run(files, contract.ENUMS?.DEATH_REASON || []), ...contractKindProblems(contract.ENUMS), ...kind.problems];
  return { problems, counts: kind.counts };
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  // contract.js is the ES module the dashboard's modules import.
  const { NZ_CONTRACT: contract } = await import(pathToFileURL(path.join(STATIC_DIR, 'contract.js')).href);
  const files = {};
  for (const f of fs.readdirSync(STATIC_DIR).filter((f) => f.endsWith('.js') && f !== 'sw.js')) {
    files[f] = fs.readFileSync(path.join(STATIC_DIR, f), 'utf8');
  }
  const { problems, counts } = checkAll(files, contract);
  if (problems.length) {
    for (const p of problems) console.error(p);
    console.error(`check-enum-literals: ${problems.length} problem(s)`);
    process.exit(1);
  }
  const { kindComparisons, otherComparisons, lookups } = counts;
  console.log(`check-enum-literals: OK (${contract.ENUMS.DEATH_REASON.length} death reasons, ${contract.ENUMS.EVENT_TYPE.length} kinds; ` +
    `${kindComparisons} kind and ${otherComparisons} other .type comparisons, ${lookups} [.type] table(s); ${Object.keys(files).length - 1} files scanned for kinds, ${Object.keys(files).length - 2} for death-reason literals)`);
}
