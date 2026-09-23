# Open audit items

Unresolved findings for this repository from the ChatGPT-led audit series
(2026-09-09 through 2026-09-11 passes 1-5, the 2026-09-17/18 passes 6-8, and
the 2026-09-19 round-4 audit, pass 9, registered below). Fields are quoted
from the audit register; line references point at the review commits listed
per item where recorded.

Open items: 0 P2 improvements, deferred findings (design/architecture)
listed per pass below, 2 upstream (teploy-cli) items (both already fixed and
adopted), plus recorded residuals inside partially-fixed findings.

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

- A02 - FIXED 2026-09-18 (commit 478337e; admin UI in the residual cluster
  session): the epoch-based issuance/revocation redesign landed. Every
  session carries its principal key + AuthEpoch; issuance captures the
  epoch inside the same locked critical section that verified credentials
  (local) or persisted the principal row (OIDC), and validation re-checks
  the epoch unconditionally on every request — issue -> revoke -> old token
  rejected is gate-tested for both identity kinds, plus an explicit
  concurrent login/revoke/request race test under -race. Admin revoke
  operations: POST /api/users/{u}/revoke-sessions and POST /api/sso/revoke;
  both now have UI surfaces (Settings > Users "Revoke sessions", and
  Settings > SSO listing principals with last sign-in).
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
- A10 (residual) - FIXED 2026-09-23 (X03 first slice, see the X03 section
  below): the approved role matrix + migration landed — capability tokens
  with presets, viewer value reads removed for NEW accounts, and the
  legacy profile preserving existing accounts until the explicit
  narrowing click. Shortcut URL/color validation remains open (minor).
  Baseline security headers landed earlier.
- A12 - CORE LANDED 2026-09-18 (commit bb519ce); REMAINDER LANDED in the
  residual cluster session: admission budgets bound non-terminal operations
  per target (server+app — the FIFO-runner granularity;
  TEPLOY_DASH_MAX_QUEUED_PER_TARGET, default 50; excess enqueues rejected
  with 429, idempotent replays of queued work still pass). Still deferred:
  per-PRINCIPAL budget carving (needs the role/policy matrix) and the
  monitor outbox scheduler restructure; the generation fence (pass 4/5)
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
- A24 - FIXED in the residual cluster session (2026-09-18): log streaming
  is SSE-only at /api/logs/{server}/{app} (same-origin enforced,
  unauthenticated requests get the JSON 401); the hand-written RFC 6455
  transport (ws.go, wsLineWriter, the /ws/ prefix and its gate
  special-cases) is deleted. The SSE fallback path WAS the surviving
  transport, so the WS branch was pure dead risk.
- A26 (residual) - FIXED 2026-09-18 via useteploy__teploy-dash-04 (commit
  bb519ce): journal persistence, byte+age retention, and replay-gap events
  landed; persistence-degraded readiness landed with the A39/A47 work in
  the residual cluster session.
- A27 - CORE LANDED 2026-09-18 (commit bb519ce): FIFO admission ordering
  (see A12). REMAINDER LANDED in the residual cluster session: operations
  carry an Actor {kind: local|sso|mcp, subject, label} recording which
  principal admitted them (sessions project from the request; MCP tokens
  ride the tool context; retries attribute to the retrying principal with
  RetryOf lineage; nil on pre-existing records). Still deferred:
  alias-fingerprint admission checks (API + schema change needing client
  coordination).
- A28 - manifest revision leases + validation work budget: cross-service
  (manifest store + operation admission) design.
