import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { harness, response } from './identity_harness.mjs';

test('D09: real GET-shaped restore rows toggle both directions with only configuration fields', { timeout: 2000 }, async () => {
  const h = harness('/restore-tests'), page = h.page('restoreTestsPage');
  const allowed = ['id', 'server', 'app', 'accessory', 'bucket', 'region', 'interval_hours', 'enabled'];
  // Match the strict Go DTO, but this is a frontend contract stub, not Go execution.
  let saved = { id: 'rt1', server: 'prod', app: 'web', accessory: 'db', bucket: 'backups', region: 'us-east-1', interval_hours: 24, enabled: true };
  const result = { last_run_at: '2026-10-06T14:00:00Z', last_ok: false, last_detail: 'verification failed', last_metric: 'rows', last_date: '2026-10-06', last_duration_ms: 1200, delivery: { status: 'delivered', attempts: 1 }, future_read_only: 'must not spread' };
  h.respond((url, options) => {
    assert.equal(url, '/api/restore-tests');
    if (options.method === 'POST') {
      const body = JSON.parse(options.body);
      if (Object.keys(body).some(key => !allowed.includes(key))) return response({ error: 'configuration fields only' }, { raw: true, status: 400 });
      saved = body;
      return response(saved, { raw: true });
    }
    return response([{ ...saved, ...result }], { raw: true });
  });
  page._alive = true;
  await page.loadTests();
  const input = page.tests[0];
  const original = structuredClone(input);
  await page.toggleEnabled(input);
  assert.equal(saved.enabled, false, 'enriched row must successfully disable');
  assert.equal(page.tests[0].enabled, false, 'refreshed state reflects the saved toggle');
  assert.deepEqual(page.tests[0].delivery, result.delivery);
  await page.toggleEnabled(page.tests[0]);
  assert.equal(saved.enabled, true, 'enriched row must successfully re-enable');
  for (const request of h.requests.filter(r => r.method === 'POST')) {
    assert.deepEqual(Object.keys(request.body).sort(), [...allowed].sort());
    for (const key of allowed.filter(k => k !== 'enabled')) assert.equal(request.body[key], original[key]);
  }
  assert.deepEqual(input, original, 'toggle must not mutate its GET input');
  const html = readFileSync(fileURLToPath(new URL('../frontend/index.html', import.meta.url)), 'utf8');
  assert.ok(html.includes('toggleEnabled(t)'), 'restore cards invoke this method with their GET row');
});
