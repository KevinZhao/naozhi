// node --test scripts/session-stream.test.mjs
// session_stream.js is a leaf (check-ws-receivers R7): it and the modules it
// imports (contract, state, ws_manager) load in node with only WebSocket and
// location stubbed, before the import. The frames it sends are read off a
// fake open socket on wsm.
import { test, afterEach, beforeEach } from 'node:test';
import assert from 'node:assert/strict';

class FakeSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  constructor() { this.readyState = FakeSocket.OPEN; this.sent = []; }
  send(data) { this.sent.push(JSON.parse(data)); }
  close() {}
}
globalThis.WebSocket = FakeSocket;
globalThis.location = { protocol: 'http:', host: 'nz.test' };

const { INITIAL_HISTORY_LIMIT, sessionStream } = await import('../internal/server/static/session_stream.js');
const { wsm } = await import('../internal/server/static/ws_manager.js');
const { selection, timers, transcript } = await import('../internal/server/static/state.js');

let conn;
beforeEach(() => {
  conn = new FakeSocket();
  wsm.conn = conn;
  Object.assign(sessionStream, { subscribedKey: null, subscribedNode: null, lastEventTimeWs: 0, _initialSubscribe: false, _pendingSubscribeKey: null, _pendingSubscribeNode: null, _subscriptionSuspended: false });
  selection.key = null;
  selection.node = 'local';
  transcript.lastEventTime = 0;
});
afterEach(() => wsm.cleanup()); // an auth_ok arms the ping interval

test('a subscribe with no cursor asks for an initial page and marks the subscribe initial', () => {
  sessionStream.subscribe('k1', 'local');
  assert.deepEqual(conn.sent, [{ type: 'subscribe', key: 'k1', limit: INITIAL_HISTORY_LIMIT }]);
  assert.equal(sessionStream._initialSubscribe, true);
  assert.equal(sessionStream._pendingSubscribeKey, 'k1');
  assert.equal(sessionStream._pendingSubscribeNode, 'local');
});

test('a subscribe with a cursor resumes after it, and names a remote node', () => {
  sessionStream.lastEventTimeWs = 4242;
  sessionStream.subscribe('k2', 'n1');
  assert.deepEqual(conn.sent, [{ type: 'subscribe', key: 'k2', node: 'n1', after: 4242 }]);
  assert.equal(sessionStream._initialSubscribe, false);
  assert.equal(sessionStream._pendingSubscribeNode, 'n1');
});

test('unsubscribe tells the server, then clears the subscription, the pending subscribe and the cursor', () => {
  Object.assign(sessionStream, { subscribedKey: 'k3', subscribedNode: 'n2', _pendingSubscribeKey: 'k4', _pendingSubscribeNode: 'local', lastEventTimeWs: 99 });
  sessionStream.unsubscribe();
  assert.deepEqual(conn.sent, [{ type: 'unsubscribe', key: 'k3', node: 'n2' }]);
  assert.deepEqual([sessionStream.subscribedKey, sessionStream.subscribedNode, sessionStream._pendingSubscribeKey, sessionStream._pendingSubscribeNode, sessionStream.lastEventTimeWs], [null, null, null, null, 0]);
});

test('unsubscribe with nothing subscribed sends nothing', () => {
  sessionStream.unsubscribe();
  assert.deepEqual(conn.sent, []);
});

test('wsm.disconnect clears the subscription and the pending subscribe, keeping the cursor', () => {
  Object.assign(sessionStream, { subscribedKey: 'k5', subscribedNode: 'local', _pendingSubscribeKey: 'k6', _pendingSubscribeNode: 'local', lastEventTimeWs: 77 });
  wsm.disconnect();
  assert.deepEqual([sessionStream.subscribedKey, sessionStream.subscribedNode, sessionStream._pendingSubscribeKey, sessionStream._pendingSubscribeNode], [null, null, null, null]);
  assert.equal(sessionStream.lastEventTimeWs, 77);
});

test('auth_ok resubscribes the selected session after the last event the transcript holds, and stops the event poll', () => {
  selection.key = 'k7';
  transcript.lastEventTime = 555;
  let cleared = false;
  timers.events = setInterval(() => assert.fail('the REST event poll must be stopped'), 60000);
  const poll = timers.events;
  const realClear = globalThis.clearInterval;
  globalThis.clearInterval = (h) => { if (h === poll) cleared = true; realClear(h); };
  try {
    wsm.onMessage({ type: 'auth_ok' });
  } finally {
    globalThis.clearInterval = realClear;
  }
  assert.equal(cleared, true);
  assert.equal(timers.events, null);
  assert.deepEqual(conn.sent, [{ type: 'subscribe', key: 'k7', after: 555 }]);
});

test('auth_ok with nothing selected subscribes nothing', () => {
  wsm.onMessage({ type: 'auth_ok' });
  assert.deepEqual(conn.sent, []);
});
