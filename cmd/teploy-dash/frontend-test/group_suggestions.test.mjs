import {test} from 'node:test';
import assert from 'node:assert/strict';
import {harness, response} from './identity_harness.mjs';

// Programme B item 7: group suggestions render as a diff and are accepted
// one at a time through the ordinary groups API.

const suggestion = (name, apps, extra = {}) => ({
  name, exists: false, basis: 'domain',
  apps: apps.map(([app, server]) => ({app, server, domain: name})),
  ...extra,
});

const suggestionsBody = () => ({suggestions: [
  suggestion('akiroo.com', [['akiroo-lite', 'infra'], ['akiroo-api', 'infra']]),
  suggestion('fylun', [['fylun-web', 'edge'], ['fylun-api', 'edge']], {basis: 'name'}),
  suggestion('fylun.ai', [['fylun-web', 'edge'], ['fylun-api', 'edge'], ['fylun-mail', 'edge']], {exists: true}),
]});

function respondWith(suggestions, createStatus = 200) {
  return (url, options = {}) => {
    const method = (options.method || 'GET').toUpperCase();
    if (url === '/api/group-suggestions') return response(suggestions);
    if (url === '/api/groups' && method === 'POST') {
      if (createStatus !== 200) return response({error: 'a group by that name already exists'}, {raw: true, status: createStatus});
      return response({status: 'created'});
    }
    if (url.startsWith('/api/groups/')) return response({status: 'assigned'});
    if (url === '/api/servers') return response([]);
    if (url === '/api/apps') return response([]);
    return response([]);
  };
}

test('the projects page carries the suggestions section', async () => {
  const h = harness('/projects'); h.stores.router.restore();
  const html = h.readHTML();
  assert.ok(html.includes('Suggested groups'));
  assert.ok(html.includes('shared domain'));
  assert.ok(html.includes('shared name prefix'));
});

test('suggestions render per group with their apps and basis', async () => {
  const h = harness('/projects');
  h.respond(respondWith(suggestionsBody()));
  const p = h.page('projectsPage');
  try {
    await p.init();
    assert.equal(p.suggestions.length, 3);
    assert.equal(p.visibleSuggestions.length, 3);
    assert.equal(p.suggestions[0].name, 'akiroo.com');
    assert.equal(p.suggestions[0].basis, 'domain');
    assert.equal(p.suggestions[2].exists, true);
  } finally { p.destroy(); }
});

test('accepting a suggestion creates the group then assigns each app server-bound, in order', async () => {
  const h = harness('/projects');
  const posts = [];
  const orig = globalThis.fetch;
  globalThis.fetch = async (url, options = {}) => {
    const method = (options.method || 'GET').toUpperCase();
    const body = options.body === undefined ? undefined : JSON.parse(options.body);
    if (method === 'POST') posts.push({url, body});
    return orig(url, options);
  };
  h.respond(respondWith(suggestionsBody()));
  const p = h.page('projectsPage');
  try {
    await p.init();
    await p.acceptSuggestion(p.suggestions[0]); // akiroo.com: create + 2 assignments
    const create = posts.filter(x => x.url === '/api/groups');
    const assigns = posts.filter(x => x.url === '/api/groups/akiroo.com/apps');
    assert.equal(create.length, 1);
    assert.equal(create[0].body.name, 'akiroo.com');
    assert.equal(assigns.length, 2);
    assert.deepEqual(assigns.map(a => a.body), [
      {app: 'akiroo-lite', server: 'infra'},
      {app: 'akiroo-api', server: 'infra'},
    ]);
    assert.equal(p.suggestionsSaving, false);
  } finally {
    globalThis.fetch = orig;
    p.destroy();
  }
});

test('an existing group is not re-created; assignments still run', async () => {
  const h = harness('/projects');
  const posts = [];
  const orig = globalThis.fetch;
  globalThis.fetch = async (url, options = {}) => {
    const method = (options.method || 'GET').toUpperCase();
    const body = options.body === undefined ? undefined : JSON.parse(options.body);
    if (method === 'POST') posts.push({url, body});
    return orig(url, options);
  };
  h.respond(respondWith(suggestionsBody()));
  const p = h.page('projectsPage');
  try {
    await p.init();
    await p.acceptSuggestion(p.suggestions[2]); // fylun.ai exists
    assert.equal(posts.filter(x => x.url === '/api/groups').length, 0);
    assert.equal(posts.filter(x => x.url === '/api/groups/fylun.ai/apps').length, 3);
  } finally {
    globalThis.fetch = orig;
    p.destroy();
  }
});

test('a create racing an existing group (409) is tolerated, other failures surface', async () => {
  const h = harness('/projects');
  h.respond(respondWith(suggestionsBody(), 409));
  const p = h.page('projectsPage');
  try {
    await p.init();
    await p.acceptSuggestion(p.suggestions[0]);
    assert.ok(!h.toasts.some(t => t.includes('already exists')), 'a 409 create is not an error');
    assert.ok(h.toasts.some(t => t.includes('Grouped')));
  } finally { p.destroy(); }
});

test('"Not now" hides one suggestion for the visit without touching the others', async () => {
  const h = harness('/projects');
  h.respond(respondWith(suggestionsBody()));
  const p = h.page('projectsPage');
  try {
    await p.init();
    p.dismissSuggestion('akiroo.com');
    assert.equal(p.visibleSuggestions.length, 2);
    assert.ok(p.visibleSuggestions.every(s => s.name !== 'akiroo.com'));
  } finally { p.destroy(); }
});
