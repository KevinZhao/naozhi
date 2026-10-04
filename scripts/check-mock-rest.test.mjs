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

test('the mock\'s /api/sessions/history is a shape the backend can produce, under the tag /api/sessions names', () => withMock({}, async (mock) => {
  const body = await (await fetch(mock.url + '/api/sessions/history')).json();
  assert.ok(body.history_sessions.length > 0, 'the mock returned no history to check');
  assert.deepEqual(schemaViolations(body, schema.responses.sessions_history, schema.defs, 'sessions_history', { strict: true }), []);
  const sessions = await (await fetch(mock.url + '/api/sessions')).json();
  assert.equal(sessions.stats.history_tag, body.history_tag);
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
// Each bad fixture names the violation the 500 must carry; the last one hides
// it behind a good entry, so a guard that looks only at the first is caught.
const BAD = {
  'an internal field': [[{ type: 'text', time: 1, jsonl_path: '/home/u/.claude/projects/p/s.jsonl' }], '[0].jsonl_path'],
  'an unregistered type': [[{ type: 'txt', time: 1 }], '[0].type="txt" is not in its enum'],
  'a nested field': [[{ type: 'ask_question', time: 1, ask_question: { stale: 1 } }], '[0].ask_question.stale'],
  'no type': [[{ time: 1, summary: 'no type' }], '[0].type is required'],
  'no time': [[{ type: 'text' }], '[0].time is required'],
  'a string time': [[{ type: 'text', time: 'notanumber' }], '[0].time is not an integer'],
  'a null entry': [[null], '[0] is not an object'],
  'a string entry': [['str'], '[0] is not an object'],
  'a bad second entry': [[...GOOD, { type: 'txt', time: 2 }], '[1].type="txt" is not in its enum'],
};
for (const r of ROUTES) {
  test(`${r.name} serves a good fixture and refuses a bad one`, async () => {
    await withMock(r.inject(GOOD), async (mock) => {
      const res = await fetch(mock.url + r.url);
      assert.equal(res.status, 200);
      assert.deepEqual(await res.json(), GOOD);
    });
    for (const [what, [entries, violation]] of Object.entries(BAD)) {
      await withMock(r.inject(entries), async (mock) => {
        const res = await fetch(mock.url + r.url);
        assert.equal(res.status, 500, `${r.name} served a fixture with ${what}`);
        assert.ok((await res.json()).error.includes(r.name + violation), `${r.name}: the 500 for ${what} does not name ${violation}`);
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

test('strict schemaViolations holds a value to its JSON type and a def to its required keys', () => {
  const defs = {
    E: { required: ['time'], properties: { time: { type: 'integer' }, ok: { type: 'boolean' }, cost: { type: 'number' }, tags: { type: 'array', items: { type: 'string' } }, meta: { type: 'any' }, sub: { type: 'object', $ref: 'F' } } },
    F: { required: ['name'], properties: { name: { type: 'string' } } },
  };
  const list = { type: 'array', items: { type: 'object', $ref: 'E' } };
  assert.deepEqual(schemaViolations([{ time: 1, ok: true, cost: 0.5, tags: ['a'], meta: 'x', sub: { name: 'n' } }, { time: 2, meta: [1], tags: null }], list, defs, 'r', { strict: true }), []);
  assert.deepEqual(schemaViolations([{ time: 1.5, ok: 'yes', cost: '0.5', tags: 'a' }, { tags: [1], sub: {} }, null, 'str'], list, defs, 'r', { strict: true }),
    ['r[0].time is not an integer', 'r[0].ok is not a boolean', 'r[0].cost is not a number', 'r[0].tags is not an array', 'r[1].time is required', 'r[1].tags[0] is not a string', 'r[1].sub.name is required', 'r[2] is not an object', 'r[3] is not an object']);
  assert.deepEqual(schemaViolations([{ time: 'x' }, {}, null], list, defs, 'r'), [], 'types or required keys were checked without opts.strict');
});

test('unionResponse takes each field from the first variant that declares it', () => {
  const typed = { properties: { sessions: { type: 'array', items: { type: 'object', $ref: 'S' } } } };
  const loose = { properties: { sessions: { type: 'array', items: { type: 'any' } }, nodes: { type: 'object' } } };
  const u = unionResponse(typed, loose);
  assert.equal(u.properties.sessions.items.$ref, 'S');
  assert.ok('nodes' in u.properties);
});
