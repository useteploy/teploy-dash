import {test} from 'node:test';
import assert from 'node:assert/strict';
import {harness,response} from './identity_harness.mjs';

test('D17: absent and cleared webhook secrets allow replacement and send only chosen semantics',async()=>{
 const h=harness('/settings'),page=h.page('settingsPage');
 const select=h.readHTML().match(/<select[^>]*x-model="webhookSecretMode"[^>]*>/)[0];
 assert.ok(!select.includes(':disabled='),'initial secret selector must allow Replace');
 h.respond((url,options)=>response(options.method==='POST'?{saved:true}:{webhook_secret_set:false}));
 page.notifications={webhook_secret_set:false};
 for(const [mode,draft,expected] of [['keep','',undefined],['replace','fixture-signing-value','fixture-signing-value'],['clear','','']]){
  page.webhookSecretMode=mode;page.webhookSecretDraft=draft;
  await page.saveNotifications();
  const body=h.requests.filter(r=>r.method==='POST').at(-1).body;
  assert.equal(body.webhook_secret,expected);
  assert.equal('webhook_secret_set' in body,false);
 }
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
