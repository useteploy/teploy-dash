// Component test for the app-detail accessories panel's D05 database-action
// slice: distinct stop/start confirmations with their blast radius, and the
// server-owned action inventory (support status + remedy) rendering.
//
// Repo convention (no build step, no CI Node job, zero dependencies): a
// plain node script that loads js/app.js under stubbed browser globals,
// captures the Alpine factories, and drives appDetailPage against stubbed
// API responses.
//
// Run: node cmd/teploy-dash/frontend-test/db_actions.test.mjs
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const failures = [];
function assert(cond, message) {
  if (!cond) failures.push(message);
}

const alpineInitListeners = [];
let lastToast = null;
let lastConfirm = null;
let confirmAnswer = true;
globalThis.document = {
  addEventListener: (type, fn) => { if (type === 'alpine:init') alpineInitListeners.push(fn); },
  getElementById: () => ({ style: {}, appendChild: (el) => { lastToast = el.textContent; } }),
  createElement: () => ({ style: {}, classList: { add() {}, remove() {} }, setAttribute() {}, remove() {}, appendChild() {} }),
  documentElement: { setAttribute() {}, getAttribute: () => 'dark' },
};
globalThis.window = { addEventListener() {}, dispatchEvent() {} };
globalThis.localStorage = { getItem: () => null, setItem() {} };
globalThis.history = { pushState() {}, replaceState() {} };
globalThis.location = { pathname: '/apps/prod/web', search: '' };
globalThis.CustomEvent = class CustomEvent {};
globalThis.confirm = (msg) => { lastConfirm = msg; return confirmAnswer; };

let apiResponses = {};
let posted = [];
globalThis.fetch = async (url, init) => {
  // POSTs are recorded even when their route is not stubbed: what the
  // component SUBMITS is the assertion target, not the stub's answer.
  // (Body-less POSTs — like the accessory actions — parse to null.)
  if (init && init.method === 'POST') posted.push({ url, body: init.body ? JSON.parse(init.body) : null });
  const route = apiResponses[url];
  if (route === undefined) {
    return { status: 404, ok: false, headers: new Headers({ 'Content-Type': 'text/plain' }), text: async () => 'not found' };
  }
  return {
    status: 200,
    ok: true,
    headers: new Headers({ 'Content-Type': 'application/json' }),
    text: async () => JSON.stringify({ data: route }),
  };
};

const components = {};
globalThis.Alpine = {
  data: (name, factory) => { components[name] = factory; },
  store: () => ({ navigate() {}, restore() {}, params: { server: 'prod', name: 'web' }, page: 'app-detail' }),
  effect: (fn) => fn(),
};

new Function(readFileSync(join(here, '../frontend/js/app.js'), 'utf8'))();
alpineInitListeners.forEach((fn) => fn());
if (!components.appDetailPage) {
  console.error('FAIL: appDetailPage component was not registered');
  process.exit(1);
}

function freshPage() {
  const page = components.appDetailPage();
  page.resource = { server: 'prod', name: 'web' };
  page.app = { name: 'web', server: 'prod' };
  page.appPath = () => '/apps/prod/web';
  return page;
}

// 1. Stop and start are DISTINCT confirmations, each naming the accessory
//    and its own blast radius — not a generic "are you sure".
{
  const page = freshPage();
  apiResponses = { '/apps/prod/web/accessories': [] };

  await page.accessoryAction({ id: 'c1', name: 'web-db', state: 'running' }, 'stop');
  assert(lastConfirm && lastConfirm.startsWith('Stop database db on prod'), `stop confirm = ${lastConfirm}`);
  assert(lastConfirm && lastConfirm.includes('lose their data connection'), 'stop confirm must state the data-connection blast radius');
  assert(posted.some((p) => p.url === '/apps/prod/web/accessories/db/stop'), 'stop must post the accessory-scoped action');

  await page.accessoryAction({ id: 'c1', name: 'web-db', state: 'stopped' }, 'start');
  assert(lastConfirm && lastConfirm.startsWith('Start database db on prod'), `start confirm = ${lastConfirm}`);
  assert(lastConfirm && lastConfirm.includes('No data is changed'), 'start confirm must state its own (lesser) blast radius');
  assert(posted.some((p) => p.url === '/apps/prod/web/accessories/db/start'), 'start must post the accessory-scoped action');
}

// 2. A declined confirmation submits nothing.
{
  const page = freshPage();
  apiResponses = { '/apps/prod/web/accessories': [] };
  posted = [];
  confirmAnswer = false;
  await page.accessoryAction({ id: 'c1', name: 'web-db' }, 'stop');
  assert(posted.length === 0, 'a declined confirmation must not submit');
  confirmAnswer = true;
}

// 3. The inventory panel renders the SERVER's classes: it fetches
//    /db-actions once and exposes support status + remedy to the template.
{
  const page = freshPage();
  apiResponses = {
    '/apps/prod/web/db-actions': {
      actions: [
        { id: 'restart', label: 'Restart', supported: false, blast_radius: 'b', remedy: 'run stop then start' },
        { id: 'version-upgrade', label: 'Version upgrade', supported: false, blast_radius: 'b2', remedy: 'from the app directory' },
      ],
    },
  };
  await page.toggleDbActions({ id: 'c1', name: 'web-db' });
  assert(page.dbActionsFor && page.dbActionsFor.id === 'c1', 'panel must track the selected accessory');
  assert(page.dbActions.length === 2 && page.dbActions[0].id === 'restart', 'inventory must come from the server response');
  assert(page.dbActions[1].remedy === 'from the app directory', 'unsupported entries must carry their remedy');

  // Toggling the same row closes the panel without refetching.
  posted = [];
  await page.toggleDbActions({ id: 'c1', name: 'web-db' });
  assert(page.dbActionsFor === null, 'second toggle must close the panel');
}

// 4. Static tripwire: the accessories table wires the distinct actions and
//    renders the inventory's support status and remedies (a detached
//    template must not pass 1-3).
{
  const html = readFileSync(join(here, '../frontend/index.html'), 'utf8');
  const accStart = html.indexOf('Accessories Table');
  const accEnd = html.indexOf('Environment Tab');
  const section = html.slice(accStart, accEnd);
  assert(accStart !== -1 && accEnd > accStart, 'accessories section not found in index.html');
  assert(section.includes('accessoryAction(a, \'stop\')') && section.includes('accessoryAction(a, \'start\')'), 'accessory rows must use the distinct-confirmation actions');
  assert(section.includes('toggleDbActions(a)'), 'accessory rows must open the db-actions inventory');
  assert(section.includes('act.supported ? \'supported\' : \'not available from dash\''), 'inventory must render support status truthfully');
  assert(section.includes('act.remedy'), 'inventory must render the remedy for unsupported classes');
}

if (failures.length) {
  console.error(`FAIL (${failures.length}):`);
  for (const f of failures) console.error('  - ' + f);
  process.exit(1);
}
console.log('db_actions.test: all assertions passed');
