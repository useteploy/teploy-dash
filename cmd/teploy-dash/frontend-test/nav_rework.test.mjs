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

// Home is the status page: needs attention, running now, servers, then the
// shortcut grid; the protect banner is a row of the status, not a banner.
{
  const home = html.slice(html.indexOf("x-data=\"homepagePage\""), html.indexOf('Projects Page'));
  for (const id of ['home-attention-title', 'home-running-title', 'home-servers-title']) assert(home.includes(`id="${id}"`), `home section ${id}`);
  assert(home.includes('x-data="homeStatus"') && home.includes('>Shortcuts</h2>'), 'home keeps the shortcut grid below the status');
  assert(home.indexOf('homeStatus') < home.indexOf('>Shortcuts</h2>'), 'status comes before shortcuts');
  assert(!html.includes('x-data="protectBanner"'), 'the protect banner is a status row now');
  assert(home.includes('unchecked()') && home.includes('Nothing needs attention'), 'a failed source is named, never read as nothing wrong');
}

// One confirm dialog: no browser confirm() or prompt() is left.
{
  const strip = (src) => src.replace(/\/\/.*$/gm, '').replace(/<!--[\s\S]*?-->/g, '');
  for (const [name, src] of [['js/app.js', js], ['index.html', html], ['js/operations.js', read('js/operations.js')]]) {
    assert(!/(^|[^\w.$])confirm\(/.test(strip(src)), `${name} still calls confirm()`);
    assert(!/(^|[^\w.$])prompt\(/.test(strip(src)), `${name} still calls prompt()`);
  }
  assert(html.includes('aria-labelledby="confirm-dialog-title"') && html.includes('$store.dialog.canSubmit()'), 'the shared dialog is rendered once');
  const general = html.slice(html.indexOf('id="app-panel-general"'), html.indexOf('id="app-panel-env"'));
  const buttons = general.slice(general.indexOf('class="btn-group"'), general.indexOf('</div>', general.indexOf('class="btn-group"')));
  assert(!buttons.includes('removeApp()'), 'Remove is not beside Restart');
  assert(general.includes('class="danger-zone"') && general.lastIndexOf('removeApp()') > general.indexOf('class="danger-zone"'), 'Remove sits in the Danger section');
  for (const m of html.matchAll(/<button[^>]*>&times;<\/button>/g)) assert(m[0].includes(':aria-label='), 'every x button is named: ' + m[0].slice(0, 80));
  assert((html.match(/:aria-label="'Actions for ' \+/g) || []).length === 3, 'card action menus are named');

  const listeners = [];
  globalThis.document = { addEventListener: (t, fn) => { if (t === 'alpine:init') listeners.push(fn); }, getElementById: () => null, documentElement: { setAttribute() {}, getAttribute: () => 'dark' } };
  const stores = {};
  globalThis.Alpine = { data() {}, store: (n, d) => { if (d !== undefined) stores[n] = d; return stores[n]; }, effect() {} };
  new Function(js + '\n;globalThis.__q = questionDialog; globalThis.__ask = askConfirm; globalThis.__form = askForm;')();
  listeners.forEach((fn) => fn());
  const q = globalThis.__q('Remove secret K?\n\nContainers keep it.');
  assert(q.title === 'Remove secret K?' && q.message === 'Containers keep it.' && q.danger && q.confirmLabel === 'Remove', 'a question splits into title, message and a verb label');
  const d = stores.dialog;
  const typed = globalThis.__ask({ title: 'Remove server x?', typed: 'x' });
  assert(!d.canSubmit(), 'a typed-name dialog starts disabled');
  d.typedValue = 'y'; assert(!d.canSubmit(), 'the wrong name keeps it disabled');
  d.typedValue = 'x'; d.submit();
  const form = globalThis.__form({ title: 'Rename', fields: [{ key: 'name', label: 'Name', value: 'a', required: true }] });
  d.values.name = 'b'; d.submit();
  const cancelled = globalThis.__ask('Delete it?');
  d.cancel();
  Promise.all([typed, form, cancelled]).then(([t, f, c]) => {
    assert(t === true && f && f.name === 'b' && c === false, 'dialogs resolve confirm, form values and cancel');
    finish();
  });
}

// One date helper: no locale date strings in templates; times go through
// x-time or relTime with the exact time in a title.
{
  assert(!/toLocale(Date)?String\(\)/.test(html), 'templates format no dates by hand');
  assert(js.includes("Alpine.directive('time'"), 'the x-time directive exists');
  assert(!/new Date\(d\)\.toLocaleString\(\)/.test(js), 'components delegate formatDate to relTime');
  const ageCopies = (js.match(/ageLabel\(seconds\) \{\n\s+return seconds === undefined \|\| seconds === null \? '(never|not checked)' : ageText\(seconds\);/g) || []).length;
  assert(ageCopies === 4, 'every ageLabel delegates to ageText (' + ageCopies + ')');
}

// Monitor and operation dots show the current state.
{
  assert(js.includes('monitorState(m)') && js.includes("c.status === 'up' ? 'up' : 'down'"), 'the monitor dot reads the newest check');
  assert(!/uptime_percent >= 99 \? 'green'/.test(js), 'the dot no longer derives from 24h uptime');
  assert(!html.includes("running: op.status === 'running' || op.status === 'succeeded'"), 'a running operation is not painted like a succeeded one');
}

// Phone layout: wide tables stack, status cards stay two-up, tab strips hint.
{
  assert(/@media \(max-width: 640px\) \{\s*\.table-wrap > table\.stack-table \{ display: block;/.test(css), 'stack-table rows become blocks on phones');
  for (const marker of ['<th>What is wrong</th>', '<h2>Every target</h2>', '<!-- Accessories Table -->', '<th>Added by</th>']) {
    const at = html.indexOf(marker);
    assert(/<table class="stack-table"/.test(html.slice(Math.max(0, at - 1500), at + 1500)), `the table near ${marker} stacks on phones`);
  }
  assert(html.includes('class="stack-problem"') && html.includes("'stack-empty'"), 'the problem text leads a stacked row and empty ones drop');
  assert(!/@media \(max-width: 400px\) \{\s*\.stats-grid, \.status-grid/.test(css), 'status cards stay two columns at phone width');
  assert((html.match(/class="tabs[^"]*" x-scroll-hint/g) || []).length >= 4 && html.includes('class="hub-tabs" aria-label="Section pages" x-scroll-hint'), 'tab strips carry the scroll hint');
  assert(js.includes("Alpine.directive('scroll-hint'") && css.includes('.more-right {'), 'the scroll hint fades the hidden side');
}

function finish() {
if (failures.length) { console.error(`FAIL (${failures.length}):`); failures.forEach((f) => console.error('  - ' + f)); process.exit(1); }
console.log('nav_rework.test: all assertions passed');
}
