// Tabs are in the URL (?tab=): reload, links and Back land on the tab.
//
// Run: node cmd/teploy-dash/frontend-test/tab_routes.test.mjs
import { harness, response } from './identity_harness.mjs';

const failures = [];
const assert = (c, m) => { if (!c) failures.push(m); };

const h = harness('/');
const replaced = [];
globalThis.history.replaceState = (_, __, url) => { replaced.push(url); const [p, q] = url.split('?'); location.pathname = p; location.search = q ? '?' + q : ''; };
const router = h.stores.router;
const go = (path, search = '') => { location.pathname = path; location.search = search; router.restore(); };

// The router reads ?tab= and writes it with replaceState, leaving the default out.
go('/deployments/prod/web', '?tab=env');
assert(router.page === 'app-detail' && router.params.tab === 'env', 'app ?tab=env is read');
router.setTab('logs', 'general');
assert(replaced.at(-1) === '/deployments/prod/web?tab=logs', 'setTab replaces the URL with the new tab: ' + replaced.at(-1));
router.setTab('general', 'general');
assert(replaced.at(-1) === '/deployments/prod/web' && !('tab' in router.params), 'the default tab is left out of the URL');

// App detail: switchTab writes the URL; a URL tab drives the page.
h.respond(() => response({}));
go('/deployments/prod/web', '?tab=deploys');
const app = h.page('appDetailPage');
app.$dispatch = () => {};
await app.init();
await h.flush();
assert(app.tab === 'deploys', 'app detail opens on the URL tab: ' + app.tab);
await app.switchTab('env');
assert(router.params.tab === 'env' && replaced.at(-1).endsWith('?tab=env'), 'app tab click writes the URL');

// Server detail.
go('/servers/prod', '?tab=proxy');
const srv = h.page('serverDetailPage');
await srv.init();
await h.flush();
assert(srv.tab === 'proxy', 'server detail opens on the URL tab: ' + srv.tab);
await srv.switchTab('overview');
assert(!('tab' in router.params), 'server overview leaves the tab out');

// Settings.
go('/settings', '?tab=links');
const settings = h.page('settingsPage');
const ready = settings.init();
assert(settings.tab === 'links', 'settings opens on the URL tab');
settings.setTab('account');
assert(router.params.tab === 'account' && replaced.at(-1) === '/settings?tab=account', 'settings tab click writes the URL');
go('/settings', '?tab=groups');
await h.flush();
assert(settings.tab === 'groups', 'settings follows the URL (Back, links)');
await ready;

if (failures.length) { console.error(`FAIL (${failures.length}):`); failures.forEach((f) => console.error('  - ' + f)); process.exit(1); }
console.log('tab_routes.test: all assertions passed');
