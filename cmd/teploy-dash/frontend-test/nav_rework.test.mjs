// Navigation and UI rework (2026-11-11): static checks over the shipped
// frontend that keep the fixed defects fixed.
//
// Run: node cmd/teploy-dash/frontend-test/nav_rework.test.mjs
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const read = (p) => readFileSync(join(here, '../frontend', p), 'utf8');
const html = read('index.html');
const js = read('js/app.js');
const css = read('css/style.css');
const failures = [];
const assert = (c, m) => { if (!c) failures.push(m); };

// Every CSS variable the frontend reads is defined.
{
  const used = new Set([...(html + js + css).matchAll(/var\((--[a-z0-9-]+)/g)].map((m) => m[1]));
  const defined = new Set([...css.matchAll(/^\s*(--[a-z0-9-]+):/gm)].map((m) => m[1]));
  for (const v of used) assert(defined.has(v), `CSS variable ${v} is used but never defined`);
}

// No owner-specific addresses or product names in a public product.
for (const bad of [/100\.108\.\d+\.\d+/, /akiroo/i, /fylun/i, /:49152/]) {
  assert(!bad.test(html) && !bad.test(js), `owner-specific text ${bad} in the frontend`);
}

// Components whose templates read `loading` declare it.
for (const name of ['cleanupPage', 'siblingsPage']) {
  const start = js.indexOf(`Alpine.data('${name}'`);
  assert(start >= 0 && /loading: true/.test(js.slice(start, start + 300)), `${name} declares loading`);
}

// Reads that used to throw on a missing object are guarded.
assert(!html.includes("x-text=\"'dismissed by ' + f.dismissed.actor\""), 'dismissed-by read is guarded');
assert(!/:href="c\.doc\.dashboard_url"/.test(html), 'sibling dashboard_url read is guarded');

// Checkboxes inside form fields are not stretched into text-input boxes.
assert(/\.field input\[type="checkbox"\][^{]*\{[^}]*width: auto/.test(css), 'field checkboxes keep their natural size');

// Labels: common intervals read as words; an ok cleanup audit says Audited.
{
  const listeners = [];
  globalThis.document = { addEventListener: (t, fn) => { if (t === 'alpine:init') listeners.push(fn); }, getElementById: () => null, documentElement: { setAttribute() {}, getAttribute: () => 'dark' } };
  globalThis.window = { addEventListener() {}, dispatchEvent() {} };
  globalThis.localStorage = { getItem: () => null, setItem() {} };
  globalThis.history = { pushState() {}, replaceState() {} };
  globalThis.location = { pathname: '/', search: '' };
  globalThis.CustomEvent = class {};
  const comps = {}, stores = {};
  globalThis.Alpine = { data: (n, f) => { comps[n] = f; }, store: (n, d) => { if (d !== undefined) stores[n] = d; return stores[n]; }, effect() {} };
  new Function(js + '\n;globalThis.__fi = formatInterval;')();
  listeners.forEach((fn) => fn());
  const fi = globalThis.__fi;
  assert(fi(1) === 'hourly' && fi(24) === 'daily' && fi(168) === 'weekly' && fi(6) === 'every 6 h' && fi(48) === 'every 2 days', 'formatInterval names common intervals');
  assert(!html.includes("'every ' + t.interval_hours + 'h'"), 'restore tests use formatInterval');
  const cleanup = comps.cleanupPage();
  assert(cleanup.auditLabel('ok', { warning: 2 }) === 'Audited — 2 warnings', 'ok audit with warnings says Audited and the count');
  assert(cleanup.auditLabel('ok', {}) === 'Audited — no warnings', 'ok audit without warnings');
  assert(!js.includes('Clean bill'), 'no "Clean bill" label');
  assert(html.includes('>History</button>') && html.includes('id="app-panel-history"'), 'the app tab reads History');
  assert(html.includes('<h1 class="page-title">Uptime</h1>'), 'monitors page title matches its tab');
}

if (failures.length) { console.error(`FAIL (${failures.length}):`); failures.forEach((f) => console.error('  - ' + f)); process.exit(1); }
console.log('nav_rework.test: all assertions passed');
