// Component test for the Servers page fleet rendering (D01 UI depth).
//
// The repo's frontend has no build step and no framework beyond Alpine.js,
// and CI deliberately runs no Node job — so this follows the repo's
// zero-dependency convention (`node --check`): a plain node script with no
// imports from node_modules. It loads js/app.js under stubbed browser
// globals, captures the Alpine component factories registered on
// alpine:init, and drives serversPage.load() against stubbed API responses.
//
// Run: node cmd/teploy-dash/frontend-test/servers_page.test.mjs
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const failures = [];
function assert(cond, message) {
  if (!cond) failures.push(message);
}

// ── Stub browser globals (only what app.js touches at load + on this page) ──
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
globalThis.location = { pathname: '/servers', search: '' };
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
    text: async () => JSON.stringify({ data: route }),
  };
};

const components = {};
globalThis.Alpine = {
  data: (name, factory) => { components[name] = factory; },
  store: () => ({ navigate() {}, restore() {} }),
};

new Function(readFileSync(join(here, '../frontend/js/app.js'), 'utf8'))();
alpineInitListeners.forEach((fn) => fn());
if (!components.serversPage) {
  console.error('FAIL: serversPage component was not registered');
  process.exit(1);
}

const iso = (minutesAgo) => new Date(Date.now() - minutesAgo * 60000).toISOString();
const healthyFleet = {
  collected_at: iso(0),
  servers: [
    {
      id: 'srv-alpha01', server: 'alpha', host: '192.0.2.10',
      last_success_at: iso(0), collected_at: iso(0), freshness: 'fresh',
      apps: [{ app: 'site' }, { app: 'api' }],
    },
    {
      id: 'srv-beta02', server: 'beta', host: '192.0.2.11',
      last_success_at: iso(45), collected_at: iso(0), freshness: 'unknown',
      error: 'ssh: connect to host 192.0.2.11 port 22: connection timed out',
      apps: [{ app: 'mail' }, { app: 'git' }, { app: 'wiki' }],
    },
    {
      id: 'srv-gamma03', server: 'gamma', host: '192.0.2.12',
      last_success_at: iso(60), collected_at: iso(0), freshness: 'stale',
      apps: [{ app: 'ci' }],
    },
  ],
};
const serversConfig = { alpha: { host: '192.0.2.10' }, beta: { host: '192.0.2.11' }, gamma: { host: '192.0.2.12' } };

async function loadPage(responses) {
  apiResponses = responses;
  const page = components.serversPage();
  await page.load();
  return page;
}

// 1. The fleet view consumes /api/fleet: per-server freshness, the error of
//    an unreachable host, its last-known app count, and the stable identity.
{
  const page = await loadPage({ '/api/servers': serversConfig, '/api/fleet': healthyFleet });
  const byName = Object.fromEntries(page.servers.map((s) => [s.name, s]));
  const alpha = byName.alpha;
  assert(alpha.freshness === 'fresh', `alpha freshness = ${alpha.freshness}, want fresh`);
  assert(!alpha.error && page.freshnessLabel(alpha) === 'Fresh', 'fresh server must carry no error and label Fresh');
  assert(alpha.id === 'srv-alpha01', 'alpha must expose the stable envelope id');

  const beta = byName.beta;
  assert(beta.freshness === 'unknown', `beta freshness = ${beta.freshness}, want unknown`);
  assert(beta.error && beta.error.includes('connection timed out'), 'unknown-freshness server must expose its exact error');
  assert(beta.appCount === 3, `beta last-known app count = ${beta.appCount}, want 3`);
  assert(!!beta.lastSuccessAt, 'unknown server must expose its last successful observation time');
  const meta = page.unreachableMeta(beta);
  assert(meta.includes('3 apps') && meta.toLowerCase().includes('last known'), `unreachable meta = ${meta}`);
  assert(page.cardDotClass(beta) === 'offline' && page.cardDotClass(alpha) === 'online', 'dot classes must distinguish unknown from fresh');
  assert(byName.gamma.freshness === 'stale', `gamma freshness = ${byName.gamma.freshness}, want stale`);

  // Unreachable servers remain visible: every configured server is present.
  assert(page.servers.length === 3, `servers rendered = ${page.servers.length}, want all 3`);
}

// 2. Additive: when /api/fleet fails, the cards render exactly as before —
//    no freshness chrome, no error chrome, /api/apps-style consumers unaffected.
{
  const page = await loadPage({
    '/api/servers': serversConfig,
    '/api/fleet': () => Promise.reject(new Error('fleet endpoint unavailable')),
  });
  const byName = Object.fromEntries(page.servers.map((s) => [s.name, s]));
  assert(page.servers.length === 3, `servers without fleet = ${page.servers.length}, want 3`);
  for (const s of page.servers) {
    assert(s.freshness === null || s.freshness === undefined, `server ${s.name} must have no freshness when fleet is unavailable (got ${s.freshness})`);
    assert(!s.error, `server ${s.name} must render no error chrome without fleet data`);
    assert(page.freshnessLabel(s) === '', `server ${s.name} must have no freshness chip label`);
  }
}

// 3. A server observed by the fleet but absent from the config list (the
//    local-state fallback) stays visible — D01: servers never vanish.
{
  const page = await loadPage({
    '/api/servers': {},
    '/api/fleet': {
      collected_at: iso(0),
      servers: [{ id: 'srv-local0', server: 'local', host: '', last_success_at: iso(0), collected_at: iso(0), freshness: 'fresh', apps: [{ app: 'blog' }] }],
    },
  });
  assert(page.servers.length === 1 && page.servers[0].name === 'local', 'fleet-only servers must stay visible');
  assert(page.servers[0].appCount === 1, 'fleet-only server app count must be projected');
}

// 4. The template actually renders what the component computes: the servers
//    card must bind the freshness chip and the error block. (Static tripwire
//    so a detached template cannot pass the component tests above.)
{
  const html = readFileSync(join(here, '../frontend/index.html'), 'utf8');
  const serversStart = html.indexOf('Servers Page');
  const serversEnd = html.indexOf('Server Detail Page');
  const section = html.slice(serversStart, serversEnd);
  assert(serversStart !== -1 && serversEnd > serversStart, 'servers page section not found in index.html');
  assert(section.includes('freshnessLabel'), 'servers card must render the freshness chip (freshnessLabel binding missing)');
  assert(section.includes('unreachableMeta'), 'servers card must render the unreachable metadata (unreachableMeta binding missing)');
  assert(section.includes('s.error'), 'servers card must render the per-server error block');
}

if (failures.length) {
  console.error(`FAIL (${failures.length}):`);
  for (const f of failures) console.error('  - ' + f);
  process.exit(1);
}
console.log('servers_page.test: all assertions passed');
