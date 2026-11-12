// "Protect my data" in the browser: against a stubbed API and the real vendored crypto.
import { test, before } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import { harness, response } from './identity_harness.mjs';

const here = dirname(fileURLToPath(import.meta.url));
const keyFile = readFileSync(join(here, 'testdata/age/test-key.txt'), 'utf8');
const prefix = readFileSync(join(here, 'testdata/age/prefix-70k.age')).toString('base64');

before(() => {
  Object.defineProperty(globalThis.crypto, 'subtle', { value: undefined, configurable: true });
  new Function(readFileSync(join(here, '../frontend/js/age.vendor.js'), 'utf8'))();
});

const baseView = (setup = {}, extra = {}) => ({
  cli_supported: true, recovery: {}, servers: [], rows: [], tests: [], intervals: ['24h'], presets: ['standard'],
  self_backup: { available: false, reason: 'x', runbook: 'docs/recovery.md' },
  setup: { supported: true, state: 'none', headline: 'Your data is not protected yet.', servers: [], recovery_doc: 'https://example/recovery.md', ...setup },
  ...extra,
});

function setupHarness(routes, setup = {}, extra = {}) {
  const h = harness('/backups');
  h.respond(url => {
    if (url === '/api/backups') return response(baseView(setup, extra));
    for (const [path, fn] of Object.entries(routes)) {
      if (url === path) return fn();
    }
    return response({}, { status: 404 });
  });
  return h;
}

const preflight = () => response({ servers: [
  { server: 'a', reachable: true, selected: true, binary_version: '0.1.55', free_bytes: 1 << 30, target_state: 'absent', tools: {} },
  { server: 'b', reachable: true, selected: false, target_state: 'configured', target_bucket: 'old', tools: {} },
  { server: 'c', reachable: true, selected: true, needs_tools: ['aws'], needs_upgrade: true, target_state: 'absent', tools: {} },
  { server: 'd', reachable: false, blocked: 'Dash could not reach this server', selected: false, tools: {} },
] });

test('the page offers the flow only on a CLI that supports it, and keeps the manual form', async () => {
  const h = setupHarness({});
  const p = h.page('backupsPage');
  await p.init();
  assert.equal(p.setupSupported(), true);
  const html = h.readHTML();
  assert.ok(html.includes('<summary>Advanced</summary>'));
  assert.ok(html.includes(`:open="!setupSupported()"`), 'an older CLI still gets its manual form open');
  assert.ok(html.includes('@submit.prevent="connectStorage()"'), 'the manual storage form is still there');
  assert.ok(html.includes('@submit.prevent="applyPolicy()"'), 'the manual policy form is still there');
  p.destroy();
});

test('preflight ticks what is safe: a server with a target and a blocked one start unticked', async () => {
  const h = setupHarness({ '/api/backups/setup/preflight': preflight });
  const p = h.page('backupsPage');
  await p.init();
  await p.openSetup();
  assert.equal(p.setupStep, 'check');
  assert.deepEqual(p.selectedServers(), ['a', 'c']);
  assert.equal(p.blockingNeeds().length, 2);
  // A person's choice survives the next check, and a blocked server stays unticked.
  p.setupTicked.c = false;
  p.setupTicked.b = true;
  await p.runPreflight();
  assert.deepEqual(p.selectedServers(), ['a', 'b']);
  assert.equal(p.blockingNeeds().length, 0);
  p.destroy();
});

test('tools install and upgrades go only for what was ticked', async () => {
  const h = setupHarness({ '/api/backups/setup/preflight': preflight });
  const p = h.page('backupsPage');
  await p.init();
  await p.openSetup();
  assert.equal(p.anyPrepareTicked(), false);
  p.setupInstall.c = true;
  assert.equal(p.anyPrepareTicked(), true);
  await p.prepareServers();
  const sent = h.requests.filter(r => r.url === '/api/backups/setup/preflight').at(-1).body;
  assert.deepEqual(sent.install_tools, ['c']);
  assert.deepEqual(sent.upgrade_binary, []);
  p.destroy();
});

