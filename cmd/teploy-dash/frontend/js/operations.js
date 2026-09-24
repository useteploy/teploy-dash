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
  Alpine.data('operationsPage', () => withAsyncLoad({
    ops: [],
    loading: true,
    statusFilter: '',
    _timer: null,
    _alive: false,
    _loadGeneration: 0,
    _loadingRequest: false,

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
        if (!this._loadingRequest) this.load();
      }, 3000);
    },
    destroy() {
      this._alive = false;
      this._loadGeneration++; // R61: in-flight responses are stale on destroy
      if (this._timer) clearInterval(this._timer);
    },

    // R61: a generation + captured filter guard the publish step — a slow
    // response for an old filter can no longer replace a newer filter's
    // list, and overlapping polls cannot interleave stale data.
    async load() {
      const generation = ++this._loadGeneration;
      const filter = this.statusFilter;
      this._loadingRequest = true;
      try {
        const q = filter ? `?status=${encodeURIComponent(filter)}` : '';
        const ops = (await api.get(`/api/operations${q}`)) || [];
        if (!this._alive || generation !== this._loadGeneration || filter !== this.statusFilter) return;
        this.ops = ops;
        this.loadError = null;
        this.loadedAt = new Date().toISOString();
      } catch (e) {
        // D07: the 3s poller keeps the last list on a failed refresh and
        // reports it as stale; a failed first load is an error state, never
        // the "No operations yet" empty state.
        if (this._alive && generation === this._loadGeneration && filter === this.statusFilter) {
          this.loadError = `Could not load operations: ${e.message}`;
        }
      } finally {
        if (generation === this._loadGeneration) {
          this._loadingRequest = false;
          if (this._alive) this.loading = false;
        }
      }
    },

    open(op) {
      Alpine.store('router').navigate('operation-detail', { id: op.id });
    },

    duration: opDuration,
    when: opWhen,
  }));

  // ── Operation detail (live) ──
  Alpine.data('operationDetailPage', () => withAsyncLoad({
    op: null,
    lines: [],
    loading: true,
    actionLoading: false,
    _es: null,
    _alive: false,

    async init() {
      this._alive = true;
      const id = Alpine.store('router').params.id;
      this.loading = true;
      this.loadError = null;
      try {
        const op = await api.get(`/api/operations/${id}`);
        if (!this._alive) return; // destroyed while loading (A50)
        this.op = op;
        this.loadedAt = new Date().toISOString();
      } catch (e) {
        // D07: an unreadable operation renders an error state with a retry —
        // the old path left the page blank below the back link (toast only).
        if (this._alive) this.loadError = `Could not load operation ${id}: ${e.message}`;
        this.loading = false;
        return;
      }
      this.loading = false;
      this.stream(id);
    },
    destroy() {
      this._alive = false;
      if (this._es) this._es.close();
      this._es = null; // R59: late promises check _es identity, never resurrect it
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
      // R58: ONE defensive decoder for every payload-bearing event — a
      // malformed frame renders a visible marker instead of throwing out
      // of the listener.
      const decodeEvent = event => {
        try {
          const value = JSON.parse(event.data);
          if (!value || typeof value.data !== 'string') throw new Error('invalid event');
          return value;
        } catch {
          this.lines.push({ text: '[an invalid event was received]\n', cls: 'op-line-err' });
          return null;
        }
      };
      const append = (e, cls) => {
        const ev = decodeEvent(e);
        if (!ev) return;
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
      // R58: explicit gap events are rendered as persistent warnings — the
      // backend emits them for retention truncation and lost output; hiding
      // them made truncated history look complete.
      source.addEventListener('gap', e => { if (this._es === source) append(e, 'op-line-err'); });
      source.addEventListener('status', e => {
        if (this._es !== source) return;
        const value = decodeEvent(e);
        if (!value || !this.op || !value.data) return;
        this.op = { ...this.op, status: value.data };
      });
      // The server ends every terminal history with this marker: close AFTER
      // the full replay, then take one authoritative snapshot (A53). R59: it
      // is the ONLY close trigger — an error-path refetch of a terminal
      // snapshot no longer closes the stream before missing terminal output
      // has been replayed; EventSource's own Last-Event-ID reconnect keeps
      // trying until the marker arrives.
      source.addEventListener('replay-complete', () => {
        if (this._es !== source) return;
        source.close();
        api.get(`/api/operations/${encodeURIComponent(id)}`).then(op => {
          if (this._es === source) this.op = op;
        }).catch(() => {});
      });
      source.onerror = () => {
        // R59: refresh the displayed status but do NOT close on a terminal
        // snapshot — the retained replay may not have arrived yet. The
        // replay-complete marker owns closure; EventSource retries on its
        // own with Last-Event-ID.
        if (this._es !== source) return;
        api.get(`/api/operations/${encodeURIComponent(id)}`).then(op => {
          if (this._es === source) this.op = op;
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
