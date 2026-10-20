// Component test for the server-authority P2 surfaces on the app page: the
// Settings tab (an app's teploy.yml stored on its server, saved with a
// compare-and-swap and merged on conflict), "Make editable here", Deploy
// pending changes, the Activity tab (server event history) and the env
// guard for keys a Teploy-home app's settings declare. Every surface is
// gated on the CLI capability that provides it.
//
// Run: node cmd/teploy-dash/frontend-test/config_home.test.mjs
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
  createElement: () => ({ style: {}, classList: { add() {} }, setAttribute() {}, remove() {}, appendChild() {}, click() {} }),
  documentElement: { setAttribute() {}, getAttribute: () => 'dark' },
};
globalThis.window = { addEventListener() {}, dispatchEvent() {} };
globalThis.localStorage = { getItem: () => null, setItem() {} };
globalThis.history = { pushState() {}, replaceState() {} };
globalThis.location = { pathname: '/deployments/prod/web', search: '' };
globalThis.CustomEvent = class CustomEvent {};
globalThis.EventSource = class EventSource { constructor() {} addEventListener() {} close() {} };
globalThis.confirm = () => true;

const components = {};
const stores = {};
const navigations = [];
globalThis.Alpine = {
  data: (name, factory) => { components[name] = factory; },
  store: (name, def) => { if (def !== undefined) stores[name] = def; return stores[name]; },
  effect: () => {},
};
stores.router = { page: 'app-detail', params: { server: 'prod', name: 'web' }, navigate: (page, params) => navigations.push([page, params]) };

// Route table for the fetch seam: [method, path] -> [status, body].
let routes = {};
const calls = [];
globalThis.fetch = async (url, init = {}) => {
  const method = init.method || 'GET';
  calls.push({ method, url, body: init.body ? JSON.parse(init.body) : undefined });
  const hit = routes[`${method} ${url}`];
  const [status, body] = hit || [404, { error: 'not found' }];
  return {
    status, ok: status >= 200 && status < 300,
    headers: new Headers({ 'Content-Type': 'application/json' }),
    text: async () => JSON.stringify(body),
  };
};

new Function(readFileSync(join(here, '../frontend/js/app.js'), 'utf8'))();
alpineInitListeners.forEach((fn) => fn());
// app.js registers its own router store; record navigations on it.
stores.router.params = { server: 'prod', name: 'web' };
stores.router.navigate = (page, params) => navigations.push([page, params]);

const base = '/api/apps/prod/web';
function newPage() {
  const page = components.appDetailPage();
  page.resource = Object.freeze({ server: 'prod', name: 'web' });
  page.role = 'admin';
  page.roleLoaded = true;
  return page;
}

// 1. Feature gating: unknown or older CLI hides every new surface.
{
  const page = newPage();
  assert(page.hasFeature('config_home') === false, 'features unknown: config home hidden');
  routes = { [`GET /api/capabilities`]: [200, { data: { features: { config_home: true, events: false } } }] };
  await page.loadFeatures();
  assert(page.hasFeature('config_home') === true, 'config_home advertised: Settings shown');
  assert(page.hasFeature('events') === false, 'events not advertised: Activity hidden');

  const failing = newPage();
  routes = {};
  await failing.loadFeatures();
  assert(failing.hasFeature('config_home') === false, 'a failed capability read hides the surfaces');
}

// 2. Home labels and desired-vs-applied.
{
  const page = newPage();
  page.configAuthority = { registered: false, home: 'folder' };
  assert(page.configHomeLabel() === 'Its folder (teploy.yml)', 'folder home label');
  assert(page.configPending() === false, 'folder apps never show pending changes');
  page.configAuthority = { registered: false, home: 'teploy', config_commit: 'b'.repeat(40), applied_commit: 'a'.repeat(40) };
  assert(page.configHomeLabel() === 'Teploy', 'teploy home label');
  assert(page.configPending() === true, 'config moved past the running release: pending');
  page.configAuthority.applied_commit = page.configAuthority.config_commit;
  assert(page.configPending() === false, 'running release has the current config: not pending');
  page.configAuthority = { registered: false };
  assert(page.configHomeLabel() === '', 'older CLI (no home): no badge');
}

