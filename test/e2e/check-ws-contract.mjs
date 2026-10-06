#!/usr/bin/env node
// check-ws-contract.mjs — the frontend half of the #2535 WS contract: the
// dashboard's wsm.on receive registrations and the backend-generated
// internal/wsproto/wsproto.schema.json must agree on the message-type set.
// Every type the frontend registers must be a type the backend declares, and
// every backend type must have a registration (a deliberately ignored type
// still gets an explicit no-op, like `unsubscribed`). Runs in the lint-js CI
// job next to eslint and the freeze/ratchet checks. The fields a handler reads
// are tsc's (wire.d.ts; scripts/ts-check.test.mjs keeps every frame typed).

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { check as checkReceivers } from '../../scripts/check-ws-receivers.mjs';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..');
const schemaPath = path.join(ROOT, 'internal', 'wsproto', 'wsproto.schema.json');

const schema = JSON.parse(fs.readFileSync(schemaPath, 'utf8'));
const backendTypes = new Set(schema.types);
const staticDir = path.join(ROOT, 'internal', 'server', 'static');
const contractPath = path.join(staticDir, 'contract.js');
const { NZ_CONTRACT } = await import(pathToFileURL(contractPath).href);
const contractKeys = new Set(Object.keys(NZ_CONTRACT.WS));

// The receive side is the wsm.on registrations across static/*.js, read by
// the same AST pass the receiver gate uses (scripts/check-ws-receivers.mjs).
const staticFiles = fs.readdirSync(staticDir)
  .filter((f) => f.endsWith('.js') && f !== 'contract.js' && f !== 'sw.js').sort()
  .map((f) => [f, fs.readFileSync(path.join(staticDir, f), 'utf8')]);
const frontendTypes = new Set(checkReceivers(staticFiles, contractKeys).regs.map((r) => r.key).filter(Boolean));
let failures = 0;
for (const t of frontendTypes) {
  if (!backendTypes.has(t)) {
    console.error(`frontend dispatches on ${JSON.stringify(t)} but the backend schema does not declare it`);
    failures++;
  }
}
for (const t of backendTypes) {
  if (!frontendTypes.has(t)) {
    console.error(`backend declares ${JSON.stringify(t)} but no module registers wsm.on for it — add a handler or an explicit no-op`);
    failures++;
  }
}
// ── Third direction: the SEND side (#2715). The receive side above has
// always had this two-way check; a frame the dashboard SENDS had nothing —
// a typo'd type went over the wire and the backend silently ignored it.
// Send sites now write `type: NZ_CONTRACT.WS.<key>` (the constant is what
// makes "this is a WS frame" machine-recognisable; a bare `type: 'x'` could
// be a Blob MIME or a list-item tag), and every key referenced must be one
// of the inbound types the generator embedded in contract.js.
// Inbound = the contract's WS keys that are not outbound schema types.
const inboundTypes = new Set([...contractKeys].filter((k) => !backendTypes.has(k)));

const sendSites = [];
for (const f of fs.readdirSync(staticDir)) {
  if (!f.endsWith('.js') || f === 'contract.js') continue;
  const js = fs.readFileSync(path.join(staticDir, f), 'utf8');
  for (const m of js.matchAll(/type:\s*NZ_CONTRACT\.WS\.([a-zA-Z_$][\w$]*)/g)) {
    sendSites.push({ file: f, key: m[1] });
  }
}
if (sendSites.length === 0) {
  console.error('check-ws-contract: no NZ_CONTRACT.WS send sites found — the send-side check has gone blind (were the constants renamed?)');
  failures++;
}
for (const { file, key } of sendSites) {
  if (!inboundTypes.has(key)) {
    console.error(`${file} sends NZ_CONTRACT.WS.${key} but ${JSON.stringify(key)} is not an inbound type the backend accepts`);
    failures++;
  }
}

if (failures) {
  console.error(`check-ws-contract: ${failures} mismatch(es) between wsproto.schema.json and dashboard.js`);
  process.exit(1);
}
console.log(`check-ws-contract: OK (${backendTypes.size} types + ${sendSites.length} send sites over ${inboundTypes.size} inbound types)`);
