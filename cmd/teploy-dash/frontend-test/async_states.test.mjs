// Component test for the D07 async-view state machine (misleading states
// are P0). Every list/detail view must distinguish loading / error / empty /
// stale: a failed FIRST load is an error state with a retry (never the empty
// state, which claimed "no monitors" while the API was down); a failed
// REFRESH keeps the earlier data and reports it as stale (age + exact
// failure); the empty state renders only after a successful load.
//
// Zero-dependency convention like the other component tests
// (node cmd/teploy-dash/frontend-test/async_states.test.mjs): app.js and
// operations.js load under stubbed browser globals, and the Alpine component
// factories are driven against stubbed API responses.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const here = dirname(fileURLToPath(import.meta.url));
const failures = [];
function assert(cond, message) {
  if (!cond) failures.push(message);
}

// ── Stub browser globals ──
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
globalThis.location = { pathname: '/monitors', search: '' };
globalThis.CustomEvent = class CustomEvent {};
globalThis.EventSource = class EventSource { constructor() {} addEventListener() {} close() {} };

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
const stores = {};
globalThis.Alpine = {
  data: (name, factory) => { components[name] = factory; },
  // Registration form (name, def) captures the store; the getter form
  // returns whatever the test last installed (the router is re-stubbed per
  // scenario).
  store: (name, def) => {
    if (def !== undefined) stores[name] = def;
    return stores[name];
  },
  effect: () => {},
};

// app.js and operations.js are classic scripts sharing globals in the
// browser (withAsyncLoad, api, showToast); load both sources in ONE
// function scope so the same sharing holds here.
new Function(
  readFileSync(join(here, '../frontend/js/app.js'), 'utf8') + '\n;' +
  readFileSync(join(here, '../frontend/js/operations.js'), 'utf8'),
)();
alpineInitListeners.forEach((fn) => fn());

const need = ['homepagePage', 'monitorsPage', 'restoreTestsPage', 'serversPage', 'templatesPage', 'appDetailPage', 'serverDetailPage', 'projectDetailPage', 'operationsPage', 'operationDetailPage'];
for (const name of need) {
  if (!components[name]) {
    console.error(`FAIL: ${name} component was not registered`);
    process.exit(1);
  }
}

// The router store is registered during alpine:init; app components read it
// for params. Give every page a stable default.
function withRouter(page, params = {}) {
  stores.router = stores.router || { page, params, navigate() {} };
  stores.router.page = page;
  stores.router.params = params;
}

const failWith = (message) => () => Promise.reject(new Error(message));
// ok() answers an enveloped route (api.get, unwrap=true); rawOk() answers a
// raw route (rawFetch.get — monitors, restore-tests return the payload
// directly, no {data} envelope).
const ok = (value) => () => ({
  status: 200, ok: true,
  headers: new Headers({ 'Content-Type': 'application/json' }),
  text: async () => JSON.stringify({ data: value }),
});
const rawOk = (value) => () => ({
  status: 200, ok: true,
  headers: new Headers({ 'Content-Type': 'application/json' }),
  text: async () => JSON.stringify(value),
});

