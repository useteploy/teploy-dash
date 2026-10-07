// Zero-dependency component harness, matching async_states.test.mjs.
// The optional source root lets audit tooling prove failures at the base SHA
// without modifying that snapshot. DOM/network/Alpine remain test doubles.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

export function harness(pathname = '/operations/A') {
  const root = process.env.DASH_FRONTEND_ROOT || join(dirname(fileURLToPath(import.meta.url)), '../frontend');
  const init = [], effects = new Set(), windowListeners = {}, components = {}, stores = {}, sources = [], requests = [], toasts = [];
  globalThis.document = {
    addEventListener: (name, fn) => { if (name === 'alpine:init') init.push(fn); },
    getElementById: id => id === 'toast-container' ? { appendChild: el => toasts.push(el.textContent) } : null,
    createElement: () => ({ style: {}, classList: { add() {} }, setAttribute() {}, remove() {}, appendChild() {} }),
    documentElement: { setAttribute() {}, getAttribute: () => 'dark' },
  };
  globalThis.window = {
    addEventListener: (name, fn) => (windowListeners[name] ||= []).push(fn),
    dispatchEvent() {},
  };
  globalThis.localStorage = { getItem: () => null, setItem() {} };
  globalThis.CustomEvent = class {};
  globalThis.location = { pathname, search: '' };
  globalThis.history = { pushState: (_, __, url) => { location.pathname = url; }, replaceState() {} };
  globalThis.EventSource = class {
    constructor(url) { this.url = url; this.closed = false; this.listeners = {}; sources.push(this); }
    addEventListener(name, fn) { this.listeners[name] = fn; }
    close() { this.closed = true; }
    emit(name, data) { this.listeners[name]?.({ data: JSON.stringify({ data }) }); }
  };
  globalThis.Alpine = {
    data: (name, factory) => { components[name] = factory; },
    store: (name, value) => { if (value !== undefined) stores[name] = value; return stores[name]; },
    effect: fn => { effects.add(fn); fn(); return fn; },
    release: fn => effects.delete(fn),
  };
  let respond = url => response({ id: url.split('/').at(-1), status: 'failed', request: { kind: 'deploy' } });
  globalThis.fetch = async (url, options = {}) => {
    requests.push({ url, ...options, body: options.body === undefined ? undefined : JSON.parse(options.body) });
    return respond(url, options);
  };
  new Function(readFileSync(join(root, 'js/app.js'), 'utf8') + '\n;' + readFileSync(join(root, 'js/operations.js'), 'utf8'))();
  init.forEach(fn => fn());
  return {
    components, stores, sources, requests, toasts, effects,
    readHTML: () => readFileSync(join(root, 'index.html'), 'utf8'),
    respond(fn) { respond = fn; },
    page(name) { const page = components[name](); page.$refs = {}; page.$nextTick = fn => fn(); return page; },
    pop(path) { location.pathname = path; (windowListeners.popstate || []).forEach(fn => fn()); },
    async flush() { for (const fn of [...effects]) fn(); for (let i = 0; i < 20; i++) await Promise.resolve(); },
  };
}

export function response(value, { raw = false, status = 200 } = {}) {
  return { status, ok: status >= 200 && status < 300, headers: new Headers({ 'Content-Type': 'application/json' }),
    text: async () => JSON.stringify(raw ? value : { data: value }) };
}
export function deferred() {
  let resolve, reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
