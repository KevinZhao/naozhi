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
//       drop that handler's reads out of it without a sound. wsm.onAuthFail
//       and wsm.onReady callbacks are held to the same rule: they read
//       auth_fail's and auth_ok's fields.
//   R5  wsm, sessionStream and cronLive are managed objects, each a literal in
//       its owner file (MANAGED):
//       (a) outside the owner the name appears only as `name.<key>` or in an
//           import from the owner: no alias, argument, computed access, spread
//           or re-export, and nowhere a property of that name (deps.wsm,
//           configureX({ wsm }));
//       (b) every `name.<key>`, and `this.<key>` in the literal's methods,
//           names a key the literal declares (derived from the AST);
//       (c) no assignment creates a key the literal does not declare;
//       (d) wsm declares exactly WSM_CORE, so business state cannot come back
//           (an extra key) and the list cannot go stale (a missing one), and
//           every declared key of the three is referenced somewhere.
//   R7  leaves (LEAVES): ws_manager.js, session_stream.js and platform.js
//       import only the modules listed, so node and the modules evaluated
//       before dashboard.js can import them without a cycle.
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
    if (n.type === 'CallExpression' && n.callee.type === 'MemberExpression' && !n.callee.computed &&
        n.callee.object.type === 'Identifier' && n.callee.object.name === 'wsm' && /^(onAuthFail|onReady)$/.test(n.callee.property.name) &&
        isFn(n.arguments[0])) {
      firstParamMsg(n.arguments[0], n.callee.property.name + ' callback', n);
      checkForwards(n.arguments[0], n);
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

// R5: each managed object and its owner file. WSM_CORE is wsm's closed key
// set: the connection, the dispatch table and the lifecycle callbacks.
export const MANAGED = { wsm: 'ws_manager.js', sessionStream: 'session_stream.js', cronLive: 'cron_live.js' };
export const WSM_CORE = [
  'conn', 'state', 'backoff', 'maxBackoff', 'reconnectTimer', 'pingTimer', 'sendCounter',
  '_everConnected', '_authBlockUntil', '_disconnectedSince', '_ready', '_stateChange', '_authFail', '_frames',
  'connect', 'cleanup', 'disconnect', 'scheduleReconnect', 'on', 'onReady', 'onStateChange', 'onAuthFail',
  'onMessage', 'startPing', 'send', 'setState', 'isConnected',
];
// R7: what each leaf may import.
export const LEAVES = {
  'platform.js': [],
  'ws_manager.js': ['./contract.js', './platform.js'],
  'session_stream.js': ['./contract.js', './state.js', './ws_manager.js'],
};

const keyName = (k) => (k.type === 'Identifier' ? k.name : (k.type === 'Literal' ? String(k.value) : null));

// checkModules applies R5 and R7 across files ([name, source] pairs).
export function checkModules(files, { managed = MANAGED, core = { wsm: WSM_CORE }, leaves = LEAVES } = {}) {
  const problems = [];
  const asts = new Map(files.map(([f, src]) => [f, espree.parse(src, { ecmaVersion: 'latest', sourceType: 'module', loc: true })]));
  const at = (f, n) => `${f}:${n.loc.start.line}`;
  // The owner literals and their declared keys.
  const decl = new Map();
  for (const [name, owner] of Object.entries(managed)) {
    let lit = null;
    walk(asts.get(owner), (n) => {
      if (n.type === 'VariableDeclarator' && n.id.type === 'Identifier' && n.id.name === name && n.init && n.init.type === 'ObjectExpression') lit = n.init;
    });
    if (!lit) { problems.push(`${owner}: no \`const ${name} = { … }\` literal to derive ${name}'s keys from (R5)`); continue; }
    decl.set(name, { owner, lit, keys: new Set(lit.properties.map((p) => !p.computed && p.key && keyName(p.key)).filter(Boolean)), used: new Set() });
  }
  const access = (f, name, key, node, write) => {
    const d = decl.get(name);
    if (!d) return;
    d.used.add(key);
    if (d.keys.has(key)) return;
    problems.push(write
      ? `${at(f, node)}: assigns ${name}.${key}, which ${name} does not declare — declare it in ${d.owner}'s literal (R5c)`
      : `${at(f, node)}: ${name}.${key} is not a key ${name} declares in ${d.owner} (R5b)`);
  };
  const isWrite = (n, parents) => {
    const p = parents[parents.length - 1];
    return (p && p.type === 'AssignmentExpression' && p.left === n) || (p && p.type === 'UpdateExpression');
  };
  const seen = new WeakSet(); // espree shares one node for `{ x }` / `import { x }`
  for (const [f, ast] of asts) {
    walk(ast, (n, parents) => {
      const p = parents[parents.length - 1];
      if (n.type === 'ImportDeclaration' || n.type === 'ExportNamedDeclaration' || n.type === 'ExportAllDeclaration') {
        if (n.source && leaves[f] && !leaves[f].includes(n.source.value)) {
          problems.push(`${at(f, n)}: ${f} is a leaf and may import only ${leaves[f].join(', ') || 'nothing'}, not ${n.source.value} (R7)`);
        }
      }
      if (n.type === 'ImportExpression' && leaves[f]) problems.push(`${at(f, n)}: ${f} is a leaf and may not import() (R7)`);
      if (n.type === 'MemberExpression' && !n.computed && n.object.type === 'ThisExpression') {
        // this.<key> inside an owner literal's methods (arrows keep `this`).
        for (const [name, d] of decl) {
          if (d.owner !== f) continue;
          const method = parents.find((q) => q.type === 'Property' && d.lit.properties.includes(q));
          if (!method || !isFn(method.value)) continue;
          const inner = parents.slice(parents.indexOf(method) + 2);
          if (inner.some((q) => q.type === 'FunctionExpression' || q.type === 'FunctionDeclaration')) continue;
          access(f, name, n.property.name, n, isWrite(n, parents));
        }
      }
      if (n.type !== 'Identifier' || !Object.hasOwn(managed, n.name) || seen.has(n)) return;
      seen.add(n);
      const name = n.name;
      const owner = managed[name];
      if (p && p.type === 'MemberExpression' && p.object === n && !p.computed) {
        access(f, name, p.property.name, p, isWrite(p, parents.slice(0, -1)));
        return;
      }
      if (p && ((p.type === 'MemberExpression' && p.property === n && !p.computed) || (p.type === 'Property' && p.key === n && !p.computed))) {
        problems.push(`${at(f, n)}: a property named ${name} — import ${name} from ${owner} where it is used (R5a)`);
        return;
      }
      if (p && p.type === 'ImportSpecifier') {
        const decl0 = parents[parents.length - 2];
        if (decl0.source.value !== './' + owner) problems.push(`${at(f, n)}: ${name} is imported from ${decl0.source.value}, not its owner ${owner} (R5a)`);
        if (p.local.name !== name) problems.push(`${at(f, n)}: ${name} is imported as ${p.local.name}; keep its name (R5a)`);
        return;
      }
      if (f === owner) return;
      problems.push(`${at(f, n)}: ${name} outside ${owner} only as ${name}.<key> — no alias, argument, computed access, spread or re-export (R5a)`);
    });
  }
  for (const [name, d] of decl) {
    if (core[name]) {
      for (const k of d.keys) if (!core[name].includes(k)) problems.push(`${d.owner}: ${name} declares ${k}, which is not in its core set — business state goes to the module that owns it (R5d)`);
      for (const k of core[name]) if (!d.keys.has(k)) problems.push(`${d.owner}: the core set lists ${name}.${k}, which ${name} no longer declares — drop the entry (R5d)`);
    }
    for (const k of d.keys) if (!d.used.has(k)) problems.push(`${d.owner}: ${name}.${k} is declared but never referenced (R5d)`);
  }
  for (const leaf of Object.keys(leaves)) if (!asts.has(leaf)) problems.push(`${leaf}: leaf not found (R7)`);
  return problems;
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const schema = JSON.parse(fs.readFileSync(path.join(ROOT, 'internal', 'wsproto', 'wsproto.schema.json'), 'utf8'));
  const dir = path.join(ROOT, 'internal', 'server', 'static');
  const files = fs.readdirSync(dir).filter((f) => f.endsWith('.js') && f !== 'contract.js' && f !== 'sw.js').sort()
    .map((f) => [f, fs.readFileSync(path.join(dir, f), 'utf8')]);
  const { problems, regs } = check(files, new Set(schema.types));
  problems.push(...checkModules(files));
  for (const p of problems) console.error(p);
  if (problems.length) { console.error(`check-ws-receivers: ${problems.length} problem(s)`); process.exit(1); }
  console.log(`check-ws-receivers: OK (${regs.length} registrations over ${schema.types.length} outbound types)`);
}
