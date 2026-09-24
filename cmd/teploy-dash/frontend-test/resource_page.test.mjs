// Component test for the D07 app-resource exemplar and the D08 alert
// delivery surface.
//
// App resource page: the general tab's overview derives from the status
// payload — observation age (never a health claim), the most recent change
// line, partial-observation errors rendered instead of dropped.
//
// Monitors: delivery status helpers classify the outbox's per-monitor state
// (pending / retrying / dead-lettered) and only failure-ish states warn.
//
// Run: node cmd/teploy-dash/frontend-test/resource_page.test.mjs
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const failures = [];
function assert(cond, message) {
  if (!cond) failures.push(message);
}

const alpineInitListeners = [];
globalThis.document = {
  addEventListener: (type, fn) => { if (type === 'alpine:init') alpineInitListeners.push(fn); },
  getElementById: () => null,
  createElement: () => ({ style: {}, classList: { add() {} }, setAttribute() {}, remove() {}, appendChild() {} }),
  documentElement: { setAttribute() {}, getAttribute: () => 'dark' },
};
globalThis.window = { addEventListener() {}, dispatchEvent() {} };
globalThis.localStorage = { getItem: () => null, setItem() {} };
globalThis.history = { pushState() {}, replaceState() {} };
globalThis.location = { pathname: '/deployments/prod/web', search: '' };
globalThis.CustomEvent = class CustomEvent {};

let apiResponses = {};
globalThis.fetch = async (url) => {
  const route = apiResponses[url];
  if (route === undefined) {
    return { status: 404, ok: false, headers: new Headers({ 'Content-Type': 'text/plain' }), text: async () => 'not found' };
  }
  if (typeof route === 'function') return route();
  return {
    status: 200,
    ok: true,
    headers: new Headers({ 'Content-Type': 'application/json' }),
    text: async () => JSON.stringify(route),
  };
};

const components = {};
const stores = {};
globalThis.Alpine = {
  data: (name, factory) => { components[name] = factory; },
  store: (name, def) => { if (def !== undefined) stores[name] = def; return stores[name]; },
  effect: () => {},
};
stores.router = { page: 'app-detail', params: { server: 'prod', name: 'web' }, navigate() {} };

new Function(readFileSync(join(here, '../frontend/js/app.js'), 'utf8'))();
alpineInitListeners.forEach((fn) => fn());

// 1. Observation age: derived from observed_at, never negative, absent when
//    the server did not stamp one.
{
  const page = components.appDetailPage();
  const minutesAgo = (m) => new Date(Date.now() - m * 60000).toISOString();

  page.app = { observed_at: minutesAgo(4), deployed_at: minutesAgo(90) };
  const age = page.observedAge();
  assert(age === '4m ago', `observedAge = ${age}, want "4m ago"`);
  assert(page.lastChangeWhen().length > 0, 'lastChangeWhen renders a timestamp');

  page.app = { observed_at: '0001-01-01T00:00:00Z' };
  assert(page.observedAge() === '', 'zero observed_at must not render an age');
  assert(page.observedWhen() === 'unknown', 'zero observed_at renders unknown');
  assert(page.lastChangeWhen() === 'not recorded', 'absent deployed_at says not recorded, not a guess');

  page.app = { observed_at: new Date(Date.now() + 60000).toISOString() };
  assert(page.observedAge() === '', 'a future timestamp must not render a negative age');

  // shortHash trims release ids for the "was <previous>" line.
  assert(page.shortHash('abcdef1234567890') === 'abcdef123456', 'shortHash trims to 12');
  assert(page.shortHash('') === '', 'shortHash of empty stays empty');
}

// 2. Partial observation errors are data the page must show, not drop.
{
  const html = readFileSync(join(here, '../frontend/index.html'), 'utf8');
  const appStart = html.indexOf('App Detail Page');
  const appEnd = html.indexOf('Monitors Page');
  const section = html.slice(appStart, appEnd);
  assert(appStart !== -1 && appEnd > appStart, 'app detail section not found');
  assert(section.includes('app.errors'), 'general tab must render the observation-errors block');
  assert(section.includes('Partial observation'), 'the partial-observation alert is labeled');
  assert(section.includes('observedAge()'), 'the overview names the observation age');
  assert(section.includes('lastChangeWhen()'), 'the overview names the recent change');
  assert(section.includes('shortHash(app.previous_hash)'), 'the overview names the previous release');
  // Recovery row: history, logs, restore tests, and configuration pointers.
  for (const marker of ['Deploy history', 'Live logs', 'Restore tests', 'environment', 'KV']) {
    assert(section.includes(marker), `recovery row must link ${marker}`);
  }
  // The recovery links navigate/switch rather than href off-page.
  assert(section.includes('openRestoreTests()'), 'restore-tests link drives the router');
}

// 3. Alert delivery classification (D08): only failure-ish states warn.
{
  const page = components.monitorsPage();
  page._alive = true;

  assert(page.deliveryWarns(null) === false, 'no delivery state must not warn');
  assert(page.deliveryLabel(null) === '', 'no delivery state has no label');
  assert(page.deliveryWarns({ status: 'delivered', attempts: 1 }) === false, 'delivered is history, not a warning');
  assert(page.deliveryWarns({ status: 'pending', attempts: 0 }) === false, 'first pending attempt is not yet a failure');
  assert(page.deliveryWarns({ status: 'pending', attempts: 2 }) === true, 'a retrying delivery warns');
  assert(page.deliveryWarns({ status: 'dead_lettered', attempts: 5 }) === true, 'a dead-lettered delivery warns');
  assert(page.deliveryLabel({ status: 'pending', attempts: 2 }).includes('attempt 2'), 'retrying label names the attempt');
  assert(page.deliveryLabel({ status: 'dead_lettered', attempts: 5 }).includes('dead-lettered'), 'dead-letter label names it');

  // And the templates render both surfaces.
  const html = readFileSync(join(here, '../frontend/index.html'), 'utf8');
  const monStart = html.indexOf('Monitors Page');
  const monEnd = html.indexOf('Restore Tests Page');
  const monSection = html.slice(monStart, monEnd);
  assert(monStart !== -1 && monEnd > monStart, 'monitors section not found');
  assert(monSection.includes('deliveryWarns(m.delivery)'), 'monitor list renders the delivery warning chip');
  assert(monSection.includes('deliveryLabel(m.delivery)'), 'monitor list renders the delivery label');
  assert(monSection.includes('selectedMonitor.delivery.last_error'), 'monitor detail shows the exact last failure');
  assert(monSection.includes('Alert delivery'), 'monitor detail has an alert delivery section');
}

if (failures.length) {
  console.error(`FAIL (${failures.length}):`);
  for (const f of failures) console.error('  - ' + f);
  process.exit(1);
}
console.log('resource_page.test: all assertions passed');
