# Changelog

All notable changes to teploy-dash are recorded here.

## [Unreleased]

### Added

- Connecting Observe links straight to its token form, filled in.
- Disk alerts: Dash checks each server's filesystems every 10 minutes and alerts at 90% full, critical at 95%, clearing below 85%.

## [0.1.24] - 2026-11-05

## [0.1.23] - 2026-11-05

## [0.1.22] - 2026-11-05

### Added

- **Siblings — product discovery** (Programme B item 1, plan section 10.1;
  needs teploy v0.1.54 on the dashboard host, capability `siblings-v1`):
  - `GET /api/siblings` and the **Siblings** page: every Teploy product the
    CLI's label report finds on the fleet (`teploy server siblings --json`,
    hourly sweep + on-demand rescan, cached in `<data>/siblings.json`), each
    candidate probed on its published ports for `/.well-known/teploy.json`
    (the forgejo family for `/api/v1/version`). Verdicts are honest:
    confirmed (with the doc's product, version, instance id and published
    URLs), found-not-discoverable (Ship ships discovery off), no answer, not
    probeable from dash, or blocked (internal). Unknown `dev.teploy.product`
    values pass through and render as unrecognised labels.
  - Probes run on the monitor network guard; tailnet/LAN addresses are probed
    only after an admin allows internal probing (audited; an editor cannot
    flip it — the A13 rule). A failed sweep keeps the last report with its
    age; a partially-read server is never shown as "no siblings".
  - The nav switcher fills from CONFIRMED discovery docs' `dashboard_url`
    only (never guessed; `TEPLOY_NAV_*_URL` still wins, the fleet-name
    fallback stays); a confirmed forgejo joins as a plain link.
- **Observe connect + per-app telemetry** (Programme B items 2-4): connect
  with an Observe service token (minted in Observe Settings; dash never sees
  an Observe admin login). Two-step connect — preview the instance so the
  owner confirms its id from the discovery doc BEFORE any token moves, then
  paste the token; dash re-reads the doc, enforces the confirmed id, and
  verifies the token with a harmless authenticated read. The token persists
  in `~/.teploy/observe.json` (0600; the notifications.json secrets-file
  pattern), surfaces only as a fingerprint, and is scrubbed from every
  stored or answered error. Per-app telemetry is opt-in, plan-first: the plan
  names the site (by `external_ref`, created if absent), the labelled
  telemetry key, every env/secret write (the OBSERVE_*/OTEL_* contract,
  `$TEPLOY_VERSION` written literally as specified) and the redeploy;
  approval sends the plan's fingerprint and apply refuses if anything moved.
  Values ride the CLI's stdin contracts; the minted key appears in no
  response, record or log. Stop-telemetry removes exactly the recorded keys
  and says the site/key live on in Observe until revoked there.
- **Group suggestions** (Programme B item 7): `GET /api/group-suggestions`
  and a Projects-page section — apps sharing a registrable domain
  (`golang.org/x/net/publicsuffix`, new dependency) or a name prefix,
  singletons dropped, already-grouped apps never suggested. Accepting uses
  the ordinary groups API (create + server-bound assignment), one suggestion
  at a time; dash never writes a group on its own.

- **Dash Backups** (plan P2, the dash half of Slice 4; needs teploy v0.1.52 on
  the dashboard host, capability `backup-fleet-v1`; older CLIs get a note and an
  empty page — the engine runs on each server, not here):
  - `GET /api/backups` and the **Backups** page: every database and volume across
    the fleet — engine, last good backup + age, newest copy size, projected
    steady-state storage, encryption, restore-test verdict, coverage word — from
    `teploy backup status --json` read per server every 30 minutes into
    `<data>/backups.json` (never probed on page load). An unread server reads
    unknown everywhere, never healthy.
  - First-run checklist (storage → alert channel → recovery-key drill → first
    verified backup), shown until complete.
  - Connect storage once: B2/R2 presets with S3-compatible behind "advanced",
    write/read/delete probe before any push, `server backup-target set` to every
    server with credentials on stdin, then the credentials are forgotten (the
    record keeps bucket names and stable per-server prefixes). Rotation
    re-prompts, pushes and verifies before telling the owner to revoke.
  - Recovery key: `teploy backup keygen` shown exactly once (re-auth required,
    stored nowhere), public key pushed as `--recipient`; the decrypt drill
    (fetch locally with `--identity`, paste the JSON) keeps a persistent banner
    until it passes. Dash never holds a private key.
  - One policy: interval, simple/standard GFS retention presets, volumes with a
    size cap, written per server via the CLI's change-only-what-you-name flags.
    Dash owns a push heartbeat monitor per server (missed-cron alerting);
    per-app exclude (audited) and per-app volume schedule overrides ride the
    same surface.
  - Automatic restore tests: one dash-owned weekly verification per covered
    database (`accessory verify-backup` into a scratch container), in a
    configurable UTC off-hours window (default 02:00–06:00), one per server at a
    time, with a free RAM/disk SSH precheck. Records are created only for
    covered databases, retired only on a complete fresh read; history kept.
  - Backup incidents (`source: "backup"`, one per cause): target unreachable,
    missing tooling, failing, stale (2× interval), uncovered database appeared;
    first-read adoption as one summary; recovery only on fresh evidence;
    retirement when a subject leaves a complete read. Missed heartbeats alert
    through the per-server push monitor (`source: "heartbeat"`).
  - Hand-written-job retirement report (P2.9, report-only): per-database
    readiness from two consecutive watched good policy backups + a green restore
    test of the same engine kind; the known job names listed as the owner's
    checklist. Removal stays a human action on the server.
  - The rebuild runbook `docs/rebuild-dash.md` for Dash itself, unit-checked
    against the paths the code writes; the page reports the self-backup CLI gap
    (no arbitrary-path targets, no owner-key-only encryption) instead of
    pretending.
