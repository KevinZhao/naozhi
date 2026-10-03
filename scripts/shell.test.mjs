// node --test scripts/shell.test.mjs
// shell.js is a leaf with no imports, so node imports it as is. Each test
// takes a fresh module instance, so one registration does not leak into the
// next.
import { test } from 'node:test';
import assert from 'node:assert/strict';

let seq = 0;
const fresh = async () => (await import('../internal/server/static/shell.js?i=' + (++seq)));
const groups = async () => (await fresh()).SHELL_GROUPS;
const implOf = (group) => Object.fromEntries(group.map((k) => [k, () => k]));

test('every slot belongs to exactly one root group', async () => {
  const { shell, SHELL_GROUPS } = await fresh();
  const flat = SHELL_GROUPS.flat();
  assert.deepEqual([...flat].sort(), Object.keys(shell).sort());
  assert.equal(new Set(flat).size, flat.length);
});

test('registerShell fills each group with the root function', async () => {
  const { shell, registerShell, SHELL_GROUPS } = await fresh();
  for (const g of SHELL_GROUPS) registerShell(implOf(g));
  for (const k of Object.keys(shell)) assert.equal(shell[k](), k);
});

test('a missing or non-function slot fails the registration and fills nothing', async () => {
  for (const g of await groups()) {
    for (const drop of g) {
      for (const bad of [undefined, null, 'x']) {
        const { shell, registerShell } = await fresh();
        const impl = implOf(g);
        if (bad === undefined) delete impl[drop]; else impl[drop] = bad;
        assert.throws(() => registerShell(impl), new RegExp('shell slot missing: ' + drop));
        assert.ok(Object.values(shell).every((v) => v === null), 'a failed registration must not fill any slot');
      }
    }
  }
});

test('a slot the table does not declare fails the registration', async () => {
  for (const g of await groups()) {
    const { shell, registerShell } = await fresh();
    const impl = implOf(g);
    impl.extra = () => 0;
    assert.throws(() => registerShell(impl), /shell has no slot extra/);
    assert.ok(Object.values(shell).every((v) => v === null));
  }
  const { registerShell } = await fresh();
  assert.throws(() => registerShell({ extra: () => 0 }), /shell has no slot extra/);
});

test("another root's slot fails the registration and fills nothing", async () => {
  const gs = await groups();
  for (const [i, g] of gs.entries()) {
    const other = gs[(i + 1) % gs.length][0];
    const { shell, registerShell } = await fresh();
    assert.throws(() => registerShell({ ...implOf(g), [other]: () => 0 }), /belongs to another root/);
    assert.ok(Object.values(shell).every((v) => v === null));
  }
});

test('a group registers once', async () => {
  for (const g of await groups()) {
    const { shell, registerShell } = await fresh();
    registerShell(implOf(g));
    const first = g.map((k) => shell[k]);
    assert.throws(() => registerShell(implOf(g)), new RegExp('shell slot registered twice: ' + g[0]));
    assert.deepEqual(g.map((k) => shell[k]), first);
  }
});

test('the table is sealed: no slot can be added past registerShell', async () => {
  const { shell } = await fresh();
  assert.ok(Object.isSealed(shell));
  assert.throws(() => { shell.extra = () => 0; }, TypeError);
});
