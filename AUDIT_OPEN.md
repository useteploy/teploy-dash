# Open audit items

Unresolved findings for this repository from the ChatGPT-led audit series
(2026-09-09 through 2026-09-11 passes 1-5; register: teploy-neutron-lullmail
expanded audit — and the 2026-09-17 source-code audit, pass 6, registered
below). Fields are quoted from the audit register; line references point at
the review commits listed per item where recorded.

Open items: 1 P2 improvement, 18 deferred findings (design/architecture),
2 upstream (teploy-cli) items, plus recorded residuals inside partially-fixed
findings (2026-09-17 pass).

## useteploy__teploy-dash-04 - P2 - Open improvement

**Bound operation retention and reduce global-lock persistence work**

- Kind: Improvement
- Evidence: Every emitted event rewrites the retained event list through atomicWrite, including file and directory sync, while the manager-wide mutex is held. Operations and their event lists are loaded into memory at startup; only events per operation have a count bound.
- Impact: A chatty command or large operation history can increase disk traffic and stall unrelated operations, reads, cancellations, and subscriptions. The actual throughput impact has not been benchmarked.
- Proposed fix: Use per-operation serialized persistence with bounded batching or an append journal, add retention/archival limits, and specify a maximum encoded event size consistent with the reader's 1 MiB scanner limit.
- Acceptance test: Benchmark several chatty operations concurrently with cancel/Get calls; test retention across restart and an event near and above the accepted byte-size limit.
- Review commit: `03061cf69843229189c33e51bdcc4edcc6e1c6de` (last reviewed 2026-09-10)
- 2026-09-17 update: still deferred; A26's immediate safety patch (16 KiB
  event-payload bound, corrupt-history isolation) landed but the journal /
  retention / batching redesign remains open here.

## 2026-09-17 source-code audit (pass 6)

Register for the audit pinned at reviewed commit
`46902d162643dac09d84d1ced9973820ec7eb535` (51 findings, A01-A51). Fixes
landed in commits fba6510, 26c2cde, a9df8f0, b180687, 0c29c95, c71cf97,
3707067, ec8e80d, 792a972, 1345e4e.

### Fixed (31)

- A01 - blank-username env-password override: authenticate canonicalizes the
  blank username to the env user BEFORE the stored-account lookup; a stored
  account shadowing the env name always wins (password AND role).
- A03 - setup race / unsafe password-reset creation: setup check + create run
  under setupMu with one canonical (trimmed) username; setPassword errors on
  unknown users; env-bootstrap migration is its own verified path.
- A04 - credential fallback on read failure: legacy auth.json migration only
  when users.json is genuinely absent; corrupt/unreadable store sets
  gate.initErr and the gate serves 503s (except /api/health) instead of
  falling back or opening setup.
- A05 - login/logout/setup CSRF: same-origin check applied to the
  unauthenticated mutation routes.
- A06 - post-login redirects: sanitizeNext (Go) and safeNext (login.html)
  reject backslash/control-character targets that browsers normalize to
  external origins.
- A08 - OIDC email trust: allowlists require a verified email claim
  (email_verified=true).
- A11 - monitor transport race: both policy transports built eagerly at init,
  map never mutated; CGNAT 100.64.0.0/10 + multicast added to the default
  block set.
- A13 - allow_internal escalation: changing allow_internal in either
  direction through POST /api/monitors requires an admin session (editors
  can still edit non-internal monitors).
- A14 - monitor input bounds: interval 5s-24h, timeout 1s-60s within
  interval, expected status 0/100-599, method normalized upper, tcp/ping
  require host:port; UI relabels ping as TCP reachability.
- A15 - restore-result upsert: Store.SaveRestoreTestResult persists only
  result fields; edits during a run survive, deleted tests are not
  resurrected; interval_hours bounded 1-8760.
- A16 - restore alias resolution: server alias resolved to the configured
  host before --host (SetHostResolver); ticker conversion overflow-capped.
- A17 - SSH trust store fails closed: malformed/unreadable known_hosts is an
  error, never accept-new; TOFU only while the file is genuinely absent;
  unknown keys accepted only when the trust record durably persists.
- A18 - SSH agent leak + IPv6: agent socket closed after the handshake; bare
  IPv6 literals no longer misdetected as host:port.
- A19 - MCP mutation bypass: deploy/rollback/container-lifecycle enqueue the
  same validated operations the UI uses and return the operation record;
  deprecated direct-SSH lifecycle functions deleted; every server-scoped
  tool resolves a registered server first (aliases no longer passed as
  literal --host values).
- A20 - MCP token revocation: Delete persists the candidate before
  publishing (failed save no longer diverges from disk); LastUsed telemetry
  flushed at most once a minute instead of a file rewrite per request.
- A21 (part) - MCP transport/args: cross-origin browser requests rejected
  per the Streamable HTTP spec; fractional/out-of-range integer args
  rejected; deploy/rollback descriptions no longer promise completion.
