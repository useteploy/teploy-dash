import {test} from 'node:test';
import assert from 'node:assert/strict';
import {harness,response} from './identity_harness.mjs';

const testView=(extra={})=>({
  cli_supported:true,min_cli_version:'v0.1.52',note:'',sweep_error:'',
  last_sweep_at:'2026-11-05T12:00:00Z',checklist_done:true,
  storage:{provider:'b2',bucket:'teploy-backups',region:'us-west-004',endpoint:'https://s3.us-west-004.backblazeb2.com',prefixes:{prod:'teploy/prod'},connected_at:'2026-11-04T03:00:00Z'},
  recovery:{banner:true,public_key:'age1lggy',drill_key:'s3://teploy-backups/prod/blog/1.age',drill_command:'TEPLOY_S3_ACCESS_KEY=<key> teploy backup fetch s3://teploy-backups/prod/blog/1.age --identity recovery.txt --json'},
  policy:{interval:'1h',preset:'standard',volumes:true,volume_max_gb:20},
  window:{start_hour:2,end_hour:6},presets:['simple','standard'],
  intervals:['1h','2h','4h','6h','12h','24h','7d'],public_url_set:true,
  servers:[
    {server:'prod',state:'ok',target:{configured:true,bucket:'teploy-backups',reachable:true},policy:{configured:true,interval:'1h'},encrypted:true,aws_cli:true,age_cli:true},
    {server:'dead',state:'unreachable',error:'dial tcp: i/o timeout'},
  ],
  rows:[
    {server:'prod',name:'blog-db',kind:'accessory',app:'blog',accessory:'db',engine:'postgres',state:'covered',last_good:'2026-11-05T11:00:00Z',last_good_age_seconds:3600,size_bytes:4096,projected_bytes:4096*26,encrypted:true,
      last_restore_test:{at:'2026-11-04T03:30:00Z',ok:true,metric:'tables:12'},evidence_age_seconds:600},
    {server:'prod',name:'omni_postgres',kind:'adopted',engine:'postgres',state:'failing',last_error:'dumping omni: exit status 1',uncovered_databases:['reports'],size_bytes:0,projected_bytes:0,encrypted:true,evidence_age_seconds:600},
    {server:'prod',name:'blog/volumes',kind:'volumes',app:'blog',state:'excluded',size_bytes:0,projected_bytes:0,evidence_age_seconds:600},
  ],
  tests:[{id:'backups-1',server:'prod',app:'blog',accessory:'db',engine:'postgres',next_due_at:'2026-11-06T03:00:00Z',last_run:{at:'2026-11-04T03:30:00Z',ok:true},history:3}],
  projected_bytes:106496,
  self_backup:{available:false,reason:'Dash does not back itself up yet.',runbook:'docs/rebuild-dash.md'},
  handwritten:[{name:'omni-backup',where:'cron lines and scripts'}],
  handwritten_rule:'Remove a hand-written job only when its database shows Ready.',
  ...extra,
});
const data=v=>response(v);

test('the /backups route renders the page and the nav names it',async()=>{
  const h=harness('/backups');h.stores.router.restore();
  assert.equal(h.stores.router.page,'backups');
  const html=h.readHTML();
  assert.ok(html.includes(`x-if="$store.router.page === 'backups'"`));
  assert.ok(html.includes('>Backups</a>'));
  assert.ok(html.includes('x-data="backupsPage"'));
});

test('rows carry coverage, encryption and restore-test evidence; unread servers are named',async()=>{
  const h=harness('/backups');
  h.respond(()=>data(testView()));
  const p=h.page('backupsPage');await p.init();
  assert.equal(p.rows.length,3);
  assert.equal(p.countBy('covered'),1);
  assert.equal(p.countBy('failing'),1);
  assert.equal(p.coverageLabel('covered'),'covered');
  assert.equal(p.coverageClass('failing'),'red');
  assert.equal(p.coverageClass('adopted'),'green');
  assert.match(p.serverProblemLine(p.servers[1]),/dead could not be read: dial tcp: i\/o timeout — its rows read unknown, not healthy/);
  assert.equal(p.bytesLabel(0),'—');
  assert.equal(p.bytesLabel(4096),'4.0 KB');
  assert.equal(p.ageLabel(null),'never');
  p.destroy();
});

test('the drill banner stays up until the drill passes; the checklist drives first run',async()=>{
  const h=harness('/backups');
  h.respond(()=>data(testView({checklist_done:false,checklist:[
    {id:'storage',label:'Connect storage',state:'done'},
    {id:'alerts',label:'Add an alert channel',state:'todo',detail:'Failures have nowhere to go.'},
    {id:'recovery',label:'Generate a recovery key and do the decrypt drill',state:'todo',detail:'The key exists but has not been proven.'},
    {id:'verified',label:'First verified backup',state:'todo'},
  ]})));
  const p=h.page('backupsPage');await p.init();
  assert.equal(p.recovery.banner,true);
  assert.equal(p.checklist.filter(i=>i.state==='done').length,1);
  const html=h.readHTML();
  assert.ok(html.includes('Recovery is unproven'),'a persistent banner, not an alert');
  assert.ok(html.includes('Get protected'),'the checklist is shown until complete');
  p.destroy();
});