test('one new server is the same flow for that server', async () => {
  const h = setupHarness({ '/api/backups/setup/preflight': () => response({ servers: [{ server: 'compute-3', reachable: true, selected: false, tools: {} }] }) });
  const p = h.page('backupsPage');
  await p.init();
  await p.openSetup('compute-3');
  assert.deepEqual(h.requests.filter(r => r.url === '/api/backups/setup/preflight').at(-1).body.servers, ['compute-3']);
  assert.deepEqual(p.selectedServers(), ['compute-3']);
  p.destroy();
});

test('the key check decides what can go on', async () => {
  let check = { missing: ['writeKeys'], buckets: [], can_create_bucket: true };
  const h = setupHarness({ '/api/backups/setup/storage/check': () => response(check) });
  const p = h.page('backupsPage');
  await p.init();
  p.setupKey = { id: ' KEY ', secret: ' SECRET ' };
  await p.checkKey();
  const sent = h.requests.filter(r => r.url === '/api/backups/setup/storage/check').at(-1).body;
  assert.equal(sent.key_id, 'KEY');
  assert.equal(p.canApplyStorage(), false, 'missing capabilities stop it');
  check = { missing: [], buckets: [{ name: 'x', refusals: ['no'], warnings: [] }, { name: 'y', refusals: [], warnings: ['careful'] }], can_create_bucket: true };
  await p.checkKey();
  assert.equal(p.canApplyStorage(), true, 'a new bucket is fine');
  p.setupBucket = 'x';
  assert.equal(p.canApplyStorage(), false, 'a refused bucket is not');
  p.setupBucket = 'y';
  assert.equal(p.canApplyStorage(), false, 'a warning needs reading');
  p.setupAccept = true;
  assert.equal(p.canApplyStorage(), true);
  check = { missing: [], capped: true, listing_error: 'daily transaction cap', buckets: [], can_create_bucket: true };
  await p.checkKey();
  assert.equal(p.canApplyStorage(), false, 'a capped listing shows why and stops');
  p.destroy();
});

test('a key limited to buckets starts on its first bucket', async () => {
  const h = setupHarness({ '/api/backups/setup/storage/check': () => response({ missing: [], buckets: [{ name: 'only', refusals: [], warnings: [] }], can_create_bucket: false }) });
  const p = h.page('backupsPage');
  await p.init();
  p.setupKey = { id: 'K', secret: 'S' };
  await p.checkKey();
  assert.equal(p.setupBucket, 'only');
  p.destroy();
});

test('applying sends the ticked servers, and a finished run forgets the key', async () => {
  let reply = { all_ok: false, servers: [{ server: 'a', verified: false, key_kept: true, error: 'timeout' }], key_note: 'n', warnings: [] };
  const h = setupHarness({
    '/api/backups/setup/preflight': preflight,
    '/api/backups/setup/storage/check': () => response({ missing: [], buckets: [], can_create_bucket: true }),
    '/api/backups/setup/storage/apply': () => response(reply),
  });
  const p = h.page('backupsPage');
  await p.init();
  await p.openSetup();
  p.setupTicked = { a: true };
  p.setupKey = { id: 'KEYID', secret: 'APPKEY' };
  p.setupPassword = 'pw';
  await p.checkKey();
  await p.applyStorage();
  const sent = h.requests.filter(r => r.url === '/api/backups/setup/storage/apply').at(-1).body;
  assert.deepEqual(sent.servers, ['a']);
  assert.equal(sent.application_key, 'APPKEY');
  assert.equal(p.setupKey.secret, 'APPKEY', 'a partial run keeps it in memory to retry');
  assert.match(p.storageResultLine(reply.servers[0]), /kept every key/);
  assert.equal(p.setupPassword, '');
  reply = { all_ok: true, servers: [{ server: 'a', verified: true }], key_note: 'n', warnings: [] };
  p.setupPassword = 'pw';
  await p.applyStorage();
  assert.equal(p.setupKey.secret, '');
  p.setupKey = { id: 'A', secret: 'B' };
  p.closeSetup();
  assert.equal(p.setupKey.secret, '', 'closing forgets it');
  p.destroy();
});

