// D5 screens: the new app-page surfaces are capability-gated in markup, the
// restore confirmation binds to the TYPED app name, and the secrets value
// input never renders its contents. Markup assertions plus one harness run
// of the restore gate and the secrets flow.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { harness, response } from './identity_harness.mjs';

test('D5: releases/backups tabs and redeploy are gated on CLI tokens in markup', () => {
  const html = harness('/apps/prod/web').readHTML();
  assert.ok(html.includes(`x-show="hasFeature('releases')"`), 'Releases tab must hide without releases-json-v1');
  assert.ok(html.includes(`x-show="hasFeature('backups')"`), 'Backups tab must hide without backup-json-v1');
  assert.ok(html.includes(`x-show="hasFeature('redeploy')"`), 'Redeploy must hide without redeploy-v1');
  assert.ok(html.includes(`x-if="hasFeature('secrets')"`, 'Secrets section must hide without secret-stdin-v1'), 'Secrets section must hide without secret-stdin-v1');
  assert.ok(html.includes(`hasFeature('rollback_to')`), 'Roll-back-to must hide without rollback-json-v1');
});

test('D5: the secrets value input never renders its contents', () => {
  const html = harness('/apps/prod/web').readHTML();
  const input = html.match(/<input[^>]*x-model="newSecretValue"[^>]*>/);
  assert.ok(input, 'secrets value input exists');
  assert.ok(input[0].includes('type="password"'), 'secrets value input is type=password');
  assert.ok(!html.includes('x-text="newSecretValue"') && !html.includes('x-text="s.value"'), 'no secret value is ever rendered back');
});

test('D5: restore submits only after the app name is typed, with the exact backup id', async () => {
  const h = harness('/apps/prod/web');
  const page = h.page('appDetailPage');
  page.resource = Object.freeze({ server: 'prod', name: 'web' });
  h.respond((url, options) => {
    if (options.method === 'POST') return response({ id: 'op1', status: 'queued', request: { kind: 'backup_restore' } });
    return response({});
  });
  page.backupRestore = { id: '20261019-120000', accessory: 'postgres' };

  // A wrong or empty confirmation never reaches the API.
  page.backupRestoreConfirm = '';
  await page.confirmBackupRestore();
  page.backupRestoreConfirm = 'other-app';
  await page.confirmBackupRestore();
  assert.equal(h.requests.filter(r => r.url.endsWith('/backups/restore')).length, 0, 'unconfirmed restore must not submit');

  page.backupRestoreConfirm = 'web';
  await page.confirmBackupRestore();
  const call = h.requests.find(r => r.url.endsWith('/backups/restore'));
  assert.ok(call, 'confirmed restore submits');
  assert.deepEqual(call.body, { id: '20261019-120000', accessory: 'postgres' });
  assert.equal(page.backupRestore, null, 'dialog closes after submit');
});

test('D5: a secret set sends key and value, then drops the value from state', async () => {
  const h = harness('/apps/prod/web');
  const page = h.page('appDetailPage');
  page.resource = Object.freeze({ server: 'prod', name: 'web' });
  let listed = [];
  h.respond((url, options) => {
    if (url.endsWith('/secrets') && !options.method) {
      listed = [{ key: 'API_KEY', value: '***', provider: 'local' }];
      return response(listed);
    }
    if (url.endsWith('/secrets') && options.method === 'POST') return response({ key: 'API_KEY' });
    return response([]);
  });
  page.newSecretKey = 'API_KEY';
  page.newSecretValue = 'typed-secret';
  await page.addSecret();
  const call = h.requests.find(r => r.url.endsWith('/secrets') && r.method === 'POST');
  assert.deepEqual(call.body, { key: 'API_KEY', value: 'typed-secret' });
  assert.equal(page.newSecretValue, '', 'the value is dropped from component state after the write');
  assert.deepEqual(page.secrets, listed);
});
