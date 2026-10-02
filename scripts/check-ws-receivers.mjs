#!/usr/bin/env node
// check-ws-receivers.mjs — the receive side of the dashboard's WS frames
// (S18, #3024). Every outbound frame type reaches its handler through the
// wsm.on dispatch table; nothing discriminates frame types by hand. Rules:
//
//   R1  no receiver discriminates an outbound frame type by hand: no
//       `switch (x.type)` with a case naming an outbound type (string literal
//       or NZ_CONTRACT.WS.<k>), no `x.type ===/!==` against one.
//   R2  every wsm.on(...) names its type as NZ_CONTRACT.WS.<outbound key>, has
//       2 or 3 arguments, and is a statement at module scope (the body of a
//       top-level IIFE counts) — not inside a function, branch or loop.
//   R3  every outbound type has a registration, and at most one unconditional one.
//   R4  blind guard: at least as many registrations as outbound types.
//   R6  a handler / claim that takes a parameter names it `msg`; a handler that
//       forwards `msg` to a same-file function reaches a parameter named `msg`;
//       forwarding it to a method its same-file object literal does not
//       define (a handler left on wsm after a move) or to an imported binding
//       is refused. check-ws-contract's
//       field check only reads `msg.<field>`, so a renamed parameter would
//       drop that handler's reads out of it without a sound.
//
// check-ws-contract.mjs reads the registered type set through check() too.
// Fixtures for every rule: scripts/check-ws-receivers.test.mjs.
import fs from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const req = createRequire(path.join(ROOT, 'test', 'e2e', 'package.json'));
const espree = req('espree');

const walk = (n, fn, parents = []) => {
  if (!n || typeof n.type !== 'string') return;
  fn(n, parents);
  for (const [k, v] of Object.entries(n)) {
    if (k === 'loc' || k === 'range') continue;
    if (Array.isArray(v)) v.forEach((c) => walk(c, fn, [...parents, n]));
    else if (v && typeof v === 'object') walk(v, fn, [...parents, n]);
  }
};
const isTypeMember = (n) => n && n.type === 'MemberExpression' && !n.computed && n.property.name === 'type';
const wsKey = (n) => n && n.type === 'MemberExpression' && !n.computed &&
  n.object.type === 'MemberExpression' && !n.object.computed &&
  n.object.object.type === 'Identifier' && n.object.object.name === 'NZ_CONTRACT' &&
  n.object.property.name === 'WS' ? n.property.name : null;
const isFn = (n) => n && /Function/.test(n.type);
// atModuleScope: the call is an expression statement directly in the Program,
// or directly in the body of a top-level IIFE `(function () { … })();`.
const atModuleScope = (parents) => {
  const [stmt, block, fn, call, outer, prog] = [...parents].reverse();
  if (!stmt || stmt.type !== 'ExpressionStatement') return false;
  if (block && block.type === 'Program') return true;
  return Boolean(block && block.type === 'BlockStatement' && isFn(fn) && fn.body === block &&
    call && call.type === 'CallExpression' && call.callee === fn &&
    outer && outer.type === 'ExpressionStatement' && prog && prog.type === 'Program');
};

