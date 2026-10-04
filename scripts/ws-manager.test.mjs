// node --test scripts/ws-manager.test.mjs
// ws_manager.js is a leaf (check-ws-receivers R7), so node imports it as is;
// only WebSocket and location are stubbed, before the import (CI does not pin
// a node version, and a newer one ships its own WebSocket). Each test takes a
// fresh module instance, so registrations and state do not leak between them.
import { test, mock } from 'node:test';
import assert from 'node:assert/strict';

const sockets = [];
class FakeSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  constructor(url) { this.url = url; this.readyState = FakeSocket.CONNECTING; this.sent = []; this.closed = false; sockets.push(this); }
  send(data) { this.sent.push(JSON.parse(data)); }
  close() { this.closed = true; }
}
globalThis.WebSocket = FakeSocket;
globalThis.location = { protocol: 'http:', host: 'nz.test' };

let seq = 0;
const fresh = async () => (await import('../internal/server/static/ws_manager.js?i=' + (++seq)));
const open = (wsm) => { wsm.conn = new FakeSocket('ws://x'); wsm.conn.readyState = FakeSocket.OPEN; return wsm.conn; };

test('claims run in registration order, the first match wins, and claims go before the fallback', async () => {
  const { wsm } = await fresh();
  const got = [];
  wsm.on('history', (msg) => got.push('fallback:' + msg.key));
  wsm.on('history', (msg) => got.push('first:' + msg.key), (msg) => msg.key.startsWith('cron:'));
  wsm.on('history', (msg) => got.push('second:' + msg.key), () => true);
  wsm.onMessage({ type: 'history', key: 'cron:1' });
  wsm.onMessage({ type: 'history', key: 'plain' });
  assert.deepEqual(got, ['first:cron:1', 'second:plain']);
});

test('the fallback runs when no claim matches', async () => {
  const { wsm } = await fresh();
  const got = [];
  wsm.on('event', (msg) => got.push('claim'), () => false);
  wsm.on('event', (msg) => got.push('fallback:' + msg.key));
  wsm.onMessage({ type: 'event', key: 'k' });
  assert.deepEqual(got, ['fallback:k']);
});

test('a second fallback for a type throws; claims may be many', async () => {
  const { wsm } = await fresh();
  wsm.on('event', () => {});
  wsm.on('event', () => {}, () => true);
  wsm.on('event', () => {}, () => true);
  assert.throws(() => wsm.on('event', () => {}), /event already has a handler/);
});

test('an unknown frame type throws at registration', async () => {
  const { wsm } = await fresh();
  assert.throws(() => wsm.on('no_such_frame', () => {}), /unknown frame type no_such_frame/);
});

test('an unregistered type, or one no handler takes, is dropped', async () => {
  const { wsm } = await fresh();
  wsm.on('event', () => { throw new Error('must not run'); }, () => false);
  assert.doesNotThrow(() => wsm.onMessage({ type: 'history' }));
  assert.doesNotThrow(() => wsm.onMessage({ type: 'no_such_frame' }));
  assert.doesNotThrow(() => wsm.onMessage({ type: 'event' }));
});

test('auth_ok: CONNECTED, backoff reset and ping before the onReady callbacks, which run in order and see the frame', async () => {
  const { wsm, WS_STATES } = await fresh();
  open(wsm);
  wsm.backoff = 16000;
  const got = [];
  wsm.onStateChange((s, prev) => got.push(`state ${prev}->${s} ever=${wsm._everConnected}`));
  wsm.onReady(() => got.push(`ready1 ${wsm.state} backoff=${wsm.backoff} ping=${wsm.pingTimer !== null}`));
  wsm.onReady((msg) => got.push('ready2 ' + msg.asset_version));
  try {
    wsm.onMessage({ type: 'auth_ok', asset_version: 'v1' });
  } finally {
    wsm.cleanup(); // the ping interval would keep node alive past a failure
  }
  assert.deepEqual(got, ['state off->connected ever=false', 'ready1 connected backoff=1000 ping=true', 'ready2 v1']);
  assert.equal(wsm.state, WS_STATES.CONNECTED);
  assert.equal(wsm._everConnected, true, 'set after the CONNECTED listeners ran');
});

test('auth_fail: the onAuthFail callbacks see the frame before the socket closes', async () => {
  const { wsm } = await fresh();
  const conn = open(wsm);
  const got = [];
  wsm.onAuthFail((msg) => got.push(`${msg.error} closed=${conn.closed}`));
  wsm.onMessage({ type: 'auth_fail', error: 'too many attempts' });
  assert.deepEqual(got, ['too many attempts closed=false']);
  assert.equal(conn.closed, true);
});

test('setState stamps _disconnectedSince on leaving CONNECTED and on a cold start, never for OFF, and clears it on CONNECTED', async () => {
  const { wsm, WS_STATES } = await fresh();
  mock.timers.enable({ apis: ['Date'], now: 1000 });
  try {
    wsm.setState(WS_STATES.OFF);
    assert.equal(wsm._disconnectedSince, 0, 'OFF is pre-boot, not an outage');
    wsm.setState(WS_STATES.CONNECTING);
    assert.equal(wsm._disconnectedSince, 1000, 'cold start arms from the first attempt');
    mock.timers.tick(500);
    wsm.setState(WS_STATES.AUTH);
    assert.equal(wsm._disconnectedSince, 1000, 'an outage in progress keeps its start');
    wsm.setState(WS_STATES.CONNECTED);
    assert.equal(wsm._disconnectedSince, 0);
    mock.timers.tick(500);
    wsm.setState(WS_STATES.DISCONNECTED);
    assert.equal(wsm._disconnectedSince, 2000, 'leaving CONNECTED stamps the clock');
  } finally {
    mock.timers.reset();
  }
});

test('while the auth block holds, connect does not dial and the reconnect waits out the deadline', async () => {
  const { wsm } = await fresh();
  mock.timers.enable({ apis: ['Date', 'setTimeout'], now: 10000 });
  try {
    const before = sockets.length;
    wsm._authBlockUntil = 15000;
    wsm.connect();
    assert.equal(sockets.length, before, 'no dial inside the lockout');
    assert.notEqual(wsm.reconnectTimer, null);
    mock.timers.tick(4999);
    assert.equal(sockets.length, before, 'no dial before the deadline, whatever the backoff');
    mock.timers.tick(501); // the deadline plus the largest jitter
    assert.equal(sockets.length, before + 1, 'dials once the block has passed');
    assert.equal(sockets[sockets.length - 1].url, 'ws://nz.test/ws');
  } finally {
    mock.timers.reset();
  }
});

test('disconnect cancels the reconnect, closes the socket and ends OFF', async () => {
  const { wsm, WS_STATES } = await fresh();
  const conn = open(wsm);
  wsm.reconnectTimer = setTimeout(() => assert.fail('reconnect must be cancelled'), 1);
  const got = [];
  wsm.onStateChange((s) => got.push(s));
  wsm.disconnect();
  assert.equal(conn.closed, true);
  assert.equal(wsm.conn, null);
  assert.equal(wsm.reconnectTimer, null);
  assert.deepEqual(got, [WS_STATES.OFF]);
});
