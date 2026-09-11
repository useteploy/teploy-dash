# Open audit items

Unresolved findings for this repository from the ChatGPT-led audit series (2026-09-09 through 2026-09-11, passes 1-5; register: teploy-neutron-lullmail expanded audit). Every P0/P1 finding has been fixed and verified; the items below are the remaining P2/P3 tail plus one item needing validation. Fields are quoted from the audit register; line references point at the review commits listed per item where recorded.

Open items: 7 P2 (7 total)

## useteploy__teploy-dash-02 - P2 - Open

**Failure writing the initial event leaves a queued operation with no worker**

- Kind: Confirmed from source
- Evidence: enqueue persists the operation and idempotency mapping before appendEventLocked. If saving that initial event fails, it returns an error without removing or terminalizing the queued operation, and without creating its cancellation handle or launching execute.
- Impact: A retry with the same idempotency key returns the stranded queued record as an existing operation. Cancel finds no cancel function. A later restart may unexpectedly execute the queued record, although the original enqueue reported failure.
- Proposed fix: Treat operation creation and its initial event as one recoverable transaction. On failure, either roll back both durable and in-memory state or persist an explicit failed/interrupted state; make accepted-work semantics unambiguous.
- Acceptance test: Fail only the initial event write. Retry the same key, cancel the returned operation, and restart the manager; assert no permanently queued or unexpectedly executed operation remains.
- Review commit: `03061cf69843229189c33e51bdcc4edcc6e1c6de` (last reviewed 2026-09-10)

## useteploy__teploy-dash-03 - P2 - Open

**Terminal-state and event persistence errors are silently discarded**

- Kind: Confirmed from source
- Evidence: finish ignores errors from saveOperation and appendEventLocked, then closes subscribers. emit also discards appendEventLocked errors. The in-memory result can therefore indicate success while durable state or output is missing.
- Impact: A restart may recover a completed operation as interrupted, and operators can lose the output needed to diagnose failures without any explicit storage-failure signal. This is distinct from whether the underlying command itself succeeded.
- Proposed fix: Surface storage health and failed persistence to callers/observers, retain a retryable pending persistence record, and distinguish command outcome from durable-record status. Do not silently announce a fully recorded terminal result when persistence failed.
- Acceptance test: Inject failures on stdout-event writes and terminal-state writes; verify visible storage errors, recovery behavior, and durable terminal results once storage recovers.
- Review commit: `03061cf69843229189c33e51bdcc4edcc6e1c6de` (last reviewed 2026-09-10)

## useteploy__teploy-dash-04 - P2 - Open improvement

**Bound operation retention and reduce global-lock persistence work**

- Kind: Improvement
- Evidence: Every emitted event rewrites the retained event list through atomicWrite, including file and directory sync, while the manager-wide mutex is held. Operations and their event lists are loaded into memory at startup; only events per operation have a count bound.
- Impact: A chatty command or large operation history can increase disk traffic and stall unrelated operations, reads, cancellations, and subscriptions. The actual throughput impact has not been benchmarked.
- Proposed fix: Use per-operation serialized persistence with bounded batching or an append journal, add retention/archival limits, and specify a maximum encoded event size consistent with the reader's 1 MiB scanner limit.
- Acceptance test: Benchmark several chatty operations concurrently with cancel/Get calls; test retention across restart and an event near and above the accepted byte-size limit.
- Review commit: `03061cf69843229189c33e51bdcc4edcc6e1c6de` (last reviewed 2026-09-10)

## useteploy__teploy-dash-05 - P2 - Open

**In-flight monitor checks can write stale results after removal or reload**