- Cleanup page and `GET /api/cleanup`: what is left over on each server from the
  CLI's read-only `server audit` — findings with evidence, sizes and severities,
  per-server checks and disk attribution. A run is a durable operation (one per
  server at a time); the cached report keeps its age, and a failed run keeps the
  last report. Partially-audited and unreachable servers are labelled as
  themselves, never clean. Needs teploy v0.1.53 on the dashboard host
  (capability `server-audit-v1`).
- Fix buttons for findings whose remedy the CLI names, re-rendered from dash's
  operation allowlist (`route remove`, `releases prune --keep N`, `server prune
  --build-cache --dangling`, orphan `remove`, `redeploy`) and run as durable
  operations; destructive remedies require the finding's app or subject name
  typed exactly, enforced server-side. After a remedy finishes, the page offers
  the re-audit that proves the finding cleared.
- Dismissals per finding id (audited): hidden from the default view but counted
  and reversible; a fresh full audit that no longer reports the finding retires
  the dismissal, while a partial or unreachable audit retires nothing.
- Contracts corpus pin bumped to teploy-cli `ebf8aa4` (rev 19); the
  server-audit envelope is registered in the contracts validator and decoded
  by the corpus tests.
- Domains page and `GET /api/domains`: one row per domain joining DNS, route,
  certificate and HTTP, naming the first failing layer with its evidence and age.
- Certificate expiry from each HTTPS monitor's TLS handshake, stored on the check
  result.
- Domain incidents (`source: "domain"`): DNS no record or wrong target, certificate
  under 14 days, certificate obtain failing over 24 hours. First results from a
  server become one summary. Needs teploy v0.1.51 on the dashboard host for the
  DNS and obtain rules; certificate expiry works without it.

### Changed

- README: the groups overlay wording — dash is the only writer of
  `~/.teploy/groups.json`; the CLI no longer reads or writes it (the stale
  "format-compatible with the CLI's embedded UI" claim removed).
- Fleet monitors: a new host-ingress domain gets a monitor only when a route on any
  server names it or public DNS answers. Existing monitors are never retired on the
  absence of that evidence.
- `teploy.yml` no longer carries the `dash.example.com` placeholder.
- The image bundles teploy CLI v0.1.51 (from v0.1.49): database engine
  detection, safe in-place restore, dump verification, cron checks and
  `domains check`.
- The database-actions inventory no longer claims data restore is unsupported:
  it is wired from the app's Backups tab as a durable operation, and since
  v0.1.52 names one database and takes the automatic safety backup (the entry,
  its blast radius and the CLI-surface test were updated together).

### Added
- **Alerts v2.** Monitors alert on incidents, not on every flip: per-monitor
  `fail_after` (default 3) / `recover_after` (default 2) / optional
  `repeat_every`, one incident per outage with a stable `incident_id`,
  recovery only when the open was sent, streaks restored from history after
  a restart, and `incident_recovered` reason `retired` when a monitor is
  deleted, disabled or retired by the fleet reconciler. Restore tests and
  failed app operations (deploy, redeploy, rollback, backup, restore,
  maintenance) use the same incident kinds.
- Alerts are held, not dropped, during maintenance, deploy locks and
  queued/running Dash operations (+2 min grace, capped at grace + the
  operation's timeout), and during silences (all / app / monitor, with
  expiry, audited). Held alerts appear as `suppressed` in delivery history.
- Event v2 wire contract (`version: 2`, `kind`, `reason`, `incident_id`,
  `rule_id`, `service`, `app`, `server`, `target`, `severity`, `started_at`,
  `duration_s`, `consecutive_failures`, `url`) with `X-Teploy-Delivery` and
  `X-Teploy-Event-Version: 2` headers; v1 fields unchanged.
- Channel list: ntfy, Slack-compatible, Discord, email, signed webhook and
  Teploy Ship, each with kind filters, Send test, and delivery health; a
  dead-lettering channel is reported through the others; `Retry-After` is
  honored. First channel sends one summary of what is already down.
- Public Dash URL setting (links in alerts), Dash self-heartbeat to external
  dead-man receivers, push/heartbeat monitor type (`/api/push/<token>`),
  open incidents and maintenance on the public status page, and an audit
  trail (`GET /api/audit`).

### Changed
- `notifications.json` migrates from `webhook_url` / `smtp_*` to a channel
  list (ids `webhook` / `email`, pending deliveries replay unchanged). The
  legacy flat fields are still accepted by `POST /api/notifications`.
- Fleet monitors record the server their app runs on.
- On the first start after the upgrade, monitors and restore tests that are
  already failing are announced as one summary (human channels) plus one
  open each for webhook/Ship channels, not one alert per monitor.
- Alert channels and heartbeat pings use the monitor network guard
  (internal addresses need the channel's or receiver's allow-internal
  option); heartbeat pings no longer follow redirects.

### Fixed
- The generic webhook field no longer suggests a Slack URL (Slack needs its
  own channel type).
- Monitor delivery status was never shown in production: the server now
  receives the alert outbox.
- Editing a fleet monitor in the UI no longer drops its app ownership.
- Delivery errors no longer expose credential URLs (Slack/Discord/ntfy
  paths, heartbeat ping URLs); stored errors are scrubbed when served.

## [0.1.21] - 2026-10-31

