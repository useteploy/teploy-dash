// Component test for the deploy-form onboarding readiness panel (D03 UI).
//
// Same zero-dependency convention as servers_page.test.mjs: loads js/app.js
// under stubbed browser globals, captures the Alpine factories, and drives
// the projectsPage deploy form against a stubbed /api/onboarding/preflight.
// Blocking checks must gate the Deploy button (canDeploy); warnings render
// non-blocking; an unreadable preflight leaves readiness unknown and gated.
//
// Run: node cmd/teploy-dash/frontend-test/onboarding_preflight.test.mjs
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
globalThis.location = { pathname: '/projects', search: '' };
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
if (!components.projectsPage) {
  console.error('FAIL: projectsPage component was not registered');
  process.exit(1);
}

const healthyPreflight = {
  id: 'srv-alpha01', server: 'alpha', host: '192.0.2.10', known: true,
  ready: true, collected_at: new Date().toISOString(), source: 'cli',
  checks: [
    { name: 'teploy_cli', result: 'pass', severity: 'blocking', detail: 'teploy v0.1.35 on the dashboard host' },
    { name: 'machine_interface', result: 'pass', severity: 'warning', detail: 'server status --json supported' },
    { name: 'ssh', result: 'pass', severity: 'blocking', detail: 'TCP 192.0.2.10:22 accepts connections' },
    { name: 'host_read', result: 'pass', severity: 'blocking', detail: 'teploy server status --json' },
    { name: 'docker', result: 'pass', severity: 'blocking', detail: 'Docker 27.0.3 reachable' },
    { name: 'disk', result: 'pass', severity: 'warning', detail: '40.0G available of 90.0G on /' },
    { name: 'caddy', result: 'pass', severity: 'warning', detail: 'Caddy reachable, 12 route(s) configured' },
  ],
};

const dockerDownPreflight = {
  ...healthyPreflight,
  ready: false,
  checks: healthyPreflight.checks.map((c) =>
    c.name === 'docker'
      ? { ...c, result: 'fail', detail: 'cannot connect to the Docker daemon at unix:///var/run/docker.sock',
          remediation: 'Start the Docker daemon on the host (`systemctl enable --now docker`), then retry.' }
      : c),
};

const caddyWarnPreflight = {
  ...healthyPreflight,
  checks: healthyPreflight.checks.map((c) =>
    c.name === 'caddy'
      ? { ...c, result: 'fail', detail: 'Caddy/proxy is not reachable on the host',
          remediation: 'Caddy is managed by teploy on the host; check `docker ps` for the caddy container.' }
      : c),
};

async function formWith(server, preflightResponse) {
  apiResponses = { ['/api/onboarding/preflight?server=' + encodeURIComponent(server)]: preflightResponse };
  const page = components.projectsPage();
  page.deployForm = { app: 'myapp', image: 'nginx:latest', domain: 'myapp.example.com', server, port: 80 };
  await page.loadPreflight();
  return page;
}

// 1. Blocking degradation gates the Deploy button; the exact error and the
//    remediation hint are exposed for rendering.
{
  const page = await formWith('alpha', dockerDownPreflight);
  assert(page.preflight && page.preflight.ready === false, 'degraded preflight must be loaded with ready=false');
  assert(page.blockingChecks.length === 1 && page.blockingChecks[0].name === 'docker',
    `blocking checks = ${JSON.stringify(page.blockingChecks)}, want the failed docker check`);
  assert(page.blockingChecks[0].remediation.includes('Docker daemon'), 'blocking failure must carry its remediation hint');
  assert(page.canDeploy === false, 'a blocking check failure must gate canDeploy');
  assert(page.preflightCheckClass(page.blockingChecks[0]).includes('fail'), 'failed blocking check needs a fail render class');
  const pass = page.preflight.checks.find((c) => c.name === 'ssh');
  assert(page.preflightCheckClass(pass).includes('pass'), 'passing check needs a pass render class');
}

