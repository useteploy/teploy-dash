// Component test for the D4 repository tab: the connect wizard's config
// write (git: block through the CAS PUT, merge retry), the host-key
// confirmation gate before any webhook registration, the typed-ref
// watermark reset, and the static wiring of the tab itself.
//
// Repo convention (no build step, zero dependencies): loads js/app.js under
// stubbed browser globals and drives appDetailPage against stubbed API
// responses.
//
// Run: node cmd/teploy-dash/frontend-test/git_source_card.test.mjs
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
let lastConfirm = null;
let confirmAnswer = true;
let lastPrompt = null;
let promptAnswer = '';
globalThis.document = {
  addEventListener: (type, fn) => { if (type === 'alpine:init') alpineInitListeners.push(fn); },
  getElementById: () => ({ style: {}, appendChild: (el) => { lastToast = el.textContent; } }),
  createElement: () => ({ style: {}, classList: { add() {}, remove() {} }, setAttribute() {}, remove() {}, appendChild() {} }),
  documentElement: { setAttribute() {}, getAttribute: () => 'dark' },
};
globalThis.window = { addEventListener() {}, dispatchEvent() {}, location: { origin: 'https://dash.example' } };
globalThis.localStorage = { getItem: () => null, setItem() {} };
globalThis.history = { pushState() {}, replaceState() {} };
globalThis.location = { pathname: '/apps/prod/web', search: '' };
globalThis.CustomEvent = class CustomEvent {};
globalThis.confirm = (msg) => { lastConfirm = msg; return confirmAnswer; };
globalThis.prompt = (msg, initial) => { lastPrompt = msg; return promptAnswer; };

