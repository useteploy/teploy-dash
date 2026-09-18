# Open audit items

Unresolved findings for this repository from the ChatGPT-led audit series
(2026-09-09 through 2026-09-11 passes 1-5; register: teploy-neutron-lullmail
expanded audit — the 2026-09-17 source-code audit, pass 6, and the 2026-09-17
round-2 audit, pass 7, registered below). Fields are quoted from the audit
register; line references point at the review commits listed per item where
recorded.

Open items: 0 P2 improvements, 14 deferred findings (design/architecture),
2 upstream (teploy-cli) items, plus recorded residuals inside partially-fixed
findings (2026-09-17 pass 6). Round-2 (pass 7) deferrals are folded into the
same deferral list below. The 2026-09-18 hardening session closed the dash-04
journal/retention item and the A02/A08 identity cluster; see the resolution
log.

## useteploy__teploy-dash-04 - P2 - FIXED 2026-09-18 (commit bb519ce)

**Bound operation retention and reduce global-lock persistence work**

- Kind: Improvement
- Evidence: Every emitted event rewrites the retained event list through atomicWrite, including file and directory sync, while the manager-wide mutex is held. Operations and their event lists are loaded into memory at startup; only events per operation have a count bound.
- 2026-09-17 update: still deferred; A26's immediate safety patch (16 KiB
  event-payload bound, corrupt-history isolation) landed but the journal /
  retention / batching redesign remains open here.
- 2026-09-18: FIXED. Events persist to a per-operation append-only JSONL
  journal (one append(2) per event, no per-event fsync or rewrite; the
  operation record remains the commit point, A25). Retention is bounded and
  configurable: per-op journal byte cap with compaction to the retained
  suffix (TEPLOY_DASH_OPERATION_JOURNAL_BYTES, default 4 MiB), terminal
  history age applied at startup (TEPLOY_DASH_OPERATION_HISTORY_DAYS,
  default 30d), and a total operation-count bound removing oldest terminal
  records first (TEPLOY_DASH_MAX_OPERATIONS, default 5000; live operations
  never removed). Replay is gap-aware: sequence holes, torn/corrupt journal
  tails (truncated at the damage offset so the journal never grows behind a
  dead line), and retention truncation are surfaced as explicit gap events,
  keeping A26's 16 KiB bound and corrupt-history isolation. The in-memory
  event window stays bounded to maxEvents; older sequences are served from
  the journal on disk via EventsAfter/Subscribe. Same-target admission now
  executes in durable FIFO order (per-target runner queue, AdmissionSeq on
  the record) — the A12/A27 core; admission budgets and idempotency
  namespacing remain deferred below.

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

- A02 - FIXED 2026-09-18 (commit 478337e): the epoch-based
  issuance/revocation redesign landed. Every session carries its principal
  key + AuthEpoch; issuance captures the epoch inside the same locked
  critical section that verified credentials (local) or persisted the
  principal row (OIDC), and validation re-checks the epoch unconditionally
  on every request — issue -> revoke -> old token rejected is gate-tested
  for both identity kinds, plus an explicit concurrent login/revoke/request
  race test under -race. Admin revoke operations:
  POST /api/users/{u}/revoke-sessions and POST /api/sso/revoke.
- A08 - FIXED 2026-09-18 (commit 478337e, residual): OIDC identity is the
  issuer-namespaced subject oidc:<sha256(issuer)[:16]>:<sub>, persisted as
  a principal row in users.json (additive; pre-principal installs load an
  empty set and their in-memory sessions die at restart as before).
  Display-name claims are attribution only; role stays IdP-authoritative
  and is read live by active sessions (re-sign-in preserves the epoch, so
  one device's sign-in does not retire another's session). A missing
  principal row is a dead session — the observe-040 semantics.
- A09 (residual) - canonical callback origin: a configured redirect URL is
  supported (TEPLOY_DASH_OIDC_REDIRECT_URL); deriving from the Host header
  remains the default. Bounded discovery landed.
- A10 (residual) - secrets role policy + shortcut URL/color validation:
  restricting env/KV/log reads to editors intentionally changes the
  documented viewer contract and needs an approved role matrix + migration.
  Baseline security headers landed.
- A12 - CORE LANDED 2026-09-18 (commit bb519ce): same-target operations
  execute in durable FIFO admission order (per-target runner queue +
  persisted AdmissionSeq). Still deferred: admission budgets (queue-depth
  caps per principal) and the monitor outbox scheduler restructure; the
  generation fence (pass 4/5) covers the common interleavings.
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
- A26 (residual) - FIXED 2026-09-18 via useteploy__teploy-dash-04 (commit
  bb519ce): journal persistence, byte+age retention, and replay-gap events
  landed; persistence-degraded readiness remains with the A39/A47
  health-contract work.
