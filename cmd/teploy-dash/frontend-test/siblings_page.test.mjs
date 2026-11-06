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

// ── Observe connect ───────────────────────────────────────────────────────

const doc = {product: 'observe', version: '0.2.0', instance_id: '9f2c0c0e6b6d4f5a8e1d3b7a2c4f6e80', dashboard_url: 'https://observe.example.org', ingest_url: 'https://ingest.example.org', capabilities: ['service_tokens']};

test('connect is two steps: preview the instance, then confirm with the token', async () => {
  const h = harness('/siblings');
  const posts = [];
  h.respond(url => {
    if (url.includes('/api/observe/preview')) return response(doc);
    if (url.endsWith('/api/observe/connect')) {
      posts.push('connect');
      return response({base_url: 'http://observe.internal:3000', instance_id: doc.instance_id, product: 'observe', token_fp: 'abc123def456', connected_at: '2026-11-05T12:00:00Z', connected_by: 'admin'});
    }
    if (url.startsWith('/api/observe')) return response({connected: false, apps: [], hint: {available: true, base_url: 'http://observe.internal:3000', instance_id: doc.instance_id}});
    return response(body());
  });
  const p = h.page('siblingsPage');
  try {
    await p.init();
    // The confirmed probe prefilled the form.
    assert.equal(p.connectForm.base_url, 'http://observe.internal:3000');
    // Step 1: preview shows the identity, token stays locked.
    await p.previewObserve();
    assert.equal(p.previewDoc.instance_id, doc.instance_id);
    assert.equal(p.previewError, '');
    // Step 2: connect sends the PREVIEWED instance id as the confirmation.
    p.connectForm.token = 'obs_svc_secret';
    await p.connectObserve();
    const connect = h.requests.find(r => r.url.endsWith('/api/observe/connect'));
    assert.equal(connect.body.confirm_instance_id, doc.instance_id);
    assert.equal(connect.body.token, 'obs_svc_secret');
    assert.equal(posts.length, 1);
  } finally { p.destroy(); }
});

test('a preview failure keeps the token locked and names the reason', async () => {
  const h = harness('/siblings');
  h.respond(url => {
    if (url.includes('/api/observe/preview')) return response({error: 'the discovery doc did not answer at http://x: no route'}, {raw: true, status: 400});
    if (url.startsWith('/api/observe')) return response({connected: false, apps: [], hint: {available: false}});
    return response(body());
  });
  const p = h.page('siblingsPage');
  try {
    await p.init();
    await p.previewObserve();
    assert.equal(p.previewDoc, null);
    assert.ok(p.previewError.includes('discovery doc did not answer'));
    assert.equal(h.requests.some(r => r.url.endsWith('/api/observe/connect')), false);
  } finally { p.destroy(); }
});

test('a connected Observe shows the instance summary and disconnects', async () => {
  const h = harness('/siblings');
  let del = 0;
  h.respond(url => {
    if (url.endsWith('/api/observe/connection') ) { del++; return response({ok: true, note: 'Revoke the service token in Observe (Settings → Service Tokens).'}); }
    if (url.startsWith('/api/observe')) {
      // After the DELETE, the next read shows disconnected.
      return response(del > 0
        ? {connected: false, apps: [], hint: {available: true, base_url: 'http://observe.internal:3000'}}
        : {connected: true, apps: [], connection: {base_url: 'http://observe.internal:3000', instance_id: doc.instance_id, product: 'observe', version: '0.2.0', token_fp: 'abc123def456', connected_at: '2026-11-05T12:00:00Z', connected_by: 'admin'}, hint: {available: true}});
    }
    return response(body());
  });
  const p = h.page('siblingsPage');
  try {
    await p.init();
    assert.equal(p.observeConnected, true);
    assert.equal(p.observeConnection.instance_id, doc.instance_id);
    await p.disconnectObserve();
    assert.equal(del, 1);
    assert.equal(p.observeConnected, false);
  } finally { p.destroy(); }
});

// ── per-app telemetry opt-in ──────────────────────────────────────────────

const planBody = {
  server: 'prod', app: 'web', external_ref: 'dash/prod/web', key_label: 'dash-telemetry',
  ingest_url: 'https://ingest.example.org', instance_id: doc.instance_id,
  site_action: 'ensure the site for dash/prod/web (created on Observe if absent — idempotent)',
  env: [
    {key: 'OBSERVE_ENDPOINT', value: 'https://ingest.example.org'},
    {key: 'OBSERVE_API_KEY', value: '[secret: the telemetry key]', secret: true},
    {key: 'OTEL_EXPORTER_OTLP_ENDPOINT', value: 'https://ingest.example.org'},
    {key: 'OTEL_EXPORTER_OTLP_HEADERS', value: '[secret: x-api-key=<the telemetry key>]', secret: true},
    {key: 'OTEL_EXPORTER_OTLP_PROTOCOL', value: 'http/protobuf'},
    {key: 'OTEL_SERVICE_NAME', value: 'web'},
    {key: 'OTEL_RESOURCE_ATTRIBUTES', value: 'service.version=$TEPLOY_VERSION,deployment.environment=production'},
  ],
  redeploy: 'queue a redeploy (durable operation) so the new environment reaches the app\u2019s containers',
  fingerprint: 'deadbeef12345678',
};