- A30 (residual) - unknown /api/* routes 404 vs SPA fallback;
  writeRawJSON re-encodes through interface{} (large-integer precision).
- A32 (residual) - 0700/0600 permission tightening for store dirs/files:
  changing modes on existing installs needs a migration note and CLI
  coordination (the CLI reads some of these paths).
- A35 - groups/projects concurrency + AppRef identity: LANDED 2026-09-23
  (X02 S4, `061c312`+`fbb0bb1`): servers.yml carries CLI-minted stable ids
  (srv-<16hex>, preserved by rename/update, legacy entries id-less);
  fleet/preflight envelope ids prefer the recorded id with the name-hash
  fallback; AdmittedServer reconciles renames by id (verified rename
  retargets the command, same-name-different-id and vanished-id refuse);
  groups.json gained server-scoped app refs (`server_apps`, explicit
  binding, ambiguous bare-name removal refuses 409 naming the servers;
  legacy apps list preserved verbatim). Cross-process locking of
  groups.json remains deferred (unchanged from the R11 note).
- A37 - fleet observation envelope (stale/partial/errors): protocol + UI
  redesign; the fleet cache already preserves last-known state and logs
  per-server failures. FIRST SLICE LANDED 2026-09-21 (see the D01 section
  below): /api/fleet per-server envelopes with bounded concurrency and
  per-server timeout; remaining scope recorded there (readiness-vs-running
  separation, backoff, UI depth, R40 inventory envelopes).
- A39 (residual) - FIXED in the residual cluster session (2026-09-18):
  readiness vs liveness separation landed (/healthz cheap liveness;
  /readyz readiness — store reachable via Ping on both backends, operation
  persistence degradation reported per channel with journal-dir probe;
  ready/degraded stay 200, unavailable is 503), and the last unjoined
  background work is joined (operation target runners drain bounded, then
  force-cancel with an explicit shutdown attribution and a fixed persist
  grace, wired between HTTP shutdown and store close). Component health
  covers store + operation persistence; monitor/restore runner joins had
  already landed (pass-7 A46).
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
- A49 - PARTIALLY FIXED in the residual cluster session (2026-09-18):
  mechanical basics landed — explicit label/for association across the
  main flows (deploy forms, server settings, monitor + restore dialogs,
  user/token forms), aria-labels on standalone controls, dialog focus
  traps (role=dialog/aria-modal, initial focus, Tab wrap, Escape) on both
  modals, skip link, aria-current nav, live regions on toasts and
  login/setup errors; visible :focus outlines were already present.
  REMAINS DEFERRED: a real-browser pass (keyboard walkthrough, screen
  reader, contrast audit) — the landed work is verified statically
  (label/id resolution checked mechanically), not interactively.
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
  sequence + per-target ordered execution (same as pass-6 A12/A27).
  Admission budgets LANDED in the residual cluster session (per-target,
  TEPLOY_DASH_MAX_QUEUED_PER_TARGET). Idempotency-key namespacing per
  principal LANDED 2026-09-22 (second D02 slice, see below); still deferred:
  per-PRINCIPAL budget carving (needs the role/policy matrix).
- A13 - actor-attribution fields LANDED in the residual cluster session
  (see pass-6 A27). Idempotency keys namespaced per principal LANDED
  2026-09-22 (see the D02 section below); wiring a key through the MCP
  surface remains additive client coordination (tokens already carry their
  own namespace the moment a tool starts sending one).
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
- A32/A33 - FIXED in the residual cluster session (2026-09-18): SSE-only
  log streaming, hand-written WebSocket deleted (same resolution as pass-6
  A24 above).
- A37 - groups optimistic concurrency + composite app identity: the
  groups.json schema is shared with the CLI; coordinated migration
  (same as pass-6 A35).
- A39 - store file/dir permission migration (0700/0600): needs a migration
  note and CLI coordination (same as pass-6 A32 residual).
- A43 (Advisory) - FIXED 2026-09-23 (X03 first slice, see below): the
  explicit secret/scope/lifetime role matrix landed (capability tokens,
  presets, legacy migration, MCP token capability sets). Token
  expiry/lifetimes remain open (machine-credential policy, D06-adjacent).
- A44 - durable bounded alert outbox with retries: reliability subsystem
  (same as pass-6 A40).
- A47 (residual) - FIXED in the residual cluster session (2026-09-18):
  readiness vs liveness endpoints and degraded-persistence reporting
  landed (same resolution as pass-6 A39 above).
- A51 - frontend load/empty/partial/stale state model: UI-state redesign;
  the loaders with user-facing impact (fleet, links, notifications, detail
  identity) got targeted honesty fixes in this pass.
- A55 - monitor selection as a canonical URL route: frontend routing
  refactor (same as pass-6 A47 residual).
- A58 - PARTIALLY FIXED in the residual cluster session (2026-09-18):
  mechanical basics landed, real-browser verification remains deferred
  (same resolution as pass-6 A49 above).
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

## Resolution log (2026-09-18 residual-cluster session)

- Admission + attribution (commit a866cca): pass-6 A12/A27 remainder and
  pass-7 A12/A13 actor half — per-target admission budgets
  (TEPLOY_DASH_MAX_QUEUED_PER_TARGET, default 50; HTTP 429 on excess,
  idempotent replays pass) and Actor attribution on operations
  (local/sso/mcp; MCP tokens via mcp.WithToken context; retries attribute
  to the retrying principal). Tests in internal/operation/manager_test.go
  and internal/server/operations_test.go.
- Health contract (commit b8b2ecd): pass-6 A39 residual and pass-7 A47
  residual — /healthz + /readyz (store Ping both backends, per-channel
  persistence degradation with journal probe, degraded stays in rotation,
  unavailable 503), bounded operation-runner shutdown joins wired in main.
  Tests in internal/server/probes_test.go and manager_test.go.
- Transport (commit 73b5ef3): pass-6 A24 and pass-7 A32/A33 — SSE-only
  /api/logs/{server}/{app}, ws.go deleted, gate /ws/ special-cases
  removed, EventSource frontend. Tests in internal/server/logs_test.go.
- SSO admin surface (commit 38497af): Settings > SSO + per-user Revoke
  sessions against the landed revocation API; principalView carries
  last_sign_in.
- Accessibility basics (commit 050a4a4): pass-6 A49 / pass-7 A58 partial —
  labels, dialog focus traps, skip link, live regions; static verification
  only, interactive browser pass remains deferred (recorded above).
- Gates at the closing docs commit: `go vet ./...` clean; `go test ./...`
  all 12 packages ok; `go test -race -count=1 ./...` all 12 packages ok;
  `make build` ok. No push performed.

## 2026-09-19 round-3 source-code audit (pass 8)

Register for the independent round-3 audit pinned at reviewed commit
`2b8cad7b62f54600556558f2e0fc60900a2dc241` (74 findings, F001-F074; that
report's IDs, not pass 6/7's). Verification-heavy round: every finding was
checked against the current source before action; standing deferrals were
kept, confirmed new defects were fixed (High first), and no new
teploy-cli-owned defects were found (F024's explicit-targeting contract and
F047's server-qualified AppRef ride the existing A15/A35 records; the two
recorded UPSTREAM items were already fixed in the CLI and adopted).

### Fixed (27)

- F001 - installer checksum verifies the file it actually downloads (the
  release basename, not teploy-dash.tar.gz), plus bounded/validated
  latest-tag discovery, conditional temp env file, loopback UI guidance.
- F002 - a principal-only (SSO-only) users.json is a valid store: zero
  local users no longer locks every authenticated request out after
  restart; setup mode requires no users AND no principals.
- F003 - /api/auth/me answers one {mode, user} envelope in every mode,
  including an explicit disabled-mode answer instead of a 401 the settings
  page misread as viewer.
- F004 - users.json and mcp-tokens.json persist via durable.Replace
  (unique temp, sync, rename, dir sync); internal/store atomicWrite gains
  the post-rename dir sync.
- F007 - SSH cancellation covers NewSession and streaming for the client's
  whole owned lifetime (watcher released in Close).
- F008 - the ssh-agent socket's Signers/sign reads ride the handshake
  deadline.
- F009 - Client.Run captures through per-stream bounded writers with a
  typed ErrOutputLimit.
- F012 - shared ssh.NormalizeAddress for dial + reachability probe (explicit
  ports no longer double-wrapped in :22; IPv6 shapes validated).
- F013 - RunStreamStdin uses os/exec-owned writer adapters + immediate Run,
  so WaitDelay bounds inherited-pipe drains (scan-before-Wait defeated it).
- F014 - runBounded surfaces context.Canceled instead of normalizing the
  group-kill exit into a nil-error result.
- F015 - secret-stdin probes cache only VERIFIED outcomes; unverified
  probes fail closed for secret-bearing writes (env/kv/template vars).
- F016 - journal replay merges events+gaps in sequence order; zero/
  duplicate/regressing sequences are framing damage (no underflow, no
  sequence reuse after recovery).
- F017 - unterminated final journal record repaired by appending the
  newline (at load AND before the next append); storage read failures no
  longer truncate the journal.
- F018 - compaction targets a 75% watermark reserving the incoming record;
  suffix sizing is single-pass.
- F019 - failed admission closes the cached journal handle and removes the
  orphan journal file.
- F020 - age and count retention apply independently (negative max age no
  longer disables the count bound).
- F022 - Manager.Shutdown seals admission synchronously under its mutex;
  straggler HTTP requests get 503 ErrShuttingDown; a timed-out HTTP drain
  force-closes remaining connections.
- F023 - monitor/restore schedulers count whole goroutine lifetimes before
  launch; Stop(ctx) honors one shared 120s worker budget in main; the
  installer unit allows 180s (was 30s vs a ~200s worst case).
- F033 (ours half) - runCheck revalidates the generation after SaveCheck,
  before touching the transition baseline or alerting. Store-side
  revision CAS remains deferred (A19).
- F035 - restore scheduling uses a resettable one-shot timer recomputed
  after each completion (no startup-phased double runs).
- F036 - RunNow returns typed ErrAlreadyRunning; HTTP 409 instead of the
  stale record posing as the requested run's verdict.
- F037 (partial) - region joins the restore-result identity compare in
  both backends; SaveRestoreTestResult returns (applied, error) and the
  runner neither alerts nor advances state off a dropped result. The
  delete/recreate incarnation token remains deferred (A19-family).
- F038 (ours half) - the restore upsert preserves the previous verdict
  only when the target identity is unchanged (retarget clears it). The
  config/result read-modify-write race stays deferred (A24-family
  store-transaction split).
- F040 (DB half) - NucleusStore.GetChecks surfaces row-decode errors
  instead of silently skipping rows. Partial-error envelopes remain
  deferred (A22).
- F055 - the MCP app-logs tool applies CheckExit (non-zero CLI exit is a
  tool error, not successful text).
- F058 - every app-detail panel loader captures resource identity and
  drops late responses (was: only loadStatus).
- F059 - activateResource clears all resource-bound state (KV values/
  scope/generation, drafts, tab, busy flags); destroy() scrubs secrets.
- F063 - logout navigates only on an acknowledged sign-out; failures stay
  visible and retryable.
- F064 - theme storage access is throw-safe (bootstrap + toggle), progress
  -bar timers generation-guarded, duplicate x-init="init()" removed.
- F068 - discovery/instructions/temp-file edges fixed with F001.
- F069 - internal/server/users.json (a synthetic bcrypt-cost-4 fixture
  committed by accident with 2a44046; no test reads it) untracked and
  gitignored with the other credential-store names. No rotation needed —
  the hash was never a live credential.
- F072 - machine-capability probes require --json in the help output, not
  merely a zero exit.

### Deferred (mapped to the standing list)

F005 (A06 residual), F006 (A06/A09 origin config), F010/F011/F030/F031
(A31 protocol redesign; F032's core landed earlier as A53), F021 (dash-03
pending-persistence design), F024/F025/F026/F039 (A15/A13/A19 residuals),
F027/F028/F029 (A34/A37 residuals), F033 store-CAS + F034 (A19/A23),
F037 incarnation tokens + F038 store-transaction split (A19/A24 family),
F041/F042 (A22/A39 residuals), F043 (A40/A44), F044/F045/F046/F047/F048
(A35/A36/A37 family), F049/F050/F051/F052/F053 (A28/A18 residual + design
work), F054/F056 (A21 residual), F057 (A43), F060 (A47), F061/F062
(A51-family), F065/F066 (A50), F067 (A51/A63 residual), F070 (A22
residual refactor), F071 (P3 API consistency), F073 (deployment topology
— single-instance constraint documented), F074 (P3 metric contract).

### Residuals inside fixed findings

- F007: the full fake-SSH-peer regression (handshake completes, session
  open stalls, cancellation must close the transport promptly) is not yet
  an executed test — the ownership change is structural and unit-covered
  only at the address/capture level.
- F022/F023: the complete Lifetime barrier for EVERY admission surface
  (straggler handlers calling monitor Reload after Stop, manifest store
  joins) remains design work; the manager seal + shared budget + joined
  schedulers close the paths that could touch a closed store.

### Round-3 gates

`go vet ./...` clean (after gofmt); `go test ./... -count=1` all 12
packages ok; `go test ./... -race -count=1` all 12 packages ok; `make
build` ok; `node --check` on both frontend bundles. One environment note:
TestRunStreamEmitsBothStreamsAndCancelsProcessGroup still self-skips on
hosts failing the group-kill preflight (unchanged from pass 7; CI runs
Linux). No push performed.

## 2026-09-19 reported UI regressions — fixed

- Deployment cards now key by server + app name. Live fleet has two `lullmail`
  deployments; app-name-only keys broke Alpine rendering. Fleet API failures
  now show an error and Retry instead of an empty-success view.
- Proxy rows use their list position as the rendering key because Caddy route
  IDs are optional (all 203 live deploy-ovh entries have empty IDs).
- Operations now explains its purpose even when history is nonempty.
- Validation: Go suite; Chromium rendered all 45 fleet apps (including both
  lullmail instances), 203 blank-ID proxy rows, and an injected API error.

## 2026-09-19 round-4 source-code audit (pass 9)

Register for the independent round-4 audit pinned at reviewed commit
`ab0c6f7e7a90a2485c80edc5cbc919863b9df5f6` (65 findings, R01-R65; that
report's IDs). Verification-heavy round like pass 8: every finding was
checked against current source before action; standing deferrals were kept;
confirmed defects were fixed (High first); no new teploy-cli-owned defects
were found (no new upstream records needed).

### Fixed (32)

- R01 - restore verification resolves ONE registered target through a
  fail-closed error-returning resolver (SetTargetResolver), rejects
  unregistered servers at creation, and never falls back to
  alias-as-host or an empty (root-defaulting) user; the run records a
  failed verification instead.
- R03 - secret-bearing writes on a VERIFIED-unsupported CLI (env set, kv
  set, template vars) are refused with upgrade guidance unless the operator
  sets TEPLOY_DASH_UNSAFE_LEGACY_SECRET_ARGV=1; empty values keep the
  compatibility path.
- R04 (partial) - the mutation origin guard runs OUTSIDE the optional auth
  gate, so --no-auth still rejects cross-origin state changes. The
  configured-Host-allowlist part stays deferred with A06.
- R05 - sameOrigin (server + MCP) compares the FULL origin via a
  normalizing originKey (scheme+host+port, default ports explicit, case
  folded, userinfo/path/query rejected); scheme is inferred from TLS or a
  trusted proxy's X-Forwarded-Proto.
- R08 - password self-service requires a LOCAL session; SSO sessions are
  directed to their identity provider.
- R10 - baselineHeaders is the outermost middleware, covering 401/403/503
  responses generated by the auth gate.
- R11 (partial) - all groups.json mutations run as locked read-modify-write
  transactions in-process. Cross-process locking with the CLI stays
  deferred (A35).
- R12 - group rename collision answers 409 inside the transaction; group
  and project create/rename enforce a route-safe 1-64 name grammar
  (existing stores stay loadable).
- R13 - homepage replacement requires If-Match (428 absent, 412 stale);
  GET advertises the ETag; both UI editors round-trip it and reload on
  conflict.
- R14 - notification config read errors surface (startup, GET, and PATCH
  refuse unreadable/corrupt state instead of treating it as defaults); the
  read-merge-save-publication sequence is serialized.
- R15 - the server package's atomicFileWrite delegates to durable.Replace
  (unique temp, sync, rename, parent-dir sync).
- R17 - manager-wide admission budget (TEPLOY_DASH_MAX_LIVE_OPERATIONS,
  default 500) plus a global executor-concurrency cap
  (TEPLOY_DASH_MAX_CONCURRENT_OPERATIONS, default 8); slot waits are
  cancellation-aware; idempotent replays stay exempt.
- R21 - the idempotency replay check runs BEFORE Build and is rechecked
  inside the admission transaction; a replay no longer depends on current
  discovery/probe availability.
- R23 - events lost to append failures are accounted per operation and paid
  down with an explicit gap marker before the next retained event.
  Residual: a restart forgets unpaid marker debt (the failed events never
  entered the journal, so recovery sees no hole).
- R24 - a recovery event-append failure degrades visibly (Health + log)
  instead of failing New and disabling the whole operation service; the
  authoritative record save stays fail-closed.
- R25 - age retention runs on a lifecycle-owned hourly loop joined at the
  START of Shutdown; count-driven sweeps are unchanged.
- R27 - cloneOperation deep-copies Actor, AdmittedServer, StartedAt,
  FinishedAt, ExitCode; Actor is copied at admission too.
- R29 - manifests are size-bounded at the store (1 MiB) and per-app
  revision history has a deterministic quota (128 distinct revisions;
  re-registering known content is free).
- R30 - the YAML depth budget counts true recursion depth (mapping values,
  sequence elements, alias hops) instead of mapping-path length.
- R33 - each monitor scheduler captures its generation at construction
  under the lifecycle lock and passes it to every check; staleness is
  checked before network work, before persistence, and before the alert
  baseline. A stopped scheduler can no longer adopt a new generation with
  old configuration.
- R36 - restore-test configuration saves preserve the CURRENT stored result
  inside the store transaction (both backends); the handler no longer
  round-trips Last* values, so an edit cannot overwrite a verification that
  completed mid-edit.
- R38 (partial) - restore startTest counts its scheduler goroutine BEFORE
  releasing the lifecycle lock (the Stop/Add race). The full
  every-surface lifetime barrier stays deferred (F022/F023 residual).
- R39 - the HTTP listener is bound BEFORE any background service starts;
  listener failure and signals funnel through one shutdown epilogue; no
  log.Fatalf inside run.
- R41 - the policy transport carries no hidden phase deadlines; the
  per-check context owns the one total deadline.
- R42 - the dial loop divides the remaining budget across remaining
  addresses so a blackholing first address cannot starve a reachable
  sibling.
- R43 - file-store GetChecks orders "latest" by CheckedAt (stable,
  append-order tie-break) matching the Nucleus backend, including the
  runner's limit=1 transition baseline.
- R44 (partial) - /readyz public responses carry coarse state+code pairs;
  internal error detail goes to the log. The write-probe part of FileStore
  readiness stays deferred (probe files on an unauthenticated endpoint are
  a load/DoS question first).
- R45 - no-auth mode treats the operator as admin for internal-monitor
  policy (isEffectiveAdmin); authenticated mode is unchanged.
- R46 - TCP/ping monitor targets require a nonempty host and numeric
  1-65535 port; restore-test APP names use the deployment grammar (dots
  allowed) instead of the store-ID grammar.
- R47 (partial) - resolveServers/cliAppRun/collectFleetApps are
  context-aware; the fleet deadline is created before discovery; the local
  read error propagates (R49 partial). Context-free adapters remain
  (AccessoryVerifyBackup) — recorded as residual.
- R48 - capability probes run under a 5s deadline, share one in-flight
  result instead of queueing on a mutex held across the subprocess, and
  cache against executable identity + 1h TTL (a replaced CLI refreshes the
  answer without a restart).
- R50 - fleet refreshes are generation-guarded and uniformly single-flight
  (warmFleet joined the latch); a sweep started before a mutation cannot
  republish its stale snapshot as fresh.
- R51 - CheckExit returns a typed ExitStatusError; the health endpoint only
  accepts a COMPLETED non-zero exit as a verdict — transport failures with
  partial stdout are 502s.
- R52 - server rename and field updates must be separate requests (mixed
  changes answer 409; the UI sequences them); the authoritative record
  must be readable before anything changes (failed registry reads fail the
  edit).
- R53 - TEPLOY_DASH_SSH_INSECURE no longer enrolls observed keys into the
  canonical known_hosts; insecure sessions accept-and-forget, with loud
  guidance to enroll real keys for CLI delegation.
- R54 - MCP validates each present argument against its advertised schema
  type (deploy's domain can no longer be silently dropped as a wrong-typed
  optional); initialize params decode errors answer -32602.
- R55 - RunJSON decodes with json.Number and exactly-one-value semantics;
  integers above 2^53 survive the interface{} round trip.
- R56 - writeRawJSON treats empty --json output as a 502 dependency error,
  not data:null.
- R57 - raw-log SSE reassembles writer chunks into logical lines (CR/CRLF
  aware, 64 KiB line budget), emits JSON log events, and surfaces stream
  failures as an explicit stream-error event; browser consumer updated in
  the same change.
- R58 - the operation browser renders gap events as persistent warnings and
  decodes every payload-bearing event defensively.
- R59 - onerror refreshes status but never closes on a terminal snapshot;
  closure belongs to the replay-complete marker, which the server now also
  emits after draining a live stream; destroy() clears the source.
- R60 (partial) - both SSE paths set per-write deadlines via
  ResponseController, open with a comment frame, and the operation stream
  heartbeats every 15s. Continuous in-stream reauthorization stays
  deferred.
- R61 - operations-list polling captures a load generation + filter and
  skips overlapping polls; destroyed components can no longer publish or
  toast.
- R62 - operation history days are range-checked before Duration
  conversion (overflow can no longer wrap negative and disable retention);
  int env knobs are platform-bounded.

### False positive / already-correct (2)

- R12's claim that project rename lacked a collision check was already
  fixed in pass 7 (A07); the group-rename half of the finding was real and
  is fixed above.
- R34's premise that the runner never revalidates after SaveCheck was
  already fixed in pass 8 (F033); the store-side revision-CAS remainder is
  the standing A19 deferral.

### Deferred (with rationale; mapped to the standing list)

- R02 - FIXED 2026-09-23 (X03 first slice, see below): viewer secret reads
  are gone for new accounts (reveal.secrets capability; legacy accounts
  keep them until the explicit narrowing). R31 (heuristic scanning
  limits) remains deferred (scanning hardening, unrelated to the matrix).
- R04 (configured public-origin allowlist), R06 (login admission
  budgeting) - A06 residual origin-config work.
- R07 (SSO session TTL vs IdP token expiry) - policy decision: capping to
  ID-token expiry forces re-auth at token cadence; needs an owner decision
  on the stale-authorization window plus validated back-channel logout for
  immediate offboarding.
- R09 (canonical callback origin) - A09 residual.
- R16 (post-rename uncertainty policy) - F021/dash-03 pending-persistence
  design.
- R18 (unified mutation serialization) - A15 residual (queue conversion
  changes the frontend contract).
- R19 (pinned-target CLI contract) - A27 residual alias-fingerprint work;
  execution-time re-resolution already fails closed (pass 7 A14).
- R20 (principal-scoped idempotency) - LANDED 2026-09-22, second D02 slice
  (see the D02 idempotency-namespacing section below).
- R22 (per-operation terminal-record repair queue) - F021/dash-03 family.
- R26 (byte-based event-cache budgets + LRU eviction) - design work
  needing memory measurements; count/window bounds landed earlier
  (dash-04).
- R28 (manifest document version nonce) - additive store-schema + API +
  frontend contract change; content-hash precondition already bounds
  content races. A28-family.
- R32 (revision leases vs queued operations) - A28 lease design.
- R34 (store-side revision-conditional check commits) - A19 store-schema
  change; the generation fence (strengthened by R33) covers the common
  interleavings.
- R35 (config-persist + scheduler-apply as one transition) - A23
  per-resource coordinator redesign.
- R37 (restore-result incarnation tokens / ABA) - F037 residual, A19
  family.
- R38 (complete lifetime barrier for every admission surface) - F022/F023
  residual design work.
- R40 (partial-error inventory envelopes, reconciliation retry) - A22/A37
  protocol redesign.
- R44 (write-probe readiness) - needs a bounded, cached probe policy for an
  unauthenticated endpoint.
- R47 (context-free restore adapter) - AccessoryVerifyBackup deliberately
  keeps its delegate-bound lifetime (a manual run should survive a client
  disconnect); revisit with a runner-owned lifecycle context.
- R49 (fleet per-server observation envelope) - A37 envelope redesign.
- R60 (continuous stream reauthorization) - session revalidation
  mid-stream needs the epoch-check wiring through SSE handlers.
- R63 (browser lifecycle tests, govulncheck in CI) - A50 residual.
- R64 (non-root container), R65 (volume/state contract) - deployment
  coordination: changing the image user breaks existing root-owned volumes
  on upgrade; needs a planned ownership migration and a documented
  state/backup contract before shipping.

### Round-4 gates

`go vet ./...` clean (after gofmt); `go test ./... -count=1` all packages
ok; `go test ./... -race -count=1` all packages ok; `make build` ok;
`node --check` on both frontend bundles. Environment note: an Xcode license
prompt appeared mid-session on the dev host; builds/commits used
`GOFLAGS=-buildvcs=false` and the CommandLineTools git. No push performed.

## Resolution log (pass 9)

- 2026-09-19 (round-4 audit at ab0c6f7): 32 findings fixed (8 partially,
  residuals recorded), 2 partial claims dismissed with evidence, remainder
  deferred with rationale mapped to the standing list. Conventional commits
  reference the round-4 finding IDs.

## 2026-09-21 D01 first slice — fleet observation envelope (A37/R49, part)

Bounded backend slice of programme workstream D01 ("a fleet view that tells
the truth"). NOT a closure of A37/R49/R40 — the protocol redesign continues;
what landed:

- `GET /api/fleet` returns one observation envelope per configured server,
  every server every time. Envelope: stable ID (deterministic
  `srv-<sha256(name)[:16]>` — server names are the only identity the CLI
  server list exposes, so the ID is name-derived; cross-rename stability
  needs the A35 server-scoped AppRef migration in the CLI contract), last
  success timestamp, collection timestamp, freshness enum
  (fresh/stale/unknown, threshold `fleetFreshAfter` = 2m, aligned with
  observationStaleAfter), partial error, source, and last-known apps.
- An unreachable/degraded server is present with its error + last-known apps
  (previous envelope merged per sweep by ID); it never vanishes from a
  successful partial response. Evidence pre-fix: the old collector logged
  the per-server error and dropped the server, and `publish` overwrote
  `lastGood` with the successful subset, destroying the flapping host's
  last-known apps too (server.go:1556-1571 at 84fdc6d).
- Bounded concurrency (`fleetMaxConcurrentProbes` = 8 worker pool; was one
  goroutine per server, unbounded) and a per-server probe timeout
  (`fleetServerProbeTimeout` = 10s) so one slow host delays only itself;
  the 30s R47 sweep deadline still bounds discovery + sweep.
- `/api/apps` and the MCP list tool keep their exact flat contract (current
  sweep's successful apps only; all-servers-failed still errors 502) — the
  honesty lives on the new endpoint; zero-success sweeps no longer clobber
  the last-known app cache.
- Tests: internal/server/fleet_test.go — all-healthy; unreachable host
  present with error + freshness unknown while siblings stay fresh (the
  load-bearing assertion, red-first: /api/fleet 404 + the vanished host
  captured at 84fdc6d); failed sweep retains last-known apps/last-success;
  slow host bounded under timeout x 4; the cap forces >= 2 waves over 16
  probes; duplicate app names across servers stay distinct in both shapes.

Remaining D01 scope (next slices): readiness-vs-running-vs-desired-state
separation per app and operation-progress projection (needs the CLI machine
contract to expose desired state); per-server refresh backoff (a flapping
host is currently re-probed at the same 60s cadence as healthy ones);
groups.json migration onto server-scoped AppRefs (A35, CLI coordination);
UI depth LANDED 2026-09-22 (second slice below — per-server
freshness/error rendering on the Servers page); R40's inventory-level
partial-error envelopes for the remaining list reads.

## Resolution log (D01 slice)

- 2026-09-21: first bounded slice landed as described above. Gates at the
  working tree (uncommitted): `go vet ./...` clean; `gofmt -l` clean;
  `go test ./... -count=1` all packages ok; `go test -race -count=1
  ./internal/server/` ok; `make build` ok. Frontend untouched (no bundle
  change to node --check). No push performed.

## 2026-09-22 D02 first slice — durable operations with honest uncertain outcomes

Bounded slice of programme workstream D02. It completes the contract AROUND
the existing manager/FIFO/journal/cancellation features (all preserved);
it does not close the deferred D02-adjacent items listed below.

**What landed — restart reconciliation (piece a):**

- On recovery, mid-flight records (queued/running/stopping) are marked
  `interrupted — outcome unknown` and carry a durable `Reconciliation`
  object. "Interrupted" describes the coordinator, never the target: no
  retry is offered until a receipt answers (previously the record said
  "verify remote state and retry" and `Retry` accepted immediately — the
  blind re-offer this slice removes).
- `Retry` refuses with a typed `ErrReconciliationPending` (API 409) while
  the state is `reconciling`; after `reconciled-applied` /
  `reconciled-not-applied` / `needs-manual-check` lands, retry is an
  explicit re-authorization again (lineage preserved). Pre-D02 interrupted
  records (nil Reconciliation) keep the old explicit-retry contract.
- A background reconcile loop (joined at Shutdown, per-read bounded by
  `receiptReadTimeout` = 15s, shutdown-canceled reads left reconciling for
  the next boot — "context canceled" is not an answer) runs the CLI's
  read-side receipts via an injected `ReceiptReader`. The production reader
  (internal/server/receipts.go) answers from `teploy app list --host
  <admitted-host> --json` — the same machine read the fleet uses — against
  the operation's ADMITTED target snapshot (A14): deploy = app present AND
  a container running exactly the requested image (presence without the
  image pin, or an unpinned image, is needs-manual-check with the exact
  reason — never a guess); remove = app absent; every other kind answers
  `no receipt reader for kind %q`. Read failures are needs-manual-check
  with the exact error, never a silent "failed". No receipt reader
  configured = every interrupted record labeled needs-manual-check with
  that reason at recovery time.
- The reconcile loop only READS: nothing is ever auto-retried by the
  restart path (A08 preserved and re-asserted in the new tests).

**What landed — cancellation states (piece b):**

- requested -> stopping/reconciling -> canceled | already_committed, every
  transition persisted and visible in the operation stream. New statuses:
  `stopping` (non-terminal; holds the runner slot while the outcome is
  determined) and `already_committed` (terminal; not retryable — the work
  is done).
- A cancel observed mid-flight no longer records plain "canceled": the
  receipt decides. Applied -> `already_committed`, message says the effect
  stood and NO rollback was performed, evidence on the record
  (`Reconciliation{state: reconciled-applied, evidence}`). Not applied ->
  `canceled` with the evidence. No answer (reader unavailable/unsupported
  kind/read failure) -> `canceled` with "outcome could not be verified:
  <exact reason>" and `needs-manual-check` on the record — cancellation is
  real (dash stopped waiting) but the effect's fate is labeled unknown.
- Cancel before start never consults a receipt (effect provably never
  began). A second Cancel while stopping is refused (`ErrNotCancelable`).
  During bounded shutdown the receipt read is skipped (shutdown stays
  fast); the record says the outcome is unknown. A `stopping` record left
  by a dying process recovers as interrupted and reconciles like any other.

**API surface (additive):** `Operation.reconciliation`
  `{state: reconciling|reconciled-applied|reconciled-not-applied|
  needs-manual-check, reason, evidence, checked_at}`; new statuses in the
  list filter; retry-pending maps to 409.

**TDD evidence (internal/operation/reconcile_test.go,
internal/server/receipts_test.go):** both mutations were written red
against pre-D02 behavior and verified red before implementation
(blind-retry: `TestRestartDoesNotReofferRetryBeforeReconciliation`;
silent-loss: `TestCancelDuringRunDoesNotRecordCertaintyItDoesNotHave`).
Post-implementation mutation checks (gate neutered; receipt branch
neutered) each fail their test; restored, all pass.

**Remaining D02 scope (next slices):**

- Idempotency namespacing by principal/resource: LANDED 2026-09-22 (see the
  D02 idempotency-namespacing section below).
- Queue bounds by PRINCIPAL (per-target and global budgets exist; the
  role/policy matrix prerequisite stands).
- Pin target identity + manifest revision at admission is partially there
  (AdmittedServer, A14) — manifest revision leases vs queued operations
  remain A28/R32 design work; R19 alias-fingerprint admission checks
  remain deferred.
- Receipt coverage: rollback / template_install / manifest_apply /
  app_lifecycle / maintenance have no decidable receipt yet (all answer
  needs-manual-check with the exact reason); deploy-without-image-pin and
  presence-without-image-match deliberately stay manual.
- Startup reconciliation is sequential (bounded, but N interrupted ops =
  N serial reads); no retry/backoff loop for needs-manual-check records.
- Reconciling canceled/already-committed TERMINAL records after the fact
  (e.g. an unverified cancel later becomes answerable) — needs a
  re-reconcile surface; also the cancel_requested recovery path (A09)
  still resolves as canceled without a receipt check.
- Stream resume-by-sequence exists; operation-stream UI depth (rendering
  reconciliation states, hiding Retry while reconciling — the button
  currently surfaces the 409 message) is frontend work, same posture as
  the D01 slice (API is the contract).
- Terminal-outcome repair debt (R22/F021 family): failed terminal-record
  persists during cancel resolution surface via Health degradation but
  have no repair queue.

## Resolution log (D02 slice)

- 2026-09-22: first bounded slice landed as described above. Gates at the
  working tree (uncommitted): `go vet ./...` clean; `gofmt -l` clean;
  `go test ./... -count=1` all packages ok; `go test -race -count=1
  ./internal/operation/ ./internal/server/` ok; `make build` ok. Frontend
  untouched (no bundle change to node --check). No push performed.

## 2026-09-22 D02 second slice — idempotency namespacing (A12/A13/R20)

Keys are scoped to the principal that submitted them (programme demand:
"namespace idempotency by principal/resource"). Closes the collision defect
(two principals using the same client key — one received the other's
operation, live and across restarts) and the unbounded key namespace.

**Design:**

- Namespace = `IdempotencyScope(actor)`: `local:<username>`, `sso:<subject>`,
  `mcp:mcp-token/<id>`, or the explicit `local:no-auth` principal
  (`NoAuthScope`) for nil actors. In auth mode every API request carries a
  session before enqueue, so nil actor at the boundary IS no-auth mode —
  named explicitly so its keys never collide with any authenticated
  principal's. Derived inside Enqueue; `Actor` (attribution) is unchanged.
- The in-memory index keys on `idemKey{principal, key}` (a struct — no
  separator-character collision is forgeable). Same principal + same key =
  replay/conflict exactly as before; different principals, same key =
  independent operations, never shared.
- Persisted namespace: the record carries `IdempotencyPrincipal` next to
  `IdempotencyKey`; restart restores the index from records within the same
  namespace. Legacy pre-namespacing records restore into a `""` namespace no
  namespaced replay matches — an upgrade can never replay one principal's
  operation to another; the cost (a replay in flight across the upgrade
  enqueues new work) is bounded by the window and accepted.
- Bounded window: `IdempotencyWindow` (default 24h; negative disables;
  env `TEPLOY_DASH_IDEMPOTENCY_WINDOW`), measured from the admitted
  operation's creation. Enforced at lookup, at restart restore, and on the
  retention-sweep cadence (expired entries pruned). Past the window the key
  is reusable for new work. MCP tokens already carry per-token namespaces
  the moment a tool starts sending a key (A19 residual wiring).

**TDD evidence (internal/operation/idempotency_test.go,
internal/server/operations_test.go):** red-first against pre-slice behavior
— `TestIdempotencyKeyNamespacedByPrincipal`,
`TestIdempotencyNamespacingSurvivesRestart`,
`TestIdempotencyNoAuthScopeIsItsOwnPrincipal`,
`TestOperationIdempotencyNamesBySessionPrincipal` all failed on the
un-namespaced code (bob received alice's operation, `replayed=true`, both
live and post-restart) before implementation; green after. Mutations
verified: principal dropped from the index key → the three namespacing
tests fail; window check removed from lookupReplayLocked →
`TestIdempotencyWindowExpiresKeys` fails; restored, all pass.

**Remaining D02 scope:** per-PRINCIPAL queue bounds (per-target + global
budgets exist; needs the role/policy matrix); operation-stream UI depth +
stream resume-by-sequence hardening on the events surface (replay machinery
exists; rendering reconciliation states / hiding Retry-while-reconciling is
frontend work); terminal-outcome repair queue (R22/F021 family); receipt
coverage for rollback/template_install/manifest_apply/app_lifecycle/
maintenance; sequential startup reconciliation + needs-manual-check
backoff; re-reconcile surface for terminal records and the cancel_requested
recovery path (A09); MCP key-passing adoption (additive, client
coordination).

## 2026-09-22 D01 second slice — fleet UI depth (A37, UI half)

The Servers page consumes `/api/fleet` additively: cards merge the
`/api/servers` config with each server's observation envelope —
freshness chip (fresh green / stale amber / unknown red; hidden when fleet
data is unavailable), the exact error of an unreachable host, its
last-known app count ("N apps (last known)") and last successful
observation time ("never observed successfully" when it has none), the
stable envelope ID, and a dot that reflects observation truth. Unreachable
servers remain visible — D01's core promise, now user-facing. Fleet-only
servers (the local fallback) also stay visible. `/api/fleet` failure leaves
the cards exactly as before (catch → null, no freshness/error chrome);
`/api/apps` consumers untouched.

**Tests:** `cmd/teploy-dash/frontend-test/servers_page.test.mjs` — zero-
dependency node component test per the repo's no-build/no-CI-Node
convention: loads app.js under stubbed browser globals, captures the Alpine
factories, drives `serversPage.load()` against stubbed `/api/servers` +
`/api/fleet`. Asserts: unknown-freshness server exposes its exact error +
last-known count + last-success time and distinct dot classes; fresh server
carries no error chrome; fleet-unavailable renders no freshness chrome on
any card; fleet-only servers stay visible; plus a static template tripwire
(the card must bind freshnessLabel/unreachableMeta/s.error). Red-first:
failed on the pre-slice component (no freshness surface). Mutations
verified: merge ignoring envelope freshness/error → 4 failures; template
bindings stripped → 3 tripwire failures; restored, all pass. Live-browser
verification: Chromium DOM (stubbed APIs) shows alpha=Fresh/no error block,
beta=Unknown + exact ssh error + "3 apps (last known) — last successful
observation ...", gamma=Stale.

**Remaining D01 scope:** readiness-vs-running-vs-desired-state separation
per app + operation-progress projection (both need the CLI machine contract
to expose desired state); per-server refresh backoff; groups.json migration
onto server-scoped AppRefs (A35, CLI coordination); R40 inventory-level
partial-error envelopes for remaining list reads.

## Resolution log (2026-09-22 second slices)

- 2026-09-22: D02 idempotency namespacing + D01 fleet UI depth landed as
  described above (working tree, uncommitted). Gates: `go vet ./...` clean;
  `gofmt -l` clean; `go test ./... -count=1` all packages ok;
  `go test -race -count=1 ./internal/operation/ ./internal/server/` ok;
  `make build` ok; `node --check` on both bundles; node component test ok.
  No push performed.

## 2026-09-22 D08 slice — monitor races + durable alert delivery

Bounded slice of programme workstream D08 ("monitor edit/delete/recreate
races, failed persistence, notification retry and backend restart preserve
intended schedules and report failures"). Two pieces landed (recon showed
restart schedule preservation itself was already covered by persisted
monitor state + transactional delete; the live defects were the store-side
CAS and the fire-and-forget alert path).

**What landed — piece a: store-side incarnation CAS on check commits
(pass-6 A19 / pass-7 A19 / round-3 F033 store half / round-4 R34, monitor
half of the F037-R37 incarnation-token family):**

- `Monitor.Incarnation` (store-assigned, monotonic per store across edits
  AND delete+recreate — an in-process clock plus the stored value as floor,
  so a recreated ID never reuses the deleted incarnation's number: no ABA
  against an in-flight check). `SaveMonitor(m *Monitor)` assigns it and
  updates the caller's copy; a client-supplied value is never trusted.
- `SaveCheck` is now a compare-and-swap: `(applied bool, err error)`,
  mirroring `SaveRestoreTestResult`. FileStore reads the stored monitor
  inside the lock and drops the row when the monitor is absent (deleted
  mid-check — the old O_APPEND|O_CREATE re-created the history file and the
  dead incarnation's row survived, which a same-ID recreate then inherited
  as a phantom transition baseline and polluted stats) or when the stored
  incarnation differs (edited/recreated mid-check). NucleusStore does the
  same with `INSERT ... SELECT WHERE EXISTS (monitors.id AND incarnation)`
  and reports zero affected rows as applied=false (additive
  `ADD COLUMN IF NOT EXISTS incarnation`, same pattern as allow_internal).
- The runner stamps each check with the scheduler-captured configuration's
  incarnation and honors applied=false: no baseline mutation, no alert, a
  log line — the store is the commit authority (the R33 generation fence
  stays as the in-memory guard; the CAS is the persisted one).

**What landed — piece b: durable alert outbox (A40 / pass-7 A44 / F043,
first slice):**

- `internal/outbox`: every monitor alert enqueue persists to an append-only
  JSONL journal in the data dir (`alert-outbox.jsonl`, one full record per
  transition, folded by delivery id on load; torn tail dropped, corrupt
  middle line fails loudly). A single worker attempts due deliveries
  through `Dispatcher.SendSync` (new synchronous verdict API; the old
  `Send` remains the documented fire-and-forget for the restore runner).
- Retry with exponential backoff (base 30s, max 30m — configurable), dead-
  letter after 5 attempts; the journal is compacted to the newest 250
  records (durable.Replace), never dropping pending (undelivered) records.
- Visibility: `GET /api/monitors` and `GET /api/monitors/{id}` carry a
  `delivery` object per monitor — status (pending/delivered/dead_lettered),
  attempts, the exact last failure, its timestamp, and the next retry time.
- Restart: pending deliveries resume (attempt count carries over),
  delivered ones are never re-sent (delivery-id dedupe = deterministic
  sha256 of monitor+status+occurrence). Semantics are at-least-once per
  channel; the config is re-read at every attempt so a Settings PATCH
  fixes pending retries with no rewiring (the PATCH no longer replaces the
  monitor runner's alerter — that would have bypassed the outbox).
- Wiring: `main` refuses to start on an unwritable journal (A32
  discipline); shutdown joins the worker inside the shared F023 budget.
  Monitor transitions go through the outbox; restore-test alerts keep the
  direct dispatcher (remainder below).

**TDD evidence:** red-first — a pre-fix probe documented the resurrect-
after-delete defect in behavior (`TestRedProbe_SaveCheckAfterDelete-
ResurrectsHistory`, run red then removed; final form in
internal/store/incarnation_test.go), and the outbox/monitor test files were
written against the not-yet-existing API (compile red captured). Mutations
verified then restored: CAS equality neutered in FileStore.SaveCheck →
`TestSaveCheckRequiresCurrentIncarnation` fails; dead-letter bound neutered
→ `TestRetriesWithBackoffThenDeadLetter` and
`TestUnreadableConfigRetriesAndSurfaces` fail; delivery field hidden from
the monitor detail response → `TestMonitorDetailExposesDeliveryStatus`
fails. Restart preservation (piece c) pinned by
`TestStart_PreservesIntendedSetAcrossRestart` (deleted stays unscheduled
and checkless, edited monitor's incarnation carries into its restarted
checks) and the outbox restart tests.

**Audit items closed by this slice:** pass-6 A19 + pass-7 A19 (store-side
revision-conditional check commits — the monitor/check half; the
restore-result conditional commit was already F037-fixed), R34 (was the
standing A19 deferral), the monitor half of the F037/R37 incarnation-token
family. A40/A44/F043 move from "best-effort by design" to first durable
slice (not fully closed — remainders below).

**Remaining D08 scope (next slices):**

- Restore-test alerts still bypass the outbox (direct dispatcher); routing
  them through it needs the restore runner's Alerter widened like the
  monitor runner's.
- Per-channel delivery accounting: a webhook that succeeded before an
  email failure is re-sent on retry (at-least-once per channel). Exposing
  the delivery id in webhook/email payloads so receivers can dedupe is an
  additive contract change.
- Backend consistency contracts JSONL vs Nucleus: the outbox journal is a
  data-dir file in BOTH backend modes (monitors/checks live in the store;
  alert delivery state does not). Moving it onto the Store interface with a
  Nucleus table is the alignment work, recorded here per the D08 mapping.
- Capacity guidance: outbox defaults (5 attempts, 30s→30m backoff, 250
  retained records, 5s poll) are asserted by tests but not documented in
  README; alert volume beyond human-scale (flapping monitors at 10s
  interval) is unbounded enqueue-wise — a per-monitor transition-rate limit
  is future work.
- Durable next_due_at for monitor schedules (pass-7 A25 residual,
  store-schema addition): restarted monitors still fire their first check
  immediately rather than waiting the true remainder (restore tests derive
  from LastRunAt; monitors do not persist a schedule anchor).
- The A23/R35 config-persist + scheduler-apply single-transition redesign
  remains the standing umbrella for the residual interleavings the CAS now
  bounds but does not eliminate (e.g. a reload whose SaveMonitor succeeded
  but whose scheduler teardown was cut short by shutdown).

## Resolution log (D08 slice)

- 2026-09-22: D08 bounded slice landed as described above (working tree,
  uncommitted). Gates: `go vet ./...` clean; `gofmt -l` clean;
  `go test ./... -count=1` all 12 packages ok; `go test -race -count=1` on
  internal/store, internal/monitor, internal/alert, internal/outbox, and
  internal/server ok; `make build` ok. Frontend untouched (no bundle change
  to node --check). No push performed.

## 2026-09-22 D03 first slice — onboarding preflight foundation

Bounded backend-first slice of programme workstream D03 ("provide distinct,
understandable entry points... BEFORE asking for application details, show
host connection/capability readiness"). This is the preflight foundation,
not a closure of D03 — the four entry points' full journeys, plan preview,
error-recovery flows past the first rung, and the template catalog remain
(recorded below).

**Recon (what existed):** servers are registered through the CLI's
servers.yml (`teploy server list --json` via resolveServers; dash adds via
/api/config/servers); the only connectivity check was the `online` flag
from a 2s TCP dial to the SSH port on /api/servers (tcpReachable); the
local CLI capability surface existed as /api/capabilities (version +
machine-interface --help probes, cached); per-host specifics existed only
as the Servers-detail `teploy server status <name> --json` read (docker
installed/version, disks with available bytes, caddy routes, scoped
errors). The deploy forms (projects page + project detail) collected
app/image/domain/server/port and posted /api/deploy with NO host check
before or during — app details were asked before any host readiness.

**What landed — preflight endpoint (internal/server/onboarding.go):**

- `GET/POST /api/onboarding/preflight?server=<name|candidate-host>` (POST
  accepts `{"server": ...}`) returns one readiness envelope per probe in
  the D01 style: stable ID, known/collected-at/error/source + a checks
  array. Each check: `name` (teploy_cli, machine_interface, ssh,
  host_read, docker, disk, caddy), `result` (pass|fail|unknown),
  `severity` (blocking|warning), detail, and an actionable remediation
  string on every non-pass (the recovery-from-failures first rung).
- Sources: CLI capability cache (teploy_cli + machine_interface),
  tcpReachable on the F012-normalized address (ssh), `teploy server status
  --json` (host_read + docker/disk/caddy specifics; scoped machine errors
  fail their check with the exact message — the reachable-degraded shape),
  with the SSH-executor fallback (uptime/free/df/docker ps round trip)
  when the CLI lacks the machine interface: disk parsed from the df
  figures, docker/caddy honestly UNKNOWN at warning tier (the fallback
  cannot verify daemon state).
- D01 envelope conventions: unknown server name → 200 with `known:false`,
  the error naming it, blocking checks unknown, plus a bare-reachability
  probe when the ref parses as a candidate host; discovery failure → the
  envelope carries "server discovery failed: ..." — never a dropped body.
  Disk tiering: <1 GiB blocking, <5 GiB warning. Caddy failures are
  warning-tier (deploys queue; domains would not answer). Whole probe
  bounded by a 20s timeout; SSH dial 3s (vars, test-tightened).

**What landed — flow gating (additive):**

- `GET /api/onboarding/entry?server=<name>` → `{server, gated, reason,
  preflight}`: gated with "no server selected" when the param is absent;
  gated with the blocking checks named (or the envelope error) when
  readiness fails; `ready` derives from no blocking failure plus the core
  path (teploy_cli/ssh/host_read) having PASSed — an unknown core check
  gates like a failure because nothing about the target was verified.
- `/api/deploy` and every other direct creation path keep their exact
  contract (recorded as a D03 follow-up to route them through preflight
  deliberately). The UI gates: both deploy forms (projects page + project
  detail) fetch preflight on server selection, render a readiness panel
  (severity-classed checks, remediation hints, re-check button), and
  disable Deploy while a blocking check fails, readiness is loading/
  unknown, or the preflight itself errored — warnings render non-blocking.
  Late preflight responses are dropped when the selection changed (A50).

**TDD evidence:** internal/server/onboarding_test.go written red first
(compile-red captured: endpoint and symbols absent) — healthy (all checks
pass, ready, version + disk detail carried), reachable-degraded (docker
daemon down via scoped machine error → blocking fail with exact message +
remediation, envelope present), unreachable (ssh + host_read fail
blocking, docker/disk/caddy unknown, envelope not dropped), unknown
server name (visible error, known:false, not dropped), discovery failure
(visible error, never empty success), POST body form, and the gating
matrix (healthy → not gated; docker down → gated with docker named; no
server → gated). Mutations verified then restored: preflightReady
neutered (degraded reads ready) → TestOnboardingEntryGatingStates,
TestOnboardingPreflightDockerDownIsBlocking, and
TestOnboardingPreflightUnreachableServerNotDropped all fail. Frontend
(cmd/teploy-dash/frontend-test/onboarding_preflight.test.mjs, node-test
convention): blocking gates canDeploy with remediation surfaced, warnings
non-blocking, healthy + complete form deployable, preflight-fetch failure
gates as unknown, no-server gates, stale-response drop, plus index.html
tripwires (both forms bind preflightChecks/preflightCheckClass/
preflightError/canDeploy/check-remediation). Frontend mutations verified
then restored: canDeploy ignoring preflight.ready → gating assertion
fails; :disabled canDeploy bindings stripped → both tripwires fail.

**Remaining D03 scope (next slices):**

- The four entry points' full journeys: existing image, Git repository,
  supported Compose stack, maintained template — today only the image path
  exists (deploy form); Git/Compose entry points are not built.
- Route the direct creation paths (/api/deploy, template install, MCP
  deploy tool) through preflight deliberately — currently additive-only
  (API consumers keep the direct contract by design this slice).
- Error recovery flows past the first rung: preflight remediation strings
  exist; wrong-DNS / registry-denial / missing-env / failed-readiness
  recovery needs the deploy-failure surfaces (operation events + app
  health) wired to the same remediation vocabulary, and a
  "successful onboarding ends at a real responding application" end-state
  (post-deploy readiness verification against the app health probe).
- Plan preview (what will be created/changed before confirm).
- Template catalog depth (the maintained-template entry point's list,
  compatibility metadata).
- Preflight result caching/backoff: each request probes live (bounded);
  a poll-heavy UI could warrant a short TTL cache keyed like the fleet
  probe timeout pattern.

## Resolution log (D03 slice)

- 2026-09-22: first bounded slice landed as described above (working
  tree, uncommitted). Gates: `go vet ./...` clean; `gofmt -l` clean;
  `go test ./... -count=1` all 12 packages ok; `go test -race -count=1
  ./internal/server/` ok; `make build` ok; `node --check` on both
  bundles; both node component tests (servers_page, onboarding_preflight)
  pass. No push performed.

## 2026-09-23 X03 first slice — capability matrix, backend-first (A10/R02/A43)

Bounded backend-first slice of programme workstream X03 ("roles backed by
resource-scoped capabilities... Default viewers do not receive env/KV
secret contents... a visible migration and explicit legacy profile rather
than silently changing every account"). Closes the standing policy
deferral A10/R02 (pass-6 A10 residual, pass-7 A43, round-4 R02 — "needs
an approved role matrix + migration": the matrix and the migration landed
here) and unblocks the D02 remainder's per-PRINCIPAL budget carving.

**What landed — capability registry (internal/caps, new package):**

- 8 stable tokens: view.metadata, reveal.secrets, execute.deploy,
  execute.mutate, restore.data, administer.credentials, administer.users,
  view.logs. Shared by the session gate and the MCP surface so both
  principal kinds speak one vocabulary. Validate/Normalize fail closed at
  write boundaries; loading drops unknown tokens (forward-compat) instead
  of bricking the store.
- Presets: viewer = view.metadata; editor/operator = viewer +
  execute.deploy + execute.mutate + view.logs; admin = all. Legacy sets
  (FROZEN, per role): viewer/editor keep reveal.secrets + view.logs
  (today's documented viewer value access), editor/admin add restore.data
  (editors could run restore verifications), admin = all.

**What landed — enforcement (internal/server/caps.go + gate seam):**

- The route table `requiredCapabilities(method, path)` annotates every
  route class; the check REPLACED the old role-rank check inside
  authGate.wrap, enforced against the LIVE capability set rebuilt from the
  principal row on every request (same seam and freshness discipline as
  the live role read). 403s name the missing capability. Fail-closed
  defaults preserved: unclassified mutation = execute.mutate, read =
  view.metadata.
- Load-bearing separations: GET env (values) and GET kv/value require
  reveal.secrets; NEW `GET /api/apps/{s}/{a}/env/keys` is the
  viewer-visible metadata variant (names only, same reduction as the MCP
  list_env_keys contract; env reads moved onto the injected CLI runner
  like kv, making the split hermetically testable); deploy-class POSTs =
  execute.deploy; env/kv writes + monitor/group/homepage/manifest
  mutations = execute.mutate; the whole restore-tests mutation surface =
  restore.data; users/SSO = administer.users; mcp-tokens/config-servers/
  registries/notifications = administer.credentials; /api/logs + app log
  + accessory logs = view.logs; /api/auth/me + /api/auth/password are
  capability-free (self-service identity).

**What landed — the honest migration:**

- dashUser/dashPrincipal gained `capability_profile` + `capabilities`
  (additive JSON; absent profile = legacy). Existing installs' accounts
  map to the legacy profile with today's effective permissions — never
  silently narrowed, never silently widened, preserved across restarts
  (pinned by tests). New accounts are created on presets.
- Settings surface: GET /api/users lists profile + effective capabilities
  per account; `POST /api/users/{u}/narrow-to-preset` is the explicit
  narrowing click — no epoch bump needed: capabilities are re-read live,
  so the SAME session is narrowed on its next request (tested).
  `PUT /api/users/{u}` with `{"capabilities":[...]}` stores a custom set
  (empty = deliberately locked; the "custom" profile marker prevents an
  empty set round-tripping back to the legacy default). Role and
  capabilities changes are one-per-request. UI: Settings → Users shows
  the profile (legacy flagged) + Narrow to preset button.
- /api/auth/me answers `capabilities` in every mode (disabled mode =
  full set), so the frontend can pick panel shapes: the env panel loads
  env/keys for metadata-only sessions (values render "(restricted)", no
  toggle), the KV Reveal button hides without reveal.secrets.

**What landed — MCP machine principals:**

- mcp.Token gained an explicit capability set recorded at mint: default =
  operator-minus-secrets (view.metadata, execute.deploy, execute.mutate,
  view.logs — documented in README; no MCP tool reveals secret values
  anyway), read_only mints = view.metadata, `CreateWithCapabilities`
  validates and rejects empty sets (round-trip widening guard). Every
  tool declares RequiredCap; tools/list hides unauthorized tools and
  tools/call refuses with the capability named. Legacy tokens (nil
  capabilities, pre-X03 files) derive their set from read_only exactly as
  before — pinned by test. POST /api/mcp-tokens accepts a capabilities
  array; GET lists the effective set.

**TDD evidence:** red-first — internal/caps/caps_test.go,
internal/server/caps_test.go, internal/mcp/caps_test.go written against
the not-yet-existing API (compile red captured: registry/presets/
requiredCapabilities/CreateWithCapabilities undefined). Post-implementation
mutations verified then restored: gate check neutered (never denies) →
TestViewerCannotRevealSecretValues + TestNarrowToPresetTakesEffectImmediately
fail; MCP tool check neutered → TestToolEnforcementNamesMissingCapability
fails; frontend loadEnv ignoring canRevealSecrets → env_caps component test
fails (2 assertions). Matrix per route class: viewer lists apps/kv keys but
env GET and kv/value 403 naming reveal.secrets and env/keys returns
names-only; operator deploys (202 via /api/operations) but cannot reveal
unless granted (custom grant → 200 with values) and cannot administer; admin
all; legacy viewer/editor/admin keep exact prior behavior incl. restart;
narrowing click effective on the same live session; MCP default/read-only/
custom/legacy scoping; whoami exposes capabilities.

**Audit items closed by this slice:** pass-6 A10 residual + pass-7 A43 +
round-4 R02 (the approved role matrix + migration the deferral waited
for); the A42-residual capability-view is subsumed by whoami capabilities.

**Remaining X03 scope (next slices):**

- Custom-role UI (the storage + API + validation landed; Settings only
  offers narrowing — a capability editor for custom sets is frontend
  work). D06-adjacent.
- Per-PRINCIPAL queue budgets (D02 remainder; prerequisite now cleared).
- OIDC mapping: principals carry the profile fields, but no IdP
  claim → capability mapping exists yet (roles stay IdP-authoritative;
  a capabilities claim + auto-narrowing policy is design work).
- Audit-log actor fields: operations carry Actor (A27) but not the
  capability set that authorized them; recording the granted-at
  capability per admission is additive schema work.
- Stream/export revocation within policy: SSE streams (logs, operation
  events) authorize at connect; continuous mid-stream reauthorization
  remains deferred (R60) — a capability revoked mid-stream takes effect
  on the next connect.
- Secret-bearing config reads (notifications/registries list payloads)
  are administer.credentials like before; splitting their secret_set
  metadata from values would follow the env/keys pattern if ever needed.

## Resolution log (X03 slice)

- 2026-09-23: first bounded slice landed as described above (working
  tree, uncommitted). Gates: `go vet ./...` clean; `gofmt -l` clean;
  `go test ./... -count=1` all 13 packages ok; `go test -race -count=1`
  on internal/caps, internal/mcp, internal/server ok; `make build` ok;
  `node --check` on both bundles; all three node component tests
  (servers_page, onboarding_preflight, env_caps) pass. No push performed.

## 2026-09-23 D04 first slice — git-source identity + webhook delivery (backend-first)

Bounded backend-first slice of programme workstream D04 ("store canonical
forge/repository identity, immutable revision, credential scope and webhook
delivery identity; handle revoked permissions, private repos, monorepos,
changed default branches and deleted refs"). Recon showed dash had NO
git-source model, so this slice CREATES the foundation; it is not a closure
of D04 (remainder below).

**Recon (file:line at base 3e9a782):** deploys reference sources as image
refs only (`Request.Image` through `teploy deploy --image`,
internal/operation/build.go:106); the closest existing surface is the
manifest store's `GitReference{Repository, Revision, ManifestPath}`
(internal/manifest/store.go:41) — a bare URL string with no forge identity,
no credential scope, no delivery record. The only webhook code was
OUTBOUND alert delivery (internal/alert). teploy-cli already owns the
SERVER-SIDE inbound path end to end (`teploy autodeploy setup/serve`): C02
landed its admission ledger
(`/deployments/<app>/.autodeploy-ledger.jsonl`, ack-after-fsync, bounded
newest-wins queue, restart resume) and commit pinning (`PushCommit`:
checkout_sha preferred, tags/deletions/zeros/malformed pin nothing). Dash
therefore LAYERS ON TOP rather than duplicating: dash's webhook admits
DASH operations (manifest applies) with its own delivery ledger following
the same C02 disciplines; the CLI's listener and its ledger are untouched
(converging the two trigger paths is recorded C02 cross-repo scope).

**What landed — the identity model (new `internal/source` package):**

- Canonical identity = forge kind + normalized clone URL. Normalization
  (url.go): credentials/userinfo stripped BEFORE anything persists, scp
  (`git@host:path`) and `ssh://` forms folded onto the https form,
  `.git`/trailing slashes dropped, scheme+host lowercased with path case
  preserved for display, default ports dropped, http kept as a distinct
  identity, query/fragment/control chars rejected. The dedupe KEY folds
  full URL case (forges match owner/repo case-insensitively); forge kind
  is a separate axis — two forges with the same owner/repo, or two kinds
  over one URL, are two identities (the F03 lesson from teploy-ship
  docs/AUDIT_2026-09-21.md: bare owner/repo lookups returned a different
  forge's configuration). Stable ID = `src-<sha256(identity)[:16]>`, the
  fleet-envelope convention.
- Records under `<dataDir>/sources/<id>/` (data-dir store in BOTH backend
  modes, like manifests): `metadata.json` (0600, durable.Replace,
  DisallowUnknownFields, canonical-URL revalidation on load) carries
  identity, an as-of-added DefaultBranch snapshot, the CredentialRef
  (opaque scoped-token reference — never a token value; bound 128 chars,
  no whitespace), mutable DisplayName separate from identity, and the
  D01-convention degradation envelope (Degraded/DegradeReason/
  DegradeCheckedAt). The webhook secret lives in its own 0600 file,
  returned exactly once on create/rotate, never in any later read
  (pinned by test reading the raw metadata.json).
- Delivery ledger per source (`deliveries.jsonl`, append + fsync, torn
  tail dropped, mid-file corruption fails loudly, compacted at 2x the
  250-record retention): provider delivery id, normalized event, branch,
  authenticated commit, disposition (admitted/duplicate/ignored/degraded/
  superseded/refused) and reason. "Refused" deliveries deliberately do
  NOT join the seen index — the forge's retry re-runs admission instead
  of being swallowed as a duplicate (C02's rollback rule).

**What landed — webhook delivery identity (internal/server/sources.go):**

- `POST /hooks/sources/{id}` — unauthenticated by design (the signature
  is the auth), gate-allowlisted like /api/mcp outside sessions; verifies
  `X-Hub-Signature-256` (constant-time HMAC-SHA256 over the raw body) or
  `X-Gitlab-Token`. Unauthenticated deliveries are NEVER recorded (an
  attacker could spray fabricated delivery ids into the ledger) — 401 +
  log only. Delivery id from `X-GitHub-Delivery`/`X-Gitlab-Event-UUID`,
  else derived from the body digest so retries still dedupe.
- Authenticated deliveries: dedupe by delivery id (200 `duplicate`);
  non-push events, unpinnable pushes (tags/deletions/malformed — the CLI
  PushCommit rules, reimplemented in source.ParsePush), unwatched
  branches (as-of-added default-branch snapshot: a forge-side default
  change never silently retargets deploys) and degraded sources are
  RECORDED-and-ignored with the exact reason — never silently dropped.
  Deleted refs produce no commit pin, hence recorded-and-ignored.
- Forwarding rides the EXISTING D02 operation queue (no second queue):
  the delivery joins git-managed manifests whose `git.repository`
  canonical URL matches (transport+case-folded; an uncanonicalizable
  manifest repository fails the whole delivery loudly), then enqueues one
  `manifest_apply` per bound manifest carrying the CURRENT registered
  manifest revision (admission pins it — a moving branch cannot change an
  admitted build) plus new `Request.SourceID`/`SourceCommit` fields
  (additive, omitempty — old request hashes unchanged; they join the
  request hash so a new commit is new work and a replayed delivery
  converges), attributed to `Actor{kind: webhook, subject:
  source/<id>}` (its own idempotency namespace) under the delivery-
  scoped key `wh:<delivery>:<server>/<app>`. Admission refusals are
  recorded as refused without marking the delivery seen and answered
  429/503 so the forge retries.
- Reorder convergence: a newer delivery cancels the still-QUEUED
  operations its source admitted for the same target (ledger marks them
  superseded-by; the queued record resolves canceled per D02 semantics)
  and admits its own — C02's newest-wins on top of FIFO. The RUNNING
  operation is never interrupted (C02's deliberate posture); a
  ledger-failure retry is identified by its idempotency key and replays
  instead of superseding itself.
- API surface (administer.credentials class — payloads carry webhook
  secrets and credential refs, the registries/notifications precedent):
  GET/POST `/api/sources`, GET/PATCH/DELETE `/api/sources/{id}`,
  POST `.../verify` (runs the injected SourceCredentialVerifier; failure
  → source degraded with the exact reason, visible on every read;
  success clears it; nil verifier answers 501 explicitly — never a
  silent degradation and never a silent pass), POST `.../rotate-secret`.
  Route matrix extended in caps_test.

**TDD evidence:** internal/source tests (url/store/deliveries) written
compile-red first; internal/server/sources_test.go written against the
not-yet-existing API (compile red captured). All three mutation checks
verified red then restored: credential-failure swallowed (MarkCredentialState
neutered) → TestSourceRevokedCredentialDegradesVisibly fails on the missing
`degraded:true` envelope; delivery id ignored (DeliverySeen neutered) →
TestSourceWebhookDedupeAndReorderConverge fails (the duplicate delivery
admitted a second operation); supersede skipped → the same test fails
(superseded operation succeeded instead of canceled).

**Remaining D04 scope (next slices):**

- GitHub App flow (installation tokens, per-installation credential
  verification) and the production SourceCredentialVerifier (forge API
  calls per kind; today verify answers 501 without one).
- Builds FROM git: the admitted manifest_apply applies dash's registered
  manifest revision; a checkout-of-commit flow (clone at the pinned SHA
  server-side or via the CLI's autodeploy contract) is the actual
  git-build path — needs a CLI contract decision (which side checks out).
- PR preview lifecycle: status, access, expiry, secret isolation,
  cleanup, link back to the source review (C06 dependency); non-push
  events are currently recorded-and-ignored.
- Monorepo path filters via the CLI contract (the CLI's changed-file
  rule); private-repo credential USE (the scoped reference is stored and
  verified-but-unused; actual authenticated fetches ride the build flow).
- Delivery identity depth: per-forge timestamp freshness on HMAC
  (delivery-id dedupe bounds replay today), payload size beyond the
  1 MiB mutation cap (large push payloads 413 before the handler),
  GitLab X-Gitlab-Signature (SHA-256 variant) alongside the token
  header, and a UI surface (sources page, delivery history rendering).
- Convergence of dash-webhook vs CLI-autodeploy triggers for the same
  app (C02's recorded cross-repo remainder).

## Resolution log (D04 slice)

- 2026-09-23: first bounded slice landed as described above (working
  tree, uncommitted). Gates: `go vet ./...` clean; `gofmt -l` clean;
  `go test ./... -count=1` all 14 packages ok; `go test -race -count=1`
  on internal/source, internal/operation, internal/server ok; `make
  build` ok. Frontend untouched (no bundle change to node --check). No
  push performed.

## X02 S7 acceptance sweep — 2026-09-23

`scripts/x02-acceptance-sweep.sh` (this repo's executable harness, ADR §6
S7). Legs and evidence (exit 0, all PASS, non-vacuous):

| Leg | Package | Tests | Result |
|---|---|---|---|
| rename-operations | ./internal/operation | 4 (rename follows identity + retarget; same-name-different-id; vanished-id; retargetCommand guards) | PASS |
| rename-groups | ./internal/server | 3 (swap survival; stable-id binding; legacy fallback binding) | PASS |
| duplicate-names | ./internal/server | 11 (same-app-two-servers distinct; ambiguous removal refuses; fleet envelope set) | PASS |
| repeated-request | ./internal/operation | 3 (replay/conflict; principal namespacing; restart survival) | PASS |
| response-loss | ./internal/operation | 11 (restart reconciliation set; cancel-during-run; future-record read-only; version stamp) | PASS |
| rollback | ./internal/operation | 1 (repointed-target refusal — the A14 gate rollback relies on) | PASS |

Vacuous-match guard included. Re-run and paste fresh output here on any
contract change.

## X01 job-3 tail — dash CI decodes the pinned contracts corpus — 2026-09-23

New `contracts` job in `.github/workflows/ci.yml` (runs on push/PR and via
the release `validate` reuse, so a tag cannot ship without it):

- Pin: `ci/contracts.pin` records `cli_rev=c413685` / `corpus_rev=2`
  (MANIFEST revision table is the authority; rev 2 = c413685's contracts/).
  The job parses the pin, checks teploy-cli out at that SHA
  (sparse: contracts), and cross-checks the pinned corpus rev against the
  MANIFEST's table before anything else runs.
- Node leg: `ci/validate-contracts.mjs` (ajv 8.17.1 + ajv-formats 3.0.1,
  pinned in `ci/package.json` + lockfile): compiles EVERY schema (a schema
  that cannot compile fails the job even with no fixtures yet), validates
  every valid/ fixture, asserts every invalid/ fixture FAILS validation,
  and refuses any fixture file its expectation table does not classify
  (corpus growth must be a conscious pin+table update, never a silent
  pass). Never skips as success.
- Go leg: `TEPLOY_CONTRACTS_DIR` (set by the job) makes
  `internal/server/contracts_corpus_test.go` fail-closed on a missing
  corpus and drives the fixtures through dash's REAL decode paths:
  version-handshake + app-list via `cli.ParseJSON` (the MI gate:
  missing `machine_interface` = pre-MI legacy, never MI 0), app-list via
  `decodeAppListEnvelope`/`assertAppStatusComplete` (extracted from
  `readMachineApps`, behavior unchanged), the preview-state ambiguous
  fixture through the adoption-refusal contract (era stays legacy, id
  must NOT satisfy the canonical grammar, both candidate branches
  present). The existing `TestContractsObservationEnvelopeGolden` also
  un-skips under the env var, so the dash-produced goldens are diffed
  against the pinned corpus in the same job.

### Found by the job on first run (upstream: teploy-cli corpus, NOT fixed here)

The pinned corpus (rev 2) does not fully decode — three defects, all
teploy-cli-side (layer rule: dash must not patch around a corpus that
misrepresents the producer):

1. `contracts/schema/server-status-envelope.schema.json` does not compile:
   it `$ref`s `#/$defs/release`/`container`/`machineError` but defines no
   `$defs` at all (the defs live in the app-list schema). The schema was
   never compiled by anything before this job.
2. `fixtures/app-list-envelope/valid/mi1.json` fails its own schema and
   dash's decode: `previous_release.ports`, `processes`, `errors` (app and
   root) are `null`; the real encoder initializes every one of those to an
   empty slice (`teploy-cli/internal/cli/machine.go` collectAppList /
   collectAppStatus, unchanged since the pre-MI era) — the golden test
   constructed the DTO with zero values instead.
3. `fixtures/app-list-envelope/legacy/pre-mi.json`: same zero-value class
   (`errors: null`); real pre-MI encoders emitted `errors: []`.

Remedy (teploy-cli): initialize the collections in
`internal/cli/contracts_golden_test.go`'s fixture literals, fix the
server-status schema's defs, regenerate, bump the MANIFEST revision table,
and then bump `ci/contracts.pin` here in the same coordinated change. The
dash job stays strict in the meantime — that red is the job working.

Deferred with this slice: dash has no preview-state importer (previews are
CLI-side state; dash reads apps via envelopes), so the ambiguous fixture's
refusal is asserted at the decode/grammar seam above; when a preview
surfacing slice lands, its importer must route era=legacy+candidates
through explicit-binding refusal and extend
`contracts_corpus_test.go`.

