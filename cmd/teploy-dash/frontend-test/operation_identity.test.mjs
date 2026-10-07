import { test } from 'node:test';
import assert from 'node:assert/strict';
import { harness, response, deferred } from './identity_harness.mjs';

const operation = id => ({ id, status: 'failed', request: { kind: 'deploy' } });

test('D05: retry A to B, Back, Forward keeps record, stream, logs and actions on the URL', { timeout: 2000 }, async () => {
  const h = harness();
  h.respond((url, opts) => response(opts.method === 'POST' && url.endsWith('/retry') ? operation('B') : operation(url.split('/').at(-1))));
  const page = h.page('operationDetailPage');
  await page.init();
  const a = h.sources.at(-1);
  a.emit('stdout', 'A only');
  await page.retry();
  await h.flush();
  assert.equal(page.op.id, 'B');
  assert.equal(a.closed, true);
  assert.equal(page.lines.length, 0);
  const b = h.sources.at(-1);
  assert.equal(b.url, '/api/operations/B/events');
  b.emit('stdout', 'B only');
  h.pop('/operations/A');
  await h.flush();
  assert.equal(page.op.id, 'A', 'Back must restore A, not retain B');
  assert.equal(b.closed, true, 'Back closes B stream');
  assert.equal(h.sources.at(-1).url, '/api/operations/A/events');
  assert.equal(page.lines.length, 0);
  b.emit('stdout', 'late B');
  assert.equal(page.lines.length, 0);
  await page.cancel();
  assert.equal(h.requests.at(-1).url, '/api/operations/A/cancel');
  h.pop('/operations/B');
  await h.flush();
  assert.equal(page.op.id, 'B');
  assert.equal(h.sources.at(-1).url, '/api/operations/B/events');
  await page.cancel();
  assert.equal(h.requests.at(-1).url, '/api/operations/B/cancel');
  page.destroy();
  assert.equal(h.effects.size, 0, 'route effect released');
});

test('D05: late initial loads and SSE refetches cannot replace a newer route', { timeout: 2000 }, async () => {
  const h = harness(), initialA = deferred(), refreshB = deferred();
  let bGets = 0;
  h.respond(url => url === '/api/operations/A' ? initialA.promise :
    url === '/api/operations/B' && ++bGets > 1 ? refreshB.promise : response(operation(url.split('/').at(-1))));
  const page = h.page('operationDetailPage');
  const initial = page.init();
  h.pop('/operations/B');
  await h.flush();
  assert.equal(page.op?.id, 'B');
  initialA.resolve(response(operation('A')));
  await initial;
  assert.equal(page.op.id, 'B');
  assert.equal(h.sources.length, 1, 'late A must not open another stream');
  const b = h.sources[0];
  b.emit('replay-complete');
  h.pop('/operations/C');
  await h.flush();
  refreshB.resolve(response(operation('B')));
  await h.flush();
  assert.equal(page.op.id, 'C');
  assert.equal(h.sources.at(-1).url, '/api/operations/C/events');
  b.emit('status', 'succeeded');
  assert.equal(page.op.status, 'failed');
  page.destroy();
});

test('D05: interrupted retry cannot redirect; repeated action and pre-effect stale clicks are ignored', { timeout: 2000 }, async () => {
  const h = harness(), retry = deferred();
  h.respond((url, opts) => opts.method === 'POST' ? retry.promise : response(operation(url.split('/').at(-1))));
  const page = h.page('operationDetailPage');
  await page.init();
  const pending = page.retry();
  const repeated = page.retry();
  await h.flush();
  assert.equal(h.requests.filter(r => r.method === 'POST').length, 1, 'no duplicate retry');
  await repeated;
  h.pop('/operations/C');
  await page.cancel(); // route changed before the reactive effect has run
  assert.equal(h.requests.filter(r => r.method === 'POST').length, 1, 'must not cancel retained A');
  await h.flush();
  retry.resolve(response(operation('B')));
  await pending;
  assert.equal(location.pathname, '/operations/C');
  assert.equal(page.op.id, 'C');
  assert.equal(page.actionLoading, false);
  page.destroy();
});

test('D05: destroyed/repeated loads ignore old failures and never resurrect streams', { timeout: 2000 }, async () => {
  const h = harness(), slow = deferred();
  let calls = 0;
  h.respond(() => ++calls === 1 ? slow.promise : response(operation('A')));
  const page = h.page('operationDetailPage');
  const initial = page.init();
  await page.init();
  slow.reject(new Error('stale failure'));
  await initial;
  assert.equal(page.loadError, null);
  assert.equal(page.op.id, 'A');
  assert.equal(h.effects.size, 1, 'initialization retry cannot leak watchers');
  const destroyedLoad = deferred();
  h.respond(() => destroyedLoad.promise);
  const pending = page.init();
  page.destroy();
  destroyedLoad.resolve(response(operation('A')));
  await pending;
  assert.equal(h.sources.filter(s => !s.closed).length, 0);
  assert.equal(h.effects.size, 0);
});