### Changed
- The image bundles teploy CLI v0.1.49 (from v0.1.47): app containers
  receive `TEPLOY_APP`, `TEPLOY_PROCESS` and `TEPLOY_VERSION`; `runtime:`,
  `security_opt:` and `tls.http_only` in `teploy.yml`; every `docker build`
  pulls current base images (`--pull`, v0.1.48) and v0.1.48's SSH and
  non-root setup fixes.

## [0.1.20] - 2026-10-21

### Changed
- The image bundles teploy CLI v0.1.47: removing an app (Remove, or
  Remove with purge) also deletes the server-built images of its removed
  containers instead of leaving them on disk.

## [0.1.19] - 2026-10-21

### Fixed
- **Git sources on self-hosted forges whose SSH and HTTP ports differ.**
  Repository matching now compares host and path only, ignoring scheme
  and port, so an app that pulls over `ssh://git@forge:2222/o/r` and a
  forge credential or webhook source spelled `http://forge:3000/o/r`
  name the same repository. Before, a push woke nothing ("no app is
  connected to this repository yet") and the credential was refused as a
  scope mismatch, leaving only the server's poll timer to deploy. The
  forge API is still derived from the credential's HTTP(S) address,
  never from an SSH port. Source IDs and webhook URLs are unchanged.
- The image bundles teploy CLI v0.1.46.

## [0.1.18] - 2026-10-20

### Added
- **App screens backed by the CLI (server-authority plan D5), with teploy
  CLI v0.1.45 or later on the dashboard host.** Dash stores none of it —
  every read and write shells out to the teploy CLI, and each surface
  hides itself when the CLI lacks its capability token (an older CLI shows
  exactly the previous page). The Environment tab gains a **Secrets**
  section: key names only (values are write-only, set through stdin and
  never displayed), managed with `teploy secret`. A **Releases** tab lists
  the server's release records with per-release **Roll back to** (the
  durable rollback operation with `--to`). The General tab gains
  **Redeploy**: a fresh deploy of the current release's recorded image
  with env and secrets re-resolved — how a secret or env edit reaches
  containers. A **Backups** tab lists the app's backups and schedules from
  the server's stored backup target, with **Run now** and a **Restore**
  behind a typed type-the-app-name confirmation (both durable operations;
  restore quiesces the app under its lock). Settings gains **Web
  instances** (replicas) and **Domain** fields for Teploy-home apps — they
  patch the YAML draft and save through the existing compare-and-swap PUT,
  and the CLI's refusal reason surfaces when replicas cannot apply. A repo
  home renders read-only with an "edit in repository" link. The Teploy-home
  env guard now defers to the CLI's own (`env-config-guard-v1`): with the
  token present, dash surfaces the CLI's refusal instead of pre-checking
  locally. Restore tests no longer store a bucket/region — the CLI
  resolves the server's backup target at run time. The bundled CLI is
  v0.1.45 and the contracts pin moves to corpus rev 16.
- **Git sources: webhook relay and the Repository tab (server-authority
  D4, dash side; needs teploy CLI v0.1.45+).** One admitter lives on the
  app's server — `teploy trigger poll` — and webhooks only wake it. A
  verified webhook push for a repository an app builds from runs
  `trigger poll --app X --host S --now --hint <sha>` and records the
  result in the delivery ledger; dash re-verifies its repo→app routing
  hints (a persisted cache under the data dir) against the app's config
  before relaying, answers the forge immediately (a deploying poll
  outlives forge timeouts) and never retries a failed wake — the server's
  own poll timer catches missed ones. The app page gains a **Repository**
  tab (Teploy-home apps only, hidden with an older CLI): connect a
  repository (forge + clone URL + branch; the `git:` block is written
  through the same CAS settings editor path), generate the read-only
  deploy key on the server, pin the forge host key with the fingerprints
  shown for confirmation, and register deploy key + push webhook through
  the forge API when dash holds a credential for that repository —
  otherwise both are shown for the operator to paste (webhook secret to
  credential-admins only). Status shows the connected repository, the
  credential fingerprint, the last admitted commit (watermark) and the
  last deployed release, with retry (typed-ref watermark reset is offered
  when a force-push is refused) and the doctor-driven offer to install
  the server poll timer.

### Removed
- **Dash-side manifest store and the D04 webhook-to-deploy pipeline.**
  App settings live on the app's server (Settings tab / `teploy config`),
  so dash no longer keeps its own copy: `internal/manifest`, the
  `/api/manifests` routes (which now answer 410 Gone with the reason), the
  git/dash-managed authority UI, the lifecycle/watermark/preview-ownership
  state, the dash-host git checkout and the `manifest_apply` /
  `manifest_plan` / `manifest_validate` / `source_preview*` operation kinds
  are gone. Historical operations with those kinds still load and display;
  retrying or re-submitting them is refused cleanly. Registered git
  sources keep their forge plumbing: credentials, webhook secrets,
  per-forge signature checks, the durable delivery ledger and push-head
  verification. Authenticated pushes are relayed to the connected app's
  server-side poll (see the git-sources entry above) and recorded as
  ignored — "no app is connected to this repository yet" — when no app
  builds from the repository; pull request deliveries answer that
  previews are not supported yet. The Teploy-home env guard moved to
  `internal/configkeys` and now also guards the MCP env tools.

## [0.1.17] - 2026-10-19

