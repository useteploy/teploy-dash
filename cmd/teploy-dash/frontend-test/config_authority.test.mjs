// Component test for the D07 config-authority indicators: which side owns
// an app's configuration (git-managed manifest / dash-managed manifest /
// unregistered), the inherited-vs-local env-key badges, and the read-only
// treatment of source-owned keys (never a silent competing edit).
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

// 1. Authority classification helpers.
{
  const page = components.appDetailPage();

  // Unregistered / unknown: no indicators, everything local.
  page.configAuthority = null;
  assert(page.authority() === null, 'absent authority answers null');
  assert(page.gitManaged() === false, 'absent authority is not git-managed');
  assert(page.envKeySource('A') === 'local', 'unknown authority labels keys local');

  page.configAuthority = { registered: false };
  assert(page.authority() === null, 'unregistered app answers null authority');

  // Git-managed: declared keys are source-owned.
  page.configAuthority = {
    registered: true, mode: 'git-managed',
    git: { repository: 'https://github.com/acme/web', revision: '0123abc', manifest_path: 'teploy.yml' },
    repository_url: 'https://github.com/acme/web',
    declared_env_keys: ['RAILS_ENV', 'LOG_LEVEL'],
  };
  assert(page.gitManaged() === true, 'git-managed recognized');
  assert(page.envKeySource('RAILS_ENV') === 'manifest', 'declared key is from the manifest');
  assert(page.envKeySource('API_TOKEN') === 'local', 'undeclared key is local (secret inputs are dash-set)');

  // Dash-managed: authority exists but never trips the git guard.
  page.configAuthority = { registered: true, mode: 'dash-managed', declared_env_keys: ['RAILS_ENV'] };
  assert(page.gitManaged() === false, 'dash-managed is not git-managed');
  assert(page.envKeySource('RAILS_ENV') === 'manifest', 'dash-managed still badges declared keys');
}

// 2. The add guard: a source-owned key never fires the edit — the
//    affordance sends the operator to the source. (The fetch seam is the
//    drive point; api is scoped inside app.js.)
{
  const page = components.appDetailPage();
  page.resource = { server: 'prod', name: 'web' };
  page.configAuthority = { registered: true, mode: 'git-managed', declared_env_keys: ['RAILS_ENV'] };
  page.newEnvKey = 'RAILS_ENV';
  page.newEnvValue = 'development';

  globalThis.fetchCount = 0;
  globalThis.fetch = async () => {
    globalThis.fetchCount++;
    return { status: 200, ok: true, headers: new Headers({ 'Content-Type': 'application/json' }), text: async () => JSON.stringify({ data: {} }) };
  };

  toasts.length = 0;
  await page.addEnvVar();
  assert(globalThis.fetchCount === 0, 'source-owned add must not reach the API');
  assert(toasts.some((t) => /RAILS_ENV/.test(t) && /source/.test(t)), 'the refusal toast names the key and the source');

  // An undeclared key under the same authority posts normally (the post
  // plus the follow-up env reload both hit the fetch seam — >= 1 proves
  // the edit itself went through).
  page.newEnvKey = 'API_TOKEN';
  await page.addEnvVar();
  assert(globalThis.fetchCount >= 1, 'undeclared key under git-managed authority posts');
}

// 3. The templates render the surfaces: authority banners, per-key badges,
//    read-only rows, the add-guard, and the general-tab configuration item.
{
  const html = readFileSync(join(here, '../frontend/index.html'), 'utf8');
  const appStart = html.indexOf('App Detail Page');
  const appEnd = html.indexOf('Monitors Page');
  const section = html.slice(appStart, appEnd);
  assert(appStart !== -1 && appEnd > appStart, 'app detail section not found');

  for (const marker of [
    'Configuration is managed in Git',        // git-managed banner
    'Create a source change',                 // the source-change affordance
    'owned by the dash manifest',             // dash-managed note
    'envKeySource(v.key)',                    // per-key badge
    'from manifest',                          // inherited label
    'in source',                              // read-only row marker
    'deleteEnvVar(v.key)',                    // remove still wired for local keys
    'Configuration</div>',                    // general-tab authority item
    'Git-managed', 'Dash-managed', 'Not registered',
  ]) {
    assert(section.includes(marker), `env/general tab must render ${marker}`);
  }
}

if (failures.length) {
  console.error(`FAIL (${failures.length}):`);
  for (const f of failures) console.error('  - ' + f);
  process.exit(1);
}
console.log('config_authority.test: all assertions passed');
