// ── Top loading bar ──
// A blue→white light sweeps left→right across the bottom of the header while
// any request is in flight, and completes at the right edge when it finishes.
// Replaces the old inline spinners; driven by every api/rawFetch call below.
const progressBar = {
  inflight: 0,
  generation: 0, // F064: a newer request's start() invalidates older hide timers
  hideTimer: null,
  resetTimer: null,
  _el() { return document.getElementById('load-bar'); },
  start() {
    this.inflight++;
    // F064: cancel a pending hide/reset from a previous completion — a new
    // request beginning inside the 220ms window used to have its bar hidden
    // (and width zeroed) by the OLD timers while it was loading.
    this.generation++;
    clearTimeout(this.hideTimer);
    clearTimeout(this.resetTimer);
    if (this.inflight !== 1) return; // only kick off on the first in-flight req
    const el = this._el();
    if (!el) return;
    el.style.transition = 'none';
    el.style.width = '0%';
    el.style.opacity = '1';
    void el.offsetWidth; // reflow so the reset lands before the sweep animates
    el.style.transition = '';
    el.style.width = '90%'; // race toward the right, easing as it goes
  },
  done() {
    if (this.inflight > 0) this.inflight--;
    if (this.inflight !== 0) return; // wait until every request has settled
    const el = this._el();
    if (!el) return;
    el.style.width = '100%'; // snap to the right edge — loading complete
    const generation = this.generation;
    this.hideTimer = setTimeout(() => {
      if (this.inflight || generation !== this.generation) return;
      el.style.opacity = '0';
      this.resetTimer = setTimeout(() => {
        if (!this.inflight && generation === this.generation) el.style.width = '0%';
      }, 300);
    }, 220);
  },
};

async function trackedFetch(...args) {
  progressBar.start();
  try {
    return await fetch(...args);
  } finally {
    progressBar.done();
  }
}