// 1. The four-state machine, driven on the polled monitors list.
{
  withRouter('monitors');
  const page = components.monitorsPage();
  page._alive = true;

  // (a) failed FIRST load -> error state, never empty.
  apiResponses = { '/api/monitors': failWith('api down') };
  await page.loadMonitors();
  assert(page.loading === false, 'monitors: loading must clear after a failed first load');
  assert(page.loadFailed === true && page.showingStale === false, 'monitors: failed first load must be error-without-data, not stale');
  assert(page.monitors.length === 0, 'monitors: no data may appear from a failed load');
  assert(!!page.loadError && page.loadError.includes('api down'), 'monitors: loadError must carry the exact failure');

  // (b) successful load -> fresh; error cleared.
  apiResponses = { '/api/monitors': rawOk([{ id: 'm1', name: 'api', type: 'http' }]) };
  await page.loadMonitors();
  assert(page.loadFailed === false, 'monitors: success must clear the error');
  assert(!!page.loadedAt, 'monitors: success must stamp loadedAt');
  assert(page.monitors.length === 1, 'monitors: data must land after success');

  // (c) failed REFRESH -> data retained, reported as stale with age + error.
  apiResponses = { '/api/monitors': failWith('refresh timed out') };
  await page.loadMonitors();
  assert(page.showingStale === true, 'monitors: failed refresh with earlier data must be stale, not error');
  assert(page.monitors.length === 1, 'monitors: stale data must be retained, never dropped');
  const banner = page.staleBannerText();
  assert(banner.includes('refresh timed out'), `monitors: stale banner must name the failure (got "${banner}")`);
  assert(/\d{4}/.test(banner), 'monitors: stale banner must name the data age (timestamp)');

  // (d) recovery -> fresh again.
  apiResponses = { '/api/monitors': rawOk([{ id: 'm1', name: 'api' }, { id: 'm2', name: 'web' }]) };
  await page.loadMonitors();
  assert(page.loadFailed === false && page.showingStale === false, 'monitors: recovery must clear stale');
  assert(page.monitors.length === 2, 'monitors: recovered refresh must update the list');

  // (e) stale-response guard: a late response for a destroyed component or
  // a newer load must not overwrite state. The older load answers "late"
  // FIRST, the newer load answers the current list — only the newer one may
  // publish.
  let calls = 0;
  apiResponses = {
    '/api/monitors': () => {
      calls++;
      return calls === 1
        ? rawOk([{ id: 'late' }])()
        : rawOk([{ id: 'm1' }, { id: 'm2' }])();
    },
  };
  const inFlight = page.loadMonitors();
  await page.loadMonitors(); // a newer generation lands second
  await inFlight;
  assert(Array.isArray(page.monitors) && page.monitors.length === 2 && !page.monitors.some(m => m.id === 'late'),
    'monitors: the older overlapping response must not replace the newer load\'s list');
}

// 2. The same machine on the homepage shortcuts grid.
{
  withRouter('homepage');
  const page = components.homepagePage();
  apiResponses = { '/api/homepage': failWith('homepage endpoint unavailable') };
  await page.load();
  assert(page.loadFailed === true && page.visibleItems.length === 0, 'homepage: failed load must be the error state, not the empty state');

  apiResponses = { '/api/homepage': ok([{ id: 'a', name: 'Forgejo', url: 'http://x' }]) };
  await page.load();
  assert(page.loadFailed === false && page.visibleItems.length === 1, 'homepage: success must clear error and land data');

  apiResponses = { '/api/homepage': failWith('boom') };
  await page.load();
  assert(page.showingStale === true && page.visibleItems.length === 1, 'homepage: failed refresh keeps earlier shortcuts as stale');
}

// 3. Servers: a dead /api/servers is an error, not "No servers configured".
{
  withRouter('servers');
  const page = components.serversPage();
  apiResponses = { '/api/servers': failWith('connection refused') };
  await page.load();
  assert(page.loadFailed === true, 'servers: failed config read must be the error state');
  assert(page.servers.length === 0, 'servers: no phantom cards from a failed read');

  apiResponses = {
    '/api/servers': ok({ prod: { host: '10.0.0.1' } }),
    '/api/fleet': ok({ collected_at: new Date().toISOString(), servers: [{ id: 'srv-1', server: 'prod', freshness: 'fresh', apps: [] }] }),
  };
  await page.load();
  assert(page.loadFailed === false && page.servers.length === 1 && page.servers[0].freshness === 'fresh', 'servers: success lands merged cards');

  // Fleet unavailable stays additive (no freshness chrome), NOT an error.
  apiResponses = { '/api/servers': ok({ prod: { host: '10.0.0.1' } }), '/api/fleet': failWith('fleet down') };
  await page.load();
  assert(page.loadFailed === false, 'servers: fleet failure must stay additive (D01), not page-level error');
  assert(page.servers[0].freshness === null, 'servers: no fleet -> no freshness chrome');

  // Config read fails with cards on screen -> stale, cards retained.
  apiResponses = { '/api/servers': failWith('connection refused') };
  await page.load();
  assert(page.showingStale === true && page.servers.length === 1, 'servers: failed refresh keeps earlier cards as stale');
}

