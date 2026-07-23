// Operation center: list + live detail for the operations API
// (/api/operations). Kept as its own module — first step of splitting the
// frontend out of app.js. Relies on globals defined there (api, showToast).

function opDuration(op) {
  if (!op.started_at) return '—';
  const end = op.finished_at ? new Date(op.finished_at) : new Date();
  const ms = end - new Date(op.started_at);
  if (ms < 1000) return `${ms}ms`;
  if (ms < 60000) return `${(ms / 1000).toFixed(1)}s`;
  return `${Math.floor(ms / 60000)}m ${Math.round((ms % 60000) / 1000)}s`;
}

function opWhen(iso) {
  if (!iso) return '—';
  return new Date(iso).toLocaleString('en-US', {
    month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false,
  });
}

document.addEventListener('alpine:init', () => {
  // ── Operations list ──
  Alpine.data('operationsPage', () => ({
    ops: [],
    loading: true,
    statusFilter: '',
    _timer: null,

    async init() {
      await this.load();
      // Light auto-refresh while any operation is still active.
      this._timer = setInterval(() => {
        if (this.ops.some(o => o.status === 'queued' || o.status === 'running')) this.load();
      }, 3000);
    },
    destroy() {
      if (this._timer) clearInterval(this._timer);
    },

    async load() {
      try {
        const q = this.statusFilter ? `?status=${this.statusFilter}` : '';
        this.ops = (await api.get(`/api/operations${q}`)) || [];
      } catch (e) {
        showToast(e.message, 'error');
      }
      this.loading = false;
    },

    open(op) {
      Alpine.store('router').navigate('operation-detail', { id: op.id });
    },

    duration: opDuration,
    when: opWhen,
  }));

  // ── Operation detail (live) ──
  Alpine.data('operationDetailPage', () => ({
    op: null,
    lines: [],
    loading: true,
    actionLoading: false,
    _es: null,

    async init() {
      const id = Alpine.store('router').params.id;
      try {
        this.op = await api.get(`/api/operations/${id}`);
      } catch (e) {
        showToast(e.message, 'error');
        this.loading = false;
        return;
      }
      this.loading = false;
      this.stream(id);
    },
    destroy() {
      if (this._es) this._es.close();
    },

    // SSE: replays the full event history, then follows live. Status events
    // refresh the header; stdout/stderr append to the log.
    stream(id) {
      this._es = new EventSource(`/api/operations/${id}/events`);
      const append = (e, cls) => {
        const ev = JSON.parse(e.data);
        // Events are line-based with the newline stripped (bufio.Scanner);
        // re-add it so the pre-wrap log viewer renders one line per event.
        this.lines.push({ text: ev.data + '\n', cls });
        this.$nextTick(() => {
          const el = this.$refs.opLog;
          if (el) el.scrollTop = el.scrollHeight;
        });
      };
      this._es.addEventListener('stdout', e => append(e, ''));
      this._es.addEventListener('stderr', e => append(e, 'op-line-err'));
      this._es.addEventListener('status', async () => {
        try { this.op = await api.get(`/api/operations/${id}`); } catch {}
        if (this.op && this.terminal()) this._es.close();
      });
      this._es.onerror = () => {
        // Terminal operations close the stream server-side; nothing to do.
        if (this.op && this.terminal()) this._es.close();
      };
    },

    terminal() {
      return ['succeeded', 'failed', 'canceled', 'interrupted'].includes(this.op?.status);
    },
    cancelable() {
      return ['queued', 'running'].includes(this.op?.status);
    },
    retryable() {
      return ['failed', 'canceled', 'interrupted'].includes(this.op?.status);
    },

    async cancel() {
      this.actionLoading = true;
      try {
        await api.post(`/api/operations/${this.op.id}/cancel`);
        showToast('Cancel requested', 'success');
      } catch (e) {
        showToast(e.message, 'error');
      }
      this.actionLoading = false;
    },

    async retry() {
      this.actionLoading = true;
      try {
        const next = await api.post(`/api/operations/${this.op.id}/retry`);
        showToast('Retry queued', 'success');
        Alpine.store('router').navigate('operation-detail', { id: next.id });
        // Re-init against the new operation.
        if (this._es) this._es.close();
        this.op = null; this.lines = []; this.loading = true;
        await this.init();
      } catch (e) {
        showToast(e.message, 'error');
      }
      this.actionLoading = false;
    },

    duration: opDuration,
    when: opWhen,
  }));
});