- Kind: Source-confirmed
- Evidence: teardownLocked stops tickers and closes stop channels, but runCheck uses its captured configuration and independently created check context. After its network work returns it always saves the result, updates lastStat and may send a transition alert.
- Impact: Removing or editing a monitor while a check is running does not invalidate that check. An old result can reintroduce lastStat after deletion or overwrite the new configuration's status and trigger a misleading alert.
- Proposed fix: Give each monitor generation a cancelable context and reject results whose generation is no longer active before persisting or alerting. Stop/Remove should cancel and, where required, wait for in-flight work without holding the shared mutex.
- Acceptance test: Block an old check, reload/remove the monitor, let a new check complete, then release the old one. Assert that the stale result cannot save state or emit alerts.
- Review commit: `03061cf69843229189c33e51bdcc4edcc6e1c6de` (last reviewed 2026-09-10)

## useteploy__teploy-dash-06 - P2 - Open

**The policy dialer never tries a second validated address**

- Kind: Source-confirmed
- Evidence: resolveAndFilter checks all DNS results, but policyDialContext passes only ips[0] to net.Dialer. It neither iterates remaining validated addresses nor implements an IPv4/IPv6 fallback.
- Impact: A service can be reported down when its first returned address is unreachable but another allowed address works. Dual-stack hosts and multi-address services are affected; this is not a request to weaken the address policy.
- Proposed fix: Attempt the already-validated addresses within the total check deadline, using an appropriate staggered fallback strategy. Do not re-resolve the hostname after filtering, and keep blocked-address rejection intact.
- Acceptance test: Inject two validated addresses with the first unreachable and the second serving a local test endpoint. Verify success within the total timeout and continued rejection of forbidden address sets.
- Review commit: `03061cf69843229189c33e51bdcc4edcc6e1c6de` (last reviewed 2026-09-10)

## useteploy__teploy-dash-07 - P2 - Open

**Every HTTP check creates an unclosed transport with its own idle connections**

- Kind: Source-confirmed
- Evidence: httpClientFor clones or creates a new Transport on every check. The base transport has no IdleConnTimeout, and checkHTTP closes the response body but never closes that transport's idle connections or retains it for reuse.
- Impact: Checks with reusable connections, especially HEAD or empty-body responses, can leave a separate socket/read loop per check until the peer closes it. A local Go behavior check retained three sockets after three equivalent cloned-transport checks.
- Proposed fix: Reuse transports partitioned by the finite network-policy modes or keep one owned transport per monitor generation. Close idle connections when that owner is retired and configure idle timeouts; avoid cross-policy connection reuse.
- Acceptance test: Run repeated HEAD checks against a keep-alive local server and count connections/goroutines. They should stay bounded and return to baseline after monitor removal.
- Review commit: `03061cf69843229189c33e51bdcc4edcc6e1c6de` (last reviewed 2026-09-10)

## useteploy__teploy-dash-08 - P2 - Open

**Do not treat a redirected, bodyless webhook request as delivered**

- Kind: Source-confirmed conditional defect
- Evidence: webhookClient has a timeout but no redirect policy. sendWebhook creates a POST with a JSON event and treats the final response as failed only at status >=400. Go’s default redirect handling converts 301/302/303 POST redirects to bodyless GETs. A redirect to a successful landing page therefore consumes the attempt without delivering the original event body or reporting a failure.
- Impact: A webhook URL redirected by a trailing-slash rule, login page or moved endpoint can silently lose monitor alerts when the final GET returns 2xx. The fixture demonstrates a loopback 302-to-200 route; no live webhook or external destination was contacted.
- Proposed fix: Reject redirects by default and explicitly classify all non-2xx responses, including 3xx, as non-delivery. If redirects are supported, narrowly permit validated same-origin method/body-preserving 307/308 flows, preserving/recomputing signatures as appropriate. Merely returning ErrUseLastResponse is insufficient unless 3xx is then rejected.
- Acceptance test: A 302-to-200 fixture must not send a bodyless GET or mark delivery successful. Direct 2xx POST must keep its body/signature. Any explicitly supported 307/308 redirect must preserve method, body and permitted origin.
- Review commit: `03061cf69843229189c33e51bdcc4edcc6e1c6de` (last reviewed 2026-09-10)