// 3. Save is a compare-and-swap against the loaded commit.
{
  const page = newPage();
  const c1 = '1'.repeat(40), c2 = '2'.repeat(40);
  routes = {
    [`GET ${base}/config`]: [200, { data: { home: 'teploy', commit: c1, applied_commit: c1, content: 'name: web\nimage: nginx:1\n' } }],
    [`GET ${base}/config/history`]: [200, { data: { commits: [{ commit: c1, author: 'cli', date: '2026-10-19T00:00:00Z', message: 'config set' }] } }],
    [`PUT ${base}/config`]: [200, { data: { home: 'teploy', commit: c2 } }],
    [`GET ${base}/config-authority`]: [200, { data: { registered: false, home: 'teploy', config_commit: c2, applied_commit: c1 } }],
  };
  await page.loadSettings();
  assert(page.settings && page.settings.home === 'teploy', 'settings loaded');
  assert(page.settingsHistory.length === 1, 'history loaded');
  assert(page.settingsDirty() === false, 'fresh load is clean');
  page.settingsDraft = 'name: web\nimage: nginx:2\n';
  assert(page.settingsDirty() === true, 'edit marks dirty');
  calls.length = 0;
  await page.saveSettings();
  const put = calls.find((c) => c.method === 'PUT');
  assert(put && put.body.expect === c1, 'save sends expect = the commit the edit started from');
  assert(put && put.body.content === 'name: web\nimage: nginx:2\n', 'save sends the whole draft');
  assert(page.configPending() === true, 'after save the authority reload shows pending changes');
}

// 4. A conflict adopts the CLI's merge and re-bases on the current commit.
{
  const page = newPage();
  const c1 = '1'.repeat(40), c3 = '3'.repeat(40);
  routes = {
    [`GET ${base}/config`]: [200, { data: { home: 'teploy', commit: c1, content: 'image: nginx:1\nport: 80\n' } }],
    [`GET ${base}/config/history`]: [200, { data: { commits: [] } }],
    [`PUT ${base}/config`]: [409, { error: 'config moved', merge: { base: c1, current: c3, merged: 'image: nginx:2\nport: 8080\n', conflicts: [{ key: 'image', mine: 'nginx:2', current: 'nginx:3' }] } }],
  };
  await page.loadSettings();
  page.settingsDraft = 'image: nginx:2\nport: 80\n';
  await page.saveSettings();
  assert(page.settingsMerge && page.settingsMerge.conflicts.length === 1, 'conflict keys are kept for review');
  assert(page.settingsDraft === 'image: nginx:2\nport: 8080\n', 'draft becomes the merged config');
  assert(page.settingsBase === c3, 'next save is against the commit that now exists');
  assert(/changed elsewhere/.test(page.settingsError), 'the conflict is explained');
  assert(page.mergeValue(undefined, true) === '(removed)', 'removed side renders');

  // A non-conflict failure is shown as-is and keeps the draft.
  routes[`PUT ${base}/config`] = [422, { error: 'env_files are not allowed in Teploy home' }];
  page.settingsMerge = null;
  page.settingsDraft = 'env_files: [x]\n';
  await page.saveSettings();
  assert(page.settingsError === 'env_files are not allowed in Teploy home', 'validation error shown verbatim');
  assert(page.settingsDraft === 'env_files: [x]\n', 'draft kept after a refused save');
}

// 5. Deploy pending changes, Make editable here, restore.
{
  const page = newPage();
  routes = {
    [`POST ${base}/deploy`]: [202, { data: { id: 'op1', status: 'queued', request: { kind: 'deploy' } } }],
  };
  navigations.length = 0;
  await page.deployPending();
  assert(navigations.some(([p, q]) => p === 'operation-detail' && q.id === 'op1'), 'deploy follows the operation');

  routes = { [`POST ${base}/config/home`]: [422, { error: 'the running release cannot be reproduced from the server records' }] };
  calls.length = 0;
  await page.makeEditableHere();
  const homeCall = calls.find((c) => c.url === `${base}/config/home`);
  assert(homeCall && homeCall.body.home === 'teploy', 'make editable posts home teploy');
  assert(/cannot be reproduced/.test(page.settingsError), 'a refused conversion explains why');

  routes = {
    [`POST ${base}/config/restore`]: [200, { data: { output: 'ok' } }],
    [`GET ${base}/config`]: [200, { data: { home: 'teploy', commit: '4'.repeat(40), content: 'x: 1\n' } }],
    [`GET ${base}/config/history`]: [200, { data: { commits: [] } }],
    [`GET ${base}/config-authority`]: [200, { data: { registered: false, home: 'teploy' } }],
  };
  calls.length = 0;
  await page.restoreSettings('1'.repeat(40));
  const restore = calls.find((c) => c.url === `${base}/config/restore`);
  assert(restore && restore.body.commit === '1'.repeat(40), 'restore posts the chosen commit');
}

