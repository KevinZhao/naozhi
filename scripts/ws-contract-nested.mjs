// ws-contract-nested.mjs — the nested-field half of test/e2e/check-ws-contract.mjs
// (#2909).
//
// A frame nests structs (EventEntry, AgentMetaPatch) that wsproto.schema.json
// expands under defs. Two kinds of read reach into them: a member chain off
// the frame (`msg.meta.tool_uses`), and a function that takes an EventEntry,
// declared on the parameter — `function f(/** @type {EventEntry} */ e)` — or
// by a `/** @param {EventEntry} e */` block right before the function, so the
// check knows which parameter to follow. Each step of a chain must be a property of the struct
// it lands in; an index step moves into an array's element struct; a step off
// a non-struct value (a string's .length) ends the chain.
import path from 'node:path';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';

// espree comes with eslint in test/e2e/node_modules (npm install there first).
const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const espree = createRequire(path.join(ROOT, 'test', 'e2e', 'package.json'))('espree');

// chain returns [root identifier, steps] for a member expression, or null when
// its root is not an identifier. A step is {name} or {computed: true}.
export function chain(node) {
  const steps = [];
  while (node.type === 'MemberExpression') {
    steps.unshift(node.computed ? { computed: true } : { name: node.property.name });
    node = node.object;
  }
  return node.type === 'Identifier' ? [node.name, steps] : null;
}

// followSteps checks steps against the struct prop describes and returns the
// first bad step as {step, def}, or null.
export function followSteps(defs, prop, steps) {
  let cur = prop;
  for (const s of steps) {
    if (!cur) return null;
    if (s.computed) {
      cur = cur.items || null;
      continue;
    }
    const def = cur.$ref ? defs[cur.$ref] : null;
    if (!def) return null;
    if (!(s.name in def.properties)) return { step: s.name, def: cur.$ref };
    cur = def.properties[s.name];
  }
  return null;
}

// walkAll visits every node; visit returning false skips the node's children.
function walkAll(node, visit) {
  if (!node || typeof node.type !== 'string') return;
  if (visit(node) === false) return;
  for (const [k, v] of Object.entries(node)) {
    if (k === 'loc' || k === 'range') continue;
    if (Array.isArray(v)) v.forEach((c) => walkAll(c, visit));
    else if (v && typeof v === 'object') walkAll(v, visit);
  }
}

// outerMembers calls fn once per member chain, at its outermost node.
function outerMembers(root, fn, skip = () => false) {
  const inner = new Set();
  walkAll(root, (n) => {
    if (skip(n)) return false;
    if (n.type === 'MemberExpression') {
      if (n.object.type === 'MemberExpression') inner.add(n.object);
      if (!inner.has(n)) fn(n);
    }
    return true;
  });
}

const isFn = (n) => /Function/.test(n.type);
const declares = (fn, name) => fn.params.some((p) => p.type === 'Identifier' && p.name === name);

// frameProp finds the struct-valued property a frame declares under field.
function frameProp(schema, field) {
  for (const f of Object.values(schema.frames)) {
    const p = f.properties?.[field];
    if (p && (p.$ref || p.items?.$ref)) return p;
  }
  return null;
}

// checkNested returns the number of nested reads checked in src, the number of
// EventEntry-typed functions, and a "line: problem" string per bad read.
export function checkNested(src, schema) {
  const defs = schema.defs || {};
  const ast = espree.parse(src, { ecmaVersion: 'latest', sourceType: 'module', loc: true, range: true, comment: true });
  const problems = [];
  let reads = 0;
  const check = (n, rootProp, what, steps) => {
    reads++;
    const bad = followSteps(defs, rootProp, steps);
    if (bad) problems.push(`${n.loc.start.line} reads ${what}, but ${bad.def} has no field ${JSON.stringify(bad.step)}`);
  };
  const label = (root, steps) => root + steps.map((s) => (s.computed ? '[]' : '.' + s.name)).join('');

  outerMembers(ast, (n) => {
    const c = chain(n);
    if (!c || c[0] !== 'msg' || c[1].length < 2 || c[1][0].computed) return;
    const p = frameProp(schema, c[1][0].name);
    if (p) check(n, p, label('msg', c[1]), c[1].slice(1));
  });

  // A block comment types a parameter when it sits right before the parameter
  // (@type) or right before the function (@param).
  const blocks = ast.comments.filter((c) => c.type === 'Block');
  const adjacent = (c, node) => c.range[1] <= node.range[0] && /^\s*$/.test(src.slice(c.range[1], node.range[0]));
  const typed = [];
  walkAll(ast, (n) => {
    if (!isFn(n)) return;
    for (const p of n.params) {
      if (p.type === 'Identifier' && blocks.some((c) => /@type\s*\{EventEntry\}/.test(c.value) && adjacent(c, p))) {
        typed.push([n, p.name]);
      }
    }
    const doc = blocks.find((c) => /@param\s*\{EventEntry\}/.test(c.value) && adjacent(c, n));
    if (doc) typed.push([n, /@param\s*\{EventEntry\}\s*([A-Za-z_$][\w$]*)/.exec(doc.value)[1]]);
  });
  const entry = { type: 'object', $ref: 'clievent.EventEntry' };
  for (const [fn, name] of typed) {
    // A nested function binding the same name shadows the entry.
    const shadows = (n) => n !== fn && isFn(n) && declares(n, name);
    outerMembers(fn.body, (n) => {
      const c = chain(n);
      if (c && c[0] === name) check(n, entry, label(name, c[1]), c[1]);
    }, shadows);
  }
  return { reads, typedFns: typed.length, problems };
}