export function checkSource(file, src, outbound) {
  const problems = [];
  const regs = [];
  const ast = espree.parse(src, { ecmaVersion: 'latest', sourceType: 'module', loc: true });
  const names = (n) => (n && n.type === 'Literal' && outbound.has(n.value) ? n.value : (outbound.has(wsKey(n)) ? wsKey(n) : null));
  // Same-file bindings: function declarations, `const f = () =>`, object-literal methods.
  const fns = new Map();
  const objs = new Map();
  const imported = new Set();
  walk(ast, (n) => {
    if (n.type === 'FunctionDeclaration' && n.id) fns.set(n.id.name, n);
    if (n.type === 'VariableDeclarator' && n.id.type === 'Identifier') {
      if (isFn(n.init)) fns.set(n.id.name, n.init);
      if (n.init && n.init.type === 'ObjectExpression') {
        const m = new Map();
        for (const p of n.init.properties) if (p.key && isFn(p.value)) m.set(p.key.name, p.value);
        objs.set(n.id.name, m);
      }
    }
    if (n.type === 'ImportSpecifier' || n.type === 'ImportNamespaceSpecifier' || n.type === 'ImportDefaultSpecifier') imported.add(n.local.name);
  });
  const at = (n) => `${file}:${n.loc.start.line}`;
  const firstParamMsg = (fn, what, node) => {
    if (fn.params.length && !(fn.params[0].type === 'Identifier' && fn.params[0].name === 'msg')) {
      problems.push(`${at(node)}: ${what} must name its frame parameter msg (R6: the field check reads msg.<field>)`);
    }
  };
  const checkForwards = (fn, node) => {
    walk(fn.body, (c) => {
      if (c.type !== 'CallExpression') return;
      c.arguments.forEach((a, i) => {
        if (!(a.type === 'Identifier' && a.name === 'msg')) return;
        let target = null;
        let root = null;
        if (c.callee.type === 'Identifier') { root = c.callee.name; target = fns.get(root); }
        else if (c.callee.type === 'MemberExpression' && !c.callee.computed) {
          let o = c.callee.object;
          while (o.type === 'MemberExpression') o = o.object;
          root = o.type === 'Identifier' ? o.name : null;
          if (c.callee.object.type === 'Identifier') {
            const obj = objs.get(c.callee.object.name);
            target = obj?.get(c.callee.property.name);
            if (obj && !target) {
              problems.push(`${at(c)}: msg is forwarded to ${c.callee.object.name}.${c.callee.property.name}, which ${c.callee.object.name} does not define (R6)`);
              return;
            }
          }
        }
        if (target) {
          const p = target.params[i];
          if (!(p && p.type === 'Identifier' && p.name === 'msg')) problems.push(`${at(c)}: msg is forwarded to a parameter not named msg (R6)`);
        } else if (root && imported.has(root)) {
          problems.push(`${at(c)}: msg is forwarded to imported ${root}; register the handler in the module that owns it (R6)`);
        }
      });
    });
  };
  walk(ast, (n, parents) => {
    if (n.type === 'SwitchStatement' && isTypeMember(n.discriminant)) {
      for (const c of n.cases) if (names(c.test)) problems.push(`${at(c)}: switch case on outbound frame type ${names(c.test)} (R1: register it with wsm.on)`);
    }
    if (n.type === 'BinaryExpression' && /^[!=]==?$/.test(n.operator) &&
        ((isTypeMember(n.left) && names(n.right)) || (isTypeMember(n.right) && names(n.left)))) {
      problems.push(`${at(n)}: .type compared with outbound frame type (R1: register it with wsm.on)`);
    }
    if (n.type === 'CallExpression' && n.callee.type === 'MemberExpression' && !n.callee.computed &&
        n.callee.object.type === 'Identifier' && n.callee.object.name === 'wsm' && n.callee.property.name === 'on') {
      const key = wsKey(n.arguments[0]);
      if (!key || !outbound.has(key)) problems.push(`${at(n)}: wsm.on must name an outbound type as NZ_CONTRACT.WS.<key> (R2)`);
      if (n.arguments.length < 2 || n.arguments.length > 3) problems.push(`${at(n)}: wsm.on takes (type, handler[, when]) (R2)`);
      if (!atModuleScope(parents)) problems.push(`${at(n)}: wsm.on must be a statement at module scope (R2)`);
      for (const [idx, what] of [[1, 'handler'], [2, 'claim']]) {
        let f = n.arguments[idx];
        if (!f) continue;
        if (f.type === 'Identifier') {
          const d = fns.get(f.name);
          if (!d) { problems.push(`${at(n)}: ${what} ${f.name} must be a function declared in this file (R6)`); continue; }
          f = d;
        }
        if (!isFn(f)) { problems.push(`${at(n)}: ${what} must be a function (R6)`); continue; }
        firstParamMsg(f, what, n);
        checkForwards(f, n);
      }
      regs.push({ file, line: n.loc.start.line, key, claim: n.arguments.length === 3 });
    }
  });
  return { problems, regs };
}

export function check(files, outbound) {
  const problems = [];
  const regs = [];
  for (const [file, src] of files) {
    const r = checkSource(file, src, outbound);
    problems.push(...r.problems);
    regs.push(...r.regs);
  }
  for (const t of outbound) {
    const all = regs.filter((r) => r.key === t);
    const plain = all.filter((r) => !r.claim);
    if (all.length === 0) problems.push(`${t}: no wsm.on handler — register one (a deliberately ignored frame gets an explicit no-op) (R3)`);
    if (plain.length > 1) problems.push(`${t}: ${plain.length} unconditional handlers (${plain.map((r) => r.file + ':' + r.line).join(', ')}) (R3)`);
  }
  if (regs.length < outbound.size) problems.push(`only ${regs.length} wsm.on registrations for ${outbound.size} outbound types — the scan has gone blind (R4)`);
  return { problems, regs };
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const schema = JSON.parse(fs.readFileSync(path.join(ROOT, 'internal', 'wsproto', 'wsproto.schema.json'), 'utf8'));
  const dir = path.join(ROOT, 'internal', 'server', 'static');
  const files = fs.readdirSync(dir).filter((f) => f.endsWith('.js') && f !== 'contract.js' && f !== 'sw.js').sort()
    .map((f) => [f, fs.readFileSync(path.join(dir, f), 'utf8')]);
  const { problems, regs } = check(files, new Set(schema.types));
  for (const p of problems) console.error(p);
  if (problems.length) { console.error(`check-ws-receivers: ${problems.length} problem(s)`); process.exit(1); }
  console.log(`check-ws-receivers: OK (${regs.length} registrations over ${schema.types.length} outbound types)`);
}
