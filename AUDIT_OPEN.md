# Open audit items

Unresolved findings for this repository from the ChatGPT-led audit series (2026-09-09 through 2026-09-11, passes 1-5; register: teploy-neutron-lullmail expanded audit). Every P0/P1 finding has been fixed and verified; the items below are the remaining P2/P3 tail plus one item needing validation. Fields are quoted from the audit register; line references point at the review commits listed per item where recorded.

Open items: 1 P2 improvement (1 total)

## useteploy__teploy-dash-04 - P2 - Open improvement

**Bound operation retention and reduce global-lock persistence work**

- Kind: Improvement
- Evidence: Every emitted event rewrites the retained event list through atomicWrite, including file and directory sync, while the manager-wide mutex is held. Operations and their event lists are loaded into memory at startup; only events per operation have a count bound.
- Impact: A chatty command or large operation history can increase disk traffic and stall unrelated operations, reads, cancellations, and subscriptions. The actual throughput impact has not been benchmarked.
- Proposed fix: Use per-operation serialized persistence with bounded batching or an append journal, add retention/archival limits, and specify a maximum encoded event size consistent with the reader's 1 MiB scanner limit.
- Acceptance test: Benchmark several chatty operations concurrently with cancel/Get calls; test retention across restart and an event near and above the accepted byte-size limit.
- Review commit: `03061cf69843229189c33e51bdcc4edcc6e1c6de` (last reviewed 2026-09-10)


## Resolution log (2026-09-12)

- teploy-dash-02, -05, -06, -07, -08: FIXED - see the audit teploy-dash-NN commits.
- teploy-dash-03: PARTIALLY FIXED - terminal/event persistence failures are now logged loudly (was silent discard). DEFERRED (design): a retryable pending-persistence record that reconciles after storage recovery is a subsystem, not a patch; needs an explicit durability policy first.
- teploy-dash-04: DEFERRED (design) - per-operation journal persistence + retention limits is a throughput/architecture change with no measured production impact; benchmark first, then decide.
