// @ts-check
//
// Playwright globalSetup: do ONE auth/login round-trip and dump the cookie
// state to disk, so all multibackend.test.js cases can rehydrate without
// hitting the per-IP login rate-limit (~5/min/IP). Without this, every
// describe-scoped beforeAll re-logs in and the run trips 429 by case 4.
//
// Activated only when NAOZHI_LIVE_E2E=1 — keeps the regular e2e suite
// (mock-server based) deterministic and offline.

const { request } = require('@playwright/test');
const fs = require('fs');
const path = require('path');

module.exports = async () => {
  if (process.env.NAOZHI_LIVE_E2E !== '1') return;
  const base = process.env.NAOZHI_BASE || 'http://127.0.0.1:8180';
  const token = process.env.NAOZHI_DASHBOARD_TOKEN;
  if (!token) {
    throw new Error('NAOZHI_DASHBOARD_TOKEN env required for live multibackend tests');
  }
  const ctx = await request.newContext({ baseURL: base });
  const resp = await ctx.post('/api/auth/login', {
    data: { token },
    headers: { 'Content-Type': 'application/json' },
  });
  if (!resp.ok()) {
    throw new Error(`auth/login ${resp.status()}: ${await resp.text()}`);
  }
  const state = await ctx.storageState();
  const out = path.join(__dirname, '.multibackend-storage.json');
  fs.writeFileSync(out, JSON.stringify(state));
  await ctx.dispose();
  process.env.NAOZHI_STORAGE_STATE = out;
};
