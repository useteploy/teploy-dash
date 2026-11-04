import {test} from 'node:test';
import assert from 'node:assert/strict';
import {harness,response} from './identity_harness.mjs';

test('D17: channel secrets keep, replace and clear with only the chosen semantics',async()=>{
 const h=harness('/settings'),page=h.page('alertsSettings');
 const select=h.readHTML().match(/<select[^>]*x-model="editor.secretMode"[^>]*>/)[0];
 assert.ok(!select.includes(':disabled='),'secret selector must allow Replace');
 h.respond((url,options)=>response(options.method==='PUT'?{id:'hook'}:{channels:[]}));
 for(const [mode,draft,expected] of [['keep','',undefined],['replace','fixture-signing-value','fixture-signing-value'],['clear','','']]){
  page.editChannel({id:'hook',type:'webhook',url:'https://h.example',url_set:true,secret_set:true,kinds:[]});
  page.editor.secretMode=mode;page.editor.secretDraft=draft;
  await page.saveChannel();
  const body=h.requests.filter(r=>r.method==='PUT').at(-1).body;
  assert.equal(body.secret,expected);
  assert.equal('secret_set' in body,false);
  assert.equal('type' in body,false,'an edit never resends the type');
 }
});

test('Alerts v2: credential URLs are kept when left blank, and the Slack placeholder is not offered to generic webhooks',async()=>{
 const h=harness('/settings'),page=h.page('alertsSettings');
 h.respond((url,options)=>response(options.method==='PUT'?{id:'chat'}:{channels:[]}));
 page.editChannel({id:'chat',type:'slack',url_set:true,url_hint:'https://hooks.slack.com/...',kinds:['incident_opened']});
 await page.saveChannel();
 let body=h.requests.filter(r=>r.method==='PUT').at(-1).body;
 assert.equal('url' in body,false,'blank credential URL must be omitted (keep)');
 assert.deepEqual(body.kinds,['incident_opened']);
 page.newChannel('ship');page.editor.url='https://ship.example/api/intake/dash';
 await page.saveChannel();
 assert.ok(page.editor,'ship without a secret must not save');
 assert.ok(!page.urlPlaceholder('webhook').includes('slack'),'webhook placeholder must not suggest Slack');
 page.newChannel('ntfy');page.editor.url='https://ntfy.sh/t';
 await page.saveChannel();
 body=h.requests.filter(r=>r.method==='PUT').at(-1).body;
 assert.equal(body.type,'ntfy');assert.equal('token' in body,false);
});

test('D20: conflicted deletions preserve the refreshed list and matching ETag for both editors',async()=>{
 for(const component of ['homepagePage','linksSettings']){
  const h=harness('/'),page=h.page(component);
  page.items=[{id:'A',name:'A',url:'https://a.test'},{id:'B',name:'B',url:'https://b.test'}];page._etag='old';
  let conflict=true;
  h.respond(()=>conflict?response({error:'changed'},{raw:true,status:412}):response({saved:true}));
  page.init=async()=>{page.items=[{id:'A',name:'A',url:'https://a.test'},{id:'B',name:'B',url:'https://b.test'},{id:'C',name:'Concurrent',url:'https://c.test'}];page._etag='new';};
  await page.remove('A');
  assert.deepEqual(page.items.map(i=>i.id),['A','B','C']);assert.equal(page._etag,'new');
  conflict=false;await page.remove('B');
  const request=h.requests.at(-1);
  assert.equal(new Headers(request.headers).get('If-Match'),'new');assert.deepEqual(request.body.map(i=>i.id),['A','C']);
 }
});

test('D06 tail: capability editor saves an explicit custom set and retains failed drafts',async()=>{
 const h=harness('/settings'),page=h.page('settingsPage');
 const user={username:'operator',capabilities:['view.metadata','execute.deploy']};
 page.editCapabilities(user);page.capabilityEditor.capabilities=['view.metadata'];
 assert.deepEqual(user.capabilities,['view.metadata','execute.deploy']);
 h.respond((url,options)=>response(options.method==='PUT'?{saved:true}:[{...user,capabilities:['view.metadata'],capability_profile:'custom'}]));
 await page.saveCapabilities();
 assert.deepEqual(h.requests[0].body,{capabilities:['view.metadata']});assert.equal(page.capabilityEditor,null);
 page.editCapabilities(user);h.respond(()=>response({error:'unavailable'},{raw:true,status:503}));
 await page.saveCapabilities();assert.ok(page.capabilityEditor);assert.equal(page.capabilityEditor.saving,false);
});

test('Alerts v2: Ship placeholder names the incidents intake; allow_internal is sent for URL channels',async()=>{
 const h=harness('/settings'),page=h.page('alertsSettings');
 assert.ok(page.urlPlaceholder('ship').endsWith('/api/incidents/intake/dash'));
 h.respond((url,options)=>response(options.method==='PUT'?{id:'akiroo'}:{channels:[]}));
 page.newChannel('webhook');page.editor.url='https://100.64.0.9/hooks/dash';page.editor.allow_internal=true;
 await page.saveChannel();
 assert.equal(h.requests.filter(r=>r.method==='PUT').at(-1).body.allow_internal,true);
});

test('Alerts v2: the push URL is read from the gated endpoint, not the monitor record',async()=>{
 const h=harness('/monitors/p1'),page=h.page('monitorsPage');
 h.respond(url=>response({path:'/api/push/'+'ab'.repeat(32)},{raw:true}));
 await page.revealPushURL({id:'p1',type:'push'});
 assert.equal(h.requests.at(-1).url,'/api/monitors/p1/push-url');
 assert.ok(page.pushURL().endsWith('/api/push/'+'ab'.repeat(32)));
});
