// node --test scripts/ws-contract-nested.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { checkNested } from './ws-contract-nested.mjs';

const schema = {
  frames: {
    history: { properties: { events: { type: 'array', items: { type: 'object', $ref: 'E' } } } },
    agent_meta: { properties: { meta: { type: 'object', $ref: 'M' } } },
  },
  defs: {
    E: { properties: { type: { type: 'string' }, detail: { type: 'string' }, ask: { type: 'object', $ref: 'A' } } },
    A: { properties: { items: { type: 'array', items: { type: 'object', $ref: 'I' } } } },
    I: { properties: { label: { type: 'string' } } },
    M: { properties: { tool_uses: { type: 'integer' } } },
  },
};
schema.defs['clievent.EventEntry'] = schema.defs.E;
const withEntry = (body) => `/** @param {EventEntry} e */\nfunction f(e) {\n${body}\n}\n`;

test('a typed function is held to the entry struct, step by step', () => {
  assert.deepEqual(checkNested(withEntry('return e.type + e.detail.length;'), schema).problems, []);
  assert.match(checkNested(withEntry('return e.detial;'), schema).problems[0], /reads e\.detial, but clievent\.EventEntry has no field "detial"/);
  assert.match(checkNested(withEntry('return e.ask.items[0].lable;'), schema).problems[0], /has no field "lable"/);
  assert.deepEqual(checkNested(withEntry('return e.ask.items[0].label;'), schema).problems, []);
});

test('a nested function binding the same name is not the entry', () => {
  const r = checkNested(withEntry('el.onclick = (e) => e.preventDefault(); return e.type;'), schema);
  assert.deepEqual(r.problems, []);
  assert.equal(r.reads, 1);
});

test('only a doc block directly before the function types it', () => {
  const src = 'function g(e) { return e.target; }\n/** @param {EventEntry} e */\n\nfunction f(e) { return e.type; }\n';
  const r = checkNested(src, schema);
  assert.equal(r.typedFns, 1);
  assert.deepEqual(r.problems, []);
  assert.equal(checkNested('/** @param {EventEntry} e */\nconst x = 1;\nfunction f(e) { return e.nope; }\n', schema).typedFns, 0);
});

test('frame chains follow the frame field into its struct', () => {
  assert.deepEqual(checkNested('function h(msg) { return msg.meta.tool_uses; }', schema).problems, []);
  assert.match(checkNested('function h(msg) { return msg.meta.tool_use; }', schema).problems[0], /M has no field "tool_use"/);
  assert.match(checkNested('function h(msg) { return msg.events[0].detial; }', schema).problems[0], /E has no field "detial"/);
});

test('a chain that leaves the struct stops being checked', () => {
  assert.deepEqual(checkNested(withEntry('return e.detail.trim().length;'), schema).problems, []);
});

test('a type comment on the parameter types it too', () => {
  const src = 'function f(/** @type {EventEntry} */ e, opts) { return e.detial + opts.x; }';
  const r = checkNested(src, schema);
  assert.equal(r.typedFns, 1);
  assert.match(r.problems[0], /has no field "detial"/);
  // A type comment on another parameter does not type this one.
  assert.equal(checkNested('function f(e, /** @type {EventEntry} */ x) { return e.nope; }', schema).problems.length, 0);
});

test('any def short name types a parameter, and an unknown one is reported', () => {
  const s2 = { frames: {}, defs: { 'sessionview.SessionSnapshot': { properties: { state: { type: 'string' } } } } };
  assert.deepEqual(checkNested('function f(/** @type {SessionSnapshot} */ s) { return s.state; }', s2).problems, []);
  assert.match(checkNested('function f(/** @type {SessionSnapshot} */ s) { return s.stat; }', s2).problems[0], /SessionSnapshot has no field "stat"/);
  assert.match(checkNested('function f(/** @type {Nope} */ s) { return s.x; }', s2).problems[0], /types s as Nope, which no schema defines/);
});

test('fields the dashboard adds to a struct are allowed and reported as used', () => {
  const s2 = { frames: {}, defs: { 'sessionview.SessionSnapshot': { properties: { state: { type: 'string' } } } } };
  const extras = { 'sessionview.SessionSnapshot': { source: 'card kind' } };
  const r = checkNested('function f(/** @type {SessionSnapshot} */ s) { return s.source + s.state; }', s2, extras);
  assert.deepEqual(r.problems, []);
  assert.deepEqual([...r.usedExtras], ['sessionview.SessionSnapshot.source']);
  assert.equal(checkNested('function f(/** @type {SessionSnapshot} */ s) { return s.source; }', s2).problems.length, 1);
});

test('a short name two defs end in is ambiguous, not a guess', async () => {
  const { defKey } = await import('./ws-contract-nested.mjs');
  assert.equal(defKey({ 'a.Entry': {}, 'b.Entry': {} }, 'Entry'), null);
  assert.equal(defKey({ 'a.Entry': {}, 'b.Other': {} }, 'Entry'), 'a.Entry');
});
