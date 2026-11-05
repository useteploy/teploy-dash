import {test} from 'node:test';
import assert from 'node:assert/strict';
import {harness,response} from './identity_harness.mjs';

const layer=(state,detail='')=>({state,detail});
const row=(host,overall,extra={})=>({host,overall,dns:layer('ok'),route:layer('ok'),cert:layer('ok'),http:layer('ok'),age_seconds:120,...extra});
const body=(extra={})=>({rows:[],servers:[],cli_supported:true,min_cli_version:'v0.1.51',...extra});
const data=v=>response(v);

test('the /domains route renders the page and the nav names it',async()=>{
 const h=harness('/domains');h.stores.router.restore();
 assert.equal(h.stores.router.page,'domains');
 const html=h.readHTML();
 assert.ok(html.includes(`x-if="$store.router.page === 'domains'"`));
 assert.ok(html.includes('>Domains</a>'));
 assert.ok(html.includes('x-data="domainsPage"'));
});

test('rows show the first failing layer; problems-only hides healthy ones',async()=>{
 const h=harness('/domains');
 h.respond(()=>data(body({rows:[
  row('good.dev','ok'),
  row('bad.dev','fail',{first_failing:'dns',evidence:'no DNS record',dns:layer('fail','no DNS record')}),
  row('label.dev','na'),
  row('blind.dev','unknown'),
 ]})));
 const p=h.page('domainsPage');await p.init();
 assert.equal(p.rows.length,4);
 assert.equal(p.firstFailingLabel(p.rows[1]),'DNS');
 assert.equal(p.layerLabel(p.rows[1].dns),'Failing');
 assert.equal(p.countBy('fail'),1);
 p.problemsOnly=true;
 assert.deepEqual(p.visibleRows.map(r=>r.host),['bad.dev','blind.dev'],'unknown is never hidden as healthy');
 p.destroy();
});

test('a server whose check failed is named, not silently absent',async()=>{
 const h=harness('/domains');
 h.respond(()=>data(body({servers:[
  {server:'ok-box',state:'ok',age_seconds:60},
  {server:'dead-box',state:'unreachable',error:'dial tcp: i/o timeout',age_seconds:5400},
  {server:'half-box',state:'partial',issues:[{scope:'caddy_logs',message:'could not read'}],age_seconds:60},
 ]})));
 const p=h.page('domainsPage');await p.init();
 assert.deepEqual(p.troubledServers.map(s=>s.server),['dead-box','half-box']);
 assert.match(p.serverProblem(p.troubledServers[0]),/dead-box could not be checked: dial tcp: i\/o timeout \(last good result: 90 min ago\)/);
 assert.match(p.serverProblem(p.troubledServers[1]),/caddy_logs: could not read/);
 p.destroy();
});

test('an older CLI is a note, not an error, and rows still render',async()=>{
 const h=harness('/domains');
 h.respond(()=>data(body({cli_supported:false,note:'needs teploy v0.1.51 or later on the dashboard host',rows:[row('m.dev','ok',{age_seconds:undefined})]})));
 const p=h.page('domainsPage');await p.init();
 assert.equal(p.cliSupported,false);assert.match(p.note,/v0\.1\.51/);assert.equal(p.loadFailed,false);
 assert.equal(p.ageLabel(undefined),'not checked');
 assert.ok(h.readHTML().includes('x-if="!loading && !cliSupported"'));
 p.destroy();
});

test('evidence age is shown in words, and a failed refresh keeps rows as stale',async()=>{
 const h=harness('/domains');let fail=false;
 h.respond(()=>fail?response({error:'boom'},{status:500}):data(body({rows:[row('a.dev','ok')]})));
 const p=h.page('domainsPage');await p.init();
 assert.equal(p.ageLabel(30),'just now');assert.equal(p.ageLabel(1800),'30 min ago');assert.equal(p.ageLabel(7200),'2 h ago');assert.equal(p.ageLabel(3*86400),'3 days ago');
 fail=true;await p.load();
 assert.equal(p.showingStale,true);assert.equal(p.rows.length,1);
 p.destroy();
});