test('the recovery key: only its public half reaches Dash, after the saved file is checked', async () => {
  const h = setupHarness({ '/api/backups/setup/recovery': () => response({}) });
  const p = h.page('backupsPage');
  await p.init();
  p.setupStorage = { bucket: 'my-bucket', endpoint: 'https://s3.us-west-004.backblazeb2.com', base: 'teploy' };
  await p.generateRecovery();
  assert.ok(p.recoveryGen.fileText.includes('# bucket: my-bucket'));
  assert.ok(p.recoveryGen.fileText.includes('# endpoint: https://s3.us-west-004.backblazeb2.com'));
  assert.ok(p.recoveryGen.fileText.includes('# base: teploy'));

  await p.saveRecovery();
  assert.equal(h.requests.filter(r => r.url === '/api/backups/setup/recovery').length, 0, 'not before the file is checked');

  await p.checkRecoveryFile(keyFile);
  assert.equal(p.recoveryProven, false);
  assert.match(p.recoveryError, /different key/);

  const publicKey = p.recoveryGen.publicKey;
  await p.checkRecoveryFile(p.recoveryGen.fileText);
  assert.equal(p.recoveryProven, true);
  await p.saveRecovery();
  const sent = h.requests.filter(r => r.url === '/api/backups/setup/recovery').at(-1).body;
  assert.equal(sent.public_key, publicKey);
  assert.equal(p.recoveryGen, null);
  for (const r of h.requests) assert.ok(!JSON.stringify(r.body || {}).includes('AGE-SECRET-KEY'), `${r.url} carried the private key`);
  p.destroy();
});

test('the key is a file, because checking and proving only take a file', () => {
  const h = setupHarness({});
  const html = h.readHTML();
  assert.ok(html.includes('Download the key file'));
  assert.ok(!html.includes('copyRecovery'));
  assert.ok(!html.includes('password manager'));
});

test('closing with an unsaved key warns that its file is no use', async () => {
  const h = setupHarness({});
  const p = h.page('backupsPage');
  await p.init();
  p.setupOpen = true;
  p.recoveryGen = { publicKey: 'age1x', fileText: 'x', fileName: 'y' };
  const seen = [];
  globalThis.confirm = text => { seen.push(text); return false; };
  await p.closeSetup();
  assert.equal(p.setupOpen, true);
  assert.match(seen[0], /delete it/);
  globalThis.confirm = () => true;
  await p.closeSetup();
  assert.equal(p.setupOpen, false);
  assert.equal(p.recoveryGen, null);
  delete globalThis.confirm;
  p.destroy();
});

test('M11: the suggested bucket has its warnings and its confirm box', async () => {
  const h = setupHarness({ '/api/backups/setup/storage/check': () => response({ missing: [], suggested_bucket: 'mine', can_create_bucket: true, buckets: [{ name: 'mine', refusals: [], warnings: ['careful'] }] }) });
  const p = h.page('backupsPage');
  await p.init();
  p.setupKey = { id: 'K', secret: 'S' };
  await p.checkKey();
  assert.equal(p.setupBucket, '');
  assert.equal(p.chosenBucket().name, 'mine');
  assert.equal(p.canApplyStorage(), false);
  p.setupAccept = true;
  assert.equal(p.canApplyStorage(), true);
  p.destroy();
});

