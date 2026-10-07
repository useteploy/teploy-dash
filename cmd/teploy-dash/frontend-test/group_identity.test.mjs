import { test } from 'node:test';
import assert from 'node:assert/strict';
import { harness, response } from './identity_harness.mjs';

const prodID = 'srv-0123456789abcdef', stageID = 'srv-fedcba9876543210';
const envelope = (server, id, apps = ['web']) => ({ server, id, apps: apps.map(app => ({ app, server, status: 'running' })) });
function configure(h, groups, fleet, flat = []) {
  h.respond(url => {
    if (url === '/api/groups') return response(groups);
    if (url === '/api/servers') return response({ prod: { id: prodID }, staging: { id: stageID } });
    if (url === '/api/fleet') return fleet instanceof Error ? Promise.reject(fleet) : response({ servers: fleet });
    if (url === '/api/apps') return response(flat);
    throw new Error(`Unexpected request ${url}`);
  });
}
const members = (page, name) => (page.groupedApps().find(g => g.name === name)?.directApps || []).map(a => `${a.server}/${a.name}`);

test('D06: GET-shaped fleet identities keep same-name apps distinct through rename/name reuse', { timeout: 2000 }, async () => {
  const h = harness('/deployments'), page = h.page('projectsPage');
  const groups = [{ name: 'Production', apps: [], server_apps: [{ server_id: prodID, app: 'web' }], projects: [] }];
  configure(h, groups, [envelope('prod', prodID), envelope('staging', stageID)], [{ app: 'web', server: 'prod' }, { app: 'web', server: 'staging' }]);
  await page.load();
  assert.equal(page.loadError, null);
  assert.deepEqual(members(page, 'Production'), ['prod/web']);
  assert.deepEqual(members(page, 'Ungrouped'), ['staging/web']);
  configure(h, groups, [envelope('renamed', prodID), envelope('prod', stageID)]);
  await page.load();
  assert.deepEqual(members(page, 'Production'), ['renamed/web']);
  assert.deepEqual(members(page, 'Ungrouped'), ['prod/web']);
  page.search = 'renamed';
  assert.deepEqual(members(page, 'Production'), ['renamed/web']);
  assert.deepEqual(members(page, 'Ungrouped'), []);
  page.openApp(page.apps[0]);
  assert.equal(location.pathname, '/deployments/renamed/web');
});

test('D06: legacy bare group/project names retain all-server matching; mixed refs do not duplicate cards', { timeout: 2000 }, async () => {
  const h = harness('/deployments'), page = h.page('projectsPage');
  const flat = [{ app: 'web', server: 'prod' }, { app: 'web', server: 'staging' }];
  configure(h, [{ name: 'Legacy', apps: ['web'], projects: [] }], [], flat);
  await page.load();
  assert.deepEqual(members(page, 'Legacy'), ['prod/web', 'staging/web']);
  assert.equal(h.requests.some(r => r.url === '/api/fleet'), false, 'unscoped groups need no extra fleet request');
  configure(h, [{ name: 'Mixed', apps: ['web'], server_apps: [{ server_id: prodID, app: 'web' }], projects: [{ name: 'Old project', apps: ['web'] }] }], [envelope('prod', prodID), envelope('staging', stageID)]);
  await page.load();
  assert.deepEqual(members(page, 'Mixed'), []);
  assert.equal(page.groupedApps()[0].projects[0].resolvedApps.length, 2);
  assert.deepEqual(members(page, 'Ungrouped'), []);
  page.groups[0].projects = [];
  assert.deepEqual(members(page, 'Mixed'), ['prod/web', 'staging/web']);
});

test('D06: unavailable fleet identities surface an error; failed envelopes never look current', { timeout: 2000 }, async () => {
  const h = harness('/deployments'), page = h.page('projectsPage');
  const groups = [{ name: 'Scoped', server_apps: [{ server_id: prodID, app: 'web' }] }];
  configure(h, groups, new Error('fleet down'));
  await page.load();
  assert.match(page.loadError || '', /fleet down/);
  configure(h, groups, [{ ...envelope('prod', prodID), error: 'offline' }, envelope('staging', stageID)]);
  await page.load();
  assert.equal(page.loadError, null);
  assert.deepEqual(members(page, 'Scoped'), []);
  assert.deepEqual(members(page, 'Ungrouped'), ['staging/web']);
  configure(h, groups, [{ ...envelope('prod', prodID), error: 'offline' }]);
  await page.load();
  assert.match(page.loadError || '', /failed for all/i);
});


test('D06: name-only removal cannot remove a different scoped card or guess among multiple bindings', { timeout: 2000 }, async () => {
  const h = harness('/deployments'), page = h.page('projectsPage');
  globalThis.confirm = () => true;
  const scoped = { server_id: prodID, app: 'web' };
  page.groups = [{ name: 'Mixed', apps: ['web'], server_apps: [scoped] }];
  page.load = async () => {};
  h.respond(() => response({ status: 'unassigned' }));
  // Invoke the actual card binding in each source snapshot, so a base run
  // passes its original app.name argument rather than inventing an input.
  const binding = h.readHTML().match(/@click.stop="(unassignFromGroup\([^"]+)"/)[1];
  const remove = app => new Function('page', 'group', 'app', `return page.${binding}`)(page, page.groups[0], app);
  await remove({ name: 'web', server: 'staging', server_id: stageID });
  assert.equal(h.requests.length, 0, 'legacy staging card must not delete the prod binding');
  assert.match(h.toasts.at(-1), /not supported/);
  page.groups[0].server_apps.push({ server_id: stageID, app: 'web' });
  await remove({ name: 'web', server: 'prod', server_id: prodID });
  assert.equal(h.requests.length, 0, 'two bindings cannot be removed by name');
  page.groups[0].server_apps = [scoped];
  await remove({ name: 'web', server: 'prod', server_id: prodID });
  assert.equal(h.requests.at(-1).url, '/api/groups/Mixed/apps/web');
  assert.equal(h.requests.at(-1).method, 'DELETE');
  page.groups[0].server_apps = [];
  await remove({ name: 'web', server: 'staging' });
  assert.equal(h.requests.length, 2, 'legacy unassignment remains supported');
  assert.ok(h.readHTML().includes('unassignFromGroup(group.name, app)'), 'card passes its full identity');
});