// ── API Client ──
// requestJSON is the single shared parser: it checks HTTP status (a plain-text
// 401/500 used to surface as a JSON parse error), understands 204, rejects
// non-JSON success payloads, and unwraps the {data: ...} envelope when asked
// (raw endpoints like monitors answer directly).
async function requestJSON(url, options = {}, unwrap = true) {
  const {body, _meta, ...init} = options;
  const headers = new Headers(init.headers || {});
  headers.set('Accept', 'application/json');
  if (body !== undefined) headers.set('Content-Type', 'application/json');
  const response = await trackedFetch(url, {
    ...init, headers, credentials: 'same-origin', cache: 'no-store',
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (_meta) _meta.etag = response.headers.get('ETag') || '';
  if (response.status === 401) {
    window.dispatchEvent(new CustomEvent('teploy:unauthorized'));
  }
  if (response.status === 204 || response.status === 205) return null;
  const text = await response.text();
  const media = response.headers.get('Content-Type') || '';
  let payload;
  if (media.includes('json') && text) {
    try { payload = JSON.parse(text); }
    catch { if (response.ok) throw new Error('Invalid JSON response'); }
  }
  if (!response.ok || payload?.error) {
    const detail = typeof payload?.error === 'string' ? payload.error : payload?.error?.message;
    const fallback = media.startsWith('text/plain') ? text.trim().slice(0, 512) : '';
    const error = new Error(detail || fallback || `Request failed (HTTP ${response.status})`);
    error.status = response.status;
    throw error;
  }
  if (payload === undefined) throw new Error('Expected a JSON response');
  if (!unwrap) return payload;
  if (payload === null || typeof payload !== 'object' || !Object.hasOwn(payload, 'data')) {
    throw new Error('Unexpected API response shape');
  }
  return payload.data;
}

const api = {
  get: (url, options = {}) => requestJSON(url, options, true),
  post: (url, body, options = {}) => requestJSON(url, {...options, method: 'POST', body}, true),
  put: (url, body, options = {}) => requestJSON(url, {...options, method: 'PUT', body}, true),
  del: (url, options = {}) => requestJSON(url, {...options, method: 'DELETE'}, true),
};

// ── Raw fetch helper for monitors (they return data directly, not wrapped) ──
const rawFetch = {
  get: (url, options = {}) => requestJSON(url, options, false),
  post: (url, body, options = {}) => requestJSON(url, {...options, method: 'POST', body}, false),
  del: (url, options = {}) => requestJSON(url, {...options, method: 'DELETE'}, false),
};

// An enqueued operation, not a completed result. Mutating actions now return
// one of these, so callers must follow it rather than report success.
function isOperation(result) {
  return !!(result && typeof result === 'object' && result.id && result.status && result.request);
}

// ── Deploy-form host readiness (D03 onboarding preflight) ──
// Shared by the two deploy forms (projects page + project detail). Loads
// /api/onboarding/preflight for the selected server and exposes the checks:
// blocking failures gate the Deploy button (canDeploy), warnings render
// non-blocking, and an unreadable preflight leaves readiness UNKNOWN and
// gated — never a silent pass. Late responses are dropped when the server
// selection changed mid-flight.
function withDeployReadiness(component) {
  component.preflight = null;
  component.preflightLoading = false;
  component.preflightError = null;
  component.loadPreflight = async function () {
    const server = this.deployForm && this.deployForm.server;
    this.preflight = null;
    this.preflightError = null;
    if (!server) return;
    this.preflightLoading = true;
    try {
      const env = await api.get('/api/onboarding/preflight?server=' + encodeURIComponent(server));
      if (this.deployForm.server !== server) return;
      this.preflight = env;
    } catch (e) {
      if (this.deployForm.server === server) this.preflightError = e.message;
    } finally {
      if (this.deployForm.server === server) this.preflightLoading = false;
    }
  };
  component.resetPreflight = function () {
    this.preflight = null;
    this.preflightLoading = false;
    this.preflightError = null;
  };
  Object.defineProperties(component, {
    preflightChecks: {
      get() { return (this.preflight && this.preflight.checks) || []; },
      enumerable: true,
    },
    blockingChecks: {
      get() { return this.preflightChecks.filter((c) => c.severity === 'blocking' && c.result !== 'pass'); },
      enumerable: true,
    },
    warningChecks: {
      get() { return this.preflightChecks.filter((c) => c.severity === 'warning' && c.result !== 'pass'); },
      enumerable: true,
    },
    canDeploy: {
      get() {
        const f = this.deployForm;
        if (!f || !f.app || !f.image || !f.domain || !f.server) return false;
        if (this.preflightLoading || this.preflightError || !this.preflight) return false;
        return this.preflight.ready !== false;
      },
      enumerable: true,
    },
  });
  component.preflightCheckClass = function (c) {
    return 'preflight-check check-' + c.result + ' sev-' + c.severity;
  };
  return component;
}

// ── Async view states (D07: misleading states are P0) ──
// withAsyncLoad gives a page the four-state discipline every async surface
// must distinguish: loading / error / empty / stale.
//   - loadError: the last FAILED load's message (null after a success);
//   - loadedAt:  the last SUCCESSFUL load's time (null until one lands).
// Derived:
//   - loadFailed:   a failure with no data behind it — render the error
//     state with a Retry, never the empty state (which used to claim "no
//     monitors yet" while the API was down);
//   - showingStale: a failure with earlier data still displayed — keep the
//     data and name its age and the exact failure; it must not paint as
//     fresh and it must not vanish.
// The empty state renders only after a successful load.
function withAsyncLoad(component) {
  component.loadError = null;
  component.loadedAt = null;
  Object.defineProperties(component, {
    loadFailed: { get() { return !!this.loadError; }, enumerable: true },
    showingStale: { get() { return !!this.loadError && !!this.loadedAt; }, enumerable: true },
  });
  component.staleBannerText = function () {
    if (!this.showingStale) return '';
    return 'Showing data from ' + formatObservedAt(this.loadedAt) + ' — refresh failed: ' + this.loadError;
  };
  return component;
}

// ── Toast ──
function showToast(message, type = 'info') {
  const container = document.getElementById('toast-container');
  const toast = document.createElement('div');
  toast.className = `toast ${type}`;
  toast.textContent = message;
  container.appendChild(toast);
  setTimeout(() => toast.remove(), 4000);
}

// ── Theme ──
// F064: storage access can throw (locked-down browsers, disabled storage) —
// a throw here used to abort Alpine initialization; unexpected stored
// values normalize to dark.
function readTheme() {
  try {
    const saved = localStorage.getItem('teploy-theme');
    return saved === 'light' ? 'light' : 'dark';
  } catch { return 'dark'; }
}

function initTheme() {
  const saved = readTheme();
  document.documentElement.setAttribute('data-theme', saved);
  return saved;
}

function toggleTheme() {
  const current = document.documentElement.getAttribute('data-theme');
  const next = current === 'dark' ? 'light' : 'dark';
  document.documentElement.setAttribute('data-theme', next);
  try { localStorage.setItem('teploy-theme', next); } catch {}
  return next;
}

// formatObservedAt renders a timestamp for humans; unparseable values pass
// through verbatim rather than rendering "Invalid Date". Top-level so both
// the fleet pages and the shared async-state helper (withAsyncLoad) use one
// formatter.
function formatObservedAt(iso) {
  const date = new Date(iso);
  return isNaN(date.getTime()) ? String(iso) : date.toLocaleString();
}

// ── Accessibility helpers ──
// trapDialogFocus keeps Tab/Shift+Tab inside an open dialog (A49/A58).
// Attach with @keydown.tab on the dialog overlay; focus is kept within the
// focusable controls it contains, wrapping at both ends. Mechanical by
// design: no heuristics, just the wrap.
function trapDialogFocus(e) {
  const container = e.currentTarget;
  if (!container) return;
  const focusables = Array.from(container.querySelectorAll(
    'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'
  )).filter(el => el.offsetParent !== null);
  if (!focusables.length) return;
  const first = focusables[0];
  const last = focusables[focusables.length - 1];
  const active = document.activeElement;
  const outside = !container.contains(active);
  if (e.shiftKey && (active === first || outside)) {
    e.preventDefault();
    last.focus();
  } else if (!e.shiftKey && (active === last || outside)) {
    e.preventDefault();
    first.focus();
  }
}

// ── Auth ──
// F063: a failed logout must not navigate as though the session ended —
// the server is the authority; only an acknowledged sign-out does.
async function logout() {
  try {
    const response = await fetch('/api/logout', {method: 'POST', credentials: 'same-origin', cache: 'no-store'});
    if (!response.ok) throw new Error(`sign-out failed (HTTP ${response.status})`);
    location.replace('/login');
  } catch (e) {
    showToast(`${e.message}. Your session may still be active — retry before walking away.`, 'error');
  }
}

// randomID generates resource IDs without crypto.randomUUID, which exists
// only in secure contexts (A57) — crypto.getRandomValues does not have that
// restriction.
function randomID() {
  const bytes = new Uint8Array(16);
  crypto.getRandomValues(bytes);
  return Array.from(bytes, b => b.toString(16).padStart(2, '0')).join('').slice(0, 21);
}

// authRole reads the role out of /api/auth/me's envelope: {mode, user:{role}}
// when auth is enabled, {mode:"disabled", ...} when it is off (A05).
function authRole(me) {
  if (!me) return null;
  if (me.mode === 'disabled') return 'admin';
  return (me.user && me.user.role) || me.role || null;
}

// authCaps reads the effective capability set out of /api/auth/me (X03).
// Null (network failure / unknown) means "don't guess" — the server is the
// enforcement point; this only chooses which panel shape to request first.
function authCaps(me) {
  if (!me) return null;
  if (me.mode === 'disabled') return ['*'];
  const caps = me.capabilities || (me.user && me.user.capabilities) || [];
  return Array.isArray(caps) ? caps : [];
}

// ── Alpine.js App ──
document.addEventListener('alpine:init', () => {
  // ── Router Store ──
  // URL-routed navigation: every page has a real path (history API), so
  // views are linkable, reloadable, and back/forward work. The server serves
  // index.html for unknown routes (SPA fallback), so deep links resolve.
  // The store API (page/params/navigate) is unchanged — pages don't know.
  //
  // D07 IA regroup: the canonical paths are unchanged (stable URLs), and
  // task-named ALIASES follow the same pages — /projects, /fleet,
  // /activity deep-link to the same views. Canonical entries stay FIRST in
  // the table so navigate() keeps pushing canonical URLs; aliases only add
  // matchURL recognition. Remove an alias and its links break, so treat them
  // as stable as the canonical paths.
  const ROUTES = [
    { page: 'homepage', path: '/' },
    { page: 'projects', path: '/deployments' },
    { page: 'project-detail', path: '/deployments/groups/:group/:project' },
    { page: 'app-detail', path: '/deployments/:server/:name' },
    { page: 'monitors', path: '/monitors' },
    { page: 'monitor-detail', path: '/monitors/:id' },
    { page: 'restore-tests', path: '/restore-tests' },
    { page: 'templates', path: '/templates' },
    { page: 'servers', path: '/servers' },
    { page: 'server-detail', path: '/servers/:name' },
    { page: 'operations', path: '/operations' },
    { page: 'operation-detail', path: '/operations/:id' },
    { page: 'settings', path: '/settings' },
    // Task-named aliases (D07): same pages, task-meaningful paths.
    { page: 'projects', path: '/projects' },
    { page: 'servers', path: '/fleet' },
    { page: 'server-detail', path: '/fleet/:name' },
    { page: 'operations', path: '/activity' },
    { page: 'operation-detail', path: '/activity/:id' },
  ];

  function routeToURL(page, params) {
    const route = ROUTES.find(r => r.page === page);
    if (!route) return '/';
    const used = new Set();
    const path = route.path.split('/').map(seg => {
      if (!seg.startsWith(':')) return seg;
      const key = seg.slice(1);
      used.add(key);
      return encodeURIComponent(params[key] ?? '');
    }).join('/') || '/';
    // Leftover params ride the query string so breadcrumb context (e.g.
    // fromGroup) survives reloads without polluting the path.
    const qs = new URLSearchParams();
    for (const [k, v] of Object.entries(params)) {
      if (!used.has(k) && v !== null && v !== undefined && v !== '') qs.set(k, v);
    }
    const q = qs.toString();
    return q ? `${path}?${q}` : path;
  }

  function matchURL(pathname, search) {
    for (const route of ROUTES) {
      const want = route.path.split('/');
      const have = pathname.replace(/\/+$/, '').split('/');
      if (want.length !== have.length && !(route.path === '/' && pathname === '/')) continue;
      if (route.path === '/' && pathname !== '/') continue;
      const params = {};
      let ok = true;
      for (let i = 0; i < want.length; i++) {
        if (want[i].startsWith(':')) {
          // A malformed percent-escape (e.g. /apps/prod/web%zz) used to throw
          // out of decodeURIComponent and blank the whole app.
          try { params[want[i].slice(1)] = decodeURIComponent(have[i] || ''); }
          catch { return { page: 'homepage', params: {} }; }
        }
        else if (want[i] !== have[i]) { ok = false; break; }
      }
      if (!ok) continue;
      // Path-derived identifiers are authoritative: a conflicting query
      // parameter must not retarget the page onto another resource.
      for (const [k, v] of new URLSearchParams(search)) {
        if (!Object.hasOwn(params, k)) params[k] = v;
      }
      return { page: route.page, params };
    }
    return { page: 'homepage', params: {} };
  }

  Alpine.store('router', {
    page: 'homepage',
    params: {},
    navigate(page, params = {}) {
      this.page = page;
      this.params = params;
      history.pushState({ page, params }, '', routeToURL(page, params));
    },
    restore() {
      const { page, params } = matchURL(location.pathname, location.search);
      this.page = page;
      this.params = params;
    },
  });
  Alpine.store('router').restore();
  window.addEventListener('popstate', () => Alpine.store('router').restore());

  // ── Service-link icons ──
  // Built-in monochrome glyphs, drawn in the current text colour so they flip
  // with the theme instead of relying on the site's favicon. Favicons are fine
  // when they are colourful; the ones that are a dark mark (or a dark tile with
  // the mark cut out) read as a smudge against a dark header. Path data is
  // 24x24 viewBox; a link can supply its own via the Icon field in Settings.
  const LINK_GLYPHS = {
    'github.com': 'M12 .297c-6.63 0-12 5.373-12 12 0 5.303 3.438 9.8 8.205 11.385.6.113.82-.258.82-.577 0-.285-.01-1.04-.015-2.04-3.338.724-4.042-1.61-4.042-1.61C4.422 18.07 3.633 17.7 3.633 17.7c-1.087-.744.084-.729.084-.729 1.205.084 1.838 1.236 1.838 1.236 1.07 1.835 2.809 1.305 3.495.998.108-.776.417-1.305.76-1.605-2.665-.3-5.466-1.332-5.466-5.93 0-1.31.465-2.38 1.235-3.22-.135-.303-.54-1.523.105-3.176 0 0 1.005-.322 3.3 1.23.96-.267 1.98-.399 3-.405 1.02.006 2.04.138 3 .405 2.28-1.552 3.285-1.23 3.285-1.23.645 1.653.24 2.873.12 3.176.765.84 1.23 1.91 1.23 3.22 0 4.61-2.805 5.625-5.475 5.92.42.36.81 1.096.81 2.22 0 1.606-.015 2.896-.015 3.286 0 .315.21.69.825.57C20.565 22.092 24 17.592 24 12.297c0-6.627-5.373-12-12-12',
    'x.com': 'M18.901 1.153h3.68l-8.04 9.19L24 22.846h-7.406l-5.8-7.584-6.638 7.584H.474l8.6-9.83L0 1.154h7.594l5.243 6.932ZM17.61 20.644h2.039L6.486 3.24H4.298Z',
    'twitter.com': 'M18.901 1.153h3.68l-8.04 9.19L24 22.846h-7.406l-5.8-7.584-6.638 7.584H.474l8.6-9.83L0 1.154h7.594l5.243 6.932ZM17.61 20.644h2.039L6.486 3.24H4.298Z',
  };

  // glyphPath returns the SVG path to draw for a link, or '' to fall back to
  // the favicon. An explicit icon always wins over the built-in table.
  window.linkGlyph = function (item) {
    if (item.icon) return item.icon;
    try {
      const host = new URL(item.url).hostname.replace(/^www\./, '');
      return LINK_GLYPHS[host] || '';
    } catch { return ''; }
  };

  // ── Header service links ──
  // Renders the shortcuts pinned in Settings > Links as icons on the right of
  // the header. Reads the same /api/homepage list the Home grid uses, and
  // reloads when either surface edits it.
  Alpine.data('navLinks', () => ({
    items: [],
    _alive: false,

    async init() {
      this._alive = true;
      await this.load();
      if (!this._alive) return; // destroyed during load (A50)
      window.addEventListener('teploy:links-changed', this._reload = () => this.load());
    },

    destroy() {
      this._alive = false;
      if (this._reload) window.removeEventListener('teploy:links-changed', this._reload);
    },

    async load() {
      try {
        const raw = (await api.get('/api/homepage')) || [];
        // Capped: the header is chrome, not a bookmarks bar.
        this.items = raw.filter(i => i.pinned).slice(0, 8).map(i => ({ ...i, _faviconFailed: false }));
      } catch (e) {
        this.items = [];
      }
    },

    faviconUrl(url) { return faviconCandidates(url)[0] || ''; },

    // Advance to the next candidate path before giving up on an icon. See
    // faviconCandidates.
    faviconError(item, el) {
      const list = faviconCandidates(item.url);
      const next = list[(list.indexOf(el.getAttribute('src')) + 1)] || '';
      if (next) { el.setAttribute('src', next); return; }
      item._faviconFailed = true;
    },

    iconLetters(name) {
      return name.trim().split(/\s+/).slice(0, 2).map(w => w[0].toUpperCase()).join('');
    },
  }));

  // Where a pinned app's icon might live.
  //
  // This used to be one hardcoded `origin + '/favicon.ico'`, in three copies.
  // That path is the 1999 convention and plenty of current software does not
  // serve it - teploy-arcade declares `<link rel="icon" type="image/svg+xml">`
  // and serves only `/favicon.svg`, which is correct and modern, and pinning it
  // here produced coloured initials instead of its icon.
  //
  // The bug survived because dash serves both paths for itself (server.go), so
  // the one app anybody tested this against could not fail.
  //
  // Ordered, not guessed: SVG first because an app that has one is saying so
  // deliberately, then the legacy path, then PNG. Reading <link rel="icon"> out
  // of the app's HTML would be more correct still and is not possible from
  // here - it is cross-origin, and these are arbitrary third-party apps.
  function faviconCandidates(url) {
    let origin;
    try { origin = new URL(url).origin; } catch { return []; }
    return [origin + '/favicon.svg', origin + '/favicon.ico', origin + '/favicon.png'];
  }

  // ── Theme Store ──
  Alpine.store('theme', {
    mode: initTheme(),
    toggle() {
      this.mode = toggleTheme();
    },
  });

  // ── CLI Status Banner ──
  Alpine.data('cliStatusBanner', () => ({
    installed: true,
    loading: true,
    async init() {
      try {
        const status = await rawFetch.get('/api/cli/status');
        this.installed = !!status?.installed;
      } catch { this.installed = false; }
      this.loading = false;
    },
  }));

  // ── Projects Page ──
  Alpine.data('projectsPage', () => withDeployReadiness({
    apps: [],
    groups: [],
    serverList: [],
    search: '',
    loading: true,
    loadError: null,
    deployingToGroup: null,
    deploying: false,
    deployForm: { app: '', image: '', domain: '', server: '', port: 80 },

    async init() {
      await this.load();
    },

    async load() {
      this.loading = true;
      this.loadError = null;
      try {
        const [apps, groups, servers] = await Promise.all([
          api.get('/api/apps'),
          api.get('/api/groups'),
          // /api/servers is viewer-readable; /api/config/servers is admin-only
          // and left a 403-catch producing an empty dropdown for editors.
          api.get('/api/servers'),
        ]);
        this.apps = apps || [];
        this.groups = groups || [];
        this.serverList = Object.keys(servers || {});
      } catch (e) {
        this.loadError = `Could not load deployments: ${e.message}`;
        showToast(this.loadError, 'error');
      }
      this.loading = false;
    },

    openDeployForm(groupName) {
      this.deployingToGroup = groupName;
      this.deployForm = { app: '', image: '', domain: '', server: '', port: 80 };
      this.resetPreflight();
    },

    async doDeploy(groupName) {
      const f = this.deployForm;
      if (!f.app || !f.image || !f.domain || !f.server) {
        showToast('All fields are required', 'error');
        return;
      }
      this.deploying = true;
      try {
        const op = await api.post('/api/deploy', f);
        // Auto-assign the app to this group. A failure here is organizational
        // metadata only — warn separately rather than swallowing it or
        // pretending the deploy failed.
        if (groupName) {
          await api.post(`/api/groups/${encodeURIComponent(groupName)}/apps`, { app: f.app })
            .catch(e => showToast(`Deploy queued, but group assignment failed: ${e.message}`, 'error'));
        }
        this.deployingToGroup = null;
        this.deployForm = { app: '', image: '', domain: '', server: '', port: 80 };
        // A deploy is a queued operation — follow it live rather than
        // claiming success before the build has even started.
        if (isOperation(op)) {
          Alpine.store('router').navigate('operation-detail', { id: op.id });
          return;
        }
        showToast(`Deployed ${f.app} successfully`, 'success');
        await this.load();
      } catch (e) {
        showToast(e.message, 'error');
      } finally {
        this.deploying = false;
      }
    },

    get filteredApps() {
      if (!this.search) return this.apps || [];
      const q = this.search.toLowerCase();
      return (this.apps || []).filter(a =>
        a.name.toLowerCase().includes(q) ||
        a.server.toLowerCase().includes(q)
      );
    },

    groupedApps() {
      const groups = (this.groups || []).map(g => {
        const projectAppNames = new Set((g.projects || []).flatMap(p => p.apps || []));
        const directApps = this.filteredApps.filter(a => (g.apps || []).includes(a.name) && !projectAppNames.has(a.name));
        const projects = (g.projects || []).map(p => ({
          ...p,
          resolvedApps: this.filteredApps.filter(a => (p.apps || []).includes(a.name)),
        }));
        return { ...g, directApps, projects, system: false };
      });
      const allAssigned = new Set((this.groups || []).flatMap(g => {
        const groupApps = g.apps || [];
        const projApps = (g.projects || []).flatMap(p => p.apps || []);
        return [...groupApps, ...projApps];
      }));
      const ungrouped = this.filteredApps.filter(a => !allAssigned.has(a.name));
      if (ungrouped.length > 0) {
        groups.push({ name: 'Ungrouped', directApps: ungrouped, projects: [], system: true });
      }
      return groups;
    },

    openApp(app, fromProject, fromGroup) {
      Alpine.store('router').navigate('app-detail', { name: app.name, server: app.server, fromProject: fromProject || null, fromGroup: fromGroup || null });
    },

    openProject(groupName, projectName) {
      Alpine.store('router').navigate('project-detail', { group: groupName, project: projectName });
    },

    async createProject(groupName) {
      const name = prompt('Project name:');
      if (!name) return;
      try {
        await api.post(`/api/groups/${encodeURIComponent(groupName)}/projects`, { name });
        showToast('Project created', 'success');
        await this.load();
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    async createGroup() {
      const name = prompt('Group name:');
      if (!name) return;
      try {
        await api.post('/api/groups', { name });
        showToast('Group created', 'success');
        await this.load();
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    async deleteProject(groupName, projectName) {
      if (!confirm(`Delete project "${projectName}"? Apps inside remain in the group.`)) return;
      try {
        await api.del(`/api/groups/${encodeURIComponent(groupName)}/projects/${encodeURIComponent(projectName)}`);
        showToast('Project deleted', 'success');
        await this.load();
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    async unassignFromGroup(groupName, appName) {
      if (!confirm(`Remove "${appName}" from group "${groupName}"?`)) return;
      try {
        await api.del(`/api/groups/${encodeURIComponent(groupName)}/apps/${encodeURIComponent(appName)}`);
        showToast('App removed from group', 'success');
        await this.load();
      } catch (e) {
        showToast(e.message, 'error');
      }
    },
  }));

  // ── Project Detail Page ──
  Alpine.data('projectDetailPage', () => withDeployReadiness(withAsyncLoad({
    apps: [],
    groups: [],
    serverList: [],
    loading: true,
    groupName: '',
    projectName: '',
    projectApps: [],
    deployingToProject: false,
    deploying: false,
    deployForm: { app: '', image: '', domain: '', server: '', port: 80 },

    async init() {
      this.groupName = Alpine.store('router').params.group;
      this.projectName = Alpine.store('router').params.project;
      await this.load();
    },

    async load() {
      this.loading = true;
      // Each fetch fails independently; the first failure is the page's
      // error state (the old per-fetch .catch(() => []) painted a dead API
      // as "No apps in this project yet" — D07).
      let failure = null;
      const [apps, groups, servers] = await Promise.all([
        api.get('/api/apps').catch(e => { failure = failure || e; return []; }),
        api.get('/api/groups').catch(e => { failure = failure || e; return []; }),
        // /api/servers is viewer-readable; /api/config/servers is admin-only
        // and left a 403-catch producing an empty dropdown for editors.
        api.get('/api/servers').catch(e => { failure = failure || e; return ({}); }),
      ]);
      this.apps = apps || [];
      this.groups = groups || [];
      this.serverList = Object.keys(servers || {});
      const group = (this.groups || []).find(g => g.name === this.groupName);
      const proj = group ? (group.projects || []).find(p => p.name === this.projectName) : null;
      const projAppNames = proj ? (proj.apps || []) : [];
      this.projectApps = (this.apps || []).filter(a => projAppNames.includes(a.name));
      if (failure) {
        this.loadError = `Could not load the project fully: ${failure.message}`;
      } else {
        this.loadError = null;
        this.loadedAt = new Date().toISOString();
      }
      this.loading = false;
    },

    openApp(app) {
      Alpine.store('router').navigate('app-detail', { name: app.name, server: app.server, fromProject: this.projectName, fromGroup: this.groupName });
    },

    async unassignFromProject(appName) {
      if (!confirm(`Remove "${appName}" from project "${this.projectName}"?`)) return;
      try {
        await api.del(`/api/groups/${encodeURIComponent(this.groupName)}/projects/${encodeURIComponent(this.projectName)}/apps/${encodeURIComponent(appName)}`);
        showToast('App removed from project', 'success');
        await this.load();
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    async deleteThisProject() {
      if (!confirm(`Delete project "${this.projectName}"? Apps remain in the group.`)) return;
      try {
        await api.del(`/api/groups/${encodeURIComponent(this.groupName)}/projects/${encodeURIComponent(this.projectName)}`);
        showToast('Project deleted', 'success');
        Alpine.store('router').navigate('projects');
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    async renameThisProject() {
      const name = prompt('Project name:', this.projectName);
      if (!name || name === this.projectName) return;
      try {
        await api.put(`/api/groups/${encodeURIComponent(this.groupName)}/projects/${encodeURIComponent(this.projectName)}`, { name });
        this.projectName = name;
        Alpine.store('router').params.project = name;
        showToast('Project renamed', 'success');
        await this.load();
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    openDeployForm() {
      this.deployingToProject = true;
      this.deployForm = { app: '', image: '', domain: '', server: '', port: 80 };
      this.resetPreflight();
    },

    async doDeploy() {
      const f = this.deployForm;
      if (!f.app || !f.image || !f.domain || !f.server) {
        showToast('All fields are required', 'error');
        return;
      }
      this.deploying = true;
      try {
        const op = await api.post('/api/deploy', f);
        // Auto-assign to the group and project; failures are metadata-only
        // and warned separately.
        const assign = (url) => api.post(url, { app: f.app })
          .catch(e => showToast(`Deploy queued, but assignment failed: ${e.message}`, 'error'));
        await assign(`/api/groups/${encodeURIComponent(this.groupName)}/apps`);
        await assign(`/api/groups/${encodeURIComponent(this.groupName)}/projects/${encodeURIComponent(this.projectName)}/apps`);
        this.deployingToProject = false;
        this.deployForm = { app: '', image: '', domain: '', server: '', port: 80 };
        if (isOperation(op)) {
          Alpine.store('router').navigate('operation-detail', { id: op.id });
          return;
        }
        showToast(`Deployed ${f.app} successfully`, 'success');
        await this.load();
      } catch (e) {
        showToast(e.message, 'error');
      } finally {
        this.deploying = false;
      }
    },
  })));

  // ── App Detail Page ──
  Alpine.data('appDetailPage', () => withAsyncLoad({
    tab: 'general',
    // resource is the IMMUTABLE identity this page was opened for (A49).
    // Every path and destructive action reads it — never the live router
    // params, which already point at the NEXT app the moment the user
    // navigates app-detail -> app-detail without a remount.
    resource: null,
    app: null,
    envVars: [],
    // D07 config authority: which side owns this app's configuration
    // (git-managed manifest / dash-managed manifest / null = unregistered or
    // unknown). Drives the env tab's authority banner, the per-key
    // inherited-vs-local badges, and the read-only treatment of
    // source-owned keys.
    configAuthority: null,
    deployLog: [],
    accessories: [],
    // D05 database-action inventory state: dbActions holds the server's
    // action classes (support status, blast radius, remedy); dbActionsFor
    // is the accessory row the panel was opened for.
    dbActions: [],
    dbActionsFor: null,
    loading: true,
    actionLoading: false,
    newEnvKey: '',
    newEnvValue: '',
    drift: null,
    driftLoading: false,
    stats: [],
    statsLoading: false,
    health: null,
    healthLoading: false,
    // KV panel. kvValues holds only what the operator explicitly revealed,
    // in component memory — never localStorage/sessionStorage, and never as a
    // substitute for asking the CLI again. A reveal is point-in-time: every
    // list refresh invalidates the revealed values (a value written since the
    // reveal must not display as current), and late responses are dropped
    // when the scope (app/accessory) changed mid-flight (A46).
    kvKeys: [],
    kvReadonly: new Set(),
    kvValues: {},
    kvGeneration: 0,
    kvPattern: '*',
    kvAccessory: 'nucleus',
    kvLoading: false,
    kvSaving: false,
    kvError: '',
    newKvKey: '',
    newKvValue: '',
    newKvTtl: '',
    // role is null until /api/auth/me answers, and stays null when auth is
    // disabled (that endpoint 401s with --no-auth). Both mean "don't hide
    // anything" — the server is the enforcement point; this only avoids
    // showing a viewer buttons that would 403.
    role: null,
    roleLoaded: false,
    // caps carries the session's effective capabilities (X03). null until
    // loaded or when they cannot be determined — the server enforces either
    // way; this only picks the metadata-first panel shape.
    caps: null,

    canRevealSecrets() {
      if (this.caps === null) return true; // unknown: let the server answer
      return this.caps.includes('*') || this.caps.includes('reveal.secrets');
    },

    async init() {
      this.activateResource(Alpine.store('router').params);
      // Same-type navigation (app A -> app B) does not remount this
      // component; watch the route identity and re-activate when it changes
      // so a stale page can never display A while acting on B (A49).
      Alpine.effect(() => {
        const params = Alpine.store('router').params;
        if (Alpine.store('router').page !== 'app-detail') return;
        if (!this.resource) return;
        if (params.server !== this.resource.server || params.name !== this.resource.name) {
          this.activateResource(params);
          this.reload();
        }
      });
      await this.reload();
    },

    // F059: clear EVERYTHING resource-bound when identity changes — not
    // just the visible panels. Previously the retained KV tab could display
    // app A's revealed values under app B, a secret draft typed for A could
    // be submitted to B, and the KV scope/accessory carried over.
    activateResource(params) {
      this.resource = Object.freeze({server: params.server, name: params.name});
      this.tab = 'general';
      this.app = null;
      this.envVars = [];
      this.configAuthority = null;
      this.deployLog = [];
      this.accessories = [];
      this.drift = null;
      this.stats = [];
      this.health = null;
      this.loadError = null;
      this.loadedAt = null;
      this.newEnvKey = '';
      this.newEnvValue = '';
      this.resetKvScope(); // bumps kvGeneration: in-flight KV reads for the old app are dropped
      this.kvAccessory = 'nucleus';
      this.kvPattern = '*';
      this.newKvKey = '';
      this.newKvValue = '';
      this.newKvTtl = '';
      this.kvSaving = false;
      this.actionLoading = false;
      this.roleLoaded = false;
      this.loading = true;
      this.statsLoading = false;
      this.driftLoading = false;
      this.healthLoading = false;
    },

    // F059: scrub secret-bearing state when the component unmounts.
    destroy() {
      this.kvValues = {};
      this.newEnvValue = '';
      this.newKvValue = '';
    },

    async reload() {
      await Promise.all([this.loadStatus(), this.loadAccessories()]);
      // Both are extra SSH round trips; run them after the page paints and in
      // parallel with each other so neither delays the view.
      this.loadDrift();
      this.loadStats();
      this.loadConfigAuthority();
    },

    // D07: which side owns this app's configuration. A manifest read is a
    // local file read (no SSH); a failure degrades to "unknown" — the env
    // tab then shows no indicators and the SERVER guard still refuses
    // source-owned edits explicitly, so a degraded read can only cost the
    // chrome, never create a competing edit.
    async loadConfigAuthority() {
      const target = this.resource;
      try {
        const value = await api.get(`${this.appPath()}/config-authority`);
        if (this.resource !== target) return; // F058
        this.configAuthority = value || null;
      } catch {
        if (this.resource === target) this.configAuthority = null;
      }
    },

    // The authority answer, null when unknown/unregistered.
    authority() { return this.configAuthority && this.configAuthority.registered ? this.configAuthority : null; },

    // Declared-key lookup against the loaded authority.
    declaredKeys() {
      const a = this.authority();
      return a && Array.isArray(a.declared_env_keys) ? a.declared_env_keys : [];
    },

    envKeySource(key) { return this.declaredKeys().includes(key) ? 'manifest' : 'local'; },

    // Git-managed authority: declared keys are owned by the repository.
    gitManaged() { const a = this.authority(); return !!a && a.mode === 'git-managed'; },

    // Resource usage per container. Like drift, a failure here (older bundled
    // CLI without `stats --app`) hides the panel rather than breaking the page.
    async loadStats() {
      const target = this.resource;
      this.statsLoading = true;
      try {
        const value = await api.get(`${this.appPath()}/stats`);
        if (this.resource !== target) return; // F058: late response for a previous app
        this.stats = value || [];
      } catch (e) {
        if (this.resource === target) this.stats = [];
      }
      if (this.resource === target) this.statsLoading = false;
    },

    // Docker reports a stopped container as all-zero rather than omitting it;
    // showing those rows implies the app is idle when it is actually down.
    get liveStats() {
      return (this.stats || []).filter(s => s.memory_usage && !s.memory_usage.startsWith('0B /'));
    },

    // Drift is a separate SSH round trip, so it loads after the page rather
    // than blocking it. A failure here must not break the detail view — an
    // older bundled CLI simply has no `drift --app`.
    async loadDrift() {
      const target = this.resource;
      this.driftLoading = true;
      try {
        const value = await api.get(`${this.appPath()}/drift`);
        if (this.resource !== target) return; // F058
        this.drift = value;
      } catch (e) {
        if (this.resource === target) this.drift = { unavailable: true, error: e.message };
      }
      if (this.resource === target) this.driftLoading = false;
    },

    // On-demand only — deliberately NOT in init(). Unlike drift and stats,
    // which read recorded state, this actively probes the app and can sit
    // there for the CLI's full health timeout, so it runs when asked rather
    // than on every page load.
    //
    // An unhealthy verdict arrives as a non-2xx with a JSON body; that is an
    // answer, not a transport failure, so it renders as a result rather than
    // an error banner.
    async loadHealth() {
      const target = this.resource;
      this.healthLoading = true;
      try {
        const value = await api.get(`${this.appPath()}/health`);
        if (this.resource !== target) return; // F058
        this.health = value;
      } catch (e) {
        // The backend returns an unhealthy VERDICT as a normal payload, so
        // anything landing here is a transport/CLI failure — an older bundled
        // CLI with no `health --app`, or an unreachable server.
        if (this.resource === target) this.health = { healthy: false, error: e.message };
      }
      if (this.resource === target) this.healthLoading = false;
    },

    appPath() {
      const { server, name } = this.resource || Alpine.store('router').params;
      return `/api/apps/${encodeURIComponent(server)}/${encodeURIComponent(name)}`;
    },

    async loadStatus() {
      const target = this.resource;
      this.loading = true;
      try {
        const app = await api.get(`${this.appPath()}/status`);
        if (this.resource !== target) return; // route changed mid-flight (A50)
        this.app = app;
        this.loadError = null;
        this.loadedAt = new Date().toISOString();
      } catch (e) {
        // D07: a failed status read renders the error state with a retry —
        // the old path left the page blank below the back link (toast only).
        if (this.resource === target) this.loadError = `Could not load ${target.name}: ${e.message}`;
      }
      if (this.resource === target) this.loading = false;
    },

    // F058: every panel loader captures the resource identity at request
    // time and drops late responses — a delayed answer for app A used to
    // populate app B's panels after same-type navigation (only loadStatus
    // was guarded), and old rows fed wrong-resource action paths.
    // X03: the env panel has two shapes. With reveal.secrets the full
    // variable map loads and each value stays behind its click-to-show.
    // Without it the panel loads the METADATA variant (names only) — the
    // server would 403 the value read, and the table says so instead of
    // pretending empty values.
    async loadEnv() {
      const target = this.resource;
      if (!this.roleLoaded) await this.loadRole();
      try {
        if (this.canRevealSecrets()) {
          const value = await api.get(`${this.appPath()}/env`);
          if (this.resource !== target) return;
          this.envVars = value || [];
        } else {
          const res = await api.get(`${this.appPath()}/env/keys`);
          if (this.resource !== target) return;
          const keys = (res && res.keys) || [];
          this.envVars = keys.map(key => ({ key, restricted: true }));
        }
      } catch (e) {
        if (this.resource === target) showToast(e.message, 'error');
      }
    },

    async loadLog() {
      const target = this.resource;
      try {
        const value = await api.get(`${this.appPath()}/log`);
        if (this.resource !== target) return;
        this.deployLog = value || [];
      } catch (e) {
        if (this.resource === target) showToast(e.message, 'error');
      }
    },

    async loadAccessories() {
      const target = this.resource;
      try {
        const value = await api.get(`${this.appPath()}/accessories`);
        if (this.resource !== target) return;
        this.accessories = value || [];
      } catch (e) {
        if (this.resource === target) showToast(e.message, 'error');
      }
    },

    // ── KV ──
    //
    // Every method here is a fresh CLI invocation. Nothing about the store is
    // held between views, and a write is followed by a re-list rather than a
    // local edit — dash renders the CLI's answer, it does not keep its own.

    canEdit() { return this.role === null || this.role === 'admin' || this.role === 'editor'; },

    async loadRole() {
      // The endpoint answers explicitly for disabled auth; null (network
      // failure) still means "don't hide anything" — the server enforces.
      const me = await api.get('/api/auth/me').catch(() => null);
      this.role = authRole(me);
      this.caps = me ? authCaps(me) : null;
      this.roleLoaded = true;
    },

    kvQuery(extra) {
      const params = new URLSearchParams(extra || {});
      if (this.kvAccessory) params.set('accessory', this.kvAccessory);
      const q = params.toString();
      return q ? `?${q}` : '';
    },

    // `k in kvValues` on a plain object answers true for inherited members
    // (constructor, toString, hasOwnProperty) — an own-property check keeps
    // prototype-named keys from rendering as already revealed (A54).
    hasKvValue(key) { return Object.hasOwn(this.kvValues, key); },

    // Any scope change drops revealed values and aborts in-flight reads
    // (A54): a value revealed under one accessory must not display under
    // another.
    resetKvScope() {
      ++this.kvGeneration;
      this.kvValues = {};
      this.kvKeys = [];
      this.kvReadonly = new Set();
      this.kvLoading = false;
      this.kvError = '';
    },

    // TTL is a nonnegative integer or unset — parseInt's silent truncation
    // accepted "12abc" and fractional values (A54).
    parseTTL(raw) {
      if (raw === '' || raw === null || raw === undefined) return undefined;
      const ttl = Number(raw);
      if (!Number.isSafeInteger(ttl) || ttl < 0) throw new Error('TTL must be a nonnegative integer');
      return ttl;
    },

    async loadKv() {
      const generation = ++this.kvGeneration;
      const scope = `${this.appPath()}\n${this.kvAccessory}`;
      this.kvLoading = true;
      this.kvError = '';
      try {
        const res = await api.get(`${this.appPath()}/kv${this.kvQuery({ pattern: this.kvPattern || '*' })}`);
        if (generation !== this.kvGeneration || scope !== `${this.appPath()}\n${this.kvAccessory}`) {
          if (generation === this.kvGeneration) this.kvLoading = false;
          return;
        }
        this.kvKeys = (res && res.keys) || [];
        // Keys the store holds but this panel cannot operate on (see the
        // Readonly field in kv.go). Rendering them with live buttons meant
        // Reveal and Remove answered "invalid kv key" as a generic toast.
        this.kvReadonly = new Set((res && res.readonly) || []);
        // A fresh listing invalidates every revealed value: presence of the
        // key does not prove the revealed value is still current.
        this.kvValues = {};
      } catch (e) {
        if (generation !== this.kvGeneration) return;
        this.kvKeys = [];
        this.kvValues = {};
        this.kvReadonly = new Set();
        this.kvError = e.message;
      }
      if (generation === this.kvGeneration) this.kvLoading = false;
    },

    async revealKv(key) {
      const generation = this.kvGeneration;
      const scope = `${this.appPath()}\n${this.kvAccessory}`;
      try {
        const res = await api.get(`${this.appPath()}/kv/value${this.kvQuery({ key })}`);
        // Drop a response that raced a scope change or a newer listing.
        if (generation !== this.kvGeneration || scope !== `${this.appPath()}\n${this.kvAccessory}`) return;
        // An unset key is an answer, not a failure — render it as one.
        this.kvValues[key] = res && res.exists ? res.value : null;
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    async setKv() {
      if (!this.newKvKey) return;
      let ttl;
      try { ttl = this.parseTTL(this.newKvTtl); }
      catch (e) { showToast(e.message, 'error'); return; }
      this.kvSaving = true;
      try {
        const body = { key: this.newKvKey, value: this.newKvValue, accessory: this.kvAccessory || 'nucleus' };
        if (ttl > 0) body.ttl = ttl;
        await api.post(`${this.appPath()}/kv`, body);
        showToast('Key set', 'success');
        this.newKvKey = '';
        this.newKvValue = '';
        this.newKvTtl = '';
        await this.loadKv();
      } catch (e) {
        showToast(e.message, 'error');
      }
      this.kvSaving = false;
    },

    async delKv(key) {
      if (!confirm(`Delete ${key}?\n\nThis KV store is shared — anything else using this accessory loses the key too.`)) return;
      try {
        await api.del(`${this.appPath()}/kv${this.kvQuery({ key })}`);
        showToast('Key removed', 'success');
        await this.loadKv();
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    async switchTab(t) {
      this.tab = t;
      if (t === 'env') await this.loadEnv();
      if (t === 'deploys') await this.loadLog();
      if (t === 'general') await this.loadAccessories();
      if (t === 'logs') this.$nextTick(() => this.$dispatch('start-logs'));
      // Lazy: an SSH round trip per open, never part of init().
      if (t === 'kv') {
        if (!this.roleLoaded) await this.loadRole();
        await this.loadKv();
      }
    },

    async doAction(action) {
      this.actionLoading = true;
      try {
        const result = await api.post(`${this.appPath()}/${action}`);
        // Long-running actions return a queued operation rather than a
        // finished result — hand off to the operation center so the user
        // watches it live instead of being told it already succeeded.
        if (isOperation(result)) {
          Alpine.store('router').navigate('operation-detail', { id: result.id });
        } else {
          showToast(`${action} successful`, 'success');
          await this.loadStatus();
        }
      } catch (e) {
        showToast(e.message, 'error');
      }
      this.actionLoading = false;
    },

    async removeApp() {
      const target = this.resource;
      // The action must match the DISPLAYED record: a stale page whose data
      // has not (yet) loaded, or no longer matches the route, must not
      // submit anything (A49).
      if (!this.app || this.app.name !== target.name || this.app.server !== target.server) return;
      if (!confirm(`Remove ${target.name} from ${target.server}?\n\nThis stops and removes its containers, removes its route, and deletes its deploy state. Volumes and accessory data are preserved.`)) return;
      const redirect = prompt('Optional: leave a permanent redirect to this URL (blank for none):', '');
      if (redirect === null) return; // cancelled
      this.actionLoading = true;
      try {
        const op = await api.post(`/api/apps/${encodeURIComponent(target.server)}/${encodeURIComponent(target.name)}/remove`, { redirect: redirect.trim() });
        if (isOperation(op)) {
          Alpine.store('router').navigate('operation-detail', { id: op.id });
        } else {
          showToast(`Removed ${target.name}`, 'success');
          Alpine.store('router').navigate('projects');
        }
      } catch (e) {
        showToast(e.message, 'error');
      }
      this.actionLoading = false;
    },

    async addEnvVar() {
      if (!this.newEnvKey) return;
      // D07: a declared key under git-managed authority belongs to the
      // repository — the affordance must send the operator to the source,
      // never fire an edit the server would (rightly) refuse as competing.
      if (this.gitManaged() && this.envKeySource(this.newEnvKey) === 'manifest') {
        showToast(`${this.newEnvKey} is declared in the git-managed manifest — change it at the source`, 'error');
        return;
      }
      try {
        await api.post(`${this.appPath()}/env`, { key: this.newEnvKey, value: this.newEnvValue });
        showToast('Env var added', 'success');
        this.newEnvKey = '';
        this.newEnvValue = '';
        await this.loadEnv();
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    async deleteEnvVar(key) {
      if (this.gitManaged() && this.envKeySource(key) === 'manifest') return; // read-only: source-owned
      if (!confirm(`Delete ${key}?`)) return;
      try {
        await api.del(`${this.appPath()}/env/${key}`);
        showToast('Env var removed', 'success');
        await this.loadEnv();
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    containerCount() {
      return (this.app?.containers || []).filter(c => c.State === 'running').length;
    },

    // ── Resource-page overview (D07 exemplar) ──
    // The general tab answers the resource questions in one place: current
    // release, freshness of the observation, and the most recent change.
    // All of it derives from the status payload already on the page.

    // Age of the observation behind the page, e.g. "4m ago"; empty when the
    // server did not stamp one (older CLI). Not a health claim: a fresh
    // observation of a stopped app is still fresh.
    observedAge() {
      const at = this.app?.observed_at;
      if (!at || String(at).startsWith('0001')) return '';
      const ms = Date.now() - new Date(at).getTime();
      if (isNaN(ms) || ms < 0) return '';
      const s = Math.floor(ms / 1000);
      if (s < 60) return s + 's ago';
      if (s < 3600) return Math.floor(s / 60) + 'm ago';
      if (s < 86400) return Math.floor(s / 3600) + 'h ago';
      return Math.floor(s / 86400) + 'd ago';
    },

    observedWhen() {
      const at = this.app?.observed_at;
      if (!at || String(at).startsWith('0001')) return 'unknown';
      return formatObservedAt(at);
    },

    // The most recent change line: when this release landed and what it
    // replaced. deployed_at absent (never deployed / older CLI) says so
    // rather than guessing.
    lastChangeWhen() {
      const at = this.app?.deployed_at;
      if (!at || String(at).startsWith('0001')) return 'not recorded';
      return formatObservedAt(at);
    },

    shortHash(h) {
      return h ? String(h).slice(0, 12) : '';
    },

    openRestoreTests() {
      Alpine.store('router').navigate('restore-tests');
    },

    accessoryName(containerName) {
      const prefix = `${this.app?.name || ''}-`;
      return containerName.startsWith(prefix) ? containerName.slice(prefix.length) : containerName;
    },

    // D05: accessory stop/start are DISTINCT operations, each with its own
    // confirmation stating its blast radius — a database stop and an app
    // restart are different agreements, and confirming them with the same
    // generic prompt is how the wrong one gets approved.
    async accessoryAction(accessory, action) {
      const name = this.accessoryName(accessory.name);
      if (action === 'stop') {
        if (!confirm(`Stop database ${name} on ${this.resource.server}?\n\nApps using ${name} lose their data connection until it is started again. In-flight queries abort. Persisted data is untouched.`)) return;
      } else if (action === 'start') {
        if (!confirm(`Start database ${name} on ${this.resource.server}?\n\nIt comes back empty-memory: connection pools and caches warm up from scratch. No data is changed.`)) return;
      }
      this.actionLoading = true;
      try {
        await api.post(`${this.appPath()}/accessories/${encodeURIComponent(name)}/${action}`);
        showToast(`${name} ${action} sent`, 'success');
        await this.loadAccessories();
      } catch (e) {
        showToast(e.message, 'error');
      }
      this.actionLoading = false;
    },

    // Opens the D05 database-action inventory panel for one accessory,
    // fetching the server-side inventory (support status is the CLI's
    // command surface, not a client guess).
    async toggleDbActions(accessory) {
      if (this.dbActionsFor && this.dbActionsFor.id === accessory.id) {
        this.dbActionsFor = null;
        return;
      }
      this.dbActionsFor = accessory;
      this.dbActions = [];
      try {
        const data = await api.get(`${this.appPath()}/db-actions`);
        this.dbActions = (data && data.actions) || [];
      } catch (e) {
        showToast(e.message, 'error');
        this.dbActionsFor = null;
      }
    },
  }));

  // ── Log Viewer Component ──
  // SSE-only (A24/A32/A33): the server deleted its hand-written WebSocket
  // transport; EventSource is the single log path. Reconnects (with the
  // server re-sending the requested tail on each reconnect) are the
  // browser's built-in behavior — same-origin is enforced server-side.
  Alpine.data('logViewer', () => ({
    source: null,
    lines: [],
    process: 'web',
    lineCount: '100',
    paused: false,
    connected: false,
    autoScroll: true,

    init() {
      this.$el.addEventListener('start-logs', () => this.connect());
    },

    connect() {
      this.disconnect();
      this.lines = [];
      const { server, name } = Alpine.store('router').params;
      const url = `/api/logs/${encodeURIComponent(server)}/${encodeURIComponent(name)}` +
        `?process=${encodeURIComponent(this.process)}&lines=${this.lineCount}`;
      const source = new EventSource(url);
      this.source = source;
      source.onopen = () => { this.connected = true; };
      source.onerror = () => { this.connected = false; };
      // R57: the server frames complete logical lines as JSON `log` events
      // (arbitrary chunk boundaries no longer become fake lines) and
      // reports stream failures as an explicit `stream-error` event.
      source.addEventListener('log', e => {
        if (this.paused) return;
        let line;
        try { line = (JSON.parse(e.data) || {}).line; } catch { line = '[invalid log event]'; }
        if (typeof line !== 'string') return;
        this.lines.push(line);
        if (this.lines.length > 5000) this.lines = this.lines.slice(-2500);
        if (this.autoScroll) {
          this.$nextTick(() => {
            const viewer = this.$refs.logContent;
            if (viewer) viewer.scrollTop = viewer.scrollHeight;
          });
        }
      });
      source.addEventListener('stream-error', e => {
        let message = 'log stream failed';
        try { message = (JSON.parse(e.data) || {}).error || message; } catch {}
        this.lines.push(`[ ${message} ]`);
      });
    },

    disconnect() {
      if (this.source) { this.source.close(); this.source = null; }
      this.connected = false;
    },

    clear() { this.lines = []; },

    togglePause() { this.paused = !this.paused; },

    switchProcess(p) {
      this.process = p;
      this.connect();
    },

    destroy() { this.disconnect(); },
  }));

  // ── Restore Tests Page ──
  Alpine.data('restoreTestsPage', () => withAsyncLoad({
    tests: [],
    servers: [],
    loading: true,
    showCreateDialog: false,
    creating: false,
    running: null,
    refreshInterval: null,
    _loadGeneration: 0,
    newTest: {
      server: '',
      app: '',
      accessory: '',
      bucket: '',
      region: 'us-east-1',
      interval_hours: '24',
    },

    async init() {
      this._alive = true;
      await this.loadTests();
      await this.loadServers();
      if (!this._alive) return; // destroyed during load (A50)
      this.refreshInterval = setInterval(() => this.loadTests(), 30000);
    },

    destroy() {
      this._alive = false;
      this._loadGeneration++; // a response still in flight is stale on destroy
      if (this.refreshInterval) clearInterval(this.refreshInterval);
    },

    async loadTests() {
      const generation = ++this._loadGeneration;
      try {
        const data = await rawFetch.get('/api/restore-tests');
        if (!this._alive || generation !== this._loadGeneration) return;
        this.tests = data || [];
        this.loadError = null;
        this.loadedAt = new Date().toISOString();
      } catch (e) {
        // D07: failed refresh keeps the last list and reports it as stale;
        // failed first load is an error state, never "No restore tests yet".
        if (this._alive && generation === this._loadGeneration) {
          this.loadError = `Could not load restore tests: ${e.message}`;
        }
      }
      if (this._alive && generation === this._loadGeneration) this.loading = false;
    },

    async loadServers() {
      try {
        const raw = (await api.get('/api/servers').catch(() => ({}))) || {};
        this.servers = Array.isArray(raw)
          ? raw
          : Object.entries(raw).map(([name, server]) => ({ name, ...server }));
      } catch { this.servers = []; }
    },

    async createTest() {
      this.creating = true;
      try {
        const body = {
          id: randomID(),
          server: this.newTest.server,
          app: this.newTest.app.trim(),
          accessory: this.newTest.accessory.trim(),
          bucket: this.newTest.bucket.trim(),
          region: this.newTest.region.trim() || 'us-east-1',
          interval_hours: parseInt(this.newTest.interval_hours) || 24,
          enabled: true,
        };
        await rawFetch.post('/api/restore-tests', body);
        this.showCreateDialog = false;
        this.resetForm();
        await this.loadTests();
        showToast('Restore test created', 'success');
      } catch (e) {
        showToast(e.message || 'Failed to save restore test', 'error');
      }
      this.creating = false;
    },

    closeDialog() {
      this.showCreateDialog = false;
      this.resetForm();
    },

    async runNow(t) {
      this.running = t.id;
      try {
        // Downloads the backup and boots a scratch container — takes a while.
        const updated = await rawFetch.post('/api/restore-tests/' + t.id + '/run', {});
        showToast(updated.last_ok ? 'Backup verified: ' + (updated.last_metric || 'ok') : 'Verification FAILED', updated.last_ok ? 'success' : 'error');
        await this.loadTests();
      } catch (e) {
        showToast('Run failed to start', 'error');
      }
      this.running = null;
    },

    async toggleEnabled(t) {
      try {
        // Config-only fields: the API rejects result fields on the upsert
        // (A24) — spread the record minus the last-result columns.
        const {last_run_at, last_ok, last_detail, last_metric, last_date, last_duration_ms, ...config} = t;
        await rawFetch.post('/api/restore-tests', {...config, enabled: !t.enabled});
        await this.loadTests();
        showToast(!t.enabled ? 'Restore test enabled' : 'Restore test disabled', 'success');
      } catch (e) {
        showToast('Failed to toggle restore test', 'error');
      }
    },

    async deleteTest(id) {
      if (!confirm('Delete this restore test?')) return;
      try {
        await rawFetch.del('/api/restore-tests/' + id);
        await this.loadTests();
        showToast('Restore test deleted', 'success');
      } catch (e) {
        showToast('Failed to delete restore test', 'error');
      }
    },

    badgeClass(t) {
      if (!t.last_run_at || t.last_run_at.startsWith('0001')) return 'gray';
      return t.last_ok ? 'green' : 'red';
    },

    // ── Alert delivery status (D08) ──
    // Restore verdicts ride the same durable outbox as monitor transitions
    // (source-scoped); a failed notification for a verification result is
    // operationally silent without this chip.
    deliveryLabel(d) {
      if (!d) return '';
      if (d.status === 'dead_lettered') return 'delivery dead-lettered';
      if (d.status === 'pending') return d.attempts > 0 ? `delivery retrying (attempt ${d.attempts})` : 'delivery pending';
      return '';
    },

    // Only failure-ish states earn the warning chip; plain "delivered" is
    // history, not an alert.
    deliveryWarns(d) {
      return !!d && (d.status === 'dead_lettered' || (d.status === 'pending' && d.attempts > 0));
    },

    resetForm() {
      this.newTest = {
        server: '', app: '', accessory: '',
        bucket: '', region: 'us-east-1', interval_hours: '24',
      };
    },

    formatDate(d) {
      return new Date(d).toLocaleString();
    },
  }));

  // ── Servers Page ──
  // D01 UI depth: cards merge the /api/servers config with /api/fleet's
  // per-server observation envelopes — freshness (fresh|stale|unknown), the
  // error of an unreachable host, its last-known app count, and the stable
  // envelope ID. Unreachable servers stay visible with their last-known
  // state instead of vanishing. Additive: if /api/fleet is unavailable the
  // cards render exactly as before, and /api/apps consumers are untouched.
  Alpine.data('serversPage', () => withAsyncLoad({
    servers: [],
    loading: true,

    async init() {
      await this.load();
    },

    async load() {
      this.loading = true;
      try {
        // /api/servers returns a { name: {host, user} } map; the card x-for
        // keys on s.name, so flatten the map into an array with name. Without
        // this the keys are all undefined and Alpine's x-for crashes the page.
        // A failure here is the page's error state — swallowing it (the old
        // .catch(() => ({}))) painted a dead API as "No servers configured".
        const raw = (await api.get('/api/servers')) || {};
        // The fleet envelopes carry each server's truth; a failure here must
        // not take the page down (additive switch — D01): cards render
        // without freshness chrome instead.
        const fleet = await api.get('/api/fleet').catch(() => null);
        const observed = new Map(((fleet && fleet.servers) || []).map(env => [env.server, env]));
        const merged = (Array.isArray(raw) ? raw : Object.entries(raw).map(([name, s]) => ({ name, ...s })))
          .map(s => withObservation(s, observed.get(s.name)));
        // A server the fleet observed but the config list lacks (e.g. the
        // local-state fallback) stays visible — servers never vanish.
        for (const env of (fleet && fleet.servers) || []) {
          if (!merged.some(s => s.name === env.server)) {
            merged.push(withObservation({ name: env.server, host: env.host }, env));
          }
        }
        this.servers = merged;
        this.loadError = null;
        this.loadedAt = new Date().toISOString();
      } catch (e) {
        // D07: keep earlier cards and report them as stale; a failed first
        // load is an error state, not the "No servers configured" empty one.
        this.loadError = `Could not load servers: ${e.message}`;
      }
      this.loading = false;
    },

    freshnessLabel(s) {
      return ({ fresh: 'Fresh', stale: 'Stale', unknown: 'Unknown' })[s.freshness] || '';
    },

    // The dot reflects observation truth when the fleet answered, and the
    // legacy online flag otherwise.
    cardDotClass(s) {
      if (s.freshness === 'fresh') return 'online';
      if (s.freshness === 'stale') return 'unknown';
      if (s.freshness === 'unknown') return 'offline';
      return s.online ? 'online' : 'offline';
    },

    // One honest line under an unreachable server's error: how many apps it
    // had when last seen, and when that was.
    unreachableMeta(s) {
      const parts = [];
      if (s.appCount !== null && s.appCount !== undefined) {
        parts.push(`${s.appCount} app${s.appCount === 1 ? '' : 's'} (last known)`);
      }
      if (s.lastSuccessAt) parts.push(`last successful observation ${formatObservedAt(s.lastSuccessAt)}`);
      else parts.push('never observed successfully');
      return parts.join(' — ');
    },

    openServer(name) {
      Alpine.store('router').navigate('server-detail', { name });
    },
  }));

  // withObservation projects one fleet envelope onto a server card. No
  // envelope (fleet unavailable, or a config-only server): freshness is null
  // so the card renders no freshness or error chrome at all.
  function withObservation(server, env) {
    if (!env) return { ...server, freshness: null, error: '', appCount: null, lastSuccessAt: null };
    return {
      ...server,
      id: env.id,
      host: server.host || env.host,
      freshness: env.freshness || null,
      error: env.error || '',
      appCount: Array.isArray(env.apps) ? env.apps.length : 0,
      lastSuccessAt: env.last_success_at && !String(env.last_success_at).startsWith('0001') ? env.last_success_at : null,
    };
  }

  // ── Server Detail Page ──
  Alpine.data('serverDetailPage', () => withAsyncLoad({
    status: null,
    proxy: null,
    tab: 'overview',
    loading: true,

    async init() {
      const name = Alpine.store('router').params.name;
      this.loading = true;
      try {
        this.status = await api.get(`/api/servers/${name}/status`);
        this.loadError = null;
        this.loadedAt = new Date().toISOString();
      } catch (e) {
        // D07: a failed status read is an error state with a retry — the old
        // path left the page blank below the back link (toast only).
        this.loadError = `Could not load server status: ${e.message}`;
      }
      this.loading = false;
    },

    async reload() {
      await this.init();
    },

    async switchTab(t) {
      this.tab = t;
      if (t === 'proxy' && !this.proxy) {
        await this.loadProxy();
      }
    },

    async loadProxy() {
      const name = Alpine.store('router').params.name;
      try {
        this.proxy = await api.get(`/api/servers/${name}/proxy`);
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    parsePercent(s) {
      return parseInt(s) || 0;
    },

    barClass(pct) {
      if (pct < 60) return 'low';
      if (pct < 85) return 'medium';
      return 'high';
    },
  }));

  // ── Settings Page ──
  Alpine.data('settingsPage', () => ({
    tab: 'servers',
    servers: {},
    groups: [],
    notifications: {},
    registries: [],
    loading: true,
    // Add server form
    newServer: { name: '', host: '', user: 'root', role: 'app' },
    // Edit server form (null when not editing)
    editingServer: null,
    // Add registry form
    newReg: { server: '', username: '', password: '' },
    // Add group form
    newGroupName: '',
    // Change password form
    pw: { current: '', next: '', confirm: '', saving: false, error: '' },
    // Secret drafts + three-state modes for notifications (A56): keep
    // (omit the field entirely), replace (send the draft), or clear (send
    // an explicit empty string). Before this, a stored secret could never
    // be removed through the form.
    smtpPasswordDraft: '',
    smtpPasswordMode: 'keep',
    webhookSecretDraft: '',
    webhookSecretMode: 'keep',
    // MCP tokens
    mcpTokens: [],
    newMcpToken: { name: '', readOnly: 'false' },
    createdMcpToken: '',
    // Current user + team management
    me: { username: '', role: 'viewer' },
    users: [],
    newUser: { username: '', password: '', role: 'editor' },
    // SSO principals (admin-only endpoint; empty when auth is off, since
    // /api/sso only exists with the session gate).
    ssoPrincipals: [],

    isAdmin() { return (this.me && this.me.role) === 'admin'; },

    async init() {
      await this.loadAll();
    },

    async loadAll() {
      this.loading = true;
      try {
        this.me = await api.get('/api/auth/me').catch(() => ({ username: '', role: 'viewer' }));
        this.me = { username: (this.me && this.me.user && this.me.user.username) || '', role: authRole(this.me) || 'viewer' };
        const admin = this.isAdmin();
        // Admin-only endpoints are only fetched for admins — non-admins would
        // just get 403s. Groups (editor-writable) and the current user load for
        // everyone.
        const [servers, groups, notifications, registries, mcpTokens, users, sso] = await Promise.all([
          admin ? api.get('/api/config/servers').catch(() => ({})) : Promise.resolve({}),
          api.get('/api/groups').catch(() => []),
          admin ? api.get('/api/notifications').catch(() => ({})) : Promise.resolve({}),
          admin ? api.get('/api/registries').catch(() => []) : Promise.resolve([]),
          admin ? api.get('/api/mcp-tokens').catch(() => []) : Promise.resolve([]),
          admin ? api.get('/api/users').catch(() => []) : Promise.resolve([]),
          admin ? api.get('/api/sso').catch(() => []) : Promise.resolve([]),
        ]);
        this.servers = servers || {};
        this.groups = groups || [];
        this.notifications = notifications || {};
        this.registries = registries || [];
        this.mcpTokens = mcpTokens || [];
        this.users = users || [];
        this.ssoPrincipals = sso || [];
        // A non-admin can't see the admin tabs; if the default landed on one,
        // move to a tab they can use.
        if (!admin && ['servers', 'notifications', 'registry', 'mcp', 'users', 'sso'].includes(this.tab)) {
          this.tab = 'groups';
        }
      } catch (e) {
        showToast(e.message, 'error');
      }
      this.loading = false;
    },

    async createUser() {
      if (!this.newUser.username || !this.newUser.password) return;
      try {
        await api.post('/api/users', {
          username: this.newUser.username,
          password: this.newUser.password,
          role: this.newUser.role,
        });
        showToast('User added', 'success');
        this.newUser = { username: '', password: '', role: 'editor' };
        this.users = await api.get('/api/users').catch(() => this.users);
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    async changeRole(username, role) {
      try {
        await api.put(`/api/users/${encodeURIComponent(username)}`, { role });
        showToast(`${username} is now ${role}`, 'success');
        this.users = await api.get('/api/users').catch(() => this.users);
      } catch (e) {
        showToast(e.message, 'error');
        this.users = await api.get('/api/users').catch(() => this.users);
      }
    },

    async resetUserPassword(username) {
      const pw = prompt(`New password for ${username} (at least 8 characters):`);
      if (!pw) return;
      try {
        await api.post(`/api/users/${encodeURIComponent(username)}/password`, { password: pw });
        showToast('Password reset', 'success');
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    async deleteUser(username) {
      if (!confirm(`Remove user ${username}? Any active sessions are ended immediately.`)) return;
      try {
        await api.del(`/api/users/${encodeURIComponent(username)}`);
        showToast('User removed', 'success');
        this.users = await api.get('/api/users').catch(() => []);
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    // A02 surface: durable session revocation — the epoch bump retires every
    // live session on its next request; the user simply signs in again.
    async revokeUserSessions(username) {
      if (!confirm(`Sign out ${username} everywhere? Active sessions end on their next request.`)) return;
      try {
        await api.post(`/api/users/${encodeURIComponent(username)}/revoke-sessions`);
        showToast(`Sessions for ${username} revoked`, 'success');
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    // X03: the explicit narrowing click — replaces a legacy account's
    // pre-matrix permissions with its role's preset. The confirm is the
    // explicit act; the server applies it to live sessions immediately.
    async narrowToPreset(username) {
      if (!confirm(`Narrow ${username} to its role's preset?\n\nPre-matrix permissions (including any env/KV value reads the old role allowed) are replaced by the preset for its current role. Active sessions pick up the change on their next request.`)) return;
      try {
        await api.post(`/api/users/${encodeURIComponent(username)}/narrow-to-preset`);
        showToast(`${username} narrowed to preset`, 'success');
        this.users = await api.get('/api/users').catch(() => this.users);
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    async revokeSSO(p) {
      if (!confirm(`Sign out ${p.username || p.subject} everywhere? Active sessions end on their next request.`)) return;
      try {
        await api.post('/api/sso/revoke', { subject: p.subject });
        showToast('SSO sessions revoked', 'success');
        this.ssoPrincipals = await api.get('/api/sso').catch(() => this.ssoPrincipals);
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    // The subject is opaque (issuer-derived); show the short tail for
    // traceability without pretending it's a name.
    ssoSubjectTail(subject) {
      return subject.length > 24 ? '…' + subject.slice(-12) : subject;
    },

    fmtDate(d) {
      if (!d || String(d).startsWith('0001')) return 'never';
      return new Date(d).toLocaleString();
    },

    async createMcpToken() {
      if (!this.newMcpToken.name) return;
      try {
        const res = await api.post('/api/mcp-tokens', {
          name: this.newMcpToken.name,
          read_only: this.newMcpToken.readOnly === 'true',
        });
        this.createdMcpToken = res.token;
        this.newMcpToken = { name: '', readOnly: 'false' };
        this.mcpTokens = await api.get('/api/mcp-tokens').catch(() => this.mcpTokens);
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    async deleteMcpToken(id) {
      if (!confirm('Revoke this token? Clients using it will lose access immediately.')) return;
      try {
        await api.del(`/api/mcp-tokens/${encodeURIComponent(id)}`);
        this.createdMcpToken = '';
        this.mcpTokens = await api.get('/api/mcp-tokens').catch(() => []);
        showToast('Token revoked', 'success');
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    serverList() {
      return Object.entries(this.servers || {}).map(([name, s]) => ({ name, ...s }));
    },

    async addServer() {
      if (!this.newServer.name || !this.newServer.host) return;
      try {
        await api.post('/api/config/servers', this.newServer);
        showToast('Server added', 'success');
        this.newServer = { name: '', host: '', user: 'root', role: 'app' };
        await this.loadAll();
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    async deleteServer(name) {
      if (!confirm(`Remove server ${name}?`)) return;
      try {
        await api.del(`/api/config/servers/${name}`);
        showToast('Server removed', 'success');
        await this.loadAll();
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    async createGroup() {
      if (!this.newGroupName) return;
      try {
        await api.post('/api/groups', { name: this.newGroupName });
        showToast('Group created', 'success');
        this.newGroupName = '';
        await this.loadAll();
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    async deleteGroup(name) {
      if (!confirm(`Delete group ${name}?`)) return;
      try {
        await api.del(`/api/groups/${encodeURIComponent(name)}`);
        showToast('Group deleted', 'success');
        await this.loadAll();
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    async renameGroup(oldName) {
      const newName = prompt('New group name:', oldName);
      if (!newName || newName === oldName) return;
      try {
        await api.put(`/api/groups/${encodeURIComponent(oldName)}`, { name: newName });
        showToast('Group renamed', 'success');
        await this.loadAll();
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    editServer(name) {
      const srv = this.servers[name] || {};
      this.editingServer = {
        originalName: name,
        name,
        host: srv.host || '',
        user: srv.user || 'root',
        role: srv.role || 'app',
      };
    },

    cancelEdit() {
      this.editingServer = null;
    },

    async saveEditServer() {
      const e = this.editingServer;
      if (!e || !e.name || !e.host) return;
      const original = this.servers[e.originalName] || {};
      try {
        // R52: the API requires rename and field updates as separate
        // requests, so a change to BOTH sequences them here — rename first
        // (carrying the unchanged fields), then the field update under the
        // new name. A failure mid-sequence surfaces with the surviving
        // identity already named in the toast, and loadAll() shows truth.
        if (e.name !== e.originalName) {
          await api.put(`/api/config/servers/${encodeURIComponent(e.originalName)}`, {
            name: e.name,
            host: original.host || e.host,
            user: original.user || 'root',
            role: original.role || 'app',
          });
          if (e.host !== (original.host || '') || e.user !== (original.user || 'root') || e.role !== (original.role || 'app')) {
            await api.put(`/api/config/servers/${encodeURIComponent(e.name)}`, {
              host: e.host, user: e.user, role: e.role,
            });
          }
        } else {
          await api.put(`/api/config/servers/${encodeURIComponent(e.originalName)}`, {
            name: e.name, host: e.host, user: e.user, role: e.role,
          });
        }
        showToast('Server updated', 'success');
        this.editingServer = null;
        await this.loadAll();
      } catch (err) {
        showToast(err.message, 'error');
        await this.loadAll();
      }
    },

    async saveNotifications() {
      // Patch semantics (A34): send only editable fields. Secrets are sent
      // only when the operator typed a new one (the GET never reveals them);
      // absent means preserve, present-but-empty means clear deliberately.
      const n = this.notifications || {};
      const body = {
        webhook_url: n.webhook_url || '',
        smtp_host: n.smtp_host || '',
        smtp_port: Number(n.smtp_port) || 0,
        smtp_user: n.smtp_user || '',
        email_to: n.email_to || '',
        email_from: n.email_from || '',
      };
      const applySecret = (field, mode, draft) => {
        if (mode === 'clear') { body[field] = ''; return; }
        if (mode === 'replace') {
          if (draft === '') throw new Error('Enter a replacement secret or switch back to Keep');
          body[field] = draft;
        }
      };
      try {
        applySecret('smtp_pass', this.smtpPasswordMode, this.smtpPasswordDraft);
        applySecret('webhook_secret', this.webhookSecretMode, this.webhookSecretDraft);
        await api.post('/api/notifications', body);
        this.smtpPasswordDraft = ''; this.smtpPasswordMode = 'keep';
        this.webhookSecretDraft = ''; this.webhookSecretMode = 'keep';
        showToast('Notifications saved', 'success');
        const n = await api.get('/api/notifications').catch(() => null);
        if (n) this.notifications = n;
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    async addRegistry() {
      if (!this.newReg.server || !this.newReg.username) return;
      try {
        await api.post('/api/registries', this.newReg);
        showToast('Registry added', 'success');
        this.newReg = { server: '', username: '', password: '' };
        await this.loadAll();
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    async deleteRegistry(server) {
      if (!confirm(`Remove registry ${server}?`)) return;
      try {
        await api.del(`/api/registries/${encodeURIComponent(server)}`);
        showToast('Registry removed', 'success');
        await this.loadAll();
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    async changePassword() {
      this.pw.error = '';
      if (!this.pw.current) { this.pw.error = 'Current password is required.'; return; }
      if (this.pw.next.length < 8) { this.pw.error = 'New password must be at least 8 characters.'; return; }
      if (this.pw.next !== this.pw.confirm) { this.pw.error = 'New passwords do not match.'; return; }
      this.pw.saving = true;
      try {
        const resp = await fetch('/api/auth/password', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({
            current_password: this.pw.current,
            new_password: this.pw.next,
            confirm_password: this.pw.confirm,
          }),
        });
        const data = await resp.json().catch(() => ({}));
        if (!resp.ok) {
          this.pw.error = data.error || 'Password change failed.';
          this.pw.saving = false;
          return;
        }
        showToast('Password changed. Signing out...', 'success');
        setTimeout(() => { location.href = '/login'; }, 1200);
      } catch {
        this.pw.error = 'Network error — try again.';
        this.pw.saving = false;
      }
    },
  }));

  // ── Templates Page ──
  Alpine.data('templatesPage', () => withAsyncLoad({
    templates: [],
    serverList: [],
    selected: null,
    installing: false,
    loading: true,
    installForm: { domain: '', server: '', vars: {} },

    async init() {
      await this.load();
    },

    async load() {
      this.loading = true;
      try {
        const [tpls, servers] = await Promise.all([
          api.get('/api/templates'),
          // /api/servers is viewer-readable; /api/config/servers is admin-only
          // and left a 403-catch producing an empty dropdown for editors.
          api.get('/api/servers').catch(() => ({})),
        ]);
        this.templates = tpls || [];
        this.serverList = Object.keys(servers || {});
        this.loadError = null;
        this.loadedAt = new Date().toISOString();
      } catch (e) {
        // D07: a failed catalog read is an error state with a retry — it
        // must not paint as the "No templates available" empty state.
        this.loadError = `Could not load templates: ${e.message}`;
      }
      this.loading = false;
    },

    selectTemplate(t) {
      this.selected = t;
      this.installForm = { domain: '', server: '', vars: {} };
    },

    async install() {
      if (!this.installForm.domain || !this.installForm.server) {
        showToast('Domain and server are required', 'error');
        return;
      }
      this.installing = true;
      try {
        const op = await api.post('/api/templates/install', {
          template: this.selected.name,
          domain: this.installForm.domain,
          server: this.installForm.server,
          vars: this.installForm.vars,
          // D05 version pin: empty (omitted server-side) on the current
          // unversioned catalog; the selected version otherwise. The
          // server re-verifies it against the catalog at submit — a moved
          // catalog answers 409 with the upgrade pointer, shown below.
          template_version: this.selected.version_state === 'versioned' ? this.selected.version : '',
        });
        if (!isOperation(op)) throw new Error('Expected an operation response');
        // A queued operation is not a completed installation — drop the
        // (possibly secret) form vars and follow it live.
        this.installForm = { domain: '', server: '', vars: {} };
        this.selected = null;
        Alpine.store('router').navigate('operation-detail', { id: op.id });
      } catch (e) {
        showToast(e.message, 'error');
      } finally {
        this.installing = false;
      }
    },
  }));

  // ── Monitors Page ──
  Alpine.data('monitorsPage', () => withAsyncLoad({
    monitors: [],
    selectedMonitor: null,
    showCreateDialog: false,
    creating: false,
    loading: true,
    cliStatus: null,
    refreshInterval: null,
    testing: false,
    testResult: null,
    editingId: null,
    _loadGeneration: 0,
    newMonitor: {
      name: '',
      type: 'http',
      method: 'GET',
      target: '',
      interval: '60000',
      timeout: '10000',
      expected_status: '', // blank = any 2xx/3xx is healthy; a value = exact match
      allow_internal: false, // opt-in: allow this monitor to reach loopback/private/link-local/cloud-metadata targets
    },

    async init() {
      this._alive = true;
      await this.loadMonitors();
      await this.loadCLIStatus();
      if (!this._alive) return; // destroyed during load (A50)
      this.refreshInterval = setInterval(() => this.loadMonitors(), 30000);
    },

    destroy() {
      this._alive = false;
      this._loadGeneration++; // a response still in flight is stale on destroy
      if (this.refreshInterval) clearInterval(this.refreshInterval);
    },

    async loadMonitors() {
      const generation = ++this._loadGeneration;
      try {
        const data = await rawFetch.get('/api/monitors');
        if (!this._alive || generation !== this._loadGeneration) return;
        this.monitors = data || [];
        this.loadError = null;
        this.loadedAt = new Date().toISOString();
      } catch (e) {
        // D07: the 30s poller keeps the last list on a failed refresh and
        // reports it as stale; a failed FIRST load is an error state, not
        // the "No monitors yet" empty state.
        if (this._alive && generation === this._loadGeneration) {
          this.loadError = `Could not load monitors: ${e.message}`;
        }
      }
      if (this._alive && generation === this._loadGeneration) this.loading = false;
    },

    async loadCLIStatus() {
      try {
        this.cliStatus = await rawFetch.get('/api/cli/status');
      } catch {}
    },

    async selectMonitor(id) {
      try {
        this.selectedMonitor = await rawFetch.get('/api/monitors/' + id);
      } catch (e) {
        showToast('Failed to load monitor', 'error');
      }
    },

    async createMonitor() {
      this.creating = true;
      try {
        const id = this.editingId || randomID();
        const body = {
          id,
          name: this.newMonitor.name,
          type: this.newMonitor.type,
          method: this.newMonitor.type === 'http' ? this.newMonitor.method : '',
          target: this.newMonitor.target,
          interval: parseInt(this.newMonitor.interval) * 1000000,
          timeout: parseInt(this.newMonitor.timeout) * 1000000,
          // Preserve the current enabled state on edit — saving an edit used
          // to silently re-enable a disabled monitor (A44).
          enabled: !!this.newMonitor.enabled,
          expected_status: parseInt(this.newMonitor.expected_status) || 0,
          allow_internal: !!this.newMonitor.allow_internal,
        };
        await rawFetch.post('/api/monitors', body);
        const wasEdit = !!this.editingId;
        this.closeDialog();
        await this.loadMonitors();
        if (wasEdit && this.selectedMonitor?.monitor?.id === id) {
          await this.selectMonitor(id);
        }
        showToast(wasEdit ? 'Monitor updated' : 'Monitor created', 'success');
      } catch (e) {
        showToast(e.message || 'Failed to save monitor', 'error');
      }
      this.creating = false;
    },

    async deleteMonitor(id) {
      if (!confirm('Delete this monitor?')) return;
      try {
        await rawFetch.del('/api/monitors/' + id);
        this.selectedMonitor = null;
        await this.loadMonitors();
        showToast('Monitor deleted', 'success');
      } catch (e) {
        showToast('Failed to delete monitor', 'error');
      }
    },

    async toggleEnabled(m) {
      try {
        const updated = { ...m, enabled: !m.enabled };
        await rawFetch.post('/api/monitors', updated);
        // Refresh detail and list
        if (this.selectedMonitor?.monitor?.id === m.id) {
          await this.selectMonitor(m.id);
        }
        await this.loadMonitors();
        showToast(updated.enabled ? 'Monitor enabled' : 'Monitor disabled', 'success');
      } catch (e) {
        showToast('Failed to toggle monitor', 'error');
      }
    },

    async testMonitor(id) {
      this.testing = true;
      this.testResult = null;
      try {
        this.testResult = await rawFetch.post('/api/monitors/' + id + '/test', {});
      } catch (e) {
        showToast('Test failed to run', 'error');
      }
      this.testing = false;
    },

    // Separate create/close transitions so a canceled edit can't leak its ID
    // into a later "Add" (A44).
    openCreate() {
      this.editingId = null;
      this.resetForm();
      this.newMonitor.enabled = true;
      this.showCreateDialog = true;
    },

    closeDialog() {
      this.showCreateDialog = false;
      this.editingId = null;
      this.resetForm();
    },

    startEdit(m) {
      // Pre-fill the create dialog with existing values; creation is an
      // upsert. Keep the monitor's enabled state so saving the edit doesn't
      // re-enable it (A44).
      this.editingId = m.id;
      this.newMonitor = {
        name: m.name,
        type: m.type,
        method: m.method || 'GET',
        target: m.target,
        interval: String(m.interval / 1000000),
        timeout: String(m.timeout / 1000000),
        expected_status: m.expected_status || '',
        allow_internal: !!m.allow_internal,
        enabled: !!m.enabled,
      };
      this.showCreateDialog = true;
    },

    resetForm() {
      this.newMonitor = {
        name: '', type: 'http', method: 'GET', target: '',
        interval: '60000', timeout: '10000', expected_status: '', allow_internal: false,
        enabled: true,
      };
    },

    getMonitorDot(m) {
      if (!m.stats || m.stats.total_checks === 0) return 'gray';
      return m.stats.uptime_percent >= 99 ? 'green' : m.stats.uptime_percent >= 95 ? 'yellow' : 'red';
    },

    // ── Alert delivery status (D08) ──
    // The outbox exposes the last delivery per monitor: state, attempts,
    // the exact last failure, and the next retry. A failed notification is
    // operationally silent without this — the monitor page is where it must
    // be visible.
    deliveryLabel(d) {
      if (!d) return '';
      if (d.status === 'dead_lettered') return 'delivery dead-lettered';
      if (d.status === 'pending') return d.attempts > 0 ? `delivery retrying (attempt ${d.attempts})` : 'delivery pending';
      return '';
    },

    // Only failure-ish states earn the warning chip; plain "delivered" is
    // history, not an alert.
    deliveryWarns(d) {
      return !!d && (d.status === 'dead_lettered' || (d.status === 'pending' && d.attempts > 0));
    },

    formatInterval(ns) {
      const ms = ns / 1000000;
      if (ms >= 60000) return (ms / 60000) + 'm';
      return (ms / 1000) + 's';
    },

    // Response times come from the API as Go time.Duration (nanoseconds); the
    // UI shows them in ms, so divide by 1e6 (they were previously shown raw,
    // i.e. ~1e6x too large).
    fmtMs(ns) {
      if (ns === null || ns === undefined || ns === '') return '--';
      return Math.round(ns / 1000000) + 'ms';
    },

    formatDate(d) {
      return new Date(d).toLocaleString();
    },
  }));

  // ── Homepage ──
  Alpine.data('homepagePage', () => withAsyncLoad({
    items: [],
    loading: true,
    editing: false,
    showForm: false,
    editingItem: null,
    form: { name: '', url: '', description: '', color: '#3b82f6' },
    colors: ['#3b82f6','#10b981','#f59e0b','#ef4444','#8b5cf6','#ec4899','#06d4c4','#f97316'],
    _dragId: null,
    _dragOverId: null,
    _didDrag: false,
    _etag: '', // R13: concurrency token for the shared shortcut list

    // Header-only links (Settings > Links, Home off) stay out of the grid but
    // remain in `items` so persisting order doesn't drop them.
    get visibleItems() { return this.items.filter(i => !i.hidden); },

    async init() {
      await this.load();
    },

    async load() {
      try {
        const meta = {};
        const raw = (await api.get('/api/homepage', {_meta: meta})) || [];
        this._etag = meta.etag || '';
        this.items = raw.map(i => ({ ...i, _faviconFailed: false }));
        this.loadError = null;
        this.loadedAt = new Date().toISOString();
      } catch(e) {
        // D07: a failed load keeps any earlier shortcuts on screen (stale)
        // and never renders as "No shortcuts yet".
        this.loadError = `Could not load shortcuts: ${e.message}`;
      }
      this.loading = false;
    },

    faviconUrl(url) { return faviconCandidates(url)[0] || ''; },

    // Advance to the next candidate path before giving up on an icon. See
    // faviconCandidates.
    faviconError(item, el) {
      const list = faviconCandidates(item.url);
      const next = list[(list.indexOf(el.getAttribute('src')) + 1)] || '';
      if (next) { el.setAttribute('src', next); return; }
      item._faviconFailed = true;
    },

    iconLetters(name) {
      return name.trim().split(/\s+/).slice(0, 2).map(w => w[0].toUpperCase()).join('');
    },

    openAdd() {
      this.editingItem = null;
      this.form = { name: '', url: '', description: '', color: '#3b82f6' };
      this.showForm = true;
    },

    openEdit(item) {
      this.editingItem = item.id;
      this.form = { name: item.name, url: item.url, description: item.description || '', color: item.color || '#3b82f6' };
      this.showForm = true;
    },

    cancelForm() {
      this.showForm = false;
      this.editingItem = null;
    },

    async saveForm() {
      const name = this.form.name.trim();
      const url  = this.form.url.trim();
      if (!name || !url) { showToast('Name and URL are required', 'error'); return; }
      // Build the candidate FIRST and persist it before publishing locally —
      // the old path replaced the record with only the form's fields (dropping
      // pinned/hidden/icon/dark_icon metadata) and closed the form even when
      // the save had failed (A45).
      const candidate = this.items.map(item => ({ ...item }));
      if (this.editingItem) {
        const idx = candidate.findIndex(item => item.id === this.editingItem);
        if (idx < 0) { showToast('Shortcut no longer exists — reload', 'error'); return; }
        candidate[idx] = { ...candidate[idx], name, url, description: this.form.description.trim(), color: this.form.color, _faviconFailed: false };
      } else {
        candidate.push({ id: Math.random().toString(36).slice(2), name, url, description: this.form.description.trim(), color: this.form.color, _faviconFailed: false });
      }
      try {
        await this.persistList(candidate);
        this.items = candidate;
        this.showForm = false;
        this.editingItem = null;
      } catch (e) {
        showToast(e.message, 'error'); // form stays open, list unchanged
      }
    },

    async remove(id) {
      const before = this.items;
      this.items = this.items.filter(i => i.id !== id);
      try {
        await this.persistList(this.items);
      } catch (e) {
        this.items = before; // deletion did not commit — undo the optimistic view
      }
    },

    // persistList is the single writer; persist() kept for the drag path.
    // R13: the save carries the ETag the list was loaded under; a 412 means
    // another editor changed it first — reload instead of overwriting.
    async persistList(list) {
      const clean = list.map(({ _faviconFailed, ...i }) => i);
      const meta = {};
      try {
        await api.put('/api/homepage', clean, {headers: {'If-Match': this._etag}, _meta: meta});
      } catch (e) {
        if (e.status === 412 || e.status === 428) {
          showToast('Shortcuts were changed by someone else — reloading', 'error');
          await this.init();
        }
        throw e;
      }
      this._etag = meta.etag || this._etag;
      window.dispatchEvent(new CustomEvent('teploy:links-changed'));
    },

    async persist() {
      try {
        await this.persistList(this.items);
      } catch(e) {
        showToast(e.message, 'error');
      }
    },

    dragStart(id, event) {
      this._dragId = id;
      this._didDrag = false;
      event.dataTransfer.effectAllowed = 'move';
    },

    dragOver(id, event) {
      event.preventDefault();
      event.dataTransfer.dropEffect = 'move';
      if (id === this._dragId) return;
      if (id === this._dragOverId) return;
      this._dragOverId = id;
      this._didDrag = true;
      const from = this.items.findIndex(i => i.id === this._dragId);
      const to   = this.items.findIndex(i => i.id === id);
      if (from === -1 || to === -1) return;
      const moved = this.items.splice(from, 1)[0];
      this.items.splice(to, 0, moved);
    },

    dragEnd() {
      const moved = this._didDrag;
      this._dragId = null;
      this._dragOverId = null;
      this._didDrag = false;
      if (moved) this.persist();
    },
  }));

  // ── Settings > Links ──
  // Table view over the same /api/homepage list the Home grid edits, plus the
  // pin flag that puts a link in the header. Home keeps drag-ordering; this is
  // the surface for adding a service and deciding whether it earns header space.
  Alpine.data('linksSettings', () => ({
    items: [],
    editingId: null,
    role: null, // null = auth disabled (no /api/auth/me route)
    _etag: '', // R13: concurrency token for the shared shortcut list
    // `home` is the inverse of the stored `hidden` flag: the form asks what to
    // show, the record stores the exception.
    form: { name: '', url: '', description: '', color: '#3b82f6', icon: '', home: true, pinned: false, dark_icon: false },

    async init() {
      // The API makes links editor-writable; with --no-auth it enforces nothing
      // and the Home grid edits freely, so match that rather than showing a
      // read-only table on an install that has no roles at all.
      this.role = await api.get('/api/auth/me').then(authRole).catch(() => null);
      try {
        const meta = {};
        const raw = (await api.get('/api/homepage', {_meta: meta})) || [];
        this._etag = meta.etag || '';
        this.items = raw.map(i => ({ ...i, _faviconFailed: false }));
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    canEdit() { return this.role === null || this.role === 'admin' || this.role === 'editor'; },

    faviconUrl(url) { return faviconCandidates(url)[0] || ''; },

    // Advance to the next candidate path before giving up on an icon. See
    // faviconCandidates.
    faviconError(item, el) {
      const list = faviconCandidates(item.url);
      const next = list[(list.indexOf(el.getAttribute('src')) + 1)] || '';
      if (next) { el.setAttribute('src', next); return; }
      item._faviconFailed = true;
    },

    iconLetters(name) {
      return name.trim().split(/\s+/).slice(0, 2).map(w => w[0].toUpperCase()).join('');
    },

    edit(item) {
      this.editingId = item.id;
      this.form = {
        name: item.name, url: item.url, description: item.description || '', color: item.color || '#3b82f6',
        icon: item.icon || '', home: !item.hidden, pinned: !!item.pinned, dark_icon: !!item.dark_icon,
      };
    },

    cancel() {
      this.editingId = null;
      this.form = { name: '', url: '', description: '', color: '#3b82f6', icon: '', home: true, pinned: false, dark_icon: false };
    },

    // Candidate-first commit (A52): a failed save keeps BOTH the edit draft
    // and the committed list intact — the old path mutated the list up front,
    // swallowed the persist error, and cleared the form as if it had saved.
    buildCandidate(fields) {
      const candidate = this.items.map(item => ({ ...item }));
      if (this.editingId) {
        const idx = candidate.findIndex(item => item.id === this.editingId);
        if (idx < 0) throw new Error('Shortcut no longer exists; reload');
        candidate[idx] = { ...candidate[idx], ...fields };
      } else {
        candidate.push({ id: randomID(), ...fields });
      }
      return candidate;
    },

    async persistCandidate(candidate) {
      const clean = candidate.map(({ _faviconFailed, ...i }) => i);
      const meta = {};
      try {
        await api.put('/api/homepage', clean, {headers: {'If-Match': this._etag}, _meta: meta});
      } catch (e) {
        // R13: the list changed underneath this editor — reload rather than
        // overwrite, then surface the original conflict.
        if (e.status === 412 || e.status === 428) {
          const role = this.role;
          await this.init();
          this.role = role;
          showToast('Shortcuts were changed by someone else — list reloaded, retry your edit', 'error');
        }
        throw e;
      }
      this._etag = meta.etag || this._etag;
      this.items = candidate;
      window.dispatchEvent(new CustomEvent('teploy:links-changed'));
    },

    async save() {
      const name = this.form.name.trim();
      const url = this.form.url.trim();
      if (!name || !url) { showToast('Name and URL are required', 'error'); return; }
      const fields = {
        name, url,
        description: this.form.description.trim(),
        color: this.form.color,
        icon: this.form.icon.trim(),
        hidden: !this.form.home,
        pinned: this.form.pinned,
        dark_icon: this.form.dark_icon,
        _faviconFailed: false,
      };
      try {
        const candidate = this.buildCandidate(fields);
        await this.persistCandidate(candidate);
        this.cancel();
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    async toggle(item, field) {
      const before = item[field];
      item[field] = !before;
      try {
        await this.persistCandidate(this.items);
      } catch (e) {
        item[field] = before; // not committed — undo the optimistic toggle
        showToast(e.message, 'error');
      }
    },

    async remove(id) {
      const before = this.items;
      const candidate = this.items.filter(i => i.id !== id);
      try {
        await this.persistCandidate(candidate);
        if (this.editingId === id) this.cancel();
      } catch (e) {
        this.items = before;
        showToast(e.message, 'error');
      }
    },
  }));
});