test('opt-in is plan-first: the plan shows every write, apply sends its fingerprint', async () => {
  const h = harness('/siblings');
  let applies = [];
  h.respond(url => {
    if (url.includes('/api/observe/telemetry/plan')) return response(planBody);
    if (url.endsWith('/api/observe/telemetry')) { applies.push(url); return response({site_id: 'st_123', key_id: 'k_1', operation: {id: 'op9', status: 'queued', request: {kind: 'redeploy'}}, env_keys: planBody.env.filter(e => !e.secret).map(e => e.key), secret_keys: ['OBSERVE_API_KEY', 'OTEL_EXPORTER_OTLP_HEADERS'], note: 'Redeploy queued — follow it in Activity.'}); }
    if (url.startsWith('/api/observe')) return response({connected: true, apps: [], connection: {base_url: 'http://observe.internal:3000', instance_id: doc.instance_id, token_fp: 'abc123def456'}, hint: {available: true}});
    if (url === '/api/apps') return response([{server: 'prod', app: 'web'}, {server: 'prod', app: 'api'}]);
    return response(body());
  });
  const p = h.page('siblingsPage');
  try {
    await p.init();
    // The fleet apps feed the picker's suggestions.
    assert.deepEqual(p.appSuggestions.map(a => a.app).sort(), ['api', 'web']);
    p.telemetryForm = {server: 'prod', app: 'web'};
    await p.planTelemetry();
    assert.equal(p.telemetryPlan.fingerprint, 'deadbeef12345678');
    assert.equal(p.telemetryPlan.env.length, 7);
    // Approve: apply carries the approved fingerprint.
    await p.applyTelemetry();
    const apply = h.requests.find(r => r.url.endsWith('/api/observe/telemetry'));
    assert.equal(apply.body.fingerprint, 'deadbeef12345678');
    assert.equal(apply.body.app, 'web');
    assert.ok(p.telemetryApplied.operation.id === 'op9');
    assert.equal(p.telemetryPlan, null); // the plan is consumed
  } finally { p.destroy(); }
});

test('a changed plan refuses to apply: the fingerprint gate is server-side, the UI surfaces it', async () => {
  const h = harness('/siblings');
  h.respond(url => {
    if (url.includes('/api/observe/telemetry/plan')) return response(planBody);
    if (url.endsWith('/api/observe/telemetry')) return response({error: 'the plan changed since it was approved (Observe moved, or the app changed) — review the plan and approve again'}, {raw: true, status: 409});
    if (url.startsWith('/api/observe')) return response({connected: true, apps: [], connection: {instance_id: doc.instance_id, token_fp: 'abc'}, hint: {available: true}});
    if (url === '/api/apps') return response([]);
    return response(body());
  });
  const p = h.page('siblingsPage');
  try {
    await p.init();
    p.telemetryForm = {server: 'prod', app: 'web'};
    await p.planTelemetry();
    await p.applyTelemetry();
    assert.ok(p.telemetryPlan, 'a refused apply keeps the plan up for re-approval');
    assert.ok(h.toasts.some(t => t.includes('plan changed')), 'the refusal reaches the owner');
  } finally { p.destroy(); }
});

test('a plan without an ingest URL names the gap instead of guessing', async () => {
  const h = harness('/siblings');
  h.respond(url => {
    if (url.includes('/api/observe/telemetry/plan')) return response({error: 'the discovery doc publishes no ingest URL (Observe has a separate ingest listener and OBSERVE_INGEST_URL is unset) — set it in Observe and plan again'}, {raw: true, status: 409});
    if (url.startsWith('/api/observe')) return response({connected: true, apps: [], connection: {instance_id: doc.instance_id, token_fp: 'abc'}, hint: {available: true}});
    if (url === '/api/apps') return response([]);
    return response(body());
  });
  const p = h.page('siblingsPage');
  try {
    await p.init();
    p.telemetryForm = {server: 'prod', app: 'web'};
    await p.planTelemetry();
    assert.equal(p.telemetryPlan, null);
    assert.ok(p.telemetryPlanError.includes('no ingest URL'));
  } finally { p.destroy(); }
});

test('stopping telemetry lists exactly the recorded keys in the confirm dialog', async () => {
  const h = harness('/siblings');
  const entry = {server: 'prod', app: 'web', env_keys: ['OBSERVE_ENDPOINT', 'OTEL_SERVICE_NAME'], secret_keys: ['OBSERVE_API_KEY'], key_id: 'k_1', site_id: 'st_123'};
  let disabled = null;
  h.respond(url => {
    if (url.endsWith('/api/observe/telemetry/disable')) { disabled = true; return response({operation: {id: 'op10', status: 'queued'}, removed: ['OBSERVE_ENDPOINT', 'OTEL_SERVICE_NAME', 'OBSERVE_API_KEY'], note: 'Redeploy queued.'}); }
    if (url.startsWith('/api/observe')) return response({connected: true, apps: [entry], connection: {instance_id: doc.instance_id, token_fp: 'abc'}, hint: {available: true}});
    if (url === '/api/apps') return response([]);
    return response(body());
  });
  const p = h.page('siblingsPage');
  try {
    await p.init();
    assert.equal(p.telemetryApps.length, 1);
    const display = p.envDisplay(entry);
    assert.ok(display.includes('OBSERVE_API_KEY (secret)'));
    p.openDisable(entry);
    await p.disableTelemetry();
    assert.equal(disabled, true);
    assert.equal(p.confirmDisable, null);
  } finally { p.destroy(); }
});
