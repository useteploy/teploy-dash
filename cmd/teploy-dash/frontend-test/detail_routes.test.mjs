import {test} from 'node:test';
import assert from 'node:assert/strict';
import {harness,response,deferred} from './identity_harness.mjs';

test('project same-type Back/Forward drops old frames/actions and rename updates URL',async()=>{
 const h=harness('/deployments/groups/G/one'),p=h.page('projectDetailPage'),slow=deferred();let first=true;
 h.respond(url=>url==='/api/groups'?(first?(first=false,slow.promise):response([{name:'G',projects:[{name:'two',apps:['two']},{name:'renamed',apps:['two']}]}])):response(url==='/api/apps'?[{app:'two'}]:{}));
 const initial=p.init();h.pop('/deployments/groups/G/two');await h.flush();assert.equal(p.projectName,'two');
 slow.resolve(response([{name:'G',projects:[{name:'one',apps:['old']}]}]));await initial;await h.flush();assert.deepEqual(p.projectApps.map(a=>a.name),['two']);
 globalThis.prompt=()=> 'renamed';const rename=deferred();h.respond((url,options)=>options.method==='PUT'?rename.promise:response(url==='/api/groups'?[{name:'G',projects:[{name:'renamed',apps:['two']}]}]:url==='/api/apps'?[{app:'two'}]:{}));
 const action=p.renameThisProject();rename.resolve(response({}));await action;assert.equal(location.pathname,'/deployments/groups/G/renamed');assert.equal(p.projectName,'renamed');
 const late=deferred();globalThis.confirm=()=>true;h.respond(()=>late.promise);const deletion=p.deleteThisProject();h.pop('/deployments/groups/G/two');await h.flush();late.resolve(response({}));await deletion;assert.equal(location.pathname,'/deployments/groups/G/two');
 p.destroy();assert.equal(h.effects.size,0);
});

test('server same-type routes clear proxy cache and reject late status/proxy frames',async()=>{
 const h=harness('/fleet/one'),p=h.page('serverDetailPage');
 // Canonical server-detail route is /servers/:name; use router explicitly.
 h.stores.router.navigate('server-detail',{name:'one'});
 const status=deferred();h.respond(url=>url.includes('/one/status')?status.promise:response({identity:url}));const initial=p.init();
 h.stores.router.navigate('server-detail',{name:'two'});await h.flush();assert.equal(p.name,'two');assert.ok(p.status.identity.includes('/two/status'));
 status.resolve(response({identity:'old'}));await initial;await h.flush();assert.ok(p.status.identity.includes('/two/status'));
 await p.loadProxy();assert.ok(p.proxy.identity.includes('/two/proxy'));
 const proxy=deferred();h.respond(url=>url.includes('/two/proxy')?proxy.promise:response({identity:url}));const pending=p.loadProxy();
 h.stores.router.navigate('server-detail',{name:'one'});await h.flush();assert.equal(p.proxy,null);proxy.resolve(response({identity:'old-proxy'}));await pending;assert.equal(p.proxy,null);
 p.destroy();assert.equal(h.effects.size,0);
});