test('M9: replacing a server key needs an explicit yes', async () => {
  const h = setupHarness({ '/api/backups/setup/preflight': preflight, '/api/backups/setup/storage/check': () => response({ missing: [], buckets: [], can_create_bucket: true }) });
  const p = h.page('backupsPage');
  await p.init();
  await p.openSetup();
  p.setupTicked = { a: true, b: true };
  p.setupKey = { id: 'K', secret: 'S' };
  await p.checkKey();
  assert.deepEqual(p.replaceConsentNeeded(), ['b']);
  assert.equal(p.canApplyStorage(), false);
  p.setupReplaceOK = true;
  assert.equal(p.canApplyStorage(), true);
  assert.ok(h.readHTML().includes('Dash cannot put the old one back'));
  p.destroy();
});

test('H3: the way to a key is plain steps, and a key missing permissions gets two ways forward', () => {
  const html = harness('/backups').readHTML();
  assert.ok(!html.includes('b2 key create'), 'not only the command line');
  assert.ok(html.includes('Add a New Application Key'));
  assert.ok(html.includes('Master Application Key'));
  assert.ok(html.includes('Dash cannot delete it'));
  assert.ok(html.includes('that would break anything else using it'));
  assert.ok(html.includes('Set up by hand'));
});

test('re-auth follows how this viewer signed in', async () => {
  for (const [mode, want] of [['password', 'password'], ['local_account', 'local_account'], [undefined, 'none']]) {
    const h = setupHarness({}, { reauth: mode });
    const p = h.page('backupsPage');
    await p.init();
    assert.equal(p.reauthMode(), want);
    p.destroy();
  }
  assert.ok(harness('/backups').readHTML().includes('sign in with a local admin account'));
});

test('a leftover notice can be dismissed', async () => {
  const h = setupHarness({ '/api/backups/setup/leftovers/dismiss': () => response({ ok: true }) });
  const p = h.page('backupsPage');
  await p.init();
  await p.dismissLeftover('K1');
  assert.deepEqual(h.requests.filter(r => r.url === '/api/backups/setup/leftovers/dismiss').at(-1).body, { key_id: 'K1' });
  p.destroy();
});

test('M5: Continue goes to the proof only when nothing is waiting to be set up', async () => {
  const h = setupHarness({ '/api/backups/setup/preflight': preflight }, { turned_on: true, servers: [{ server: 'a', state: 'not_protected' }] });
  const p = h.page('backupsPage');
  await p.init();
  await p.openSetup();
  assert.equal(p.setupStep, 'check');
  p.destroy();
  const h2 = setupHarness({}, { turned_on: true, servers: [{ server: 'a', state: 'waiting_first_backup' }] });
  const p2 = h2.page('backupsPage');
  await p2.init();
  await p2.openSetup();
  assert.equal(p2.setupStep, 'prove');
  p2.destroy();
});

test('M10: a key that worked but could not be recorded is not a key that failed', async () => {
  let fail = true;
  const h = setupHarness({
    '/api/backups/setup/proof/start': () => response({ server: 'a', key: 's3://b/k.age', length: 71680, data_base64: prefix }),
    '/api/backups/setup/proof/result': () => (fail ? response({ error: 'down' }, { raw: true, status: 502 }) : response({ recorded: true })),
  });
  const p = h.page('backupsPage');
  await p.init();
  await p.proveWith(keyFile);
  assert.deepEqual(p.proofUnrecorded, { key: 's3://b/k.age' });
  assert.equal(p.proofState, null);
  assert.ok(!/did not open your backup/.test(p.setupError));
  assert.equal(h.requests.filter(r => r.url === '/api/backups/setup/proof/result').every(r => r.body.passed), true, 'never recorded as a failure');
  fail = false;
  await p.retryProofRecord();
  assert.equal(p.proofUnrecorded, null);
  assert.deepEqual(p.proofState, { passed: true, key: 's3://b/k.age' });
  p.destroy();
});

test('a bad file and an unreadable backup are not blamed on the key', async () => {
  const h = setupHarness({ '/api/backups/setup/proof/start': () => response({ error: 'server down' }, { raw: true, status: 502 }) });
  const p = h.page('backupsPage');
  await p.init();
  await p.proveWith('hello');
  assert.match(p.setupError, /does not look like/);
  await p.proveWith(keyFile);
  assert.match(p.setupError, /server down/);
  p.destroy();
});

