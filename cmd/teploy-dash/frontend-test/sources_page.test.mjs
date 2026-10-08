import {test} from 'node:test';
import assert from 'node:assert/strict';
import {harness,response,deferred} from './identity_harness.mjs';
function sourceHarness() {
 const h=harness('/sources');
 h.respond(url=>response(url==='/api/sources'?[]:url.endsWith('/previews')?{live:[],operations:[]}:{id:url.split('/').at(-1)}));
 return h;
}
test('source selection and destruction seal late detail/list publication',async()=>{
 const h=sourceHarness(),p=h.page('sourcesPage'),old=deferred();
 h.respond(url=>url==='/api/sources/old'?old.promise:response(url.endsWith('/previews')?{}:{id:'new'}));
 const pending=p.select('old');await p.select('new');old.resolve(response({id:'old'}));await pending;assert.equal(p.selected.id,'new');
 const list=deferred();h.respond(()=>list.promise);const load=p.load();p.destroy();list.resolve(response([{id:'old'}]));await load;
 await p.select('old');assert.equal(p.selected,null);assert.equal(p.webhookSecret,'');assert.deepEqual(p.sources,[]);
});
for(const phase of ['post','list','detail','previews']) for(const change of ['destroy','select']) {
 test(`rotate secret cannot revive after ${change} during ${phase}`,async()=>{
  const h=sourceHarness(),p=h.page('sourcesPage');await p.select('old');const paused=deferred();
  const pauseURL={post:'/api/sources/old/rotate-secret',list:'/api/sources',detail:'/api/sources/old',previews:'/api/sources/old/previews'}[phase];let waiting=false;
  h.respond(url=>{if(url===pauseURL){waiting=true;return paused.promise};return response(url.endsWith('rotate-secret')?{webhook_secret:'once-old'}:url==='/api/sources'?[]:url.endsWith('/previews')?{}:{id:url.split('/').at(-1)})});
  const action=p.action('rotate-secret');for(let n=0;n<80&&!waiting;n++)await Promise.resolve();assert.equal(waiting,true);
  if(change==='destroy')p.destroy();else await p.select('new');
  paused.resolve(response(phase==='post'?{webhook_secret:'once-old'}:phase==='list'?[]:phase==='detail'?{id:'old'}:{}));await action;
  assert.equal(p.webhookSecret,'');assert.equal(p.selected?.id,change==='destroy'?undefined:'new');
 });
}
test('failed create refresh cannot publish once-only secret',async()=>{
 const h=sourceHarness(),p=h.page('sourcesPage');
 h.respond((url,options)=>options.method==='POST'?response({id:'created',webhook_secret:'once-created'}):Promise.reject(new Error('refresh unavailable')));
 await p.create();assert.equal(p.webhookSecret,'');assert.equal(p.loadError,'refresh unavailable');
});
for(const kind of ['create','verify']) for(const phase of ['post','list','detail','previews']) for(const change of ['destroy','select']) {
 test(`${kind} cannot publish after ${change} during ${phase}`,async()=>{
  const h=sourceHarness(),p=h.page('sourcesPage');await p.select('old');const paused=deferred();
  const issuing=kind==='create'?'created':'old';
  const postURL=kind==='create'?'/api/sources':'/api/sources/old/verify';
  let waiting=false;
  h.respond((url,options)=>{
   const isPost=options.method==='POST';
   const pause=phase==='post'?isPost&&url===postURL:phase==='list'?!isPost&&url==='/api/sources':url===`/api/sources/${issuing}${phase==='previews'?'/previews':''}`;
   if(pause){waiting=true;return paused.promise;}
   return response(isPost?{id:issuing,webhook_secret:'once-issued'}:url==='/api/sources'?[]:url.endsWith('/previews')?{}:{id:url.split('/').at(-1)});
  });
  const pending=kind==='create'?p.create():p.action('verify');
  for(let n=0;n<80&&!waiting;n++)await Promise.resolve();assert.equal(waiting,true);
  if(change==='destroy')p.destroy();else await p.select('new');
  paused.resolve(response(phase==='post'?{id:issuing,webhook_secret:'once-issued'}:phase==='list'?[]:phase==='detail'?{id:issuing}:{}));
  await pending;assert.equal(p.webhookSecret,'');assert.equal(p.selected?.id,change==='destroy'?undefined:'new');
 });
}
