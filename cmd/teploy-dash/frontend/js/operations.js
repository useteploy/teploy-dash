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
    _alive: false,

    async init() {
      // A50: navigating away during the initial load destroys the component
      // BEFORE the interval exists; the alive flag stops init's continuation
      // from installing an orphan poller.
      this._alive = true;
      await this.load();
      if (!this._alive) return;
      // Poll while the page is visible — not only while the list already
      // holds an active operation (an empty or terminal-only list never
      // discovered work started elsewhere until a manual refresh).
      this._timer = setInterval(() => {
        if (document.visibilityState !== 'visible') return;
        this.load();
      }, 3000);
    },
    destroy() {
      this._alive = false;
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
    _alive: false,

    async init() {
      this._alive = true;
      const id = Alpine.store('router').params.id;
      try {
        const op = await api.get(`/api/operations/${id}`);
        if (!this._alive) return; // destroyed while loading (A50)
        this.op = op;
      } catch (e) {
        if (this._alive) showToast(e.message, 'error');
        this.loading = false;
        return;
      }
      this.loading = false;
      this.stream(id);
    },
    destroy() {
      this._alive = false;
      if (this._es) this._es.close();
    },

    // SSE: replays the full event history, then follows live. Status events
    // are interpreted from the EVENT ITSELF — a replayed historical status
    // is not a live transition, and the old refetch-on-every-status closed
    // the stream on a terminal snapshot before the replayed output arrived
    // (A53). The server terminates terminal histories with an explicit
    // replay-complete marker.
    stream(id) {
      const source = new EventSource(`/api/operations/${id}/events`);
      this._es = source;
      const maxLines = 5000;
      const append = (e, cls) => {
        const ev = JSON.parse(e.data);
        // Events are line-based with the newline stripped (bufio.Scanner);
        // re-add it so the pre-wrap log viewer renders one line per event.
        this.lines.push({ text: ev.data + '\n', cls });
        if (this.lines.length > maxLines) {
          this.lines = this.lines.slice(-Math.floor(maxLines / 2));
        }
        this.$nextTick(() => {
          const el = this.$refs.opLog;
          if (el) el.scrollTop = el.scrollHeight;
        });
      };
      source.addEventListener('stdout', e => { if (this._es === source) append(e, ''); });
      source.addEventListener('stderr', e => { if (this._es === source) append(e, 'op-line-err'); });
      source.addEventListener('status', e => {
        if (this._es !== source) return;
        let status = '';
        try { status = (JSON.parse(e.data) || {}).data || ''; } catch { return; }
        if (!this.op || !status) return;
        this.op = { ...this.op, status };
      });
      // The server ends every terminal history with this marker: close AFTER
      // the full replay, then take one authoritative snapshot (A53).
      source.addEventListener('replay-complete', () => {
        if (this._es !== source) return;
        source.close();
        api.get(`/api/operations/${encodeURIComponent(id)}`).then(op => {
          if (this._es === source) this.op = op;
        }).catch(() => {});
      });
      source.onerror = () => {
        // Terminal operations close the stream server-side; anything else is
        // a dropped connection — re-fetch the authoritative status instead of
        // showing an indefinitely "running" operation. EventSource retries
        // with Last-Event-ID on its own.
        if (this._es !== source) return;
        api.get(`/api/operations/${encodeURIComponent(id)}`).then(op => {
          if (this._es !== source) return;
          this.op = op;
          if (this.terminal()) source.close();
        }).catch(() => {});
      };
    },

    terminal() {
      return ['succeeded', 'failed', 'canceled', 'interrupted'].includes(this.op?.status);
    },
    cancelable() {
      // cancel_requested is transitional: the durable intent is already
      // recorded, so offer no second button (A09).
      return ['queued', 'running'].includes(this.op?.status);
    },
    retryable() {
      // A secret-bearing operation cannot be replayed from the redacted
      // request — the server would refuse it; don't offer the button.
      return !this.op?.has_secrets &&
        ['failed', 'canceled', 'interrupted'].includes(this.op?.status);
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
