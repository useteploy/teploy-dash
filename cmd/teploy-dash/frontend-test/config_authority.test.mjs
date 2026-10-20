// Component test for the Teploy-home authority indicators: the env tab's
// settings note, the inherited-vs-local key badges, and the read-only
// treatment of keys the app's Teploy settings declares (never a silent
// competing edit). The git/dash-managed manifest surfaces were removed.
//
// Run: node cmd/teploy-dash/frontend-test/config_authority.test.mjs
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const failures = [];
function assert(cond, message) {
  if (!cond) failures.push(message);
}

const alpineInitListeners = [];
const toasts = [];
globalThis.document = {
  addEventListener: (type, fn) => { if (type === 'alpine:init') alpineInitListeners.push(fn); },
  getElementById: (id) => (id === 'toast-container'
    ? { appendChild: (el) => toasts.push(el.textContent || '') }
    : null),
  createElement: () => ({ style: {}, classList: { add() {} }, setAttribute() {}, remove() {}, appendChild() {} }),
  documentElement: { setAttribute() {}, getAttribute: () => 'dark' },
};
globalThis.window = { addEventListener() {}, dispatchEvent() {} };
globalThis.localStorage = { getItem: () => null, setItem() {} };
globalThis.history = { pushState() {}, replaceState() {} };
globalThis.location = { pathname: '/deployments/prod/web', search: '' };
globalThis.CustomEvent = class CustomEvent {};
globalThis.EventSource = class EventSource { constructor() {} addEventListener() {} close() {} };

globalThis.fetch = async () => ({ status: 404, ok: false, headers: new Headers(), text: async () => '' });
let posted = 0;
globalThis.fetchCount = 0;

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

// 1. Teploy-home classification helpers.
{
  const page = components.appDetailPage();

  // No config home (older CLI or read failure): no indicators, keys local.
  page.configAuthority = null;
  assert(page.teployHome() === false, 'absent authority is not Teploy-home');
  assert(page.envKeySource('A') === 'local', 'unknown authority labels keys local');

  // Folder home: everything local.
  page.configAuthority = { home: 'folder', declared_env_keys: ['RAILS_ENV'] };
  assert(page.teployHome() === false, 'folder home is not Teploy-home');
  assert(page.envKeySource('RAILS_ENV') === 'local', 'folder-home keys are local');

  // Teploy home: declared keys belong to the settings.
  page.configAuthority = { home: 'teploy', declared_env_keys: ['RAILS_ENV', 'LOG_LEVEL'] };
  assert(page.teployHome() === true, 'Teploy home recognized');
  assert(page.envKeySource('RAILS_ENV') === 'settings', 'declared key is from settings');
  assert(page.envKeySource('API_TOKEN') === 'local', 'undeclared key is local (secret inputs stay dash-set)');
}

// 2. The add guard: a settings-declared key never fires the edit — the
//    affordance sends the operator to Settings.
{
  const page = components.appDetailPage();
  page.resource = { server: 'prod', name: 'web' };
  page.configAuthority = { home: 'teploy', declared_env_keys: ['RAILS_ENV'] };
  page.newEnvKey = 'RAILS_ENV';
  page.newEnvValue = 'development';

  globalThis.fetchCount = 0;
  globalThis.fetch = async () => {
    globalThis.fetchCount++;
    return { status: 200, ok: true, headers: new Headers({ 'Content-Type': 'application/json' }), text: async () => JSON.stringify({ data: {} }) };
  };

  toasts.length = 0;
  await page.addEnvVar();
  assert(globalThis.fetchCount === 0, 'settings-declared add must not reach the API');
  assert(toasts.some((t) => /RAILS_ENV/.test(t) && /Settings/.test(t)), 'the refusal toast names the key and Settings');

  // An undeclared key under the same authority posts normally.
  page.newEnvKey = 'API_TOKEN';
  await page.addEnvVar();
  assert(globalThis.fetchCount >= 1, 'undeclared key under Teploy-home authority posts');
}

// 3. The templates render the surfaces: the settings note, per-key badges,
//    read-only rows, and no leftover manifest authority UI.
{
  const html = readFileSync(join(here, '../frontend/index.html'), 'utf8');
  const appStart = html.indexOf('App Detail Page');
  const appEnd = html.indexOf('Monitors Page');
  const section = html.slice(appStart, appEnd);
  assert(appStart !== -1 && appEnd > appStart, 'app detail section not found');

  for (const marker of [
    'Settings for this app are edited in Teploy', // settings note
    'envKeySource(v.key)',                         // per-key badge
    'from settings',                               // inherited label
    'in settings',                                 // read-only row marker
    'deleteEnvVar(v.key)',                         // remove still wired for local keys
  ]) {
    assert(section.includes(marker), `env tab must render ${marker}`);
  }
  for (const stale of ['git-managed', 'Git-managed', 'Dash-managed', 'Not registered', 'from manifest', 'in source', 'authority()']) {
    assert(!section.includes(stale), `env/general tab must not render removed ${stale}`);
  }
}

if (failures.length) {
  console.error(`FAIL (${failures.length}):`);
  for (const f of failures) console.error('  - ' + f);
  process.exit(1);
}
console.log('config_authority.test: all assertions passed');
