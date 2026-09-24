#!/bin/sh
# teploy-dash release receipt (R01): records the inputs a binary was built
# from, checksums the artifact, and smoke-tests that it boots and serves.
# NO release is performed — tagging, goreleaser, image pushes and installs
# stay owner-controlled. This is the "supported build path recreates the
# artifact" half of R01's acceptance: another machine runs this script and
# compares receipts.
#
# Usage:
#   scripts/release-receipt.sh [receipt-file]   # default ./release-receipt.txt
#
# Recorded inputs: go toolchain, commit + tree state, UI toolchain (none —
# the frontend is committed vanilla HTML/CSS/JS embedded via go:embed; the
# per-asset checksums ARE the UI provenance), binary size + sha256, and the
# smoke result (binary boots, /api/health answers 200, / serves the login
# surface).
#
# Release-verify procedure (run on the machine that will ship):
#   1. git fetch && git status          # confirm the exact commit, clean tree
#   2. scripts/release-receipt.sh       # build + checksum + smoke
#   3. Compare against the receipt made on the build machine: go version,
#      commit, binary sha256, and the frontend asset digest must match.
#   4. Tag and run goreleaser ONLY if the receipts agree (owner action).

set -eu

RECEIPT="${1:-release-receipt.txt}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$ROOT/teploy-dash-receipt"

log() { printf '== %s\n' "$*"; }
die() { printf '!! %s\n' "$*" >&2; exit 1; }

command -v go >/dev/null 2>&1 || die "missing required tool: go"
command -v curl >/dev/null 2>&1 || die "missing required tool: curl"
command -v sha256sum >/dev/null 2>&1 || command -v shasum >/dev/null 2>&1 || die "missing a sha256 tool (sha256sum or shasum)"

sha() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
	else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# sha_stdin digests a stream (the UI manifest) with whichever tool exists —
# the `||` fallback in a pipeline cannot work: the last command (cut) always
# succeeds.
sha_stdin() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum | cut -d' ' -f1
	else shasum -a 256 | cut -d' ' -f1; fi
}

cd "$ROOT"

# ── Inputs ────────────────────────────────────────────────────────────────

GO_VERSION="$(go version)"
GO_ENV="$(go env GOOS GOARCH | tr '\n' ' ')"

if command -v git >/dev/null 2>&1 && git rev-parse --git-dir >/dev/null 2>&1; then
	COMMIT="$(git rev-parse HEAD)"
	BRANCH="$(git rev-parse --abbrev-ref HEAD)"
	if [ -n "$(git status --porcelain)" ]; then TREE_STATE="dirty"; else TREE_STATE="clean"; fi
else
	COMMIT="unknown (no git)"; BRANCH="-"; TREE_STATE="unknown"
fi

# UI provenance: no build step exists (deliberate — same pattern as the
# CLI's embedded UI), so the toolchain is the committed assets themselves.
# Digest the whole frontend tree, sorted and stable, plus a count.
UI_MANIFEST="$(
	cd cmd/teploy-dash/frontend && find . -type f | sort | while IFS= read -r f; do
		printf '%s  %s\n' "$(sha "$f")" "$f"
	done
)"
UI_FILES="$(printf '%s\n' "$UI_MANIFEST" | wc -l | tr -d ' ')"
UI_DIGEST="$(printf '%s\n' "$UI_MANIFEST" | sha_stdin)"

# ── Build ─────────────────────────────────────────────────────────────────

log "building ./teploy-dash-receipt"
go build -o "$BIN" ./cmd/teploy-dash || die "build failed"
BIN_SIZE="$(wc -c < "$BIN" | tr -d ' ')"
BIN_SHA="$(sha "$BIN")"

# ── Smoke: boots, health answers, login surface serves ───────────────────

log "smoke: boot + serve"
SMOKE_DIR="$(mktemp -d)"
SMOKE_PORT=""
cleanup() {
	if [ -n "${SMOKE_PID:-}" ] && kill -0 "$SMOKE_PID" 2>/dev/null; then
		kill "$SMOKE_PID" 2>/dev/null || true
		wait "$SMOKE_PID" 2>/dev/null || true
	fi
	rm -rf "$SMOKE_DIR" "$BIN"
}
trap cleanup EXIT INT TERM

for port in 34590 34591 34592 34593 34594 34595; do
	"$BIN" --host 127.0.0.1 --port "$port" --data "$SMOKE_DIR" --deployments "$SMOKE_DIR/deployments" >/dev/null 2>&1 &
	SMOKE_PID=$!
	i=0
	while [ "$i" -lt 50 ]; do
		if curl -sf -o /dev/null "http://127.0.0.1:$port/api/health"; then
			SMOKE_PORT="$port"
			break
		fi
		if ! kill -0 "$SMOKE_PID" 2>/dev/null; then break; fi
		i=$((i + 1))
		sleep 0.1
	done
	[ -n "$SMOKE_PORT" ] && break
	kill "$SMOKE_PID" 2>/dev/null || true
	wait "$SMOKE_PID" 2>/dev/null || true
done
[ -n "$SMOKE_PORT" ] || die "binary never served /api/health"

HEALTH_CODE="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$SMOKE_PORT/api/health")"
ROOT_CODE="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$SMOKE_PORT/")"
LOGIN_CODE="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$SMOKE_PORT/login")"
[ "$HEALTH_CODE" = "200" ] || die "smoke: /api/health answered $HEALTH_CODE, want 200"
case "$ROOT_CODE:$LOGIN_CODE" in
	200:*|*:"200"|302:*) ;;
	*) die "smoke: no login surface (/ = $ROOT_CODE, /login = $LOGIN_CODE)" ;;
esac

kill "$SMOKE_PID" 2>/dev/null || true
wait "$SMOKE_PID" 2>/dev/null || true
SMOKE_PID=""

# ── Receipt ───────────────────────────────────────────────────────────────

{
	echo "teploy-dash build receipt (R01)"
	echo "generated:  $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
	echo
	echo "inputs:"
	echo "  go:           $GO_VERSION"
	echo "  platform:     $GO_ENV"
	echo "  commit:       $COMMIT"
	echo "  branch:       $BRANCH"
	echo "  tree:         $TREE_STATE"
	echo "  ui toolchain: none — committed assets embedded via go:embed"
	echo "  ui files:     $UI_FILES"
	echo "  ui digest:    $UI_DIGEST"
	echo
	echo "artifact:"
	echo "  size:         $BIN_SIZE bytes"
	echo "  sha256:       $BIN_SHA"
	echo
	echo "smoke:"
	echo "  boots:        yes"
	echo "  health:       $HEALTH_CODE"
	echo "  root:         $ROOT_CODE"
	echo "  login:        $LOGIN_CODE"
} | tee "$RECEIPT"

log "receipt written to $RECEIPT"
