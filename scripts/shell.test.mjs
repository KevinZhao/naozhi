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
        // Dropping a one-slot group's only slot leaves a registration of nothing.
        const want = Object.keys(impl).length ? 'shell slot missing: ' + drop : 'shell: registration names no slot';
        assert.throws(() => registerShell(impl), new RegExp(want));
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

test('a registration that names no slot fails', async () => {
  const { shell, registerShell } = await fresh();
  assert.throws(() => registerShell({}), /shell: registration names no slot/);
  assert.ok(Object.values(shell).every((v) => v === null));
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

test("each shell root registers exactly one group, and every group has its root", async () => {
  const fs = await import('node:fs');
  const { SHELL_GROUPS } = await fresh();
  const roots = JSON.parse(fs.readFileSync(new URL('./js-ratchet.caps.json', import.meta.url), 'utf8')).shellRoots;
  const registered = roots.map((f) => {
    const src = fs.readFileSync(new URL('../internal/server/static/' + f, import.meta.url), 'utf8');
    const calls = [...src.matchAll(/^registerShell\(\{([^}]*)\}\);$/gm)];
    assert.equal(calls.length, 1, f + ' must call registerShell once at top level');
    return calls[0][1].split(',').map((k) => k.trim()).sort().join(',');
  });
  assert.deepEqual(registered.sort(), SHELL_GROUPS.map((g) => [...g].sort().join(',')).sort());
});