- A27 - CORE LANDED 2026-09-18 (commit bb519ce): FIFO admission ordering
  (see A12). Still deferred: actor attribution fields and
  alias-fingerprint admission checks (API + schema change needing client
  coordination).
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

## 2026-09-17 round-2 source-code audit (pass 7)

Register for the independent round-2 audit pinned at reviewed commit
`1633875ea4cb85db58cbb9653eb9eb0fdeaa622d` (64 findings, A01-A64; finding
IDs below are that report's, NOT pass 6's — the two series overlap in
numbering but not in content). The report itself is register-only (not
copied into this repository).

Context at remediation time: the two items dash had reported upstream were
FIXED in teploy-cli — UPSTREAM-1 became the stdin secret contract
(`env set KEY --stdin`, `kv set KEY --stdin`, `template install
--var-stdin`) and UPSTREAM-2 became atomic `server rename` / `server
update` with typed ErrServerExists / ErrServerNotFound. Where the report
called for it, dash ADOPTED both (see A11 and A38 below); adoption on the
dash side was ours to do.

### Fixed in this pass (44)

- A01 - stored users can change their own passwords: handleChangePassword
  dispatches to the CAS reset instead of the env-migration helper.
- A02 - env bootstrap is materialized into a stored admin account once at
  startup; the implicit env-password authentication branch is deleted, so
  deleting the account removes the identity instead of re-enabling a
  retired bootstrap password (survives restart).
- A03 - per-account AuthEpoch assigned from a persisted monotonic counter
  (advanced across deletion); sessions embed the epoch captured by
  authenticate's locked read and are revalidated with live role on every
  request; self password changes compare-and-swap (409 on conflict).
- A05 - /api/auth/me registered in every mode; --no-auth answers an
  explicit {mode:"disabled", full-capability} envelope; frontend consumers
  updated.
- A07 - bounded route-safe username grammar for new accounts; unknown
  roles rejected at the create/update boundary; duplicate group/project
  rename destinations answer 409.
