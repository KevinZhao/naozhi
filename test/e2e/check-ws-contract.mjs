#!/usr/bin/env node
// check-ws-contract.mjs — the frontend half of the #2535 WS contract: the
// dashboard's wsm.on receive registrations and the backend-generated
// internal/wsproto/wsproto.schema.json must agree on the message-type set.
// Every type the frontend registers must be a type the backend declares, and
// every backend type must have a registration (a deliberately ignored type
// still gets an explicit no-op, like `unsubscribed`). Runs in the lint-js CI
// job next to eslint and the freeze/ratchet checks.

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { stripCommentsAndStrings } from '../../scripts/js-deps-freeze.mjs';
import { checkNested } from '../../scripts/ws-contract-nested.mjs';
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

// ── Fields: every field the dashboard reads off a frame must be a property
// some frame declares. The type checks above catch a renamed type; this one
// catches a renamed or dropped field, which the frontend would otherwise read
// as undefined without a sound. Every WS handler takes the frame as `msg`, so
// the scan is `msg.<name>` reads; writes (`msg.after = …` on an outgoing
// subscribe) are not reads of the wire.
const schemaFields = new Set();
for (const f of Object.values(schema.frames)) {
  for (const k of Object.keys(f.properties || {})) schemaFields.add(k);
}
// Fields the dashboard adds itself before handing a frame on, with where.
const FRONTEND_FIELDS = {
  // dashboard.js maps a run frame's owner_id onto job_id for the cron bus.
  job_id: 'cron:run-started / cron:run-ended bus detail',
};
const fieldReads = new Map(); // field -> Set(file)
for (const f of fs.readdirSync(staticDir)) {
  if (!f.endsWith('.js') || f === 'contract.js' || f === 'sw.js') continue;
  const js = stripCommentsAndStrings(fs.readFileSync(path.join(staticDir, f), 'utf8'));
  for (const m of js.matchAll(/\bmsg\.([A-Za-z_$][\w$]*)(?![\w$])(?!\s*=[^=])/g)) {
    if (!fieldReads.has(m[1])) fieldReads.set(m[1], new Set());
    fieldReads.get(m[1]).add(f);
  }
}
if (fieldReads.size === 0) {
  console.error('check-ws-contract: no msg.<field> reads found — the field check has gone blind (was the handler parameter renamed?)');
  failures++;
}
for (const [field, files] of fieldReads) {
  if (schemaFields.has(field) || field in FRONTEND_FIELDS) continue;
  console.error(`${[...files].join(', ')} read${files.size === 1 ? 's' : ''} msg.${field}, which no frame in wsproto.schema.json declares`);
  failures++;
}
for (const field of Object.keys(FRONTEND_FIELDS)) {
  if (!fieldReads.has(field)) {
    console.error(`FRONTEND_FIELDS lists ${field} but nothing reads msg.${field} any more — drop the entry`);
    failures++;
  } else if (schemaFields.has(field)) {
    console.error(`FRONTEND_FIELDS lists ${field} but the schema now declares it — drop the entry`);
    failures++;
  }
}

// ── Nested fields (#2909): see scripts/ws-contract-nested.mjs. Typed
// parameters resolve against the WS defs and the REST response schemas' defs.
const REST_SCHEMAS = [path.join(ROOT, 'internal', 'dashboard', 'session', 'testdata', 'rest.schema.json')];
const defs = { ...(schema.defs || {}) };
for (const p of REST_SCHEMAS) Object.assign(defs, JSON.parse(fs.readFileSync(p, 'utf8')).defs);
const typedSchema = { ...schema, defs };
// Fields the dashboard adds to a backend struct itself, with where.
const FRONTEND_STRUCT_FIELDS = {
  'sessionview.SessionSnapshot': {
    source: "renderSidebar marks a card 'managed' or 'terminal' (a discovered CLI session)",
    type_label: 'renderSidebar copies a discovered session\'s type-chip label (backend.Profile.TerminalLabel) onto its card',
  },
};
const usedStructExtras = new Set();
let nestedReads = 0;
let typedFns = 0;
for (const f of fs.readdirSync(staticDir)) {
  if (!f.endsWith('.js') || f === 'contract.js' || f === 'sw.js') continue;
  const r = checkNested(fs.readFileSync(path.join(staticDir, f), 'utf8'), typedSchema, FRONTEND_STRUCT_FIELDS);
  for (const u of r.usedExtras) usedStructExtras.add(u);
  nestedReads += r.reads;
  typedFns += r.typedFns;
  for (const p of r.problems) {
    console.error(`${f}:${p}`);
    failures++;
  }
}
if (!defs['clievent.EventEntry']) {
  console.error('check-ws-contract: the schema has no defs for clievent.EventEntry — regenerate it (go generate ./internal/wsproto)');
  failures++;
}
for (const [def, fields] of Object.entries(FRONTEND_STRUCT_FIELDS)) {
  for (const field of Object.keys(fields)) {
    if (!usedStructExtras.has(def + '.' + field)) {
      console.error(`FRONTEND_STRUCT_FIELDS lists ${def}.${field} but no typed function reads it any more — drop the entry`);
      failures++;
    } else if (defs[def]?.properties?.[field]) {
      console.error(`FRONTEND_STRUCT_FIELDS lists ${def}.${field} but the schema now declares it — drop the entry`);
      failures++;
    }
  }
}
if (typedFns === 0) {
  console.error('check-ws-contract: no function types a parameter — the nested-field check has gone blind');
  failures++;
}

if (failures) {
  console.error(`check-ws-contract: ${failures} mismatch(es) between wsproto.schema.json and dashboard.js`);
  process.exit(1);
}
console.log(`check-ws-contract: OK (${backendTypes.size} types + ${sendSites.length} send sites over ${inboundTypes.size} inbound types + ${fieldReads.size} frame fields read + ${nestedReads} nested reads in ${typedFns} typed functions)`);