// 2. Warnings are non-blocking: caddy down leaves the form deployable while
//    the warning check stays visible.
{
  const page = await formWith('alpha', caddyWarnPreflight);
  assert(page.preflight.ready === true, 'warning-tier failure must not flip ready');
  assert(page.warningChecks.length === 1 && page.warningChecks[0].name === 'caddy',
    `warning checks = ${JSON.stringify(page.warningChecks)}`);
  assert(page.canDeploy === true, 'warnings must not gate canDeploy when the form is complete');
}

// 3. Healthy host: form complete + ready -> deployable; incomplete form
//    still blocks regardless of readiness.
{
  const page = await formWith('alpha', healthyPreflight);
  assert(page.canDeploy === true, 'healthy readiness + complete form must allow deploy');
  page.deployForm.app = '';
  assert(page.canDeploy === false, 'an incomplete form must stay undeployable');
}

// 4. Preflight unreadable (endpoint/transport failure): readiness is
//    UNKNOWN, shown as an error, and the form stays gated — never a silent
//    pass.
{
  const page = await formWith('alpha', () => Promise.reject(new Error('preflight endpoint unreachable')));
  assert(!!page.preflightError, 'a failed preflight fetch must surface preflightError');
  assert(page.canDeploy === false, 'unknown readiness must gate the deploy button');
}

// 5. No server selected: no preflight is fetched and the form is gated.
{
  const page = components.projectsPage();
  page.deployForm = { app: 'myapp', image: 'nginx:latest', domain: 'myapp.example.com', server: '', port: 80 };
  page.preflight = null;
  assert(page.canDeploy === false, 'no server selected must gate the deploy button');
}

// 6. Late responses are dropped when the server selection changed
//    mid-flight (A50 discipline).
{
  let release;
  const gate = new Promise((resolve) => { release = resolve; });
  apiResponses = {
    '/api/onboarding/preflight?server=alpha': async () => {
      await gate;
      return { status: 200, ok: true, headers: new Headers({ 'Content-Type': 'application/json' }),
        text: async () => JSON.stringify({ data: dockerDownPreflight }) };
    },
    '/api/onboarding/preflight?server=beta': { ...healthyPreflight, id: 'srv-beta02', server: 'beta' },
  };
  const page = components.projectsPage();
  page.deployForm = { app: 'a', image: 'i', domain: 'd', server: 'alpha', port: 80 };
  const stale = page.loadPreflight();
  page.deployForm.server = 'beta';
  await page.loadPreflight();
  release();
  await stale;
  assert(page.preflight && page.preflight.server === 'beta', `stale alpha response must not overwrite beta: ${page.preflight && page.preflight.server}`);
}

// 7. Template tripwire: the deploy forms must actually render the readiness
//    panel and gate the button (a detached component cannot pass 1-6).
{
  const html = readFileSync(join(here, '../frontend/index.html'), 'utf8');
  const groupStart = html.indexOf('grp-deploy-app-');
  const groupEnd = html.indexOf('card-grid', groupStart);
  const projStart = html.indexOf('id="proj-deploy-app"');
  const projEnd = html.indexOf('card-grid', projStart);
  assert(groupStart !== -1 && projStart !== -1, 'deploy form sections not found in index.html');
  const forms = [html.slice(groupStart, groupEnd), html.slice(projStart, projEnd)];
  for (let i = 0; i < forms.length; i++) {
    const form = forms[i];
    assert(form.includes('preflightChecks'), `form ${i} must render the readiness checks (preflightChecks binding missing)`);
    assert(form.includes('preflightCheckClass'), `form ${i} must bind severity/result classes (preflightCheckClass missing)`);
    assert(form.includes('preflightError'), `form ${i} must render the readiness-unknown error state`);
    assert(form.includes('canDeploy'), `form ${i} must gate the Deploy button on canDeploy`);
    assert(form.includes('check-remediation'), `form ${i} must render remediation hints`);
  }
}

if (failures.length) {
  console.error(`FAIL (${failures.length}):`);
  for (const f of failures) console.error('  - ' + f);
  process.exit(1);
}
console.log('onboarding_preflight.test: all assertions passed');
