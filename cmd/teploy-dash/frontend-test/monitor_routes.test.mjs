import {test} from 'node:test';
import assert from 'node:assert/strict';
import {harness,response,deferred} from './identity_harness.mjs';

const monitor=id=>({monitor:{id,name:'m-'+id,target:'https://'+id,enabled:true},stats:null,checks:[]});
const raw=v=>response(v,{raw:true});

test('/monitors/:id deep link opens that monitor and the list route closes it',async()=>{
 const h=harness('/monitors/a');h.stores.router.restore();assert.equal(h.stores.router.page,'monitor-detail');
 h.respond(url=>url==='/api/monitors'?raw([{id:'a'},{id:'b'}]):url.startsWith('/api/monitors/')?raw(monitor(url.split('/').at(-1))):raw({}));
 const p=h.page('monitorsPage');await p.init();
 assert.equal(p.detailId,'a');assert.equal(p.selectedMonitor.monitor.id,'a');assert.equal(p.monitors.length,2);
 p.openList();await h.flush();assert.equal(location.pathname,'/monitors');assert.equal(p.detailId,'');assert.equal(p.selectedMonitor,null);
 await p.selectMonitor('b');assert.equal(location.pathname,'/monitors/b');assert.equal(p.selectedMonitor.monitor.id,'b');
 h.pop('/monitors');await h.flush();assert.equal(p.selectedMonitor,null,'Back returns to the list');
 h.pop('/monitors/b');await h.flush();assert.equal(p.selectedMonitor.monitor.id,'b','Forward reopens the monitor');
 p.destroy();assert.equal(h.effects.size,0);
});

test('a late detail response for a monitor the route left is dropped; test flash does not leak',async()=>{
 const h=harness('/monitors/a');h.stores.router.restore();const slow=deferred();
 h.respond(url=>url==='/api/monitors/a'?slow.promise:url==='/api/monitors'?raw([]):raw(monitor(url.split('/').at(-1))));
 const p=h.page('monitorsPage');const initial=p.init();
 p.testResult={status:'down'};
 h.stores.router.navigate('monitor-detail',{id:'b'});await h.flush();
 assert.equal(p.selectedMonitor.monitor.id,'b');assert.equal(p.testResult,null);
 slow.resolve(raw(monitor('a')));await initial;await h.flush();assert.equal(p.selectedMonitor.monitor.id,'b');
 p.destroy();
});

test('an unknown monitor id is an error state with retry, not a blank page',async()=>{
 const h=harness('/monitors/gone');h.stores.router.restore();
 h.respond(url=>url==='/api/monitors'?raw([]):raw({error:'monitor not found'},{status:404}));
 const p=h.page('monitorsPage');await p.init();
 assert.equal(p.selectedMonitor,null);assert.match(p.detailError,/gone/);
 const html=h.readHTML();
 assert.ok(html.includes(`x-if="$store.router.page === 'monitors' || $store.router.page === 'monitor-detail'"`),'monitor-detail route must render the monitors component');
 assert.ok(html.includes('x-if="detailId && !selectedMonitor"'),'deep-link loading/error state must render');
 p.destroy();
});
