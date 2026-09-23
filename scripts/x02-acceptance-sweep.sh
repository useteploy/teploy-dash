#!/usr/bin/env bash
# X02 S7 acceptance sweep — teploy-dash (ADR §6 S7).
# One executable harness running this repo's legs of the programme's
# acceptance line (:89): rename, duplicate app names, repeated request,
# response loss, rollback — target/history identity preserved end to end.
# Any leg failing (or matching no tests — vacuous pass is a broken pin)
# fails the sweep. Receipt: paste the output into AUDIT_OPEN when the
# contract changes.
set -u
cd "$(dirname "$0")/.."
fail=0
log=$(mktemp)

leg() {
  label="$1"; pkg="$2"; pattern="$3"
  printf '== %-24s %-30s ' "$label" "$pkg"
  listed=$(go test -list "$pattern" "$pkg" 2>/dev/null | grep -c '^Test')
  if [ "${listed:-0}" -eq 0 ]; then
    echo "FAIL (no tests matched: $pattern)"
    fail=1
    return
  fi
  if go test -count=1 "$pkg" -run "$pattern" >"$log" 2>&1; then
    echo "PASS ($listed test(s))"
  else
    echo "FAIL ($listed test(s), log: $log)"
    tail -5 "$log"
    fail=1
  fi
}

# rename: queued work follows server identity (verified rename retargets,
# same-name-different-id and vanished-id refuse); group bindings survive
# name swaps without re-keying.
leg rename-operations  ./internal/operation 'TestExecuteFollowsRenamedServer|TestExecuteRefusesSameNameDifferentServer|TestExecuteRefusesVanishedID|TestRetargetCommand'
leg rename-groups      ./internal/server    'TestGroupBindingSurvivesNameSwap|TestGroupAppBindingRecordsStableID|TestGroupAppBindingUsesNameHashFallbackForLegacy'

# duplicate app names: same app on two servers stays two distinct bindings;
# ambiguous bare-name removal refuses naming the servers; fleet envelopes
# stay per-server.
leg duplicate-names    ./internal/server    'TestGroupSameAppTwoServersDistinctAndAmbiguousRemoval|TestFleet'

# repeated request: scoped idempotency — same principal replays, different
# principals sharing a raw key are independent, differing payload 409s,
# namespacing survives restart.
leg repeated-request   ./internal/operation 'TestIdempotencyReplayAndConflict|TestIdempotencyKeyNamespacedByPrincipal|TestIdempotencyNamespacingSurvivesRestart'

# response loss: reconciliation resolves interrupted outcomes against the
# target's receipts (D02); record versions from a newer dash start
# read-only instead of corrupting history.
leg response-loss      ./internal/operation 'TestRestart.*Reconcil|TestCancelDuringRun|TestFutureRecordVersionStartsReadOnly|TestRecordVersionStampedAndReloadedMutable'

# rollback: a repointed/same-named-different server never receives queued
# work aimed at the admitted target (the A14 gate rollback relies on).
leg rollback           ./internal/operation 'TestExecuteRefusesRepointedTarget'

exit $fail