// 4. Detail views: a failed read renders an error state with retry, not a
//    blank page (app, server, operation).
{
  withRouter('app-detail', { server: 'prod', name: 'web' });
  const app = components.appDetailPage();
  app.resource = Object.freeze({ server: 'prod', name: 'web' });
  apiResponses = { '/api/apps/prod/web/status': failWith('ssh: connect timed out') };
  await app.loadStatus();
  assert(app.loading === false && app.app === null && app.loadFailed === true, 'app-detail: failed status must set the error state');
  assert(app.loadError.includes('ssh: connect timed out'), 'app-detail: error names the failure');

  apiResponses = { '/api/apps/prod/web/status': ok({ app: 'web', server: 'prod', status: 'running' }) };
  await app.loadStatus();
  assert(app.loadFailed === false && !!app.app, 'app-detail: retry path clears error and lands data');

  withRouter('server-detail', { name: 'prod' });
  const srv = components.serverDetailPage();
  apiResponses = { '/api/servers/prod/status': failWith('unreachable') };
  await srv.init();
  assert(srv.loadFailed === true && srv.status === null, 'server-detail: failed status must set the error state');

  withRouter('operation-detail', { id: 'op1' });
  const op = components.operationDetailPage();
  op._alive = true;
  apiResponses = { '/api/operations/op1': failWith('operation not found') };
  await op.init();
  assert(op.loadFailed === true && op.op === null && op.loading === false, 'operation-detail: failed read must set the error state');
}

// 5. Operations list (operations.js): the 3s poller's failure modes.
{
  withRouter('operations');
  const page = components.operationsPage();
  page._alive = true;

  apiResponses = { '/api/operations': failWith('gateway unavailable') };
  await page.load();
  assert(page.loadFailed === true && page.ops.length === 0, 'operations: failed first load is the error state, not "No operations yet"');

  apiResponses = { '/api/operations': ok([{ id: 'a', status: 'running' }]) };
  await page.load();
  assert(page.loadFailed === false && page.ops.length === 1, 'operations: success clears error');

  apiResponses = { '/api/operations': failWith('poll failed') };
  await page.load();
  assert(page.showingStale === true && page.ops.length === 1, 'operations: failed poll keeps the list as stale');
  assert(page.staleBannerText().includes('poll failed'), 'operations: stale banner names the failure');
}

// 6. Project detail: a partial failure is surfaced, not painted as an empty
//    project.
{
  withRouter('project-detail', { group: 'g', project: 'p' });
  const page = components.projectDetailPage();
  page.groupName = 'g';
  page.projectName = 'p';
  apiResponses = {
    '/api/apps': ok([{ app: 'web', server: 'prod', name: 'web' }]),
    '/api/groups': failWith('groups unreadable'),
    '/api/servers': ok({}),
  };
  await page.load();
  assert(page.loadFailed === true, 'project-detail: a failed groups read must surface, not paint empty');
  assert(page.loadError.includes('groups unreadable'), 'project-detail: error names the failed part');
}

// 7. Static tripwire: the templates actually render what the components
//    compute (a detached template must not pass the tests above).
{
  const html = readFileSync(join(here, '../frontend/index.html'), 'utf8');
  const staleBanners = (html.match(/stale-banner/g) || []).length;
  assert(staleBanners >= 8, `index.html must render the stale banner on the async views (found ${staleBanners})`);
  const retryBlocks = (html.match(/role="alert"/g) || []).length;
  assert(retryBlocks >= 8, `index.html must render error states with retry (found ${retryBlocks})`);
  // The empty states that used to paint on failure now gate on !loadFailed.
  for (const marker of [
    '!loading && !loadFailed && monitors.length === 0',
    '!loading && !loadFailed && tests.length === 0',
    '!loading && !loadFailed && ops.length === 0',
    '!loading && !loadFailed && visibleItems.length === 0',
  ]) {
    assert(html.includes(marker), `empty state must gate on !loadFailed: ${marker}`);
  }
  // Detail views render their error state with a retry action.
  assert(html.includes('!loading && !app && loadFailed'), 'app-detail error block missing');
  assert(html.includes('!loading && !status && loadFailed'), 'server-detail error block missing');
  assert(html.includes('!loading && !op && loadFailed'), 'operation-detail error block missing');
}

if (failures.length) {
  console.error(`FAIL (${failures.length}):`);
  for (const f of failures) console.error('  - ' + f);
  process.exit(1);
}
console.log('async_states.test: all assertions passed');
