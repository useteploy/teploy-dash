// Component test for the Templates page's D05 versioned-package slice:
// version-state rendering, the install pin payload, and the 409
// catalog-moved path.
//
// Repo convention (no build step, no CI Node job, zero dependencies): a
// plain node script that loads js/app.js under stubbed browser globals,
// captures the Alpine factories, and drives templatesPage against stubbed
// API responses.
//
// Run: node cmd/teploy-dash/frontend-test/templates_page.test.mjs
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
// The element stub doubles as #toast-container (showToast mounts children
// whose textContent is the message) and as progressBar's #load-bar (which
// sets style.width), so it carries both.
globalThis.document = {
  addEventListener: (type, fn) => { if (type === 'alpine:init') alpineInitListeners.push(fn); },
  getElementById: () => ({ style: {}, appendChild: (el) => { lastToast = el.textContent; } }),
  createElement: () => ({ style: {}, classList: { add() {}, remove() {} }, setAttribute() {}, remove() {}, appendChild() {} }),
  documentElement: { setAttribute() {}, getAttribute: () => 'dark' },
};
globalThis.window = { addEventListener() {}, dispatchEvent() {} };
globalThis.localStorage = { getItem: () => null, setItem() {} };
globalThis.history = { pushState() {}, replaceState() {} };
globalThis.location = { pathname: '/templates', search: '' };
globalThis.CustomEvent = class CustomEvent {};

let apiResponses = {};
globalThis.fetch = async (url, init) => {
  const route = apiResponses[url];
  if (route === undefined) {
    return { status: 404, ok: false, headers: new Headers({ 'Content-Type': 'text/plain' }), text: async () => 'not found' };
  }
  if (typeof route === 'function') return route(init);
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
if (!components.templatesPage) {
  console.error('FAIL: templatesPage component was not registered');
  process.exit(1);
}

const versionedCatalog = [
  {
    name: 'postgres-admin', description: 'PostgreSQL 16 with Adminer',
    version: '1.4.0', version_state: 'versioned', variables: ['domain'],
    upgrade_notes: 'pg16 major upgrades require dump/restore', backup_scope: 'accessory dump',
    installed: { server: 'prod', version: '1.3.0', operation_id: 'op1' },
    upgrade: { from: '1.3.0', to: '1.4.0', notes: 'pg16 major upgrades require dump/restore', backup_scope: 'accessory dump' },
  },
  {
    name: 'jellyfin', description: 'Media server', version_state: 'unversioned',
    variables: ['domain'],
  },
];

async function loadPage(responses) {
  apiResponses = responses;
  const page = components.templatesPage();
  await page.init();
  return page;
}

// 1. The page consumes the enriched catalog: version states, installed and
//    upgrade info pass through to the cards.
{
  const page = await loadPage({ '/api/templates': versionedCatalog, '/api/servers': { prod: { host: '192.0.2.10' } } });
  assert(page.templates.length === 2, `templates = ${page.templates.length}, want 2`);
  const pg = page.templates.find((t) => t.name === 'postgres-admin');
  assert(pg.version_state === 'versioned' && pg.version === '1.4.0', 'versioned entry must carry its version');
  assert(pg.installed && pg.installed.version === '1.3.0', 'installed version must surface');
  assert(pg.upgrade && pg.upgrade.to === '1.4.0', 'upgrade info must surface');
  const jf = page.templates.find((t) => t.name === 'jellyfin');
  assert(jf.version_state === 'unversioned', 'unversioned entry must say so, not guess');
  assert(!jf.upgrade && !jf.installed, 'unversioned entry must carry no upgrade/installed chrome');
}

// 2. Install pins the SELECTED version on a versioned catalog and omits the
//    pin on an unversioned one.
{
  let posted;
  const page = await loadPage({
    '/api/templates': versionedCatalog,
    '/api/servers': { prod: { host: '192.0.2.10' } },
    '/api/templates/install': (init) => {
      posted = JSON.parse(init.body);
      return { status: 202, ok: true, headers: new Headers({ 'Content-Type': 'application/json' }), text: async () => JSON.stringify({ data: { id: 'op2', status: 'queued', request: {} } }) };
    },
  });

  page.selectTemplate(page.templates.find((t) => t.name === 'postgres-admin'));
  page.installForm = { domain: 'db.example', server: 'prod', vars: {} };
  await page.install();
  assert(posted && posted.template_version === '1.4.0', `versioned install pin = ${posted && posted.template_version}, want 1.4.0`);

  page.selectTemplate(page.templates.find((t) => t.name === 'jellyfin'));
  page.installForm = { domain: 'media.example', server: 'prod', vars: {} };
  await page.install();
  assert(posted && posted.template_version === '', `unversioned install pin = ${posted && posted.template_version}, want empty`);
}

// 3. A moved catalog (409) keeps the form open and surfaces the server's
//    remedy message — the operator re-selects rather than silently
//    installing a version they never saw.
{
  lastToast = null;
  const page = await loadPage({
    '/api/templates': versionedCatalog,
    '/api/servers': { prod: { host: '192.0.2.10' } },
    '/api/templates/install': () => ({
      status: 409, ok: false,
      headers: new Headers({ 'Content-Type': 'application/json' }),
      text: async () => JSON.stringify({ error: 'template postgres-admin moved from v1.4.0 (selected) to v1.5.0 (current); review the upgrade notes and re-select before installing' }),
    }),
  });
  page.selectTemplate(page.templates.find((t) => t.name === 'postgres-admin'));
  page.installForm = { domain: 'db.example', server: 'prod', vars: {} };
  await page.install();
  assert(lastToast && lastToast.includes('moved from v1.4.0'), `409 toast = ${lastToast}`);
  assert(page.selected !== null, 'a refused pin must keep the install form open for re-selection');
  assert(page.installing === false, 'installing flag must reset after a refusal');
}

// 4. Static tripwire: the template actually renders the version badge and
//    the upgrade block (a detached template must not pass 1-3).
{
  const html = readFileSync(join(here, '../frontend/index.html'), 'utf8');
  const start = html.indexOf('Templates Page');
  const end = html.indexOf('Operations Page');
  const section = html.slice(start, end);
  assert(start !== -1 && end > start, 'templates page section not found in index.html');
  assert(section.includes("t.version_state === 'versioned'"), 'template card must render the version/unversioned badge');
  assert(section.includes('selected.upgrade.from'), 'install form must render the upgrade block');
  assert(section.includes('t.installed.server'), 'template card must render the installed state');
}

if (failures.length) {
  console.error(`FAIL (${failures.length}):`);
  for (const f of failures) console.error('  - ' + f);
  process.exit(1);
}
console.log('templates_page.test: all assertions passed');
