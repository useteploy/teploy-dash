// Component test for the D07 IA regroup: task-named destinations with
// stable canonical URLs.
//
// 1. ROUTES: the task-named aliases (/projects, /fleet, /activity and their
//    detail paths) resolve to the same pages as the canonical paths; the
//    canonical paths still resolve; navigate() keeps pushing CANONICAL URLs
//    (aliases are deep-link-in only, so no link churn).
// 2. The nav template groups and names the destinations (Projects, Fleet,
//    Activity) with the aria-current bindings intact.
// 3. The tabbed resource page exposes tablist/tab/tabpanel semantics.
//
// Run: node cmd/teploy-dash/frontend-test/ia_routes.test.mjs
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
globalThis.localStorage = { getItem: () => null, setItem() {} };
globalThis.CustomEvent = class CustomEvent {};
globalThis.location = { pathname: '/', search: '' };
const pushed = [];
globalThis.history = {
  pushState: (state, _, url) => pushed.push(url),
  replaceState: () => {},
};
const windowListeners = {};
globalThis.window = {
  addEventListener: (t, fn) => { (windowListeners[t] ||= []).push(fn); },
  dispatchEvent() {},
};

const stores = {};
globalThis.Alpine = {
  data: () => {},
  store: (name, def) => { if (def !== undefined) stores[name] = def; return stores[name]; },
  effect: () => {},
};

new Function(readFileSync(join(here, '../frontend/js/app.js'), 'utf8'))();
alpineInitListeners.forEach((fn) => fn());

const router = stores.router;
assert(!!router, 'router store was not registered');

// 1. Deep links: alias paths and canonical paths resolve to the same pages.
{
  const resolve = (pathname) => {
    globalThis.location = { pathname, search: '' };
    router.restore();
    return { page: router.page, params: { ...router.params } };
  };

  const cases = [
    // canonical paths unchanged (stable URLs).
    ['/deployments', 'projects'],
    ['/deployments/prod/web', 'app-detail'],
    ['/servers', 'servers'],
    ['/servers/prod', 'server-detail'],
    ['/operations', 'operations'],
    ['/operations/abc', 'operation-detail'],
    // task-named aliases deep-link to the same pages, with params.
    ['/projects', 'projects'],
    ['/fleet', 'servers'],
    ['/fleet/prod', 'server-detail'],
    ['/activity', 'operations'],
    ['/activity/abc', 'operation-detail'],
  ];
  for (const [path, page] of cases) {
    const got = resolve(path);
    assert(got.page === page, `${path} resolved to ${got.page}, want ${page}`);
  }
  const detail = resolve('/fleet/prod');
  assert(detail.params.name === 'prod', `/fleet/prod param name = ${detail.params.name}, want prod`);
  const opDetail = resolve('/activity/op1');
  assert(opDetail.params.id === 'op1', `/activity/op1 param id = ${opDetail.params.id}, want op1`);
}

// 2. navigate() pushes CANONICAL URLs even when the page was entered via an
//    alias — bookmarks and back/forward keep one address space.
{
  globalThis.location = { pathname: '/fleet', search: '' };
  router.restore();
  assert(router.page === 'servers', 'alias entry failed');
  pushed.length = 0;
  router.navigate('server-detail', { name: 'prod' });
  assert(pushed[0] === '/servers/prod', `navigate pushed ${pushed[0]}, want canonical /servers/prod`);
  pushed.length = 0;
  router.navigate('operations');
  assert(pushed[0] === '/operations', `navigate pushed ${pushed[0]}, want canonical /operations`);
  pushed.length = 0;
  router.navigate('projects');
  assert(pushed[0] === '/deployments', `navigate pushed ${pushed[0]}, want canonical /deployments`);
}

// 3. Refresh/back navigation preserves state: popstate re-resolves the URL
//    through the same matchURL, so Back from an alias-entered page lands on
//    the right view with the right params.
{
  const handlers = windowListeners.popstate || [];
  assert(handlers.length >= 1, 'a popstate handler must be registered at init');
  globalThis.location = { pathname: '/fleet/prod', search: '' };
  handlers.forEach((fn) => fn());
  assert(router.page === 'server-detail' && router.params.name === 'prod',
    `popstate re-resolution landed on ${router.page} ${JSON.stringify(router.params)}, want server-detail {name:prod}`);
  globalThis.location = { pathname: '/activity', search: '' };
  handlers.forEach((fn) => fn());
  assert(router.page === 'operations', `popstate to /activity landed on ${router.page}, want operations`);
}

// 4. Static tripwire: the nav actually renders the regrouped destinations,
//    separators, and task titles; the resource page renders tablist
//    semantics; page titles renamed.
{
  const html = readFileSync(join(here, '../frontend/index.html'), 'utf8');
  for (const label of ['>Projects</a>', '>Fleet</a>', '>Activity</a>', '>Monitors</a>', '>Restore Tests</a>', '>Templates</a>', '>Settings</a>']) {
    assert(html.includes(label), `nav must render ${label}`);
  }
  assert(!html.includes('>Deployments</a>'), 'nav must not render the old Deployments label');
  const seps = (html.match(/class="nav-sep"/g) || []).length;
  assert(seps === 2, `nav must render two group separators (found ${seps})`);
  assert(html.includes('title="Groups, projects, and the apps deployed to them"'), 'Projects nav link must carry its task title');
  assert(html.includes('title="Every server this dash manages'), 'Fleet nav link must carry its task title');
  assert(html.includes('title="Operations started through dash'), 'Activity nav link must carry its task title');
  // Page titles follow the nav names.
  assert(html.includes('<h1 class="page-title">Projects</h1>'), 'projects page title');
  assert(html.includes('<h1 class="page-title">Fleet</h1>'), 'fleet page title');
  assert(html.includes('<h1 class="page-title">Activity</h1>'), 'activity page title');
  assert(!html.includes('&larr; Deployments</a>'), 'back-links must say Projects, not Deployments');
  assert(html.includes('&larr; Projects</a>') && html.includes('&larr; Fleet</a>') && html.includes('&larr; Activity</a>'), 'back-links renamed');
  // Tab semantics on the resource page (app) and the server page.
  assert(html.includes('role="tablist"'), 'tabs render tablist semantics');
  const tabs = (html.match(/role="tab"/g) || []).length;
  assert(tabs === 7, `expected 7 role=tab buttons (5 app + 2 server), found ${tabs}`);
  const panels = (html.match(/role="tabpanel"/g) || []).length;
  assert(panels === 7, `expected 7 role=tabpanel containers, found ${panels}`);
  assert(html.includes('aria-selected='), 'tabs expose aria-selected');
  assert(html.includes('@keydown.arrow-right.prevent='), 'tablist supports arrow-key navigation');
}

if (failures.length) {
  console.error(`FAIL (${failures.length}):`);
  for (const f of failures) console.error('  - ' + f);
  process.exit(1);
}
console.log('ia_routes.test: all assertions passed');