test('proving recovery decrypts the real prefix and records a pass', async () => {
  const h = setupHarness({
    '/api/backups/setup/proof/start': () => response({ server: 'a', key: 's3://b/k.age', length: 71680, data_base64: prefix }),
    '/api/backups/setup/proof/result': () => response({ recorded: true }),
  });
  const p = h.page('backupsPage');
  await p.init();
  await p.proveWith(keyFile);
  assert.deepEqual(p.proofState, { passed: true, key: 's3://b/k.age' });
  const sent = h.requests.filter(r => r.url === '/api/backups/setup/proof/result').at(-1).body;
  assert.deepEqual(sent, { key: 's3://b/k.age', passed: true });
  for (const r of h.requests) assert.ok(!JSON.stringify(r.body || {}).includes('AGE-SECRET-KEY'));
  p.destroy();
});

test('proving with the wrong key records a failure and says what it means', async () => {
  const h = setupHarness({
    '/api/backups/setup/proof/start': () => response({ server: 'a', key: 's3://b/k.age', length: 71680, data_base64: prefix }),
    '/api/backups/setup/proof/result': () => response({ recorded: true }),
  });
  const p = h.page('backupsPage');
  await p.init();
  const other = await globalThis.TeployAge.generate();
  await p.proveWith(`# a comment\n${other.identity}\n`);
  assert.equal(p.proofState, null);
  assert.match(p.setupError, /did not open your backup/);
  assert.equal(h.requests.filter(r => r.url === '/api/backups/setup/proof/result').at(-1).body.passed, false);
  p.destroy();
});

test('alerts use the settings channel API with a test send, and can be skipped', async () => {
  const seen = [];
  const h = harness('/backups');
  h.respond((url, options) => {
    seen.push([options.method, url]);
    if (url === '/api/backups') return response(baseView());
    if (url.endsWith('/test')) return response({ ok: true });
    return response({});
  });
  const p = h.page('backupsPage');
  await p.init();
  p.alertForm.url = 'https://ntfy.sh/t';
  await p.saveAlert();
  assert.deepEqual(p.alertTest, { ok: true });
  assert.ok(seen.some(([m, u]) => m === 'PUT' && u.startsWith('/api/notifications/channels/ntfy-')));
  assert.equal(h.requests.find(r => r.method === 'PUT').body.url, 'https://ntfy.sh/t');
  await p.skipAlerts();
  assert.ok(seen.some(([, u]) => u === '/api/backups/setup/alerts/skip'));
  p.destroy();
});

test('turning on names the ticked servers and the start choice, and unticking skips', async () => {
  const h = setupHarness({
    '/api/backups/setup/coverage': () => response({ servers: [{ server: 'a', items: [{ name: 'blog/volumes', kind: 'volumes', app: 'blog', size_bytes: 2048 }], unmanaged: [{ name: 'pg' }, { name: 'scratch' }], not_coverable: [] }], multiplier: 39, interval: '24h', preset: 'standard', volumes: false }),
    '/api/backups/setup/turn-on': () => response({ all_ok: true, servers: [{ server: 'a', ok: true, run: { status: 'started' } }] }),
  });
  const p = h.page('backupsPage');
  await p.init();
  p.setupTicked = { a: true };
  await p.loadCoverage();
  assert.equal(p.setupCoverage.multiplier, 39);
  assert.equal(p.setupPolicy.skip_volumes, true, 'starts from what is saved');
  assert.equal(p.isSkipped('a', 'pg'), false, 'ticked by default');
  assert.match(p.coverageLine(p.setupCoverage.servers[0].items[0]), /blog files, 2\.0 KB/);
  p.setSkipped('a', 'scratch', true);
  await p.turnOn('now');
  const sent = h.requests.filter(r => r.url === '/api/backups/setup/turn-on').at(-1).body;
  assert.deepEqual(sent.servers, ['a']);
  assert.equal(sent.start, 'now');
  assert.deepEqual(sent.skip, { a: ['scratch'] });
  assert.equal(sent.interval, '24h');
  assert.match(p.turnOnLine(p.turnOnResult.servers[0]), /backing up now/);
  p.destroy();
});

