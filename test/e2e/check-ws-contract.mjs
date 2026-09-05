#!/usr/bin/env node
// check-ws-contract.mjs — the frontend half of the #2535 WS contract: the
// dashboard's wsm.onMessage switch and the backend-generated
// internal/wsproto/wsproto.schema.json must agree on the message-type set.
// Every case the frontend dispatches on must be a type the backend declares,
// and every backend type must have a case (a deliberately ignored type still
// gets an explicit no-op case, like `unsubscribed`). Runs in the lint-js CI
// job next to eslint and the freeze/ratchet checks.

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const schemaPath = path.join(ROOT, 'internal', 'wsproto', 'wsproto.schema.json');
const dashboardPath = path.join(ROOT, 'internal', 'server', 'static', 'dashboard.js');

const schema = JSON.parse(fs.readFileSync(schemaPath, 'utf8'));
const backendTypes = new Set(schema.types);

// Extract the case labels of the wsm onMessage dispatch. The switch is
// located by its `switch (msg.type)` header; case labels must have the quote
// adjacent to `case` so comment prose can never match.
const src = fs.readFileSync(dashboardPath, 'utf8');
const switchStart = src.indexOf('switch (msg.type)');
if (switchStart < 0) {
  console.error('check-ws-contract: dashboard.js has no `switch (msg.type)` dispatch');
  process.exit(1);
}
// Bound the scan to the switch body: from the header to its closing brace,
// tracked with a simple depth counter starting at the first `{` after it.
let i = src.indexOf('{', switchStart);
let depth = 0;
let end = i;
for (; end < src.length; end++) {
  const c = src[end];
  if (c === '{') depth++;
  else if (c === '}') {
    depth--;
    if (depth === 0) break;
  }
}
const body = src.slice(i, end);
const frontendTypes = new Set();
for (const m of body.matchAll(/case\s+['"]([a-z_]+)['"]\s*:/g)) {
  frontendTypes.add(m[1]);
}

let failures = 0;
for (const t of frontendTypes) {
  if (!backendTypes.has(t)) {
    console.error(`frontend dispatches on ${JSON.stringify(t)} but the backend schema does not declare it`);
    failures++;
  }
}
for (const t of backendTypes) {
  if (!frontendTypes.has(t)) {
    console.error(`backend declares ${JSON.stringify(t)} but dashboard.js has no case for it — add a handler or an explicit no-op case`);
    failures++;
  }
}
if (failures) {
  console.error(`check-ws-contract: ${failures} mismatch(es) between wsproto.schema.json and dashboard.js`);
  process.exit(1);
}
console.log(`check-ws-contract: OK (${backendTypes.size} types, front and back agree)`);
