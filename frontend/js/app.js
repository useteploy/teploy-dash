// ── API Client ──
const api = {
  async get(url) {
    const res = await fetch(url);
    const json = await res.json();
    if (json.error) throw new Error(json.error);
    return json.data;
  },
  async post(url, body) {
    const res = await fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    const json = await res.json();
    if (json.error) throw new Error(json.error);
    return json.data;
  },
  async put(url, body) {
    const res = await fetch(url, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    const json = await res.json();
    if (json.error) throw new Error(json.error);
    return json.data;
  },
  async del(url) {
    const res = await fetch(url, { method: 'DELETE' });
    const json = await res.json();
    if (json.error) throw new Error(json.error);
    return json.data;
  },
};

// ── Raw fetch helper for monitors (they return data directly, not wrapped) ──
const rawFetch = {
  async get(url) {
    const res = await fetch(url);
    return await res.json();
  },
  async post(url, body) {
    const res = await fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    return await res.json();
  },
  async del(url) {
    await fetch(url, { method: 'DELETE' });
  },
};

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

// ── Alpine.js App ──
document.addEventListener('alpine:init', () => {
  // ── Router Store ──
  Alpine.store('router', {
    page: 'projects',
    params: {},
    navigate(page, params = {}) {
      this.page = page;
      this.params = params;
    },
  });

  // ── Theme Store ──
  Alpine.store('theme', {
    mode: initTheme(),
    toggle() {
      this.mode = toggleTheme();
    },
  });

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
        await api.post('/api/deploy', f);
        // Auto-assign the app to this group
        if (groupName) {
          await api.post(`/api/groups/${encodeURIComponent(groupName)}/apps`, { app: f.app }).catch(() => {});
        }
        showToast(`Deployed ${f.app} successfully`, 'success');
        this.deployingToGroup = null;
        this.deployForm = { app: '', image: '', domain: '', server: '', port: 80 };
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
        await api.post('/api/deploy', f);
        // Auto-assign to the group and project
        await api.post(`/api/groups/${encodeURIComponent(this.groupName)}/apps`, { app: f.app }).catch(() => {});
        await api.post(`/api/groups/${encodeURIComponent(this.groupName)}/projects/${encodeURIComponent(this.projectName)}/apps`, { app: f.app }).catch(() => {});
        showToast(`Deployed ${f.app} successfully`, 'success');
        this.deployingToProject = false;
        this.deployForm = { app: '', image: '', domain: '', server: '', port: 80 };
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

    async init() {
      await this.loadStatus();
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
        await api.post(`${this.appPath()}/${action}`);
        showToast(`${action} successful`, 'success');
        await this.loadStatus();
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
        this.servers = (await api.get('/api/servers').catch(() => [])) || [];
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
    // Add registry form
    newReg: { server: '', username: '', password: '' },
    // Add group form
    newGroupName: '',

    async init() {
      await this.loadAll();
    },

    async loadAll() {
      this.loading = true;
      try {
        [this.servers, this.groups, this.notifications, this.registries] = await Promise.all([
          api.get('/api/config/servers').catch(() => ({})),
          api.get('/api/groups').catch(() => []),
          api.get('/api/notifications').catch(() => ({})),
          api.get('/api/registries').catch(() => []),
        ]);
        this.servers = this.servers || {};
        this.groups = this.groups || [];
        this.registries = this.registries || [];
      } catch (e) {
        showToast(e.message, 'error');
      }
      this.loading = false;
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
    newMonitor: {
      name: '',
      type: 'http',
      target: '',
      interval: '60000',
      timeout: '10000',
      expected_status: 200,
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
        const id = crypto.randomUUID().replace(/-/g, '').slice(0, 21);
        const body = {
          id,
          name: this.newMonitor.name,
          type: this.newMonitor.type,
          target: this.newMonitor.target,
          interval: parseInt(this.newMonitor.interval) * 1000000,
          timeout: parseInt(this.newMonitor.timeout) * 1000000,
          enabled: true,
          expected_status: parseInt(this.newMonitor.expected_status) || 200,
          method: 'GET',
        };
        await rawFetch.post('/api/monitors', body);
        this.showCreateDialog = false;
        this.resetForm();
        await this.loadMonitors();
        showToast('Monitor created', 'success');
      } catch (e) {
        showToast('Failed to create monitor', 'error');
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

    resetForm() {
      this.newMonitor = {
        name: '', type: 'http', target: '',
        interval: '60000', timeout: '10000', expected_status: 200,
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

    formatDate(d) {
      return new Date(d).toLocaleString();
    },
  }));
});
