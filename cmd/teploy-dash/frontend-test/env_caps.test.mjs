// Component test for the app-detail env panel's X03 capability split.
//
// Repo convention (no build step, no CI Node job, zero dependencies): a
// plain node script that loads js/app.js under stubbed browser globals,
// captures the Alpine factories, and drives appDetailPage.loadEnv() against
// stubbed /api/auth/me + env endpoints.
//
// Run: node cmd/teploy-dash/frontend-test/env_caps.test.mjs
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
globalThis.location = { pathname: '/apps/prod/web', search: '' };
globalThis.CustomEvent = class CustomEvent {};

let apiResponses = {};
globalThis.fetch = async (url) => {
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

async function loadEnvWith(me, envPayload) {
  apiResponses = {
    '/api/auth/me': me,
    '/apps/prod/web/env': envPayload,
    '/apps/prod/web/env/keys': envPayload,
  };
  const page = components.appDetailPage();
  page.resource = { server: 'prod', name: 'web' };
  page.appPath = () => '/apps/prod/web';
  await page.loadEnv();
  return page;
}

// 1. A session WITH reveal.secrets loads the full variable map (values stay
//    behind the per-row click-to-show in the template).
{
  const page = await loadEnvWith(
    { mode: 'local', capabilities: ['reveal.secrets', 'view.metadata'], user: { username: 'op', role: 'editor' } },
    { API_KEY: 'hunter2', COUNT: '7' },
  );
  assert(page.canRevealSecrets(), 'reveal.secrets session must report canRevealSecrets');
  assert(page.envVars.API_KEY === 'hunter2', 'reveal session must hold the value map');
}

// 2. A session WITHOUT reveal.secrets loads the METADATA variant: names
//    only, marked restricted — and never requests the value endpoint.
{
  const requested = [];
  const realFetch = globalThis.fetch;
  globalThis.fetch = async (url) => { requested.push(url); return realFetch(url); };
  const page = await loadEnvWith(
    { mode: 'local', capabilities: ['view.metadata'], user: { username: 'v', role: 'viewer' } },
    { keys: ['API_KEY', 'COUNT'] },
  );
  globalThis.fetch = realFetch;
  assert(!page.canRevealSecrets(), 'metadata-only session must not report canRevealSecrets');
  assert(requested.some((u) => u.endsWith('/env/keys')) && !requested.some((u) => u.endsWith('/env')),
    `metadata-only session requested ${requested.join(', ')} — must hit env/keys and NOT env`);
  assert(Array.isArray(page.envVars) && page.envVars.length === 2 && page.envVars[0].restricted,
    'metadata session rows must be restricted name-only entries');
  assert(!JSON.stringify(page.envVars).includes('hunter2'), 'metadata session must hold no values');
}

// 3. Disabled auth answers the full-capability envelope; the panel keeps the
//    value shape (the '*' sentinel from authCaps).
{
  const page = await loadEnvWith({ mode: 'disabled', capabilities: [] }, { API_KEY: 'x' });
  assert(page.canRevealSecrets(), 'disabled-auth mode must keep the value panel');
}

// 4. Template tripwire: the env value cell renders '(restricted)' for
//    restricted rows and keeps the click-to-show otherwise.
{
  const html = readFileSync(join(here, '../frontend/index.html'), 'utf8');
  const cell = html.match(/v\.restricted \? '\(restricted\)'/);
  assert(cell, 'index.html env cell must render (restricted) for metadata-only rows');
  assert(/v\.restricted \|\| \(show = !show\)/.test(html), 'index.html env cell must not toggle restricted rows');
}

if (failures.length) {
  console.error(failures.map((f) => `FAIL: ${f}`).join('\n'));
  process.exit(1);
}
console.log('env_caps.test: all assertions passed');
