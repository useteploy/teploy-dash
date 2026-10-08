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


test('D06: scoped deletion sends exact identity and legacy deletion cannot touch scoped refs', async () => {
  const h = harness('/deployments'), page = h.page('projectsPage');
  globalThis.confirm = () => true;
  page.groups = [{ name: 'Mixed', apps: ['web'], server_apps: [{ server_id: prodID, app: 'web' }, {server_id: stageID, app: 'web'}] }];
  page.load = async () => {};
  h.respond(() => response({ status: 'unassigned' }));
  const binding = h.readHTML().match(/@click.stop="(unassignFromGroup\([^"]+)"/)[1];
  const remove = app => new Function('page', 'group', 'app', `return page.${binding}`)(page, page.groups[0], app);
  await remove({ name: 'web', server: 'prod', server_id: prodID });
  assert.equal(h.requests.at(-1).url, `/api/groups/Mixed/apps/web?server_id=${prodID}`);
  await remove({ name: 'web', server: 'staging', server_id: stageID });
  assert.equal(h.requests.at(-1).url, `/api/groups/Mixed/apps/web?server_id=${stageID}`);
  page.groups[0].server_apps = [];
  await remove({ name: 'web', server: 'staging' });
  assert.equal(h.requests.at(-1).url, '/api/groups/Mixed/apps/web?legacy=1');
});

test('D06: scoped project membership hides only its own direct card and detail/removal use full identity', async () => {
 const h = harness('/deployments'), page = h.page('projectsPage');
 globalThis.confirm = () => true;
 const groups = [{name:'G',apps:['web'],projects:[{name:'P',apps:[],server_apps:[{app:'web',server_id:prodID}]}]}];
 configure(h, groups, [envelope('renamed',prodID),envelope('staging',stageID)]);
 await page.load();
 assert.deepEqual(members(page,'G'),['staging/web']);
 assert.equal(page.groupedApps()[0].projects[0].resolvedApps[0].server,'renamed');
 h.stores.router.navigate('project-detail',{group:'G',project:'P'});
 const detail=h.page('projectDetailPage'); detail.groupName='G';detail.projectName='P';
 await detail.load();
 assert.deepEqual(detail.projectApps.map(a => `${a.server}/${a.name}`),['renamed/web']);
 const card=detail.projectApps[0]; detail.load=async()=>{};
 h.respond(()=>response({status:'unassigned'}));
 await detail.unassignFromProject(card);
 assert.equal(h.requests.at(-1).url,`/api/groups/G/projects/P/apps/web?server_id=${prodID}`);
 assert.ok(h.readHTML().includes('unassignFromProject(app)'));
});

test('D06: both deployment forms bind group and project to the selected server', async () => {
 for (const component of ['projectsPage','projectDetailPage']) {
  const h=harness('/deployments'),page=h.page(component);
  if(component==='projectDetailPage') h.stores.router.navigate('project-detail',{group:'G',project:'P'});
  page.groupName='G';page.projectName='P';
  page.deployForm={app:'web',image:'example/web:1',domain:'web.test',server:'prod',port:80};
  page.load=async()=>{};
  h.respond(url => response(url==='/api/deploy'?{id:'A',status:'queued',request:{kind:'deploy'}}:{status:'assigned'}));
  await page.doDeploy('G');
  const assignments=h.requests.filter(r=>r.url.startsWith('/api/groups/'));
  assert.equal(assignments.length,component==='projectsPage'?1:2);
  for(const request of assignments) assert.deepEqual(request.body,{app:'web',server:'prod'});
 }
});
