// ── Top loading bar ──
// A blue→white light sweeps left→right across the bottom of the header while
// any request is in flight, and completes at the right edge when it finishes.
// Replaces the old inline spinners; driven by every api/rawFetch call below.
const progressBar = {
  inflight: 0,
  _el() { return document.getElementById('load-bar'); },
  start() {
    this.inflight++;
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
    setTimeout(() => {
      el.style.opacity = '0';
      setTimeout(() => { el.style.width = '0%'; }, 300);
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
const api = {
  async get(url) {
    const res = await trackedFetch(url);
    const json = await res.json();
    if (json.error) throw new Error(json.error);
    return json.data;
  },
  async post(url, body) {
    const res = await trackedFetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    const json = await res.json();
    if (json.error) throw new Error(json.error);
    return json.data;
  },
  async put(url, body) {
    const res = await trackedFetch(url, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    const json = await res.json();
    if (json.error) throw new Error(json.error);
    return json.data;
  },
  async del(url) {
    const res = await trackedFetch(url, { method: 'DELETE' });
    const json = await res.json();
    if (json.error) throw new Error(json.error);
    return json.data;
  },
};

// ── Raw fetch helper for monitors (they return data directly, not wrapped) ──
const rawFetch = {
  async get(url) {
    const res = await trackedFetch(url);
    return await this.parse(res);
  },
  async post(url, body) {
    const res = await trackedFetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    return await this.parse(res);
  },
  async del(url) {
    const res = await trackedFetch(url, { method: 'DELETE' });
    return await this.parse(res);
  },
  async parse(res) {
    const text = await res.text();
    let data = null;
    if (text) {
      try { data = JSON.parse(text); } catch { data = text; }
    }
    if (!res.ok) {
      throw new Error(data?.error || (typeof data === 'string' ? data : '') || `Request failed (${res.status})`);
    }
    return data;
  },
};

// An enqueued operation, not a completed result. Mutating actions now return
// one of these, so callers must follow it rather than report success.
function isOperation(result) {
  return !!(result && typeof result === 'object' && result.id && result.status && result.request);
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
function initTheme() {
  const saved = localStorage.getItem('teploy-theme') || 'dark';
  document.documentElement.setAttribute('data-theme', saved);
  return saved;
}

function toggleTheme() {
  const current = document.documentElement.getAttribute('data-theme');
  const next = current === 'dark' ? 'light' : 'dark';
  document.documentElement.setAttribute('data-theme', next);
  localStorage.setItem('teploy-theme', next);
  return next;
}

// ── Auth ──
async function logout() {
  await fetch('/api/logout', {method: 'POST'}).catch(() => {});
  location.href = '/login';
}

// ── Alpine.js App ──
document.addEventListener('alpine:init', () => {
  // ── Router Store ──
  // URL-routed navigation: every page has a real path (history API), so
  // views are linkable, reloadable, and back/forward work. The server serves
  // index.html for unknown routes (SPA fallback), so deep links resolve.
  // The store API (page/params/navigate) is unchanged — pages don't know.
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
        if (want[i].startsWith(':')) params[want[i].slice(1)] = decodeURIComponent(have[i] || '');
        else if (want[i] !== have[i]) { ok = false; break; }
      }
      if (!ok) continue;
      for (const [k, v] of new URLSearchParams(search)) params[k] = v;
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

    async init() {
      await this.load();
      window.addEventListener('teploy:links-changed', () => this.load());
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

    faviconUrl(url) {
      try { return new URL(url).origin + '/favicon.ico'; } catch { return ''; }
    },

    iconLetters(name) {
      return name.trim().split(/\s+/).slice(0, 2).map(w => w[0].toUpperCase()).join('');
    },
  }));

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
  Alpine.data('projectsPage', () => ({
    apps: [],
    groups: [],
    serverList: [],
    search: '',
    loading: true,
    deployingToGroup: null,
    deploying: false,
    deployForm: { app: '', image: '', domain: '', server: '', port: 80 },

    async init() {
      await this.load();
    },

    async load() {
      this.loading = true;
      try {
        const [apps, groups, servers] = await Promise.all([
          api.get('/api/apps').catch(() => []),
          api.get('/api/groups').catch(() => []),
          api.get('/api/config/servers').catch(() => ({})),
        ]);
        this.apps = apps || [];
        this.groups = groups || [];
        this.serverList = Object.keys(servers || {});
      } catch (e) {
        showToast(e.message, 'error');
      }
      this.loading = false;
    },

    openDeployForm(groupName) {
      this.deployingToGroup = groupName;
      this.deployForm = { app: '', image: '', domain: '', server: '', port: 80 };
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
        // Auto-assign the app to this group
        if (groupName) {
          await api.post(`/api/groups/${encodeURIComponent(groupName)}/apps`, { app: f.app }).catch(() => {});
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
      }
      this.deploying = false;
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
  Alpine.data('projectDetailPage', () => ({
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
      try {
        const [apps, groups, servers] = await Promise.all([
          api.get('/api/apps').catch(() => []),
          api.get('/api/groups').catch(() => []),
          api.get('/api/config/servers').catch(() => ({})),
        ]);
        this.apps = apps || [];
        this.groups = groups || [];
        this.serverList = Object.keys(servers || {});
        const group = (this.groups || []).find(g => g.name === this.groupName);
        const proj = group ? (group.projects || []).find(p => p.name === this.projectName) : null;
        const projAppNames = proj ? (proj.apps || []) : [];
        this.projectApps = (this.apps || []).filter(a => projAppNames.includes(a.name));
      } catch (e) {
        showToast(e.message, 'error');
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
        // Auto-assign to the group and project
        await api.post(`/api/groups/${encodeURIComponent(this.groupName)}/apps`, { app: f.app }).catch(() => {});
        await api.post(`/api/groups/${encodeURIComponent(this.groupName)}/projects/${encodeURIComponent(this.projectName)}/apps`, { app: f.app }).catch(() => {});
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
      }
      this.deploying = false;
    },
  }));

  // ── App Detail Page ──
  Alpine.data('appDetailPage', () => ({
    tab: 'general',
    app: null,
    envVars: [],
    deployLog: [],
    accessories: [],
    loading: true,
    actionLoading: false,
    newEnvKey: '',
    newEnvValue: '',
    drift: null,
    driftLoading: false,
    stats: [],
    statsLoading: false,

    async init() {
      await Promise.all([this.loadStatus(), this.loadAccessories()]);
      // Both are extra SSH round trips; run them after the page paints and in
      // parallel with each other so neither delays the view.
      this.loadDrift();
      this.loadStats();
    },

    // Resource usage per container. Like drift, a failure here (older bundled
    // CLI without `stats --app`) hides the panel rather than breaking the page.
    async loadStats() {
      this.statsLoading = true;
      try {
        this.stats = (await api.get(`${this.appPath()}/stats`)) || [];
      } catch (e) {
        this.stats = [];
      }
      this.statsLoading = false;
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
      this.driftLoading = true;
      try {
        this.drift = await api.get(`${this.appPath()}/drift`);
      } catch (e) {
        this.drift = { unavailable: true, error: e.message };
      }
      this.driftLoading = false;
    },

    appPath() {
      const { server, name } = Alpine.store('router').params;
      return `/api/apps/${server}/${name}`;
    },

    async loadStatus() {
      this.loading = true;
      try {
        this.app = await api.get(`${this.appPath()}/status`);
      } catch (e) {
        showToast(e.message, 'error');
      }
      this.loading = false;
    },

    async loadEnv() {
      try {
        this.envVars = await api.get(`${this.appPath()}/env`);
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    async loadLog() {
      try {
        this.deployLog = await api.get(`${this.appPath()}/log`);
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    async loadAccessories() {
      try {
        this.accessories = await api.get(`${this.appPath()}/accessories`);
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
      const name = this.app.name;
      if (!confirm(`Remove ${name} from ${this.app.server}?\n\nThis stops and removes its containers, removes its route, and deletes its deploy state. Volumes and accessory data are preserved.`)) return;
      const redirect = prompt('Optional: leave a permanent redirect to this URL (blank for none):', '');
      if (redirect === null) return; // cancelled
      this.actionLoading = true;
      try {
        const op = await api.post(`${this.appPath()}/remove`, { redirect: redirect.trim() });
        if (isOperation(op)) {
          Alpine.store('router').navigate('operation-detail', { id: op.id });
        } else {
          showToast(`Removed ${name}`, 'success');
          Alpine.store('router').navigate('projects');
        }
      } catch (e) {
        showToast(e.message, 'error');
      }
      this.actionLoading = false;
    },

    async addEnvVar() {
      if (!this.newEnvKey) return;
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

    accessoryName(containerName) {
      const prefix = `${this.app?.name || ''}-`;
      return containerName.startsWith(prefix) ? containerName.slice(prefix.length) : containerName;
    },
  }));

  // ── Log Viewer Component ──
  Alpine.data('logViewer', () => ({
    ws: null,
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
      if (this.ws) this.ws.close();
      this.lines = [];
      const { server, name } = Alpine.store('router').params;
      const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
      const url = `${proto}//${location.host}/ws/logs/${server}/${name}?process=${this.process}&lines=${this.lineCount}`;
      this.ws = new WebSocket(url);
      this.ws.onopen = () => { this.connected = true; };
      this.ws.onclose = () => { this.connected = false; };
      this.ws.onmessage = (e) => {
        if (this.paused) return;
        this.lines.push(e.data);
        if (this.lines.length > 5000) this.lines = this.lines.slice(-2500);
        if (this.autoScroll) {
          this.$nextTick(() => {
            const viewer = this.$refs.logContent;
            if (viewer) viewer.scrollTop = viewer.scrollHeight;
          });
        }
      };
    },

    disconnect() {
      if (this.ws) { this.ws.close(); this.ws = null; }
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
  Alpine.data('restoreTestsPage', () => ({
    tests: [],
    servers: [],
    loading: true,
    showCreateDialog: false,
    creating: false,
    running: null,
    refreshInterval: null,
    newTest: {
      server: '',
      app: '',
      accessory: '',
      bucket: '',
      region: 'us-east-1',
      interval_hours: '24',
    },

    async init() {
      await this.loadTests();
      await this.loadServers();
      this.refreshInterval = setInterval(() => this.loadTests(), 30000);
    },

    destroy() {
      if (this.refreshInterval) clearInterval(this.refreshInterval);
    },

    async loadTests() {
      try {
        const data = await rawFetch.get('/api/restore-tests');
        this.tests = data || [];
      } catch (e) {
        console.error('Failed to load restore tests:', e);
      }
      this.loading = false;
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
          id: crypto.randomUUID().replace(/-/g, '').slice(0, 21),
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
        showToast('Failed to save restore test', 'error');
      }
      this.creating = false;
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
        await rawFetch.post('/api/restore-tests', { ...t, enabled: !t.enabled });
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
  Alpine.data('serversPage', () => ({
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
        const raw = (await api.get('/api/servers').catch(() => ({}))) || {};
        this.servers = Array.isArray(raw)
          ? raw
          : Object.entries(raw).map(([name, s]) => ({ name, ...s }));
      } catch (e) {
        this.servers = [];
      }
      this.loading = false;
    },

    openServer(name) {
      Alpine.store('router').navigate('server-detail', { name });
    },
  }));

  // ── Server Detail Page ──
  Alpine.data('serverDetailPage', () => ({
    status: null,
    proxy: null,
    tab: 'overview',
    loading: true,

    async init() {
      const name = Alpine.store('router').params.name;
      this.loading = true;
      try {
        this.status = await api.get(`/api/servers/${name}/status`);
      } catch (e) {
        showToast(e.message, 'error');
      }
      this.loading = false;
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
    // MCP tokens
    mcpTokens: [],
    newMcpToken: { name: '', readOnly: 'false' },
    createdMcpToken: '',
    // Current user + team management
    me: { username: '', role: 'viewer' },
    users: [],
    newUser: { username: '', password: '', role: 'editor' },

    isAdmin() { return (this.me && this.me.role) === 'admin'; },

    async init() {
      await this.loadAll();
    },

    async loadAll() {
      this.loading = true;
      try {
        this.me = await api.get('/api/auth/me').catch(() => ({ username: '', role: 'viewer' }));
        const admin = this.isAdmin();
        // Admin-only endpoints are only fetched for admins — non-admins would
        // just get 403s. Groups (editor-writable) and the current user load for
        // everyone.
        const [servers, groups, notifications, registries, mcpTokens, users] = await Promise.all([
          admin ? api.get('/api/config/servers').catch(() => ({})) : Promise.resolve({}),
          api.get('/api/groups').catch(() => []),
          admin ? api.get('/api/notifications').catch(() => ({})) : Promise.resolve({}),
          admin ? api.get('/api/registries').catch(() => []) : Promise.resolve([]),
          admin ? api.get('/api/mcp-tokens').catch(() => []) : Promise.resolve([]),
          admin ? api.get('/api/users').catch(() => []) : Promise.resolve([]),
        ]);
        this.servers = servers || {};
        this.groups = groups || [];
        this.notifications = notifications || {};
        this.registries = registries || [];
        this.mcpTokens = mcpTokens || [];
        this.users = users || [];
        // A non-admin can't see the admin tabs; if the default landed on one,
        // move to a tab they can use.
        if (!admin && ['servers', 'notifications', 'registry', 'mcp', 'users'].includes(this.tab)) {
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
      try {
        await api.put(`/api/config/servers/${encodeURIComponent(e.originalName)}`, {
          name: e.name, host: e.host, user: e.user, role: e.role,
        });
        showToast('Server updated', 'success');
        this.editingServer = null;
        await this.loadAll();
      } catch (err) {
        showToast(err.message, 'error');
      }
    },

    async saveNotifications() {
      try {
        await api.post('/api/notifications', this.notifications);
        showToast('Notifications saved', 'success');
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
  Alpine.data('templatesPage', () => ({
    templates: [],
    serverList: [],
    selected: null,
    installing: false,
    loading: true,
    installForm: { domain: '', server: '', vars: {} },

    async init() {
      try {
        const [tpls, servers] = await Promise.all([
          api.get('/api/templates').catch(() => []),
          api.get('/api/config/servers').catch(() => ({})),
        ]);
        this.templates = tpls || [];
        this.serverList = Object.keys(servers || {});
      } catch (e) {
        showToast(e.message, 'error');
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
        await api.post('/api/templates/install', {
          template: this.selected.name,
          domain: this.installForm.domain,
          server: this.installForm.server,
          vars: this.installForm.vars,
        });
        showToast(`Installed ${this.selected.name}`, 'success');
        this.selected = null;
      } catch (e) {
        showToast(e.message, 'error');
      }
      this.installing = false;
    },
  }));

  // ── Monitors Page ──
  Alpine.data('monitorsPage', () => ({
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
      await this.loadMonitors();
      await this.loadCLIStatus();
      this.refreshInterval = setInterval(() => this.loadMonitors(), 30000);
    },

    destroy() {
      if (this.refreshInterval) clearInterval(this.refreshInterval);
    },

    async loadMonitors() {
      try {
        const data = await rawFetch.get('/api/monitors');
        this.monitors = data || [];
      } catch (e) {
        console.error('Failed to load monitors:', e);
      }
      this.loading = false;
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
        const id = this.editingId || crypto.randomUUID().replace(/-/g, '').slice(0, 21);
        const body = {
          id,
          name: this.newMonitor.name,
          type: this.newMonitor.type,
          method: this.newMonitor.type === 'http' ? this.newMonitor.method : '',
          target: this.newMonitor.target,
          interval: parseInt(this.newMonitor.interval) * 1000000,
          timeout: parseInt(this.newMonitor.timeout) * 1000000,
          enabled: true,
          expected_status: parseInt(this.newMonitor.expected_status) || 0,
          allow_internal: !!this.newMonitor.allow_internal,
        };
        await rawFetch.post('/api/monitors', body);
        const wasEdit = !!this.editingId;
        this.showCreateDialog = false;
        this.editingId = null;
        this.resetForm();
        await this.loadMonitors();
        if (wasEdit && this.selectedMonitor?.monitor?.id === id) {
          await this.selectMonitor(id);
        }
        showToast(wasEdit ? 'Monitor updated' : 'Monitor created', 'success');
      } catch (e) {
        showToast('Failed to save monitor', 'error');
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

    startEdit(m) {
      // Pre-fill the create dialog with existing values; creation is an upsert.
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
      };
      this.showCreateDialog = true;
    },

    resetForm() {
      this.newMonitor = {
        name: '', type: 'http', method: 'GET', target: '',
        interval: '60000', timeout: '10000', expected_status: '', allow_internal: false,
      };
    },

    getMonitorDot(m) {
      if (!m.stats || m.stats.total_checks === 0) return 'gray';
      return m.stats.uptime_percent >= 99 ? 'green' : m.stats.uptime_percent >= 95 ? 'yellow' : 'red';
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
  Alpine.data('homepagePage', () => ({
    items: [],
    loading: true,
    editing: false,
    showForm: false,
    editingItem: null,
    form: { name: '', url: '', description: '', color: '#3b82f6' },
    colors: ['#3b82f6','#10b981','#f59e0b','#ef4444','#8b5cf6','#ec4899','#06b6d4','#f97316'],
    _dragId: null,
    _dragOverId: null,
    _didDrag: false,

    // Header-only links (Settings > Links, Home off) stay out of the grid but
    // remain in `items` so persisting order doesn't drop them.
    get visibleItems() { return this.items.filter(i => !i.hidden); },

    async init() {
      try {
        const raw = (await api.get('/api/homepage')) || [];
        this.items = raw.map(i => ({ ...i, _faviconFailed: false }));
      } catch(e) {
        showToast(e.message, 'error');
      }
      this.loading = false;
    },

    faviconUrl(url) {
      try { return new URL(url).origin + '/favicon.ico'; } catch { return ''; }
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
      if (this.editingItem) {
        const idx = this.items.findIndex(i => i.id === this.editingItem);
        if (idx !== -1) {
          this.items[idx] = { id: this.editingItem, name, url, description: this.form.description.trim(), color: this.form.color, _faviconFailed: false };
        }
      } else {
        this.items.push({ id: Math.random().toString(36).slice(2), name, url, description: this.form.description.trim(), color: this.form.color, _faviconFailed: false });
      }
      await this.persist();
      this.showForm = false;
      this.editingItem = null;
    },

    async remove(id) {
      this.items = this.items.filter(i => i.id !== id);
      await this.persist();
    },

    async persist() {
      try {
        const clean = this.items.map(({ _faviconFailed, ...i }) => i);
        await api.put('/api/homepage', clean);
        window.dispatchEvent(new CustomEvent('teploy:links-changed'));
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
    // `home` is the inverse of the stored `hidden` flag: the form asks what to
    // show, the record stores the exception.
    form: { name: '', url: '', description: '', color: '#3b82f6', icon: '', home: true, pinned: false, dark_icon: false },

    async init() {
      // The API makes links editor-writable; with --no-auth it enforces nothing
      // and the Home grid edits freely, so match that rather than showing a
      // read-only table on an install that has no roles at all.
      this.role = await api.get('/api/auth/me').then(u => (u && u.role) || null).catch(() => null);
      try {
        const raw = (await api.get('/api/homepage')) || [];
        this.items = raw.map(i => ({ ...i, _faviconFailed: false }));
      } catch (e) {
        showToast(e.message, 'error');
      }
    },

    canEdit() { return this.role === null || this.role === 'admin' || this.role === 'editor'; },

    faviconUrl(url) {
      try { return new URL(url).origin + '/favicon.ico'; } catch { return ''; }
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
      if (this.editingId) {
        const idx = this.items.findIndex(i => i.id === this.editingId);
        if (idx !== -1) this.items[idx] = { ...this.items[idx], ...fields };
      } else {
        this.items.push({ id: Math.random().toString(36).slice(2), ...fields });
      }
      await this.persist();
      this.cancel();
    },

    async toggle(item, field) {
      item[field] = !item[field];
      await this.persist();
    },

    async remove(id) {
      this.items = this.items.filter(i => i.id !== id);
      if (this.editingId === id) this.cancel();
      await this.persist();
    },

    async persist() {
      try {
        const clean = this.items.map(({ _faviconFailed, ...i }) => i);
        await api.put('/api/homepage', clean);
        window.dispatchEvent(new CustomEvent('teploy:links-changed'));
      } catch (e) {
        showToast(e.message, 'error');
      }
    },
  }));
});
