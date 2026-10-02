// node --test scripts/check-mock-rest.test.mjs — the e2e mock's REST
// responses against the backend's REST schemas (#2909), and its event routes
// against the WS schema's EventEntry def.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';
import { schemaViolations, unionResponse, EVENT_ENTRIES } from './check-mock-rest.mjs';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const schema = JSON.parse(fs.readFileSync(path.join(ROOT, 'internal', 'dashboard', 'session', 'testdata', 'rest.schema.json'), 'utf8'));
const wsDefs = JSON.parse(fs.readFileSync(path.join(ROOT, 'internal', 'wsproto', 'wsproto.schema.json'), 'utf8')).defs;
const { startMockServer } = createRequire(import.meta.url)(path.join(ROOT, 'test', 'e2e', 'mock-server.js'));

async function withMock(overrides, fn) {
  const mock = await startMockServer(overrides);
  try {
    return await fn(mock);
  } finally {
    mock.server.close();
  }
}

test('the mock\'s /api/sessions is a shape the backend can produce', () => withMock({}, async (mock) => {
  const body = await (await fetch(mock.url + '/api/sessions')).json();
  assert.ok(body.sessions.length > 0, 'the mock returned no sessions to check');
  const shape = unionResponse(schema.responses.sessions, schema.responses.sessions_multi);
  assert.deepEqual(schemaViolations(body, shape, schema.defs, 'sessions'), []);
}));

test('the mock\'s default events are EventEntries the backend can send', () => withMock({}, async (mock) => {
  const res = await fetch(mock.url + '/api/sessions/events?key=k');
  assert.equal(res.status, 200);
  const body = await res.json();
  assert.ok(body.length > 0, 'the mock returned no events to check');
  assert.deepEqual(schemaViolations(body, EVENT_ENTRIES, wsDefs, 'events'), []);
}));

// The guard is what holds every spec's injected fixture to the schema: each
// event route serves a good fixture and refuses a bad one with 500.
const ROUTES = [
  { name: 'sessions_events', url: '/api/sessions/events?key=k', inject: (entries) => ({ events: entries }) },
  { name: 'discovered_preview', url: '/api/discovered/preview?session_id=s', inject: (entries) => ({ discoveredPreview: entries }) },
  { name: 'sessions_agent_events', url: '/api/sessions/agent_events?key=k&task_id=t1', inject: (entries) => ({ agentEvents: { t1: entries } }) },
];
const GOOD = [{ type: 'text', time: 1, summary: 'hi' }];
const BAD = {
  'an internal field': [{ type: 'text', time: 1, jsonl_path: '/home/u/.claude/projects/p/s.jsonl' }],
  'an unregistered type': [{ type: 'txt', time: 1 }],
  'a nested field': [{ type: 'ask_question', time: 1, ask_question: { stale: 1 } }],
};
for (const r of ROUTES) {
  test(`${r.name} serves a good fixture and refuses a bad one`, async () => {
    await withMock(r.inject(GOOD), async (mock) => {
      const res = await fetch(mock.url + r.url);
      assert.equal(res.status, 200);
      assert.deepEqual(await res.json(), GOOD);
    });
    for (const [what, entries] of Object.entries(BAD)) {
      await withMock(r.inject(entries), async (mock) => {
        const res = await fetch(mock.url + r.url);
        assert.equal(res.status, 500, `${r.name} served a fixture with ${what}`);
        assert.match((await res.json()).error, new RegExp(r.name));
      });
    }
  });
}

test('the WS EventEntry def is the wire view the guard needs', () => {
  const def = wsDefs['clievent.EventEntry'];
  assert.ok(def, 'wsproto.schema.json has no clievent.EventEntry def');
  assert.ok(def.properties.type.enum.length > 0, 'EventEntry.type has no enum');
  assert.ok(!('jsonl_path' in def.properties), 'the def still declares jsonl_path');
});

test('schemaViolations finds a field at any depth and skips undescribed values', () => {
  const defs = {
    S: { properties: { key: { type: 'string' }, subs: { type: 'array', items: { type: 'object', $ref: 'T' } }, meta: { type: 'any' } } },
    T: { properties: { name: { type: 'string' } } },
  };
  const top = { properties: { list: { type: 'array', items: { type: 'object', $ref: 'S' } } } };
  assert.deepEqual(schemaViolations({ list: [{ key: 'a', subs: [{ name: 'x' }], meta: { anything: 1 } }] }, top, defs, 'r'), []);
  assert.deepEqual(schemaViolations({ list: [{ key: 'a', stale: 1, subs: [{ name: 'x', nick: 'y' }] }], extra: 2 }, top, defs, 'r'),
    ['r.list[0].stale', 'r.list[0].subs[0].nick', 'r.extra']);
});

test('schemaViolations holds a value to its property\'s enum', () => {
  const defs = { E: { properties: { type: { type: 'string', enum: ['text', 'user'] }, summary: { type: 'string' } } } };
  const list = { type: 'array', items: { type: 'object', $ref: 'E' } };
  assert.deepEqual(schemaViolations([{ type: 'text', summary: 'anything' }, { type: 'user' }], list, defs, 'r'), []);
  assert.deepEqual(schemaViolations([{ type: 'txt' }, {}], list, defs, 'r'), ['r[0].type="txt" is not in its enum']);
});

test('unionResponse takes each field from the first variant that declares it', () => {
  const typed = { properties: { sessions: { type: 'array', items: { type: 'object', $ref: 'S' } } } };
  const loose = { properties: { sessions: { type: 'array', items: { type: 'any' } }, nodes: { type: 'object' } } };
  const u = unionResponse(typed, loose);
  assert.equal(u.properties.sessions.items.$ref, 'S');
  assert.ok('nodes' in u.properties);
});
