import {test} from 'node:test';
import assert from 'node:assert/strict';
import {harness, response} from './identity_harness.mjs';

// The /api/siblings shape the page renders: per-server read state and one
// probe row per (server, product), honest unknowns included.
const candidate = (extra = {}) => ({
  server: 'infra', product: 'observe', containers: ['observe-web-7cf98cae', 'observe-worker-7cf98cae'],
  app: 'observe', label_version: '7cf98cae', container_state: 'running',
  addresses: ['100.108.123.49:3000'], state: 'confirmed',
  base_url: 'http://100.108.123.49:3000',
  doc: {product: 'observe', version: '0.2.0', instance_id: '9f2c0c0e6b6d4f5a', dashboard_url: 'https://observe.fylun.ai', ingest_url: 'https://ingest.fylun.ai', capabilities: ['service_tokens', 'sites_external_ref', 'otlp_http']},
  version: '0.2.0', probed_at: '2026-11-05T12:00:00Z', age_seconds: 240, recognized: true,
  ...extra,
});

const body = (extra = {}) => ({
  servers: [
    {server: 'infra', state: 'ok', last_attempt_at: '2026-11-05T12:00:00Z', checked_at: '2026-11-05T12:00:00Z', age_seconds: 240},
    {server: 'edge', state: 'partial', error: 'container listing failed part-way', last_attempt_at: '2026-11-05T12:00:00Z', age_seconds: 240},
  ],
  candidates: [
    candidate(),
    candidate({server: 'infra', product: 'ship', state: 'found_not_discoverable', error: '100.108.123.51:3000 answered 404', doc: undefined, base_url: '', addresses: ['100.108.123.51:3000']}),
    candidate({server: 'edge', product: 'forgejo', state: 'not_probeable', error: 'loopback-only bind on the server, not reachable from dash', doc: undefined, addresses: [], version: '11.0.1'}),
    candidate({server: 'edge', product: 'lullmail', state: 'blocked_internal', error: 'target is on a private/tailnet address — enable "allow internal" for sibling probing', doc: undefined, recognized: false, addresses: ['100.108.123.60:8080']}),
  ],
  allow_internal: false, cli_supported: true, cli_version: 'v0.1.53', min_cli_version: 'v0.1.53',
  ...extra,
});

test('the /siblings route renders the page and the nav names it', async () => {
  const h = harness('/siblings'); h.stores.router.restore();
  assert.equal(h.stores.router.page, 'siblings');
  const html = h.readHTML();
  assert.ok(html.includes(`x-if="$store.router.page === 'siblings'"`));
  assert.ok(html.includes('>Siblings</a>'));
  assert.ok(html.includes('x-data="siblingsPage"'));
});

test('probe verdicts render their honest labels, unknown products included', async () => {
  const h = harness('/siblings');
  h.respond(url => response(body()));
  const p = h.page('siblingsPage');
  try {
    await p.init();
  assert.equal(p.candidates.length, 4);
  assert.equal(p.probeLabel('confirmed'), 'Confirmed');
  assert.equal(p.probeLabel('found_not_discoverable'), 'Found, not discoverable');
  assert.equal(p.probeLabel('not_probeable'), 'Not probeable from dash');
  assert.equal(p.probeLabel('blocked_internal'), 'Blocked (internal)');
  assert.equal(p.candidates[3].recognized, false); // unknown_labeled passes through
  assert.equal(p.serverStateLabel('partial'), 'Partially read');
  // The confirmed doc's evidence names instance and URLs.
  assert.ok(p.probeHint(p.candidates[0]).includes('instance 9f2c0c0e'));
  assert.ok(p.probeHint(p.candidates[0]).includes('no dashboard URL published') === false);
  } finally { p.destroy(); }
});

test('an empty dashboard_url in a doc is called out, not guessed', async () => {
  const h = harness('/siblings');
  h.respond(url => response(body({
    candidates: [candidate({doc: {product: 'observe', version: '0.2.0', instance_id: '9f2c0c0e', dashboard_url: '', ingest_url: ''}})],
  })));
  const p = h.page('siblingsPage');
  try {
    await p.init();
    const hint = p.probeHint(p.candidates[0]);
    assert.ok(hint.includes('no dashboard URL published'));
    assert.ok(hint.includes('no ingest URL published'));
  } finally { p.destroy(); }
});

test('rescan posts once and surfaces a conflict as a toast, not a crash', async () => {
  const h = harness('/siblings');
  let scans = 0;
  h.respond(url => {
    if (url.endsWith('/api/siblings/scan')) {
      scans++;
      return response({error: 'a sibling sweep is already running'}, {raw: true, status: 409});
    }
    return response(body());
  });
  const p = h.page('siblingsPage');
  try {
    await p.init();
    await p.rescan();
    assert.equal(scans, 1);
    assert.ok(h.toasts.some(t => t.includes('already running')));
  } finally {
    p.destroy();
  }
});

test('the allow-internal toggle posts the setting and starts a rescan', async () => {
  const h = harness('/siblings');
  const posts = [];
  h.respond(url => {
    if (url.endsWith('/api/siblings/settings')) return response({allow_internal: true});
    if (url.endsWith('/api/siblings/scan')) return response({started: true});
    return response(body());
  });
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (url, options = {}) => {
    if ((options.method || 'GET').toUpperCase() === 'POST') posts.push(url);
    return originalFetch(url, options);
  };
  const p = h.page('siblingsPage');
  try {
    await p.init();
    await p.setAllowInternal(true);
    assert.equal(p.allowInternal, true);
    assert.deepEqual(posts.slice().sort(), ['/api/siblings/scan', '/api/siblings/settings']);
  } finally {
    globalThis.fetch = originalFetch;
    p.destroy();
  }
});

test('an unsupported CLI keeps the page honest instead of empty', async () => {
  const h = harness('/siblings');
  h.respond(url => response(body({cli_supported: false, candidates: [], servers: [], note: 'Sibling discovery needs teploy v0.1.53 or later on the dashboard host.'})));
  const p = h.page('siblingsPage');
  try {
    await p.init();
    assert.equal(p.cliSupported, false);
    assert.ok(p.note.includes('v0.1.53'));
  } finally { p.destroy(); }
});