test('connecting storage probes and pushes once, then forgets the keys in the form',async()=>{
  const h=harness('/backups');
  h.respond(url=>url.endsWith('/api/backups/storage')
    ?data({probe:{write:true,read:true,delete:'removed'},servers:[{server:'prod',prefix:'teploy/prod',pushed:true,verified:true},{server:'stage',prefix:'teploy/stage',pushed:true,verified:false,error:'stored, but could not be verified'}],all_ok:false})
    :data(testView()));
  const p=h.page('backupsPage');await p.init();
  p.storage={provider:'b2',bucket:'teploy-backups',region:'us-west-004',account_id:'',endpoint:'',base_prefix:'teploy',access_key:'KEYID',secret_key:'SECRET',allow_internal:false,delete_refusal_ok:false};
  await p.connectStorage();
  const post=h.requests.find(r=>r.url.endsWith('/api/backups/storage'));
  assert.ok(post,'the connect request was sent');
  assert.equal(post.body.access_key,'KEYID');
  assert.equal(post.body.provider,'b2');
  assert.equal(p.storage.access_key,'','the form forgets the keys');
  assert.equal(p.storageResult.all_ok,false);
  assert.equal(p.storageResult.servers[1].verified,false);
  p.destroy();
});

test('the recovery key is generated once, confirmed by public key, and the drill records',async()=>{
  const h=harness('/backups');
  const key={public_key:'age1lggy',private_key:'AGE-SECRET-KEY-1',file_text:'# Teploy backup recovery key.\nAGE-SECRET-KEY-1\n',file_name:'teploy-recovery-key.txt'};
  h.respond(url=>{
    if(url.endsWith('/api/backups/recovery-key/generate'))return data(key);
    if(url.endsWith('/api/backups/recovery-key/confirm'))return data({servers:[],all_ok:true});
    if(url.endsWith('/api/backups/recovery-key/drill'))return data({ok:true});
    return data(testView({recovery:{banner:true,public_key:'age1lggy'}}));
  });
  const p=h.page('backupsPage');await p.init();
  p.reauthPassword='hunter2!';
  await p.generateKey();
  assert.deepEqual(p.keygen,key);
  const gen=h.requests.find(r=>r.url.endsWith('/api/backups/recovery-key/generate'));
  assert.equal(gen.body.password,'hunter2!');
  await p.confirmKey();
  const conf=h.requests.find(r=>r.url.endsWith('/api/backups/recovery-key/confirm'));
  assert.equal(conf.body.public_key,'age1lggy');
  assert.equal(p.keygen,null,'the private half is dropped once confirmed');
  p.drillOutput='{"key":"s3://teploy-backups/prod/blog/1.age","size_bytes":4000,"encrypted":true}';
  const pasted=p.drillOutput;
  await p.submitDrill();
  const drill=h.requests.find(r=>r.url.endsWith('/api/backups/recovery-key/drill'));
  assert.equal(drill.body.output,pasted);
  p.destroy();
});

test('the one policy posts interval, preset, volumes and the window hours',async()=>{
  const h=harness('/backups');
  h.respond(url=>url.endsWith('/api/backups/policy')
    ?data({servers:[{server:'prod',ok:true}],all_ok:true})
    :data(testView()));
  const p=h.page('backupsPage');await p.init();
  assert.equal(p.policy.interval,'1h','the form adopts the applied policy');
  assert.equal(p.policy.preset,'standard');
  p.policy.window_start_hour=1;p.policy.window_end_hour=5;
  await p.applyPolicy();
  const post=h.requests.find(r=>r.url.endsWith('/api/backups/policy'));
  assert.deepEqual({interval:post.body.interval,preset:post.body.preset,volumes:post.body.volumes,volume_max_gb:post.body.volume_max_gb},
    {interval:'1h',preset:'standard',volumes:true,volume_max_gb:20});
  assert.equal(post.body.window_start_hour,1);
  assert.equal(post.body.window_end_hour,5);
  assert.equal(p.policyResult.all_ok,true);
  p.destroy();
});

test('excluding a target states a reason and is auditable; including takes none',async()=>{
  const h=harness('/backups');
  h.respond(url=>url.endsWith('/api/backups/exclude')
    ?data({excludes:['shop/db']})
    :data(testView()));
  const p=h.page('backupsPage');await p.init();
  const row={server:'prod',name:'shop-db',exclude_name:'shop/db',state:'covered'};
  p.beginExclude(row);
  assert.equal(p.excludeFor,row);
  p.excludeReason='moved to RDS';
  await p.submitExclude(true);
  const post=h.requests.find(r=>r.url.endsWith('/api/backups/exclude'));
  assert.deepEqual({server:post.body.server,name:post.body.name,excluded:post.body.excluded,reason:post.body.reason},
    {server:'prod',name:'shop/db',excluded:true,reason:'moved to RDS'});
  assert.equal(p.excludeFor,null);
  p.beginExclude({server:'prod',name:'shop-db',exclude_name:'shop/db'});
  await p.submitExclude(false);
  const include=h.requests.filter(r=>r.url.endsWith('/api/backups/exclude')).pop();
  assert.equal(include.body.excluded,false);
  p.destroy();
});

test('an older CLI is a note, not an error',async()=>{
  const h=harness('/backups');
  h.respond(()=>data(testView({cli_supported:false,note:'Backups needs teploy v0.1.52 or later on the dashboard host.',rows:[]})));
  const p=h.page('backupsPage');await p.init();
  assert.equal(p.view.cli_supported,false);
  assert.equal(p.loadFailed,false);
  const html=h.readHTML();
  assert.ok(html.includes('x-if="!loading && view && !view.cli_supported"'));
  p.destroy();
});
