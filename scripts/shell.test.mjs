// node --test scripts/shell.test.mjs
// shell.js is a leaf with no imports, so node imports it as is. Each test
// takes a fresh module instance, so one registration does not leak into the
// next.
import { test } from 'node:test';
import assert from 'node:assert/strict';

let seq = 0;
const fresh = async () => (await import('../internal/server/static/shell.js?i=' + (++seq)));
const slots = async () => Object.keys((await fresh()).shell);

test('registerShell fills every slot with the root function', async () => {
  const { shell, registerShell } = await fresh();
  const impl = Object.fromEntries(Object.keys(shell).map((k) => [k, () => k]));
  registerShell(impl);
  for (const k of Object.keys(shell)) assert.equal(shell[k](), k);
});

test('a missing or non-function slot fails the registration and fills nothing', async () => {
  for (const drop of await slots()) {
    for (const bad of [undefined, null, 'x']) {
      const { shell, registerShell } = await fresh();
      const impl = Object.fromEntries(Object.keys(shell).map((k) => [k, () => k]));
      if (bad === undefined) delete impl[drop]; else impl[drop] = bad;
      assert.throws(() => registerShell(impl), new RegExp('shell slot missing: ' + drop));
      assert.ok(Object.values(shell).every((v) => v === null), 'a failed registration must not fill any slot');
    }
  }
});

test('a slot the table does not declare fails the registration', async () => {
  const { shell, registerShell } = await fresh();
  const impl = Object.fromEntries(Object.keys(shell).map((k) => [k, () => k]));
  impl.extra = () => 0;
  assert.throws(() => registerShell(impl), /shell has no slot extra/);
  assert.ok(Object.values(shell).every((v) => v === null));
});

test('the table is sealed: no slot can be added past registerShell', async () => {
  const { shell } = await fresh();
  assert.ok(Object.isSealed(shell));
  assert.throws(() => { shell.extra = () => 0; }, TypeError);
});