- A08 - recovery never auto-replays persisted queued records (post-rename
  dir-sync uncertainty made an errored enqueue's record visible); queued
  and running records surface as interrupted for explicit retry.
- A09 - Cancel persists a cancel_requested record before acknowledging;
  recovery resolves it as canceled; setRunning refuses to start one.
- A10 - redactedRequest preserves empty template variable values.
- A11 (UPSTREAM-1 adoption + ours) - env set / kv set / template vars move
  onto the CLI's stdin contract with one-time capability probes and legacy
  fallback; RunStream/RunContext/RunWithStdin/checkExit errors never embed
  the argv; line redaction covers multiline secret components.
- A14 - operation records persist the admitted host/user snapshot;
  execution re-resolves and fails ops whose alias was repointed or removed.
- A18 - manifest inspection bounded by depth (64) and node-count (10k)
  budgets (aliases were already cycle-checked).
- A20 - API minimum monitor interval matches the runner's 10s floor; HTTP
  monitor targets structurally validated at the boundary.
- A21 - history cleanup copies retained records as original bytes and
  aborts on malformed lines (the old corrupt branch re-encoded a zero-value
  record over the evidence).
- A24 - restore-test upserts take a config-only DTO rejecting result
  fields; SaveRestoreTestResult (both backends) applies a result only when
  the stored target identity matches the run's; toggle posts config-only.
- A25 (part) - per-test single-flight claim for manual + scheduled runs;
  restarted schedules derive their first fire from the persisted last-run
  time (overdue runs fire promptly, fresh ones wait the true remainder).
- A26 - transport failures are failed verifications even against a lying
  ok:true stdout (the adapter separates the CLI's documented nonzero-exit
  verdict semantics from transport errors); failure branches clear stale
  metric fields; unpersisted results send no alert.
- A27 - RunContext/RunWithStdin share one bounded, context-aware primitive
  (4 MiB/1 MiB capture budgets that kill the child and surface the overflow
  cause, process-group cleanup, WaitDelay).
- A28 - a scanner failure terminates the child process group immediately
  and is returned as the primary error.
- A30 - TOFU enrollment under a process mutex with the trust file re-read
  inside it; every parse/verify error fatal; only a genuinely unknown host
  (empty Want) enrolls; append/sync/close errors joined.
- A34 (part) - resolveServers returns an error; failed discovery no longer
  selects the local-state path or serves an empty fleet as success.
- A35 - writeRawJSON forwards exact validated bytes (no interface{}
  round-trip corrupting integers above 2^53).
- A36 (part) - unmatched /api/ routes return a JSON 404; the SPA fallback
  serves GET/HEAD only.
- A38 (UPSTREAM-2 adoption + ours) - server edit uses the CLI's atomic
  `server rename`/`server update`; typed errors map to 409/404; the
  remove+add emulation and its unreliable rollback are deleted.
- A40 - the shared homepage list is validated before replace (bounded
  count, unique route-safe IDs, absolute credential-free HTTP(S) URLs,
  #RRGGBB colors, bounded name/description/icon).
- A41 (part) - tools/call validates raw arguments against the advertised
  top-level schema (unknown fields and nulls rejected before any effect).
- A42 (part) - MCP-Protocol-Version header validated; request IDs follow
  the string/number contract with null answered -32600.
- A45 - SMTP requires STARTTLS unless smtp_allow_insecure is set; auth
  configured without server AUTH fails loudly; net.JoinHostPort.
- A46 (part) - monitor and restore runners join in-flight work (bounded,
  loud on stragglers) before shutdown closes the store.
- A47 (part) - the Nucleus-fallback file store gets the same InitErr
  startup check as the direct path.
- A48 - exclusive flock on <dataDir>/.instance.lock refuses a second
  dashboard instance over the same data directory (unix).
- A49 - the app detail page pins an immutable resource snapshot, watches
  route identity, and reads every path/action (incl. remove confirmation)
  from the snapshot.
- A50 - pollers/listeners install only if the component survived its
  initial load; late responses from superseded resources don't write.
- A52 - Links editor commits candidates; failed saves keep draft and
  committed list; toggles/removals roll back.
- A53 - the operation stream interprets status events from the event data
  and closes after an explicit replay-complete marker (added server-side);
  replaced-stream guards.
- A54 - KV reveal state uses own-property checks; accessory changes reset
  the scope; TTLs validated as nonnegative integers.
- A56 - notification secrets get keep/replace/clear controls.
- A57 - resource IDs from crypto.getRandomValues (insecure-context safe);
  restore dialog no longer references undefined editingId state.
- A59 - CI, release, and source Dockerfile build on Go 1.27.1.
- A61 - installer downloads archive + checksums from the same resolved
  tag, requires exactly one matching row, verifies before extraction,
  bounded curl timeouts/retries.
- A62 - absolute-charset prefix validation; destination dir created; the
  unit's ExecStart uses the real prefix and binds loopback by default;
  credentials written privately (umask 077 + install 0600); rotation
  guidance matches the materialized-account behavior.
- A63 - --no-auth refuses non-loopback binds without the explicit
  TEPLOY_DASH_UNSAFE_NO_AUTH=1 override.
- A64 - source Dockerfile accepts VERSION/VCS_REF/BUILD_DATE build args
  and injects them via ldflags.
- A06 (part) - setup decodes its body before taking setupMu and gets the
  same-origin check; the per-IP lockout applies only to unauthenticated
  traffic. A04 (part) - the OIDC exchange/verify phase has a dedicated
  15s deadline. A22 (part) - undecodable store entries are logged as
  skipped rather than silently dropped.

### False positives / already-correct (2)

- A15's lock/unlock MCP claim (partial): teploy_lock/teploy_unlock already
  route through the same validated builders the lifecycle tools use; the
  report's own minimum (reusing operation kinds) is satisfied for the MCP
  surface. The remaining direct-CLI env/kv/lock paths are deferred below.
- A28's WaitDelay note re RunStream: pass 6 landed WaitDelay + readers-
  before-Wait; the genuinely new part (scan-failure cancellation) is fixed
  above.

### Deferred (round-2 items, with rationale; folded into the open list)

- A04 (residual) - FIXED 2026-09-18 (commit 478337e): OIDC identity is now
  issuer+sub (see pass-6 A08 above). Canonical callback URL validation
  remains with the A09/A06 origin-config work.
- A06 (residual) - configured-public-origin comparisons and login-admission
  budgeting (bcrypt concurrency cap): needs an origin config surface and
  load measurements; the lockout/NAT and setup gaps are fixed.
- A12 - CORE LANDED 2026-09-18 (commit bb519ce): durable FIFO admission
  sequence + per-target ordered execution (same as pass-6 A12/A27). Still
  deferred: admission budgets and idempotency-key namespacing (client
  coordination).
- A13 - idempotency keys namespaced per principal and wired into UI/MCP +
  actor-attribution fields: additive API + operation-schema change needing
  client coordination.
- A15 (residual) - routing env/kv/lock mutations through the operation
  queue and unifying template-vs-app lock targets: single-mutation-boundary
  redesign; the env/kv direct paths are single SSH round trips with typed
  errors today, and the queue conversion changes the frontend contract.
- A16 - FIXED 2026-09-18 via useteploy__teploy-dash-04 (commit bb519ce):
  journal persistence, retention, and gap negotiation landed.
- A17 - manifest revision leases vs admitted operations: with no
  auto-replay (A08) the crash case is closed; the live delete-while-queued
  case fails visibly at execution. Lease coordination remains design work
  (same as pass-6 A28).
- A19 - conditional (revision-guarded) check commits in both stores:
  store-schema change; the generation fence covers the common
  interleavings (same as pass-6 A12).
- A22 (residual) - partial-error envelopes for list reads, bounded
  recent-history index for status aggregation, sub-ms precision
  normalization across backends: API-contract redesign.
- A23 - configuration-persist and runner-reconcile as one ordered
  transition: per-resource coordinator redesign.
- A25 (residual) - persisting next_due_at as durable state (vs deriving
  from LastRunAt as landed): store-schema addition.
- A29 (residual) - bounded Client.Run output and agent-signing deadlines:
  the SSH read paths that matter (fleet/logs) are bounded elsewhere;
  remaining hardening rides the A22/A31 envelope work.
- A31 - remote-read observation envelope, exact container identity,
  stderr surfacing: protocol redesign across remote/machine.
- A32/A33 - WebSocket replacement and full stream framing/revocation: same
  deferral as pass-6 A24 (SSE fallback exists and is same-origin-checked).
- A37 - groups optimistic concurrency + composite app identity: the
  groups.json schema is shared with the CLI; coordinated migration
  (same as pass-6 A35).
- A39 - store file/dir permission migration (0700/0600): needs a migration
  note and CLI coordination (same as pass-6 A32 residual).
- A43 (Advisory) - explicit secret/scope/lifetime role matrix and token
  expiry/scopes: policy decision; viewer value access remains documented
  and tested behavior.
- A44 - durable bounded alert outbox with retries: reliability subsystem
  (same as pass-6 A40).
- A47 (residual) - readiness vs liveness endpoints and degraded-persistence
  reporting: health-contract redesign (same as pass-6 A39 residual).
- A51 - frontend load/empty/partial/stale state model: UI-state redesign;
  the loaders with user-facing impact (fleet, links, notifications, detail
  identity) got targeted honesty fixes in this pass.
- A55 - monitor selection as a canonical URL route: frontend routing
  refactor (same as pass-6 A47 residual).
- A58 - accessibility pass: same as pass-6 A49.
- A60 - browser/installer/CLI-contract CI, action/image digest pinning:
  same as pass-6 A50 residual.

### Round-2 gates

`go vet ./...` clean; `go test ./...` all 12 packages ok; `go test -race
-count=1 ./...` all 12 packages ok; `make build` ok. Two environment-bound
timing issues were investigated to root cause and are NOT regressions:
TestRunStreamEmitsBothStreamsAndCancelsProcessGroup hangs whenever macOS
group-kill misses a just-forked background child (a plain-Go reproduction
with zero dash code hangs identically on this host; CI runs Linux) — the
test now preflights the host's group-kill behavior and skips with an
explicit reason where the platform can't do it, keeping the full assertion
on Linux; and one internal/server TempDir-cleanup race in
TestMCPMutationsRouteThroughOperations was fixed by waiting for terminal
operation records. No push performed.

## Resolution log (pass 7)

- 2026-09-17 (pass 7, round-2 audit at 1633875): 44 findings fixed (14
  partially, residuals recorded), 2 false-positive/partial claims
  dismissed with evidence, 18 deferred with rationale (merged above);
  UPSTREAM-1 and UPSTREAM-2 adoptions landed (A11, A38). Conventional
  commits reference the round-2 finding IDs.

## Resolution log (2026-09-18 hardening session)

- Identity cluster (commit 478337e): pass-6 A02 and the A08 residual (=
  pass-7 A04 residual) FIXED — unified principal store in users.json with
  per-principal AuthEpoch, unconditional per-request version checks for
  local and SSO sessions, issuer-namespaced OIDC identity, admin
  revocation endpoints, additive migration (empty principal set on legacy
  stores, duplicate subjects fail closed). Acceptance gates in
  internal/server/principals_test.go (issue -> revoke -> old token
  rejected for both kinds, role-change revocation, issuer scoping,
  concurrent revocation races under -race).
- Journal cluster (commit bb519ce): useteploy__teploy-dash-04 FIXED (and
  with it pass-6 A26's residual, pass-7 A16) plus the A12/A27
  (pass-6) / A12 (pass-7) ordering cores — append-only per-operation
  journal, configurable size+age+count retention, gap-event replay,
  bounded in-memory window with disk fallback, FIFO admission ordering.
  Tests in internal/operation/journal_test.go.
- Gates at the closing docs commit: `go vet ./...` clean; `go test ./...`
  all 12 packages ok; `go test -race -count=1 ./...` all 12 packages ok
  (two consecutive race runs clean); `make build` ok. README documents the
  new endpoints and retention env vars. No push performed.