// 6. Activity: newest first, actor and detail rendering.
{
  const page = newPage();
  routes = {
    [`GET ${base}/events`]: [200, { data: { events: [
      { seq: 1, id: 'e1', kind: 'deploy', outcome: 'succeeded', ts: '2026-10-19T00:00:00Z', release: 'abcdef1234567890', actor: { client: 'cli', label: 'tyler' }, detail: { image: 'nginx:1' } },
      { seq: 2, id: 'e2', kind: 'env.set', outcome: 'succeeded', ts: '2026-10-19T00:01:00Z', actor: { client: 'dash', label: 'admin', op: 'op1' }, detail: { keys: ['API_TOKEN'] } },
    ] } }],
  };
  await page.loadEvents();
  assert(page.events.length === 2 && page.events[0].seq === 2, 'events render newest first');
  assert(page.eventActor(page.events[0]) === 'Dashboard (admin)', 'dash actor labelled');
  assert(page.eventActor(page.events[1]) === 'CLI (tyler)', 'cli actor labelled');
  assert(page.eventSummary(page.events[0]) === 'API_TOKEN', 'env events show key names only');
  assert(page.eventSummary(page.events[1]).includes('abcdef123456') && page.eventSummary(page.events[1]).includes('nginx:1'), 'deploy shows release and image');
  assert(page.eventOutcomeClass({ outcome: 'failed' }) === 'stopped', 'failure is red');

  routes = {};
  await page.loadEvents();
  assert(page.eventsError !== '', 'a failed read is an error state, not an empty list');
}

// 7. Env guard: a key a Teploy-home app's settings declare is not editable
//    from the Environment tab.
{
  const page = newPage();
  page.configAuthority = { registered: false, home: 'teploy', declared_env_keys: ['LOG_LEVEL'] };
  assert(page.envKeyLocked('LOG_LEVEL') === true, 'declared key locked in teploy home');
  assert(page.envKeyLocked('API_TOKEN') === false, 'undeclared key stays editable');
  page.newEnvKey = 'LOG_LEVEL';
  page.newEnvValue = 'debug';
  calls.length = 0;
  toasts.length = 0;
  await page.addEnvVar();
  assert(calls.length === 0, 'locked key never reaches the API');
  assert(toasts.some((t) => /LOG_LEVEL/.test(t) && /Settings/.test(t)), 'the refusal points to Settings');

  page.configAuthority = { registered: false, home: 'folder', declared_env_keys: [] };
  assert(page.envKeyLocked('LOG_LEVEL') === false, 'folder home: nothing locked');
}

// 8. Identity change clears the settings draft (no cross-app leak).
{
  const page = newPage();
  page.settingsDraft = 'secret-looking draft';
  page.events = [{ seq: 1 }];
  page.activateResource({ server: 'prod', name: 'other' });
  assert(page.settingsDraft === '' && page.settings === null, 'draft cleared on app change');
  assert(page.events.length === 0, 'events cleared on app change');
}

// 9. Deploy form: a new app is created in Teploy home when the CLI can; an
//    existing app (or an older CLI) keeps today's request.
{
  const page = components.projectsPage();
  page.apps = [{ name: 'old', server: 'prod' }];
  routes = { [`GET /api/capabilities`]: [200, { data: { features: { deploy_home: true, config_home: true } } }] };
  const fresh = await page.deployRequest({ app: 'new', server: 'prod', image: 'nginx', domain: 'x.example.com', port: 80 });
  assert(fresh.home === 'teploy', 'new app is created in Teploy home');
  const again = await page.deployRequest({ app: 'old', server: 'prod', image: 'nginx', domain: 'x.example.com', port: 80 });
  assert(again.home === undefined, 'an existing app redeploys as before');
  routes = { [`GET /api/capabilities`]: [200, { data: { features: {} } }] };
  const older = await page.deployRequest({ app: 'new', server: 'prod', image: 'nginx', domain: 'x.example.com', port: 80 });
  assert(older.home === undefined, 'older CLI: unchanged request');

  const tpl = components.templatesPage();
  routes = { [`GET /api/capabilities`]: [200, { data: { features: { config_home: true } } }] };
  assert((await tpl.installHome()).home === 'teploy', 'template install into Teploy home when supported');
  routes = {};
  assert((await tpl.installHome()).home === undefined, 'template install unchanged when unknown');
}

// 10. Markup: the tabs are capability-gated and the panels render.
{
  const html = readFileSync(join(here, '../frontend/index.html'), 'utf8');
  for (const marker of [
    `x-show="hasFeature('config_home')"`,
    `x-show="hasFeature('events')"`,
    'id="app-panel-settings"',
    'id="app-panel-activity"',
    'Make editable here',
    'Deploy pending changes',
    'Settings edited in',
    'settingsMerge.conflicts',
    'restoreSettings(c.commit)',
  ]) {
    assert(html.includes(marker), `index.html must render ${marker}`);
  }
}

if (failures.length) {
  console.error(`FAIL (${failures.length}):`);
  for (const f of failures) console.error('  - ' + f);
  process.exit(1);
}
console.log('config_home.test: all assertions passed');