- A22 (part) - RunStream: pipe readers finish BEFORE cmd.Wait (os/exec
  contract), scanner errors fail the command, WaitDelay bounds post-cancel
  drain.
- A25 - rejected enqueue executes after restart: initial event file written
  first, the queued record is the commit point; nothing published or
  executed until the record persists.
- A26 (part) - event size: payloads sanitized to UTF-8 and bounded to 16 KiB
  at ingestion (escaping can expand ~6x past the 1 MiB reader limit); a
  corrupt history for one operation no longer disables the service.
- A29 - discarded strict-decode errors: every unchecked strictDecode call
  site (env set, remove, deploy, server add, registry login) plus the
  operations POST now reject malformed input before side effects.
- A30 (part) - CLI failure becomes success: server/template/registry list
  reads return 502 on CLI failure instead of empty success; RunJSON errors
  on non-JSON output (the registry-login exit-code claim was already
  covered by RunWithStdin's checkExit — false positive at this snapshot).
- A31 - destructive history cleanup: every scan/decode/write/sync/close
  error aborts that file's rewrite and preserves the original; corrupt
  records kept, not dropped; temp files cleaned on failure.
- A32 - config store integrity: monitors/restore-tests/notifications/
  homepage/groups written via same-directory temp+rename; MkdirAll and
  remove errors surfaced (absent = documented idempotent success);
  NewFileStore records InitErr and main refuses to start.
- A33 - Nucleus SQL: COALESCE on empty-set aggregates, avg scaled before
  Duration conversion, monitor+checks delete in one transaction.
- A34 - notification round-trip: patch DTO with pointer secrets (absent =
  preserve, present = replace/clear); GET adds webhook_secret_set; UI posts
  only editable fields with separate secret drafts.
- A36 (part) - server rename: destination collision rejected with 409
  (ServerAdd is an upsert and silently overwrote the destination).
- A38 - false-healthy status page: per-monitor freshness window
  (2*interval+timeout), stale/no-evidence reads unknown, all-unknown/empty
  never aggregates to operational; fetch failures show a visible banner.
- A41 - browser API helpers: one requestJSON parser (status checked, 204,
  non-JSON success rejected, text errors surfaced, unauthorized event).
- A42 (part) - editor deploy forms: target lists read viewer-readable
  /api/servers instead of admin-only /api/config/servers.
- A43 - template install honesty: queued operation followed to the operation
  center; try/finally busy flags; secret vars dropped after admission;
  group-assignment failures surfaced.
- A44 (part) - monitor edit state: explicit openCreate/closeDialog (Edit ->
  Cancel -> Add cannot overwrite the edited monitor); edit preserves the
  enabled flag.
- A45 - shortcut edit data loss: candidate persisted before publishing;
  original record spread (pinned/hidden/icon metadata kept); failed save
  keeps the editor open; failed delete reverts.
- A46 - KV reveal caching: listing refresh invalidates revealed values;
  reveal/list responses generation+scope guarded.
- A47 (part) - routing: malformed percent-escapes no longer throw;
  path-derived params authoritative over conflicting query params.
- A48 (part) - operation center: list polls while the page is visible
  regardless of contents; log viewer bounded; retry hidden for
  secret-bearing operations; SSE errors re-fetch authoritative status.

### False positive (1)

- A30 (registry-login exit status): cli.RunWithStdin already applied
  checkExit, so a non-zero exit never reached the success path. The rest of
  A30 was real and is fixed above.

### Upstream (teploy-cli) — recorded for the parallel CLI audit session

- UPSTREAM-1 (from A23) - secrets in argv: `env set` values, template
  `--var` values, and KV values travel in the teploy argv, visible in the
  host process list. Dash cannot fix this alone; it needs a CLI stdin (or
  private-descriptor) secret-input contract with capability detection. Dash
  redacts known values from logs/events and passes the registry password via
  stdin already, but argv exposure and multiline-chunk redaction gaps remain
  until the CLI changes. Do not send a made-up flag to the current CLI.
- UPSTREAM-2 (from A36) - atomic server rename/update: dash's rename is
  remove+add (the CLI has no edit command), which cannot preserve
  tags/vpn_ip and is non-atomic across the two processes. Needs a
  `teploy server update/rename` (or a documented shared-config transaction)
  in the CLI; dash now rejects destination collisions and restores on
  failure, but the metadata loss on rename remains CLI-owned.

### Deferred (design/architecture, with rationale)

- A02 - session issuance races account revocation (High): the epoch-based
  issuance/revocation transaction is a session-layer redesign. Mitigations
  in place: 24h session TTL, all sessions invalidated on password/role
  change and deletion; the residual window is seconds-long. Revisit with
  A08's issuer+sub identity work (below).
- A08 (residual) - OIDC identity from issuer+sub instead of display-name
  claims: session identity model change; needs the same redesign as A02.
- A09 (residual) - canonical callback origin: a configured redirect URL is
  supported (TEPLOY_DASH_OIDC_REDIRECT_URL); deriving from the Host header
  remains the default. Bounded discovery landed.
- A10 (residual) - secrets role policy + shortcut URL/color validation:
  restricting env/KV/log reads to editors intentionally changes the
  documented viewer contract and needs an approved role matrix + migration.
  Baseline security headers landed.
- A12 - monitor reload/delete stale-result atomicity: needs the per-slot
  lock + outbox scheduler restructure. The generation fence (pass 4/5)
  covers the common interleavings.
- A19 (residual) - MCP idempotency keys scoped per token: additive protocol
  change; requires client coordination.
- A21 (residual) - strict per-tool DTO decoding and full JSON-RPC envelope
  validation: the hand-rolled transport is deliberately minimal; full
  conformance testing belongs with A24's transport decision.
- A22 (residual) - unified bounded runner (I05) for regular/stdin/streaming
  CLI calls: the ordering/cancellation defects are fixed; unifying buffers
  and process-group handling across all helpers is a refactor with no
  measured need.
- A23 - upstream, see UPSTREAM-1.
- A24 - hand-written WebSocket: replacing it (maintained implementation or
  SSE-only) changes the frontend log path; the SSE fallback already exists
  and same-origin is enforced. Defer with the frontend log-viewer rework.
- A26 (residual) - journal persistence, byte+age retention, replay-gap
  events, persistence-degraded readiness: folded into open item
  useteploy__teploy-dash-04 above.
- A27 - FIFO queue, actor attribution, alias-fingerprint checks: operation
  scheduler redesign; current per-target semaphore prevents concurrent
  same-target execution but does not order enqueues.
- A28 - manifest revision leases + validation work budget: cross-service
  (manifest store + operation admission) design.
- A30 (residual) - unknown /api/* routes 404 vs SPA fallback;
  writeRawJSON re-encodes through interface{} (large-integer precision).
- A32 (residual) - 0700/0600 permission tightening for store dirs/files:
  changing modes on existing installs needs a migration note and CLI
  coordination (the CLI reads some of these paths).
- A35 - groups/projects concurrency + AppRef identity: groups.json is a
  shared CLI contract; server-scoped app identity is a schema migration
  requiring CLI coordination. Save-error surfacing landed.
- A37 - fleet observation envelope (stale/partial/errors): protocol + UI
  redesign; the fleet cache already preserves last-known state and logs
  per-server failures.
- A39 (residual) - readiness vs liveness separation, component-health
  reporting, and bounded Shutdown/Wait joins across monitor/restore/
  operation runners. run()-error propagation, listener-failure exit,
  require-Nucleus check, and DSN redaction landed.
- A40 - durable bounded alert outbox + SMTP TLS/auth policy: reliability
  subsystem; current delivery is best-effort by design.
- A42 (residual) - explicit auth-mode/capability view (/api/auth/me
  semantics under --no-auth): needs a small API addition plus UI wiring.
- A44 (residual) - server-generated monitor/test IDs: removes the
  crypto.randomUUID secure-context requirement; needs POST-create/PUT-update
  route semantics plus frontend coordination.
- A47 (residual) - router replace() for renames and uniform component
  lifetime patterns (AbortController, route-generation guards): frontend
  refactor.
- A49 - accessibility pass (Low): keyboard/focus/labels/dialog management
  across the SPA; needs real browser testing.
- A50 (residual) - browser-contract tests, govulncheck in CI, reviewed
  toolchain pin (1.24 is past end of support; latest supported series at
  audit date is 1.27.x), action/image digest pinning. Race tests, gofmt
  gate, module consistency, and release-depends-on-CI landed.
- A51 (residual) - version metadata build args in the source Dockerfile,
  image smoke test. Named-volume/persistence docs, loopback bind, TLS
  guidance, and state-format wording corrected.

## Resolution log

- 2026-09-12: teploy-dash-02, -05, -06, -07, -08 FIXED; -03 PARTIALLY FIXED
  (loud terminal-persist logging) with the pending-persistence design
  DEFERRED; -04 DEFERRED (design) — see the open item above.
- 2026-09-17 (pass 6, audit at 46902d1): 31 findings fixed (5 partially,
  residuals recorded above), 1 false positive (A30 registry-login exit), 2
  upstream items recorded for teploy-cli (A23, A36), remainder deferred
  with rationale. Gates at the closing commit: `go vet ./...` clean;
  `go test ./...` all packages ok; `go test -race -count=1 ./...` all
  packages ok (one unreproduced timing flake in internal/server observed on
  a single loaded run, clean on 5 consecutive re-runs); `make build` ok;
  CI now enforces race + gofmt + tidy gates. No push performed.
