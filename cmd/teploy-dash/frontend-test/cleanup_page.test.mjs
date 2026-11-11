import {test} from 'node:test';
import assert from 'node:assert/strict';
import {harness,response} from './identity_harness.mjs';

// The audit envelope shapes the cleanup page renders. remedy.runnable is
// dash's verdict (it can rebuild the command), not the CLI's.
const finding=(id,kind,sev,extra={})=>({id,kind,severity:sev,subject:id.split(':')[1],evidence:'evidence of '+kind,...extra});
const remedy={available:true,runnable:true,destructive:false,confirm:false,argv:['teploy','route','remove','--app','ghost','--host','prod']};
const destructiveRemedy={available:true,runnable:true,destructive:true,confirm:true,argv:['teploy','remove','--app','orphan','--host','prod','-y']};

const detailBody=(extra={})=>({server:'prod',configured:true,state:'ok',checks:[{name:'apps',status:'ok'}],disk:null,
  findings:[
    finding('stale_route:ghost','stale_route','warning',{app:'ghost',remedy:{...remedy}}),
    finding('orphan_state:orphan','orphan_state','warning',{app:'orphan',remedy:{...destructiveRemedy}}),
    finding('disk_pressure:/','disk_pressure','critical',{remedy:{available:false,runnable:false,destructive:false,confirm:false}}),
    finding('future:x','future','info',{remedy:{available:true,runnable:false,destructive:false,confirm:false,argv:['teploy','telemetry','scrub'],note:'dash cannot run this remedy itself (unknown command)'}}),
  ],dismissed_count:0,cli_supported:true,cli_version:'v0.1.53',min_cli_version:'v0.1.53',...extra});

const fleetBody=(extra={})=>({servers:[
  {server:'prod',configured:true,state:'ok',age_seconds:120,counts:{critical:1,warning:2,info:1,dismissed:0,remediable:2}},
  {server:'edge',configured:true,state:'partial',age_seconds:90,checks_unknown:2,counts:{critical:0,warning:1,info:0,dismissed:0,remediable:1}},
  {server:'gone',configured:true,state:'never',counts:{critical:0,warning:0,info:0,dismissed:0,remediable:0}},
  {server:'dead',configured:true,state:'unreachable',age_seconds:5400,error:'dial tcp: i/o timeout',counts:{critical:0,warning:0,info:0,dismissed:0,remediable:0}},
],cli_supported:true,cli_version:'v0.1.53',min_cli_version:'v0.1.53',...extra});

test('the /cleanup route renders the page and the nav names it',async()=>{
  const h=harness('/cleanup');h.stores.router.restore();
  assert.equal(h.stores.router.page,'cleanup');
  const html=h.readHTML();
  assert.ok(html.includes(`x-if="$store.router.page === 'cleanup'"`));
  // Cleanup is a tab of the Servers section, and a server's report is the
  // Cleanup tab of its server page (/cleanup/:server still opens it).
  assert.ok(h.stores.router.hubTabs.some(([page, label]) => page === 'cleanup' && label === 'Cleanup'));
  assert.equal(h.stores.router.section, 'servers');
  assert.equal((html.match(/x-data="cleanupPage"/g) || []).length, 2, 'the list page and the server tab');
  assert.ok(html.includes(`x-if="tab === 'cleanup'"`));
  const d = harness('/cleanup/prod'); d.stores.router.restore();
  assert.equal(d.stores.router.page, 'server-detail');
  assert.deepEqual({ ...d.stores.router.params }, { name: 'prod', tab: 'cleanup' });
});

test('fleet rows are honest: partial, never and unreachable never read clean',async()=>{
  const h=harness('/cleanup');
  h.respond(url=>url.includes('/api/cleanup/')?response(detailBody()):response(fleetBody()));
  const p=h.page('cleanupPage');await p.init();
  assert.equal(p.rows.length,4);
  assert.equal(p.stateLabel('partial'),'Partially audited');
  assert.equal(p.stateLabel('unreachable'),'Unreachable');
  assert.equal(p.stateLabel('never'),'Not audited yet');
  assert.equal(p.stateClass('unreachable'),'red');
  assert.equal(p.countsSummary(p.rows[0]),'1 critical, 2 warning, 1 info');
  assert.equal(p.rows[2].state,'never'); // never audited is not ok
  p.destroy();
});

test('running an audit posts once and says an operation is queued',async()=>{
  const h=harness('/cleanup');
  let audits=0;
  h.respond(url=>{
    if(url.endsWith('/audit')){audits++;return response({id:'op1',status:'queued',request:{kind:'server_audit'}});}
    return response(fleetBody());
  });
  const p=h.page('cleanupPage');await p.init();
  await p.runAudit('prod');
  assert.equal(audits,1);
  assert.deepEqual(h.requests.filter(r=>r.method==='POST').map(r=>r.url),['/api/cleanup/prod/audit']);
  p.destroy();
});