### Added
- **App settings stored on the server (server-authority spec P2).** With
  teploy CLI v0.1.44 or later on the dashboard host, the app page gains a
  **Settings** tab: the app's `teploy.yml` as kept on its own server. Apps
  whose settings are edited in Teploy save with a compare-and-swap; when
  someone else saved first (laptop, CI, another tab) the CLI merges both
  edits key by key and overlapping keys are listed for review. History and
  restore, "Deploy pending changes" when the settings moved past the
  running release, "Make editable here" for folder apps (adopted only if
  the server records reproduce the running release exactly) and "Edit in
  a folder instead". Users without the admin capability save with
  `--unprivileged`, so a change that reaches the host is refused.
- **Activity tab: the app's server event history**, written by every
  client (CLI, CI, this dashboard). Dashboard actions are recorded as
  `dash:<user>`, linked to the operation id.
- **New apps start in Teploy home.** The Deploy form and template install
  create new apps with their settings on the server (template passwords go
  to the server's secret store, never into the settings). Existing apps
  redeploy as before.
- **Env guard for Teploy-home apps.** Keys an app's settings declare are
  edited in Settings; the Environment tab marks them and the server refuses
  a competing edit.

- **Readiness vs liveness probes.** `/healthz` is a cheap, dependency-free
  liveness check (a broken store should not get the process restarted);
  `/readyz` reports readiness: store reachable, operation persistence not
  degraded. `ready` and `degraded` answer 200 (degraded stays in rotation);
  `unavailable` answers 503. Both are auth-exempt like `/api/health`.
- **Per-target admission budgets for operations.**
  `TEPLOY_DASH_MAX_QUEUED_PER_TARGET` (default 50) bounds how many
  non-finished operations may be queued for one server+app; excess
  enqueues are rejected with HTTP 429 instead of accumulating without
  bound. Idempotent replays of already-queued work still pass.
- **Actor attribution on operations.** Every operation records which
  principal admitted it (`actor.kind` local/sso/mcp, subject, label):
  the signed-in session for UI paths, the API token for MCP mutations, the
  retrying principal for retries. Records predating the field carry none.
- **SSO admin surface.** Settings > SSO lists the OIDC identities known to
  the instance (with last sign-in) and revokes their sessions; Settings >
  Users gains a per-account "Revoke sessions" button.

### Changed
- The theme choice moved from the header to Settings > Account
  (Appearance: Dark / Light, saved in this browser).
- On phones, pinned links appear at the top of the menu as named icon
  tiles; the header itself stays brand + menu button.
- Bundled teploy CLI v0.1.44; contracts corpus pin rev 13.
- The CLI capability cache follows the binary's identity: an upgraded or
  replaced `teploy` is re-probed at once instead of after the TTL.
- Every server-authority surface (Settings, Activity, Teploy-home deploys) is capability-gated: with an older CLI on PATH the
  page is unchanged and the new API routes answer 501 with the remedy.
- **Machine interface 2: `server list` decode adopts the CLI's envelope.**
  Dash's maximum supported machine interface is now 2 (MI 2 = MI 1 plus
  the CLI's `server list --json` reshape; no other envelope changed). The
  server-list decode paths (fleet discovery, the registry edit read, the
  settings servers list) decode BOTH wire shapes during the transition —
  the new `{machine_interface, servers[], observed_at}` envelope and the
  legacy bare map-of-servers from a pre-reshape CLI (logged once as
  deprecated) — so dash works against either CLI, while an envelope from
  a newer interface is still refused centrally with the upgrade remedy.
  `/api/config/servers` keeps answering the bare-map shape the frontend
  reads; the CLI's wire reshape is normalized away from the UI. Contracts
  pin: teploy-cli `28a42cd`, corpus rev 4.
- **Log streaming is SSE-only.** The log viewer path moved from the
  hand-written WebSocket to `/api/logs/{server}/{app}`
  (`text/event-stream`, same-origin enforced); the hand-rolled RFC 6455
  upgrade and frame writer are deleted.
- **Shutdown joins in-flight operations.** On SIGTERM/SIGINT the operation
  runners drain first (bounded); stragglers are force-canceled with an
  explicit "dashboard shutting down" attribution and a persist grace,
  replacing the hard exit that killed CLI children mid-write.
- **Accessibility basics.** Explicit label associations on the main flows'
  controls, focus traps + Escape on the monitor and restore-test dialogs,
  a skip link, current-page landmarks on the nav, and live regions on
  toasts and login/setup errors. Verified statically; a real-browser
  accessibility pass remains open.
- **The bundled teploy CLI is 0.1.33, up from 0.1.27.** Dash shells out to this
  binary for deploy and rollback, so every app it deployed inherited the
  stale-image defect: teploy skipped `docker pull` whenever the tag was already
  in the server's local cache, so a floating tag could serve a build older than
  the registry indefinitely. 0.1.33 pulls every tag and caches only
  digest-pinned references. 0.1.31 had already fixed the other half, taking
  the deploy version from the image rather than git HEAD — at 0.1.27 dash had
  neither, which is the combination that let a container be named after a
  commit its image did not contain.

- Bundled teploy CLI v0.1.42 (secret-free release metadata, locked
  `remove`, `setup` installs git and python3).
- Contracts CI pin moves from corpus rev 7 to rev 9.

### Fixed
- **SSH fallback reads `state.json`.** When the CLI on PATH predates
  `app list --json`, the fleet falls back to reading state over SSH. That
  path read only the legacy `state` file, so every app deployed by a current
  CLI (which writes `state.json`) was missing. It now prefers `state.json`,
  falls back to `state`, refuses state schemas newer than it understands, and
  skips tombstoned apps. The local (no servers configured) reader does the
  same.
- **Fleet decode honours the machine-interface gate.** `app list --json`
  envelopes from a newer CLI machine interface are refused instead of
  mapped, like every other CLI decode.
- **`groups.json` is locked across processes.** A second dash (or a local
  dev instance) sharing the same home can no longer lose a group edit.

## [0.1.16] - 2026-08-26

### Fixed
- **The alert email sanitized its Subject but not its From/To.** `sanitizeHeader`
  was applied to the monitor name and status, so a monitor name could not inject
  SMTP headers — but `email_from` and `email_to` went into the message raw, so a
  configured address containing CR/LF could append its own headers (an unwanted
  `Bcc:`, for one). Message construction moved into `buildEmailMessage`, which
  sanitizes all three, with a test that asserts an injected `Bcc` does not reach
  the header block.
- **The monitor starter logged how many monitors existed, not how many started.**
  `Started %d monitors` printed `len(monitors)`, so monitors skipped for being
  disabled or unparseable were still counted as running — the log said everything
  was up while it was not. It now counts actual starts.
- **The on-demand health endpoint could not be tested, and CI had been red for
  ten days because of it.** The `action == "health"` branch called package-level
  `cli.IsInstalled()` while the server already carries the injectable
  `s.cliInstalled`, which defaults to exactly that function. Production behaviour
  is unchanged; the difference is that a test can inject it again, so
  `TestAppHealth_UnhealthyVerdictIsAnAnswerNotAnError` no longer passes only on a
  machine that happens to have the teploy binary on PATH. Four consecutive red CI
  runs (2026-08-16 to 2026-08-26), including the run on the deployed tag.

## [0.1.15] - 2026-08-21

### Added
- **Browse the shared KV store.** List by pattern, reveal, set and remove.
  Reads need viewer, writes need editor, and it answers inline the way drift
  and stats do — nothing is cached. Keys the panel cannot safely drive
  (flag-shaped values and the like) render read-only rather than offering
  buttons that fail.
- **On-demand health check in the app detail view**, so an operator can ask
  rather than wait for the next deploy to tell them.
### Added
- Settings → Links: manage shortcuts to the services you run alongside Teploy
  (Forgejo, Proxmox, TrueNAS, anything with a URL) and pin any of them to the
  header. Pinned links render as icons on the right of every page, mirroring
  the product switcher on the left. It is the same list the Home grid uses, so
  a service is only ever entered once.
- Home and Header are independent per link, so a link can be a header icon
  without taking a card on Home.
- Links can carry a monochrome glyph (SVG path data, 24×24) drawn in the
  current text colour, so the mark is white on the dark theme and black on the
  light one. GitHub and X have built-in glyphs; their favicons are a dark tile
  that reads as a smudge in a dark header. Links without a glyph can instead be
  flagged as having a dark favicon, which puts it on a light tile.

### Fixed
- **The Accessories table was blank.** Pre-existing, and a plain seam bug: the
  frontend read Go-cased fields while the endpoint passed the CLI's lowercase
  JSON straight through. A route-wiring test was added after finding five
  validation tests that still passed with the dispatch arms deleted.
- Bootstrap token handling, graceful shutdown, copy-on-write users, and a
  dial-time SSRF policy.
- A delegated command now reports how long it actually ran before timing out.
- A pinned app's icon is found when it only ships an SVG.
- No ghost SSO button, and no login form when already signed in; the fleet is
  served stale-first. Siblings survive the fleet cache expiring.
- The sign-in wordmark reads Teploy Dash.

### Changed
- **The bundled CLI is pinned to v0.1.27.** v0.1.26 was the first release where
  `bind:` and `ingress: host` deploys survive their own health check — the
  path Dash itself is deployed on.
- **The source-build Dockerfile now verifies the CLI download** against
  `checksums.txt` before extracting, and resolves the architecture through
  `TARGETARCH`. It previously piped a hardcoded amd64 URL straight into tar —
  and since that is the file a source build uses, the image actually running in
  production was the one not checking what it executed.
  `Dockerfile.goreleaser` already did this.
- The header right side is now icons only: the `self-hosted` label is gone (it
  said nothing an operator didn't know), and the theme toggle is a sun/moon
  button instead of a "Light"/"Dark" word.
- Sign out moved from the header to Settings → Account, alongside the password
  form. It is a rare action that did not need permanent chrome.

## [0.1.14] - 2026-07-28

### Changed
- The header leads with the Teploy wordmark and lets the product switcher
  carry the product name, replacing the combined "TEPLOY DASH" that showed the
  brand twice. The same shape now applies across Dash, Observe and Ship.
- The switcher always names the current product. It previously hid entirely
  when no sibling was configured, so a single-product install had nothing
  saying which dashboard it was; it becomes a dropdown only when a sibling is
  reachable.

### Fixed
- The fleet cache is warmed once at startup. Sibling discovery reads that
  cache, so after a restart the switcher was missing entries until someone
  opened the deployments page — and that first fleet view paid a full SSH
  sweep.

## [0.1.13] - 2026-07-27

### Added
- The cross-product switcher configures itself. Dash already SSH-polls the
  whole fleet, so it now infers a sibling's URL from deploy state instead of
  requiring `TEPLOY_NAV_*` for products it can already see: a running app named
  `observe`/`ship` resolves to its domain, or to the server address and
  published port when it has none (`ingress: host`). Reserved documentation
  domains (RFC 2606, e.g. the sample `observe.example.com`) are rejected rather
  than linked. `TEPLOY_NAV_*` still wins when set — needed for a product behind
  a tunnel, or on a server this dashboard does not manage — and discovery reads
  only the warm fleet cache, so `/api/nav` never triggers an SSH sweep.

## [0.1.12] - 2026-07-27

### Fixed
- The Docker image could not build. An unused indirect dependency had drifted
  up to a version requiring Go 1.25 while the builder image, CI, and `go.mod`
  all target 1.23/1.24, so `go mod download` — which resolves the whole module
  graph, not just what is imported — failed before compilation started. The
  release binaries were unaffected (they only build what is imported), which
  is why this surfaced at deploy time rather than in CI. Pinned back to a
  version the rest of the graph agrees with; every module now builds on 1.24.

## [0.1.11] - 2026-07-27

### Added
- Teams and roles. The dashboard is now multi-user with three roles matching
  teploy-observe: **admin** (manage users, servers, settings, secrets),
  **editor** (deploy, rollback, restart, env — the operator), and **viewer**
  (read-only). Accounts live in `users.json`; a pre-existing single-user
  `auth.json` migrates automatically to the first admin on upgrade, so no
  operator is locked out. Roles are enforced at the auth gate: reads need
  viewer, mutations need editor, and account/credential/config routes need
  admin — failing closed so an unclassified mutating route requires editor,
  never viewer. Login now takes a username; manage accounts in Settings →
  Users (admin only). Changing a password or role signs out only that user's
  sessions, and the last admin can't be demoted or removed. RBAC governs the
  dashboard surface — it does not replace server SSH controls.
- Single sign-on (OIDC). Dash can act as an OpenID Connect relying party:
  delegate login to your own identity provider (Okta, Azure AD/Entra, Google
  Workspace, Keycloak, Authentik — "generic OIDC") or to Teploy Platform acting
  as the IdP for Cloud. The IdP authenticates the user; Dash verifies the signed
  ID token (authorization-code flow with PKCE, state, and nonce) and maps a
  claim to the same admin/editor/viewer roles — a `teploy_role` claim wins,
  otherwise a group claim is matched to configured admin/editor/viewer groups,
  otherwise a configurable default (viewer). The role is re-read from the token
  on every login, so the IdP stays authoritative. Local username/password login
  remains as the break-glass path so a down IdP never locks everyone out. Enable
  by setting `TEPLOY_DASH_OIDC_ISSUER` and `TEPLOY_DASH_OIDC_CLIENT_ID` (see the
  README for the full variable list); when unset, SSO is simply absent.
- Cross-product dashboard switcher. A top-left dropdown lets you jump between the
  deployed Teploy dashboards — Dash, Observe, and Ship. Configure the sibling
  URLs with `TEPLOY_NAV_OBSERVE_URL` and `TEPLOY_NAV_SHIP_URL` (the same env
  convention is used by all three products); the switcher only appears once at
  least one sibling URL is set. Served from `/api/nav`.
- Resources panel on the app detail page: CPU, memory, network and block I/O
  per container, from the CLI's `stats`. Stopped containers are filtered out —
  Docker reports them as all-zero rather than omitting them, and showing those
  rows reads as "idle" when the app is actually down.

## [0.1.10] - 2026-07-26

### Added
- MCP server at `POST /api/mcp` — Claude Code, Cursor, or any MCP client can
  inspect the fleet and run deploy actions. 17 curated tools; reads come from
  the CLI's server state files, actions delegate to the CLI binary exactly
  like the dashboard buttons, so MCP joins the existing single source of
  truth (no second state store, nothing to desync). Bearer-token auth with
  per-token read-only mode; tokens managed in Settings → MCP (hashed at rest,
  immediate revocation). Env values never cross the MCP boundary — the env
  tool returns variable names only.
- Operation center. Every long-running action — deploy, rollback, remove,
  start/stop/restart, maintenance, template install — is now a recorded
  operation with live streaming logs (SSE, resumable via `Last-Event-ID`),
  cancel, retry, and an idempotency key, instead of a request that blocks
  until the CLI exits. `/operations` lists them; `/operations/{id}` follows
  one live.
- URL routing. Every view has a real path (`/deployments`, `/servers/{name}`,
  `/operations/{id}`), so views are linkable and reloadable and browser
  back/forward work.
- Drift panel on the app detail page: whether an app's live containers still
  match what was deployed — a container stopped outside teploy, or an old
  version still running.
- `Remove` action to retire an app: stops and removes its containers, removes
  its proxy route, and deletes its deploy state. Volumes and accessory data
  are always preserved from the dashboard.
- Signed monitor webhook deliveries.
- `main.version` ldflags variables now exist, so goreleaser's long-standing
  `-X main.version=...` flags actually take effect (previously a silent
  no-op).

### Fixed
- The Deployments view rendered empty for every app: the fleet API serialized
  each app as `app` while the frontend keyed on `name`, so the list crashed on
  undefined keys and showed nothing.
- The Servers page rendered blank, and server detail was a black page — the
  server list is a map, not an array, and the detail endpoint delegated to a
  CLI verb that reports app state rather than host metrics.
- Static-site deploys (no container by design) were reported as stopped.
- CLI-delegated actions (logs, deploys, env, rollback) failed from the
  container: the server alias was passed where a raw host was expected, and
  accepted host keys were never recorded for the bundled CLI to reuse.
- Replaced inline spinners with a top-loading bar in the header.

## [0.1.9] - 2026-07-15

### Added
- Public, unauthenticated status page (opt-in via `--public-status` / `TEPLOY_DASH_PUBLIC_STATUS`; off by default, `/status` and `/api/status` 404 when disabled). Deliberately leaks only monitor name, up/down, and 24h uptime % — no target/IP, server name, response body, or config. `/api/status` is JSON; `/status` is a self-contained, theme-aware HTML page (inline CSS/JS, no external deps) that polls it every 30s and shows an overall operational/degraded/down banner.
- Restore Tests: scheduled proof that backups actually restore. A test picks a server/app/accessory plus an S3 bucket and an interval; the runner shells out to the CLI's `verify-backup` verb (server-state driven, no `teploy.yml` needed) on that schedule and persists only the last structured result. Runs hourly/daily, not on the 10s poll cadence — a test that already ran doesn't re-run on a dash restart. Fail/recovery alerts ride the existing webhook/SMTP dispatcher. New API (`/api/restore-tests` list/upsert, `/{id}` get/delete, `/{id}/run`) and a Restore Tests UI page matching the monitors pattern.

### Fixed
- Bundled `teploy` CLI in the Docker image bumped to v0.1.20 (from v0.1.17, itself a fix for a build that wasn't statically linked and couldn't run in the Alpine-based image at all). Brings every delegate-to-CLI action several releases of fixes forward, plus the new CLI features (OpenBao secrets, plan/drift/heal, kv, staged rollout, and more) within reach of the dashboard's delegate model.

## [0.1.8] - 2026-06-08

### Security
- Closed a path-traversal / arbitrary-file-write RCE: a client-controlled monitor ID flowed into `filepath.Join` in the file store, so `POST /api/monitors` with an id like `../../etc/cron.d/x` wrote a file as root. Monitor IDs are now validated (`^[A-Za-z0-9_-]+$`) at the HTTP boundary and in every file-store method.
- Adding/editing a server in the dashboard no longer silently downgrades a non-root fleet server to root — the SSH user/role are forwarded to the CLI and preserved on edit. A same-name edit upserts in place (no remove+add) so the server's `tags`/`vpn_ip` survive. (Requires teploy-cli with lowercase JSON keys on `server list --json` so the frontend reads the real user — shipped alongside.)
- The per-IP auth backoff honors `X-Forwarded-For` only from a configured `TEPLOY_DASH_TRUSTED_PROXY`, so running behind Caddy keys the lockout on the real client instead of self-DoSing every client onto the proxy IP; the failed-attempt map also prunes expired entries.
- Hardened the auth layer: per-source-IP failed-attempt backoff (brute-force resistance), a same-origin requirement on state-changing requests (CSRF defense — browsers auto-send Basic-Auth creds cross-origin), an Origin check on the WS/SSE log stream, and the registry password is now passed to the CLI over stdin instead of on the argv.
- Monitor HTTP method is constrained to GET/HEAD/POST so a monitor can't be configured to issue a destructive verb. (A private-range SSRF block was deliberately not added — monitoring internal/Tailscale fleet hosts is the primary use case and monitors are admin-only, so a default block would break legitimate monitoring.)

### Fixed
- Nucleus check inserts no longer collide: the primary key was `time.Now().UnixNano()`, which dropped a check (PK violation) when two landed in the same nanosecond. Now a random int64 (no schema migration).
- Monitor lifecycle no longer leaks: deleting a monitor now stops its checker (ticker + goroutine were leaking and kept recording checks for a deleted monitor); `startMonitor` is idempotent (tears down any existing checker first); and the last-known status is seeded from the most recent persisted check so the first check after a restart can fire a transition alert.
- CLI delegate and Nucleus calls are now time-bounded (a 20m ceiling on the CLI subprocess, 10s per Nucleus query) so a hung SSH session or wedged DB can't block a request forever.
- Fleet failures are surfaced in the logs instead of being silently swallowed — a server-list failure or a fully-unreachable fleet previously rendered as an empty success with no clue why.
- File-store check history is returned newest-first, matching the Nucleus store's ordering, so the UI shows a consistent order regardless of backend.
- Response times are rendered in milliseconds — they were shown as raw `time.Duration` nanoseconds (~1e6× too large) in the monitor list, detail, history, and test-now views.
- Monitor `POST` now validates id / type / target (subsumed by the ID validation above).

### Docs
- CLAUDE.md: dropped the "incident tracking" overclaim (not implemented); main.go cleanup comment references `store.RetentionDays` rather than a stale "30 days".

### Fixed (earlier this session)
- Failed delegated CLI actions are now reported as failures in the dashboard. `internal/cli.Run` returns a nil Go error when the `teploy` binary runs but exits non-zero, and the frontend only treats a top-level `error` field as a failure — so a failed deploy, rollback, env set/unset, lock/unlock, maintenance toggle, template install, accessory action, or server/registry add/remove was being shown to the user as success. Mutating commands now go through `cli.RunChecked`, which turns a non-zero exit (with the CLI's stderr) into a Go error so failures flow through the handlers' normal error path.
- Webhook alert delivery now uses a 10s HTTP client timeout. Each alert is sent in its own goroutine via the default (timeout-less) client, so a hanging webhook endpoint leaked a goroutine on every monitor state transition.
- Email alerts strip CR/LF from the monitor name and status before placing them in the `Subject` header, preventing SMTP header injection from a crafted monitor name.
- Uptime monitor HTTP checks now honor the configured `expected_status`. Previously the value was read and stored but never compared — the check always judged up/down purely on the 2xx/3xx range, so a monitor set to expect e.g. `401` or `201` was scored incorrectly. When `expected_status` is unset, any 2xx/3xx is still treated as up. The create-monitor form's Expected Status field is now genuinely optional (blank = lenient 2xx/3xx); it previously defaulted to `200` and always sent it, which would have made the new exact-match logic flag healthy `301`/`302`/`204` responses as down.
- Uptime monitor HTTP checks now apply the monitor's own `timeout` (via a per-request context). Previously HTTP used a hardcoded 10s client timeout and the configured per-monitor timeout was silently ignored (only TCP/ping checks respected it).
- SSH fleet connections now bound the SSH handshake with a deadline (TCP dial was already bounded). `ssh.NewClientConn` honors neither the context nor any built-in timeout, so a host that accepted TCP but stalled the handshake could hang a fleet refresh indefinitely; the multi-server refresh also now has an overall 30s cap.
- App actions (lock, unlock, maintenance on/off, status, deploy log, accessory list/stop/start/logs) and the env editor now pass `--app <name>` and `--user` to the `teploy` CLI. Previously the delegate wrappers and the direct `cli.Run` calls in the server passed only `--host`, so the CLI fell back to loading a `teploy.yml` from its working directory — which dash doesn't have — and the action either failed or targeted the wrong app; and without `--user`, actions only worked on root-SSH servers. Now the server's configured SSH user is threaded through and the target app is resolved from server state. Requires teploy-cli ≥ the build that added `--app` to these commands. (The `env` wrappers also gained `--app`; the `logs` wrapper now uses the correct `--tail` flag instead of the nonexistent `--lines`.) Accessory `upgrade/backup/restore` are rejected with a clear message from the dashboard — they need `teploy.yml` on the CLI host.
- `internal/state` was reading `state.json` with JSON parsing, but the teploy CLI writes `state` (no extension) in key=value format. The mismatch silently returned an empty app list from the local-state fallback path in `server.collectFleetApps` (taken when no servers are configured). Rewritten to read the actual filename + format. Includes table-test coverage for the parser + ListApps directory walk.

### Changed
- Consolidated the CLI state-file parser. The key=value `state` format is now parsed in one place (`internal/state.Parse`); the SSH fleet path (`internal/remote`) feeds the file bytes through it instead of re-implementing the parse, so the two paths can't drift on key names.

### Docs
- README: removed stale Scoop install section (Windows + scoops were dropped from goreleaser in v0.1.1 — the install command was broken). Replaced with Docker / GHCR multi-arch instructions for the image we DO publish.
- README: corrected the uptime monitoring section to match the code — dropped "expected body substring" (never implemented), "p50 / p95 latency" and "last incident" (stats compute uptime %, check counts, and average response time only), and fixed the file-store retention from "30 days" to the actual 7 days. Clarified that ping is a TCP-connect probe (no raw ICMP) and how expected-status matching works.
- README: monitor section now mentions ping (in addition to HTTP / TCP) — matches what `internal/monitor/monitor.go` actually supports.
- CLAUDE.md: corrected project structure (fleet aggregator lives in `internal/server/server.go`, not in a separate `internal/fleet/aggregator.go` that doesn't exist); corrected multi-server description (dash SSH-polls each server in CLI's servers.yml, NOT peer-pull between teploy-dash instances).
- `.goreleaser.yaml`: annotated the goreleaser 2.x deprecations we can't cleanly migrate yet (`brews:`, `dockers:` / `docker_manifests:` → `dockers_v2:`).

### Chore
- `go mod tidy` added missing `golang.org/x/sys v0.28.0 // indirect`.
- New: `/_internal/` and `/INTERNAL.md` gitignored at repo root for private notes (per the cross-repo `internal_notes_convention`).

## v0.1.1 — 2026-05-26

### Added
- Multi-arch Docker images on GHCR: `ghcr.io/useteploy/teploy-dash:{version,latest}` (linux/amd64 + linux/arm64), built via `Dockerfile.goreleaser` + `docker_manifests`. Binary embeds the SPA, so the runtime image is minimal.

### Removed
- Windows builds + Scoop bucket entry. teploy-dash is server-shaped tooling; Windows users were a small slice and the Scoop manifest was a maintenance tax. Linux + macOS only going forward.

## v0.1.0 — 2026-05-10

Initial public release. Ships as a single static Go binary with the
Alpine.js SPA embedded via `//go:embed`.

### Added
- Multi-server fleet view: app list, per-app status, image hashes,
  domain, container state, with a 60-second cache so the page doesn't
  SSH on every refresh.
- App actions: stop / start / restart over SSH; deploy, rollback, lock,
  unlock, maintenance on/off, registry login, env get / set / unset,
  template install — all delegated to the `teploy` CLI so the CLI stays
  the source of truth.
- WebSocket log tailing per app, with SSE fallback for clients that
  don't speak WS.
- Persistent groups + projects (`~/.teploy/groups.json`,
  format-compatible with the CLI's embedded UI).
- Umbrel-style template catalog (`/api/templates`,
  `/api/templates/install`).
- Uptime monitoring: HTTP and TCP checks with per-monitor interval,
  timeout, expected status, expected body substring; 24-hour stats
  (uptime %, p50 / p95 latency); manual "test now"; daily cleanup of
  the file-store keeps 30 days of checks.
- Storage: Nucleus over pgwire (preferred) or rolling JSONL files
  (fallback). Falls back automatically on Nucleus connect failure.
- Webhook + SMTP alerts on monitor state transitions, persisted to
  `~/.teploy/notifications.json` and reloadable from the UI without
  restart.
- HTTP Basic Auth middleware on every route except `/api/health`,
  using `subtle.ConstantTimeCompare` to prevent timing attacks.
  Refuses to start without `TEPLOY_DASH_PASSWORD` unless `--no-auth`
  is set.
- Release infra: `goreleaser` for cross-compiled tarballs (linux,
  darwin, windows × amd64, arm64), Homebrew tap, Scoop bucket, POSIX
  install script with optional systemd unit.