test('retiring needs an explicit choice', async () => {
  const h = setupHarness({ '/api/backups/setup/retire': () => response({ notes: [] }) });
  const p = h.page('backupsPage');
  await p.init();
  p.beginRetire('old');
  await p.submitRetire();
  assert.match(p.setupError, /keep or remove/);
  assert.equal(h.requests.filter(r => r.url === '/api/backups/setup/retire').length, 0);
  p.retireData = 'keep';
  p.setupKey = { id: 'K', secret: 'S' };
  await p.submitRetire();
  assert.equal(h.requests.filter(r => r.url === '/api/backups/setup/retire').at(-1).body.data, 'keep');
  assert.equal(p.retireFor, null);
  assert.equal(p.setupKey.secret, '');
  p.destroy();
});

test('plain HTTP outside localhost and a tailnet warns before a key is pasted', () => {
  const h = setupHarness({});
  const p = h.page('backupsPage');
  const cases = [['http:', 'dash.example.com', true], ['https:', 'dash.example.com', false], ['http:', 'localhost', false],
    ['http:', '127.0.0.1', false], ['http:', '100.108.1.2', false], ['http:', 'infra.tail1234.ts.net', false], ['http:', '100.200.1.2', true], ['http:', '192.168.1.5', true]];
  for (const [protocol, hostname, want] of cases) {
    globalThis.location = { pathname: '/backups', search: '', protocol, hostname };
    assert.equal(p.insecurePage(), want, `${protocol}//${hostname}`);
  }
});

test('states read in plain words', () => {
  const h = setupHarness({});
  const p = h.page('backupsPage');
  assert.equal(p.setupStateClass('protected'), 'green');
  assert.equal(p.setupStateClass('failing'), 'red');
  assert.equal(p.setupStateLabel('capped'), 'daily cap reached');
  assert.equal(p.setupStateLabel('restore_test_overdue'), 'restore test overdue');
  assert.equal(p.setupStateClass('not_covered'), 'red');
  assert.equal(p.setupStateClass('all_skipped'), 'gray');
  assert.match(p.turnOnLine({ server: 'a', skipped: true }), /skipped, as you chose/);
  assert.equal(p.canBackUpNow({ state: 'late' }), true);
  assert.equal(p.canBackUpNow({ state: 'protected' }), false);
});

test('the hand-written-jobs list shows only when some are found', async () => {
  const h = setupHarness({});
  const p = h.page('backupsPage');
  await p.init();
  assert.equal(p.hasHandwritten(), false);
  p.view.handwritten = [{ name: 'omni-backup', where: 'cron' }];
  assert.equal(p.hasHandwritten(), true);
  assert.ok(h.readHTML().includes('x-show="hasHandwritten()"'));
  p.destroy();
});

test('the home page says so until protected, and a failed read says nothing', async () => {
  const h = harness('/');
  let view = baseView();
  const ok = (url) => url === '/api/backups' ? response(view) : response({});
  h.respond(ok);
  const home = h.components.homeStatus();
  await home.init();
  const protect = () => home.attention().find((r) => r.key === 'protect');
  assert.ok(protect(), 'unprotected: the protect row shows');
  view = baseView({ state: 'protected' });
  await home.loadOne('backups');
  assert.equal(protect(), undefined);
  view = baseView({ supported: false });
  await home.loadOne('backups');
  assert.equal(protect(), undefined);
  // A failed first read shows no protect row, and names backups as unchecked.
  const fresh = h.components.homeStatus();
  h.respond((url) => url === '/api/backups' ? response({}, { status: 500 }) : response({}));
  await fresh.init();
  assert.equal(fresh.attention().find((r) => r.key === 'protect'), undefined);
  assert.ok(fresh.unchecked().some((s) => s.key === 'backups'), 'a failed backups read is named as unchecked');
  home.destroy(); fresh.destroy();
  assert.ok(h.readHTML().includes('x-data="homeStatus"'));
});

