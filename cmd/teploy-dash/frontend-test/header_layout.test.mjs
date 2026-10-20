// Header layout: the theme choice lives in Settings > Account (not the
// header), and pinned links have a phone home inside the menu panel.
//
// Run: node cmd/teploy-dash/frontend-test/header_layout.test.mjs
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const html = readFileSync(join(here, '../frontend/index.html'), 'utf8');
const css = readFileSync(join(here, '../frontend/css/style.css'), 'utf8');
const failures = [];
const assert = (c, m) => { if (!c) failures.push(m); };

const nav = html.slice(html.indexOf('<nav class="navbar"'), html.indexOf('</nav>'));
assert(!nav.includes('$store.theme'), 'the header must not carry the theme toggle');
const account = html.slice(html.indexOf("tab === 'account'"));
assert(account.includes("$store.theme.set('dark')") && account.includes("$store.theme.set('light')"), 'Settings > Account offers Dark and Light');
const menu = nav.slice(nav.indexOf('id="primary-nav"'));
assert(menu.includes('class="nav-pins"') && menu.includes('x-data="navLinks"'), 'pinned links render inside the menu panel');
assert(/\.nav-pins \{ display: none; \}/.test(css), 'menu pins are hidden by default (desktop shows header icons)');
assert(/@media \(max-width: 640px\)[\s\S]*\.nav-pins \{ display: block;/.test(css), 'menu pins show on phones');

// The theme store keeps toggle() and gains set().
const listeners = [];
globalThis.document = {
  addEventListener: (t, fn) => { if (t === 'alpine:init') listeners.push(fn); },
  getElementById: () => null,
  createElement: () => ({ style: {}, classList: { add() {} }, setAttribute() {}, remove() {}, appendChild() {} }),
  documentElement: { attrs: { 'data-theme': 'dark' }, setAttribute(k, v) { this.attrs[k] = v; }, getAttribute(k) { return this.attrs[k]; } },
};
globalThis.window = { addEventListener() {}, dispatchEvent() {} };
const stored = {};
globalThis.localStorage = { getItem: (k) => stored[k] ?? null, setItem: (k, v) => { stored[k] = v; } };
globalThis.history = { pushState() {}, replaceState() {} };
globalThis.location = { pathname: '/', search: '' };
globalThis.CustomEvent = class {};
const stores = {};
globalThis.Alpine = { data() {}, store: (n, d) => { if (d !== undefined) stores[n] = d; return stores[n]; }, effect() {} };
new Function(readFileSync(join(here, '../frontend/js/app.js'), 'utf8'))();
listeners.forEach((fn) => fn());
stores.theme.set('light');
assert(stores.theme.mode === 'light' && stored['teploy-theme'] === 'light' && document.documentElement.getAttribute('data-theme') === 'light', 'set(light) applies and persists');
stores.theme.set('bogus');
assert(stores.theme.mode === 'dark', 'unknown modes normalize to dark');
stores.theme.toggle();
assert(stores.theme.mode === 'light', 'toggle still works');

if (failures.length) { console.error(`FAIL (${failures.length}):`); failures.forEach((f) => console.error('  - ' + f)); process.exit(1); }
console.log('header_layout.test: all assertions passed');
