// node --test scripts/check-mock-rest.test.mjs — the e2e mock's REST
// responses against the backend's REST schemas (#2909).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';
import { extraFields, unionResponse } from './check-mock-rest.mjs';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const schema = JSON.parse(fs.readFileSync(path.join(ROOT, 'internal', 'dashboard', 'session', 'testdata', 'rest.schema.json'), 'utf8'));
const { startMockServer } = createRequire(import.meta.url)(path.join(ROOT, 'test', 'e2e', 'mock-server.js'));

test('the mock\'s /api/sessions is a shape the backend can produce', async () => {
  const mock = await startMockServer();
  try {
    const body = await (await fetch(mock.url + '/api/sessions')).json();
    assert.ok(body.sessions.length > 0, 'the mock returned no sessions to check');
    const shape = unionResponse(schema.responses.sessions, schema.responses.sessions_multi);
    assert.deepEqual(extraFields(body, shape, schema.defs, 'sessions'), []);
  } finally {
    mock.server.close();
  }
});

test('extraFields finds a field at any depth and skips undescribed values', () => {
  const defs = {
    S: { properties: { key: { type: 'string' }, subs: { type: 'array', items: { type: 'object', $ref: 'T' } }, meta: { type: 'any' } } },
    T: { properties: { name: { type: 'string' } } },
  };
  const top = { properties: { list: { type: 'array', items: { type: 'object', $ref: 'S' } } } };
  assert.deepEqual(extraFields({ list: [{ key: 'a', subs: [{ name: 'x' }], meta: { anything: 1 } }] }, top, defs, 'r'), []);
  assert.deepEqual(extraFields({ list: [{ key: 'a', stale: 1, subs: [{ name: 'x', nick: 'y' }] }], extra: 2 }, top, defs, 'r'),
    ['r.list[0].stale', 'r.list[0].subs[0].nick', 'r.extra']);
});

test('unionResponse takes each field from the first variant that declares it', () => {
  const typed = { properties: { sessions: { type: 'array', items: { type: 'object', $ref: 'S' } } } };
  const loose = { properties: { sessions: { type: 'array', items: { type: 'any' } }, nodes: { type: 'object' } } };
  const u = unionResponse(typed, loose);
  assert.equal(u.properties.sessions.items.$ref, 'S');
  assert.ok('nodes' in u.properties);
});