test('no emoji in the page, and the layout holds at phone width', () => {
  const root = join(here, '../frontend');
  const emoji = /\p{Extended_Pictographic}/u;
  const js = readFileSync(join(root, 'js/app.js'), 'utf8');
  const start = js.indexOf('function backupSetupComponent');
  const end = js.indexOf('// ── Backups Page (plan P2)');
  assert.ok(start > 0 && end > start);
  assert.equal(emoji.test(js.slice(start, end)), false);
  const html = readFileSync(join(root, 'index.html'), 'utf8');
  const section = html.slice(html.indexOf('<!-- Protect my data'), html.indexOf('<!-- First-run checklist'));
  assert.equal(emoji.test(section), false);
  // The old fixed 340px grid track overflowed a phone.
  assert.ok(html.includes('minmax(min(340px,100%),1fr)'));
  assert.ok(!html.includes('minmax(340px,1fr)'));
  const css = readFileSync(join(root, 'css/style.css'), 'utf8');
  assert.ok(css.includes('.setup-step input:not([type="checkbox"]):not([type="radio"]), .setup-step select { width: 100%; max-width: 100%; }'));
});

test('the home list stays a glance: one row before setup, grouped after, capped', async () => {
  const h = harness('/');
  const rows = [];
  for (let i = 0; i < 12; i++) rows.push({ server: i < 9 ? 'a' : 'b', name: 'app' + i + '/volumes', state: 'uncovered' });
  rows.push({ server: 'a', name: 'ship/postgres', state: 'failing', last_error: 'upload failed' });
  const open = rows.map((r) => ({ key: 'backup:' + r.server + ':uncovered:' + r.name, source: 'backup', name: r.name + ' on ' + r.server + ' is not covered', severity: 'warning' }));
  open.push({ key: 'm1', source: 'monitor', name: 'Shop', severity: 'critical' });
  let view = baseView({}, { rows });
  h.respond((url) => url === '/api/backups' ? response(view) : url === '/api/alerts/incidents' ? response({ open }) : response({}));
  const home = h.components.homeStatus();
  await home.init();
  let keys = home.attention().map((r) => r.key);
  assert.deepEqual(keys, ['inc:m1', 'protect', 'bk:a/ship/postgres'], 'before setup: the monitor, one protect row, and the failing backup');
  view = baseView({ state: 'partial', headline: 'One of two servers is protected.' }, { rows });
  await home.loadOne('backups');
  const att = home.attention();
  keys = att.map((r) => r.key);
  assert.deepEqual(keys, ['inc:m1', 'protect', 'bk:a/ship/postgres', 'bk-uncovered:a', 'bk-uncovered:b'], 'after setup: uncovered targets are one row per server');
  assert.equal(att[3].title, '9 targets on a are not backed up');
  assert.equal(att[3].detail, 'app0/volumes, app1/volumes, app2/volumes and 6 more');
  // A failed backups read keeps the backup incidents, so nothing goes unsaid.
  const fresh = h.components.homeStatus();
  h.respond((url) => url === '/api/backups' ? response({}, { status: 500 }) : url === '/api/alerts/incidents' ? response({ open }) : response({}));
  await fresh.init();
  assert.equal(fresh.attention().length, open.length);
  assert.equal(fresh.attentionShown().length, 8);
  assert.equal(fresh.attentionHidden(), open.length - 8);
  fresh.showAllAttention = true;
  assert.equal(fresh.attentionShown().length, open.length);
  home.destroy(); fresh.destroy();
  const html = h.readHTML();
  assert.ok(html.includes('attentionShown()') && html.includes("'Show ' + attentionHidden() + ' more'"));
});
