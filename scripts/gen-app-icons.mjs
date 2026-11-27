#!/usr/bin/env node
// Draws the >_naozhi app icon and writes every variant the dashboard serves:
//   internal/server/static/favicon.svg             browser tab (full tile)
//   internal/server/static/icons/icon-{192,512}.png  manifest "any": macOS
//       layout, the tile at 824/1024 with a drop shadow in the margin, since
//       Chrome on macOS puts an "any" icon in the Dock unchanged
//   internal/server/static/icons/maskable-512.png    manifest "maskable":
//       full bleed, the glyph inside the 80% safe zone
//   internal/server/static/icons/apple-touch-icon.png  Safari "Add to Dock" /
//       iOS home screen: opaque 180px square, Safari applies its own mask
// Needs rsvg-convert (brew install librsvg). Run: node scripts/gen-app-icons.mjs
import { execFileSync } from 'node:child_process';
import { mkdirSync, writeFileSync, mkdtempSync, rmSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { tmpdir } from 'node:os';
import { fileURLToPath } from 'node:url';

const root = join(dirname(fileURLToPath(import.meta.url)), '..');
const staticDir = join(root, 'internal/server/static');
const iconDir = join(staticDir, 'icons');

// Superellipse |x|^5 + |y|^5 = 1: close to the continuous-corner squircle of
// macOS app icons, which a plain rounded rect is visibly not at Dock size.
function squircle(x0, y0, s, n = 5, steps = 256) {
  const c = s / 2;
  const pts = [];
  for (let i = 0; i < steps; i++) {
    const t = (2 * Math.PI * i) / steps;
    const ct = Math.cos(t), st = Math.sin(t);
    const x = c + c * Math.sign(ct) * Math.abs(ct) ** (2 / n);
    const y = c + c * Math.sign(st) * Math.abs(st) ** (2 / n);
    pts.push(`${(x0 + x).toFixed(1)} ${(y0 + y).toFixed(1)}`);
  }
  return `M${pts.join('L')}Z`;
}

// The artwork on a 1024 full-bleed tile: a dark slate ground, a warm ">"
// and a cream "_" cursor, each with a soft glow.
const defs = `
  <linearGradient id="bg" x1="0" y1="0" x2="1" y2="1">
    <stop offset="0" stop-color="#2a3442"/>
    <stop offset="0.55" stop-color="#151b24"/>
    <stop offset="1" stop-color="#0a0d12"/>
  </linearGradient>
  <radialGradient id="halo" cx="0.42" cy="0.5" r="0.55">
    <stop offset="0" stop-color="#d97757" stop-opacity="0.22"/>
    <stop offset="1" stop-color="#d97757" stop-opacity="0"/>
  </radialGradient>
  <linearGradient id="sheen" x1="0" y1="0" x2="0" y2="1">
    <stop offset="0" stop-color="#ffffff" stop-opacity="0.10"/>
    <stop offset="0.5" stop-color="#ffffff" stop-opacity="0"/>
  </linearGradient>
  <linearGradient id="chev" x1="0" y1="0" x2="0" y2="1">
    <stop offset="0" stop-color="#f6ad8b"/>
    <stop offset="0.5" stop-color="#e0825f"/>
    <stop offset="1" stop-color="#c2603f"/>
  </linearGradient>
  <linearGradient id="cur" x1="0" y1="0" x2="0" y2="1">
    <stop offset="0" stop-color="#fff4ea"/>
    <stop offset="1" stop-color="#f2d9c4"/>
  </linearGradient>
  <filter id="glow" x="-50%" y="-50%" width="200%" height="200%">
    <feGaussianBlur stdDeviation="22"/>
  </filter>`;

const chevron = 'M300 330 L530 512 L300 694';
const cursor = { x: 590, y: 642, w: 190, h: 90, r: 45 };

function art(withSheen) {
  return `
  <rect width="1024" height="1024" fill="url(#bg)"/>
  <rect width="1024" height="1024" fill="url(#halo)"/>
  ${withSheen ? '<rect width="1024" height="1024" fill="url(#sheen)"/>' : ''}
  <g filter="url(#glow)" opacity="0.55">
    <path d="${chevron}" stroke="#e0825f" stroke-width="108" stroke-linecap="round" stroke-linejoin="round" fill="none"/>
    <rect x="${cursor.x}" y="${cursor.y}" width="${cursor.w}" height="${cursor.h}" rx="${cursor.r}" fill="#f2d9c4"/>
  </g>
  <path d="${chevron}" stroke="url(#chev)" stroke-width="108" stroke-linecap="round" stroke-linejoin="round" fill="none"/>
  <rect x="${cursor.x}" y="${cursor.y}" width="${cursor.w}" height="${cursor.h}" rx="${cursor.r}" fill="url(#cur)"/>`;
}

const tile = squircle(0, 0, 1024);

// Full tile clipped to the squircle, with a hairline edge: the tab favicon.
const faviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1024 1024">
<defs>${defs}
  <clipPath id="clip"><path d="${tile}"/></clipPath>
</defs>
<g clip-path="url(#clip)">${art(true)}</g>
<path d="${tile}" fill="none" stroke="#ffffff" stroke-opacity="0.10" stroke-width="8"/>
</svg>
`;

// macOS layout (Apple's 1024 grid: 824 tile, 100 margin, shadow below).
const s = 824 / 1024;
const macSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1024 1024">
<defs>${defs}
  <clipPath id="clip"><path d="${tile}"/></clipPath>
  <filter id="shadow" x="-20%" y="-20%" width="140%" height="140%">
    <feGaussianBlur stdDeviation="14"/>
  </filter>
</defs>
<g transform="translate(100 112) scale(${s})" filter="url(#shadow)" opacity="0.45">
  <path d="${tile}" fill="#000"/>
</g>
<g transform="translate(100 100) scale(${s})">
  <g clip-path="url(#clip)">${art(true)}</g>
  <path d="${tile}" fill="none" stroke="#ffffff" stroke-opacity="0.10" stroke-width="8"/>
</g>
</svg>
`;

// Square full bleed: the platform supplies the shape.
const bleedSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1024 1024">
<defs>${defs}</defs>
${art(false)}
</svg>
`;

mkdirSync(iconDir, { recursive: true });
writeFileSync(join(staticDir, 'favicon.svg'), faviconSVG);

const tmp = mkdtempSync(join(tmpdir(), 'nzicon-'));
try {
  const mac = join(tmp, 'mac.svg');
  const bleed = join(tmp, 'bleed.svg');
  writeFileSync(mac, macSVG);
  writeFileSync(bleed, bleedSVG);
  const render = (src, size, out) =>
    execFileSync('rsvg-convert', ['-w', String(size), '-h', String(size), '-o', join(iconDir, out), src]);
  render(mac, 192, 'icon-192.png');
  render(mac, 512, 'icon-512.png');
  render(bleed, 512, 'maskable-512.png');
  render(bleed, 180, 'apple-touch-icon.png');
} finally {
  rmSync(tmp, { recursive: true, force: true });
}