test('report-only and unrunnable findings show no fix button; runnable ones do',async()=>{
  const h=harness('/cleanup/prod');
  h.respond(()=>response(detailBody()));
  const p=h.page('cleanupPage');await p.init();
  assert.equal(p.detail.server,'prod');
  assert.equal(p.visibleFindings.length,4);
  assert.equal(p.kindLabel('stale_route'),'Stale route');
  assert.equal(p.kindLabel('future'),'future'); // unknown kinds pass through
  const stale=p.visibleFindings.find(f=>f.id==='stale_route:ghost');
  assert.equal(stale.remedy.runnable,true);
  const pressure=p.visibleFindings.find(f=>f.id==='disk_pressure:/');
  assert.equal(pressure.remedy.available,false); // the CLI named no command
  const future=p.visibleFindings.find(f=>f.id==='future:x');
  assert.equal(future.remedy.runnable,false); // dash cannot rebuild it
  p.destroy();
});

test('a destructive remedy only posts after the exact name is typed',async()=>{
  const h=harness('/cleanup/prod');
  let remedyBody=null;
  h.respond(url=>{
    if(url.endsWith('/remedy'))return response({id:'op2',status:'queued',request:{kind:'remove'}});
    return response(detailBody());
  });
  const p=h.page('cleanupPage');await p.init();
  const orphan=p.visibleFindings.find(f=>f.id==='orphan_state:orphan');
  assert.equal(p.confirmName(orphan),'orphan'); // the app when it has one

  p.openRemedy(orphan);
  assert.ok(p.confirmFinding,'destructive remedies open the typed dialog');
  p.confirmText='orphan-2'; // a near miss
  await p.confirmRemedy();
  assert.equal(h.requests.filter(r=>r.url.endsWith('/remedy')).length,0,'no POST before the name matches');
  assert.ok(p.confirmFinding,'the dialog stays open on a mismatch');

  p.confirmText='orphan';
  await p.confirmRemedy();
  const posted=h.requests.find(r=>r.url.endsWith('/remedy'));
  assert.ok(posted,'the exact name posts');
  remedyBody=posted.body;
  assert.deepEqual(remedyBody,{id:'orphan_state:orphan',confirm:'orphan'});
  assert.equal(p.confirmFinding,null);
  p.destroy();
});

test('a non-destructive remedy is one click',async()=>{
  const h=harness('/cleanup/prod');
  h.respond(url=>url.endsWith('/remedy')?response({id:'op3',status:'queued',request:{kind:'route_remove'}}):response(detailBody()));
  const p=h.page('cleanupPage');await p.init();
  await p.runRemedy(p.visibleFindings.find(f=>f.id==='stale_route:ghost'),'');
  const posted=h.requests.find(r=>r.url.endsWith('/remedy'));
  assert.deepEqual(posted.body,{id:'stale_route:ghost'});
  assert.deepEqual(p.pending,[{server:'prod',opId:'op3',findingId:'stale_route:ghost'}]);
  p.destroy();
});

test('a finished remedy offers the re-audit that proves the finding cleared',async()=>{
  const h=harness('/cleanup/prod');
  h.respond(url=>{
    if(url.includes('/api/operations/'))return response({id:'op3',status:'succeeded',request:{}});
    if(url.endsWith('/remedy'))return response({id:'op3',status:'queued',request:{kind:'route_remove'}});
    return response(detailBody());
  });
  const p=h.page('cleanupPage');await p.init();
  await p.runRemedy(p.visibleFindings.find(f=>f.id==='stale_route:ghost'),'');
  await p.pollPending();
  assert.equal(p.pending.length,0,'a terminal operation leaves the pending list');
  assert.deepEqual(p.finishedServers,['prod']);
  p.destroy();
});

test('dismissals post per finding id and refresh',async()=>{
  const h=harness('/cleanup/prod');
  h.respond(url=>{
    if(url.endsWith('/dismiss'))return response({ok:true});
    return response(detailBody());
  });
  const p=h.page('cleanupPage');await p.init();
  const ghost=p.visibleFindings.find(f=>f.id==='stale_route:ghost');
  await p.dismiss(ghost);
  const posted=h.requests.find(r=>r.url.endsWith('/dismiss'));
  assert.deepEqual(posted.body,{id:'stale_route:ghost'});
  p.destroy();
});

test('an older CLI is a note on the page, not an error',async()=>{
  const h=harness('/cleanup');
  h.respond(()=>response(fleetBody({cli_supported:false,note:'Server audits need teploy v0.1.53 or later on the dashboard host.'})));
  const p=h.page('cleanupPage');await p.init();
  assert.equal(p.cliSupported,false);
  assert.match(p.note,/v0\.1\.53/);
  assert.equal(p.loadFailed,false,'the page still renders its rows');
  assert.equal(p.rows.length,4);
  p.destroy();
});