let routes = {};
const requests = [];
globalThis.fetch = async (url, init) => {
  const method = (init && init.method) || 'GET';
  const body = init && init.body !== undefined ? JSON.parse(init.body) : undefined;
  requests.push({ method, url, body });
  const handler = routes[`${method} ${url}`] !== undefined ? routes[`${method} ${url}`] : routes[url];
  if (handler === undefined) {
    return { status: 404, ok: false, headers: new Headers({ 'Content-Type': 'text/plain' }), text: async () => 'not found' };
  }
  if (typeof handler === 'function') return handler(body);
  return {
    status: 200,
    ok: true,
    headers: new Headers({ 'Content-Type': 'application/json' }),
    text: async () => JSON.stringify({ data: handler }),
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

function freshPage() {
  const page = components.appDetailPage();
  page.resource = { server: 'prod', name: 'web' };
  page.app = { name: 'web', server: 'prod' };
  page.appPath = () => '/apps/prod/web';
  page.features = { git_source: true, trigger_poll: true, config_home: true };
  page.configAuthority = { home: 'teploy' };
  return page;
}

function requestsFor(method, urlPart) {
  return requests.filter((r) => r.method === method && r.url.includes(urlPart));
}

// 1. setGitBlock: appends the block, or replaces an existing one (block or
//    flow style) without touching the rest of the config.
{
  const page = freshPage();
  const appended = page.setGitBlock('app: web\nimage: example/web:1\n', 'git@github.com:owner/repo.git', 'main');
  assert(appended === 'app: web\nimage: example/web:1\ngit:\n  repository: git@github.com:owner/repo.git\n  branch: main\n', `appended = ${JSON.stringify(appended)}`);

  const replaced = page.setGitBlock('app: web\ngit:\n  repository: https://old.example/x.git\n  branch: dev\n  poll: 10m\nimage: example/web:1\n', 'https://new.example/y.git', 'trunk');
  assert(replaced === 'app: web\ngit:\n  repository: https://new.example/y.git\n  branch: trunk\nimage: example/web:1\n', `replaced = ${JSON.stringify(replaced)}`);

  const flow = page.setGitBlock('app: web\ngit: {repository: https://old.example/x.git, branch: dev}\nimage: example/web:1\n', 'https://new.example/y.git', 'main');
  assert(flow === 'app: web\ngit:\n  repository: https://new.example/y.git\n  branch: main\nimage: example/web:1\n', `flow replaced = ${JSON.stringify(flow)}`);

  const empty = page.setGitBlock('', 'https://new.example/y.git', 'main');
  assert(empty === 'git:\n  repository: https://new.example/y.git\n  branch: main\n', `empty = ${JSON.stringify(empty)}`);
}

// 2. The connect wizard writes the git: block through the CAS config PUT
//    and then generates the deploy key.
{
  const page = freshPage();
  routes = {
    'GET /apps/prod/web/config': { home: 'teploy', commit: 'a'.repeat(40), content: 'app: web\nimage: example/web:1\n' },
    'PUT /apps/prod/web/config': { home: 'teploy', commit: 'b'.repeat(40), content: 'app: web\n' },
    'POST /apps/prod/web/source/key': { machine_interface: 2, type: 'ssh', public_key: 'ssh-ed25519 AAAA key', fingerprint: 'SHA256:abc', created: true },
  };
  requests.length = 0;
  page.gitConnectStart();
  page.gitForm = { forge: 'github', repository: 'git@github.com:owner/repo.git', branch: 'main' };
  await page.gitConnectSubmit();
  const puts = requestsFor('PUT', '/apps/prod/web/config');
  assert(puts.length === 1, `config PUTs = ${puts.length}`);
  assert(puts[0].body.expect === 'a'.repeat(40), 'config PUT must be compare-and-swap against the loaded commit');
  assert(puts[0].body.content.includes('git:\n  repository: git@github.com:owner/repo.git\n  branch: main\n'), `config content = ${JSON.stringify(puts[0].body.content)}`);
  const keys = requestsFor('POST', '/apps/prod/web/source/key');
  assert(keys.length === 1 && keys[0].body.type === 'ssh', 'deploy key generated after the config write');
  assert(page.gitStep === 'hostkey' && page.gitKeyInfo && page.gitKeyInfo.fingerprint === 'SHA256:abc', `wizard state = ${page.gitStep}`);
  assert(page.gitError === '', `unexpected error: ${page.gitError}`);
}

// 3. A 409 merge is retried once against the merged content and commit.
{
  const page = freshPage();
  let putCalls = 0;
  routes = {
    'GET /apps/prod/web/config': { home: 'teploy', commit: 'a'.repeat(40), content: 'app: web\n' },
    'PUT /apps/prod/web/config': (body) => {
      putCalls++;
      if (putCalls === 1) {
        return {
          status: 409,
          ok: false,
          headers: new Headers({ 'Content-Type': 'application/json' }),
          text: async () => JSON.stringify({ error: 'conflict', merge: { merged: 'app: web\nreplicas: 2\n', current: 'c'.repeat(40) } }),
        };
      }
      return { status: 200, ok: true, headers: new Headers({ 'Content-Type': 'application/json' }), text: async () => JSON.stringify({ data: { home: 'teploy', commit: 'd'.repeat(40) } }) };
    },
    'POST /apps/prod/web/source/key': { type: 'ssh', public_key: 'k', fingerprint: 'SHA256:abc' },
  };
  requests.length = 0;
  page.gitConnectStart();
  page.gitForm = { forge: 'github', repository: 'git@github.com:owner/repo.git', branch: 'main' };
  await page.gitConnectSubmit();
  const puts = requestsFor('PUT', '/apps/prod/web/config');
  assert(puts.length === 2, `config PUTs after merge retry = ${puts.length}`);
  assert(puts[1].body.expect === 'c'.repeat(40) && puts[1].body.content.includes('replicas: 2'), 'retry must build on the merged content');
  assert(page.gitStep === 'hostkey', `wizard state after merge = ${page.gitStep}`);
}

// 4. The host-key confirmation gate: no webhook registration until the
//    pinned fingerprints are confirmed; the registration carries the form
//    and the browser's own origin as the webhook base.
{
  const page = freshPage();
  routes = {
    'POST /apps/prod/web/source/host-key': { machine_interface: 2, type: 'host-key', created: true, host_key_fingerprints: ['SHA256:first', 'SHA256:second'] },
    'POST /apps/prod/web/source/register': { registered: true, source_id: 'src-0123456789abcdef', webhook_url: 'https://dash.example/hooks/sources/src-0123456789abcdef' },
  };
  page.gitConnectStart();
  page.gitForm = { forge: 'forgejo', repository: 'https://forge.example/owner/repo.git', branch: 'main' };
  page.gitKeyInfo = { public_key: 'ssh-ed25519 AAAA key', fingerprint: 'SHA256:abc' };
  await page.gitPinHostKey('');
  assert(page.gitHostKeys && page.gitHostKeys.host_key_fingerprints.length === 2, 'host keys pinned');

  confirmAnswer = false;
  await page.gitConfirmHostKeys();
  assert(requestsFor('POST', '/apps/prod/web/source/register').length === 0, 'a declined fingerprint confirmation must not register');
  assert(lastConfirm && lastConfirm.includes('SHA256:first') && lastConfirm.includes('SHA256:second'), 'confirmation must show the pinned fingerprints');

  confirmAnswer = true;
  await page.gitConfirmHostKeys();
  const registers = requestsFor('POST', '/apps/prod/web/source/register');
  assert(registers.length === 1, `register posts = ${registers.length}`);
  assert(registers[0].body.forge === 'forgejo' && registers[0].body.repository === 'https://forge.example/owner/repo.git', `register body = ${JSON.stringify(registers[0].body)}`);
  assert(registers[0].body.webhook_base === 'https://dash.example', 'webhook base must be the dashboard origin');
  assert(registers[0].body.public_key === 'ssh-ed25519 AAAA key', 'register must carry the generated public key');
  assert(page.gitStep === 'done' && page.gitRegister && page.gitRegister.registered, `done state = ${page.gitStep}`);

  // The paste flow: registered=false surfaces the once-only secret.
  page.gitStep = 'hostkey';
  routes['POST /apps/prod/web/source/register'] = { registered: false, source_id: 'src-0123456789abcdef', webhook_url: 'https://dash.example/hooks/sources/src-0123456789abcdef', public_key: 'ssh-ed25519 AAAA key', webhook_secret: 'whsec-once' };
  await page.gitConfirmHostKeys();
  assert(page.gitRegister && page.gitRegister.registered === false && page.gitRegister.webhook_secret === 'whsec-once', 'paste kit must expose the once-only secret');
}

// 5. The watermark reset requires the ref typed back exactly.
{
  const page = freshPage();
  routes = { 'POST /apps/prod/web/source/reset-watermark': { machine_interface: 2, app: 'web', ref: 'refs/heads/main', reset: true } };
  page.gitShow = { repository: 'https://forge.example/owner/repo.git', branch: 'main', trigger: { ref: 'refs/heads/main', watermark: 'a'.repeat(40), refused_head: 'b'.repeat(40) } };
  requests.length = 0;

  promptAnswer = 'main';
  await page.gitResetWatermark();
  assert(requestsFor('POST', '/apps/prod/web/source/reset-watermark').length === 0, 'a mistyped ref must not reset');
  assert(lastToast && lastToast.includes('Type the ref'), `mistyped toast = ${lastToast}`);

  promptAnswer = 'refs/heads/main';
  await page.gitResetWatermark();
  const resets = requestsFor('POST', '/apps/prod/web/source/reset-watermark');
  assert(resets.length === 1 && resets[0].body.ref === 'refs/heads/main' && resets[0].body.confirm === 'refs/heads/main', `reset body = ${JSON.stringify(resets[0] && resets[0].body)}`);

  promptAnswer = null; // cancelled
  await page.gitResetWatermark();
  assert(requestsFor('POST', '/apps/prod/web/source/reset-watermark').length === 1, 'a cancelled prompt must not reset');
}

// 6. Status load and the missing-timer offer.
{
  const page = freshPage();
  routes = {
    'GET /apps/prod/web/source': { machine_interface: 2, repository: 'https://forge.example/owner/repo.git', branch: 'main', poll: '5m', credential: { type: 'ssh', fingerprint: 'SHA256:abc' }, trigger: { ref: 'refs/heads/main', watermark: 'c'.repeat(40) } },
    'GET /apps/prod/web/source/timer': { installed: false, known: true, detail: 'git-source apps (web) but no teploy-trigger-poll.timer' },
  };
  await page.loadGitSource();
  assert(page.gitShow && page.gitSourceConnected() && page.gitShow.credential.fingerprint === 'SHA256:abc', 'status must come from config source show');
  assert(page.gitTimerMissing(), 'missing timer must be detectable');

  routes['POST /apps/prod/web/source/install-timer'] = { machine_interface: 2, host: '192.0.2.10', timer: 'teploy-trigger-poll.timer' };
  routes['GET /apps/prod/web/source/timer'] = { installed: true, known: true, detail: 'installed' };
  requests.length = 0;
  await page.gitInstallTimer();
  assert(requestsFor('POST', '/apps/prod/web/source/install-timer').length === 1, 'install timer must post');
  assert(!page.gitTimerMissing(), 'timer state must refresh after install');
}

// 7. Static tripwires: the tab is gated on the git-source token and a
//    Teploy-home app, and the panel wires the wizard and status actions
//    (a detached template must not pass 1-6).
{
  const html = readFileSync(join(here, '../frontend/index.html'), 'utf8');
  const tabStart = html.indexOf('id="app-tab-repository"');
  const panelStart = html.indexOf('id="app-panel-repository"');
  const panelEnd = html.indexOf('id="app-panel-activity"');
  assert(tabStart !== -1 && panelStart !== -1 && panelEnd > panelStart, 'repository tab/panel not found in index.html');
  const tabButton = html.slice(tabStart - 400, tabStart + 300);
  assert(tabButton.includes("hasFeature('git_source') && teployHome()"), 'tab must hide without the git-source token or on non-Teploy homes');
  const panel = html.slice(panelStart, panelEnd);
  for (const wired of ['gitConnectSubmit()', 'gitPinHostKey', 'gitConfirmHostKeys()', 'gitResetWatermark()', 'gitRetry()', 'gitInstallTimer()']) {
    assert(panel.includes(wired), `panel must wire ${wired}`);
  }
  assert(panel.includes('gitTimerMissing()'), 'panel must offer the timer install only when the doctor reports it missing');
  assert(panel.includes('refused_head'), 'panel must surface a refused force-push head');
}

if (failures.length) {
  console.error(`FAIL: ${failures.length} assertion(s):`);
  for (const f of failures) console.error(`  - ${f}`);
  process.exit(1);
}
console.log('ok git_source_card');
