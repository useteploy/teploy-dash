package server

// kv.go — the CLI's shared Nucleus KV store, surfaced in the dashboard.
//
// ARCHITECTURAL INVARIANT (_internal/PARITY-PLAN.md section 0): dash never
// owns a second model of authoritative state. Every read below shells out to
// the CLI and renders what came back; nothing is cached, not even for the
// length of a page view. `internal/store` is deliberately untouched by this
// file — persisting kv here would be exactly the "cache authoritative state
// for speed" move the plan names as the cliff.
//
// TRUST BOUNDARY (teploy-cli/internal/cli/kv.go:12-16): Nucleus KV is ONE
// global keyspace with no server-enforced namespaces. Key prefixes are
// hygiene, not isolation — anything sharing the accessory can read or flush
// everything. The panel targets an accessory, not an app's private store.

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/useteploy/teploy-dash/internal/cli"
)

// defaultKVAccessory matches the CLI's own --accessory default
// (teploy-cli/internal/cli/kv.go:75).
const defaultKVAccessory = "nucleus"

// kvMaxValueBytes bounds a value written through the dashboard. The CLI has no
// limit of its own; this keeps a single request from pushing an arbitrarily
// large blob through an SSH exec and a SQL literal.
const kvMaxValueBytes = 64 * 1024

var (
	// kvKeyPattern is deliberately narrower than what Nucleus accepts. Keys
	// reaching the CLI's argv must not be flag-shaped, and the panel is a
	// convenience surface, not a general-purpose byte-key editor: a key
	// outside this set fails visibly with a 400 rather than being mangled.
	kvKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+=~,#-]{0,255}$`)

	// kvPatternPattern is kvKeyPattern plus the glob metacharacters
	// `teploy kv list` accepts, which may also lead (a bare "*" is the
	// default listing).
	kvPatternPattern = regexp.MustCompile(`^[A-Za-z0-9*?\[\]][A-Za-z0-9._:/@+=~,#*?\[\]-]{0,255}$`)

	// kvAccessoryPattern mirrors the CLI's config.validName
	// (teploy-cli/internal/config/app.go:22), which config.ValidateIdentifier
	// applies to --accessory before it becomes a container name. Rejecting
	// here turns a CLI-side error into a clear 400.
	kvAccessoryPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$|^[a-z0-9]$`)
)

func validKVKey(k string) bool     { return kvKeyPattern.MatchString(k) }
func validKVPattern(p string) bool { return kvPatternPattern.MatchString(p) }

// kvAccessory resolves and validates the ?accessory= / body accessory field,
// defaulting to the CLI's own default.
func kvAccessory(name string) (string, bool) {
	if name == "" {
		return defaultKVAccessory, true
	}
	if len(name) > 63 || !kvAccessoryPattern.MatchString(name) {
		return "", false
	}
	return name, true
}

// validKVValue reports whether a value can be carried on the CLI's argv.
// A NUL byte cannot appear in an argv at all (exec rejects it), and the size
// cap is ours.
func validKVValue(v string) bool {
	return len(v) <= kvMaxValueBytes && !strings.ContainsRune(v, 0)
}

// cliKVRun invokes one `teploy kv` subcommand against a server's accessory.
//
// It does NOT reuse cliAppRun, and must not be "simplified" onto it: cliAppRun
// APPENDS --host/--app/--user after the caller's parts, which would place them
// after the `--` terminator below and turn them into positional arguments,
// tripping the subcommands' cobra.ExactArgs. TestAppKV_TerminatesFlagsBeforePositionals
// fails if that happens.
//
// Two argv details are load-bearing, both verified against the real v0.1.26
// binary rather than reasoned about:
//
//   - `--json` is MANDATORY on reads. Without it the CLI prints
//     "Connecting to <user>@<host>..." to STDOUT before the payload
//     (teploy-cli/internal/cli/connect.go:62-64), which dash would hand back
//     as the key's value. kv's own output is human text either way; --json
//     only silences that banner.
//   - `--` is MANDATORY before the positionals. kv keys and values are
//     arbitrary strings, and a leading "-" is otherwise parsed as a flag —
//     including the global `--key`, which is the SSH PRIVATE KEY PATH, not a
//     kv key. A kv key must never travel as a named flag.
//
// ctx comes from the request, so a closed browser tab cancels the SSH
// subprocess instead of holding it to the delegate's 20-minute ceiling.
func (s *Server) cliKVRun(ctx context.Context, serverName, appName, accessory, sub string, flagArgs []string, positional ...string) (*cli.Result, error) {
	args := []string{"kv", sub,
		"--host", s.serverHost(serverName),
		"--app", appName,
		"--accessory", accessory,
		"--json",
	}
	if u := s.serverUser(serverName); u != "" {
		args = append(args, "--user", u)
	}
	args = append(args, flagArgs...)
	args = append(args, "--")
	args = append(args, positional...)

	result, err := s.runCLI(ctx, args...)
	if err != nil {
		return result, err
	}
	return result, cli.CheckExit(result, args)
}

// kvParseKeys turns `teploy kv list` output into a slice. The command prints
// one key per line (teploy-cli/internal/cli/kv.go:209-211); there is no
// machine-readable mode.
func kvParseKeys(stdout string) []string {
	keys := []string{}
	for _, line := range strings.Split(stdout, "\n") {
		if k := strings.TrimSpace(line); k != "" {
			keys = append(keys, k)
		}
	}
	return keys
}

// kvNotSet reports whether a failed `teploy kv get` failed because the key has
// no value, rather than because the call itself failed.
//
// This is the ONLY place dash string-matches the CLI's human output, and the
// coupling is single-sourced here on purpose. The message comes from
// teploy-cli/internal/cli/kv.go:90:
//
//	return fmt.Errorf("key %q is not set", args[0])
//
// and is matched EXACTLY, rebuilt from the same format string. A reworded CLI
// therefore fails closed — the call surfaces as an error — instead of quietly
// reporting every key as absent.
//
// Note the CLI treats an empty string and the literal "NULL" as unset
// (kv.go:89), so dash cannot distinguish "absent" from "set to empty" either.
func kvNotSet(key string, result *cli.Result, err error) bool {
	want := fmt.Sprintf("key %q is not set", key)
	candidates := make([]string, 0, 3)
	if err != nil {
		candidates = append(candidates, err.Error())
	}
	if result != nil {
		candidates = append(candidates, result.Stderr, result.Stdout)
	}
	for _, c := range candidates {
		for _, line := range strings.Split(c, "\n") {
			if strings.TrimSpace(line) == want {
				return true
			}
		}
	}
	return false
}

// ── Responses ────────────────────────────────────────────────────────────

type kvListResponse struct {
	Keys      []string `json:"keys"`
	Pattern   string   `json:"pattern"`
	Accessory string   `json:"accessory"`
	// Readonly lists the keys the store holds that this panel cannot operate
	// on, because validKVKey is deliberately narrower than what Nucleus
	// accepts (a key reaching the CLI's argv must not be flag-shaped). Without
	// this the list served keys whose Reveal and Remove buttons then answered
	// "invalid kv key" as a generic toast — reproduced live with "-legacy",
	// "cache|v1", "user name" and "pct%2". The panel renders these as present
	// but not actionable, which is the truth, rather than offering an action
	// that is guaranteed to fail.
	Readonly []string `json:"readonly,omitempty"`
}

type kvValueResponse struct {
	Key       string `json:"key"`
	Value     string `json:"value,omitempty"`
	Exists    bool   `json:"exists"`
	Accessory string `json:"accessory"`
}

type kvWriteResponse struct {
	Key       string `json:"key"`
	Accessory string `json:"accessory"`
	OK        bool   `json:"ok"`
}

// ── Handlers ─────────────────────────────────────────────────────────────
//
// All four answer inline, like the drift/stats/health cases they sit beside,
// rather than becoming operations: each is a single SSH round trip against an
// already-running container, not a mutation of deploy state. They also
// deliberately do NOT invalidate the fleet cache the way lock/unlock do — kv
// is not deploy state and never appears in the fleet view, so dropping the
// cache would force a full multi-server SSH sweep after every kv write.
//
// RBAC falls out of requiredRole (internal/server/users.go:87): the GETs are
// viewer, POST and DELETE are editor, with no per-route code needed. The rows
// in TestRequiredRole pin that so a future adminOnlyPrefixes edit can't move
// it silently.

// handleKVList lists keys matching a glob. GET, viewer.
func (s *Server) handleKVList(w http.ResponseWriter, r *http.Request, serverName, appName string) {
	if !s.cliInstalled() {
		writeError(w, "teploy CLI not installed")
		return
	}
	pattern := r.URL.Query().Get("pattern")
	if pattern == "" {
		pattern = "*"
	}
	if !validKVPattern(pattern) {
		writeError(w, "invalid kv pattern")
		return
	}
	accessory, ok := kvAccessory(r.URL.Query().Get("accessory"))
	if !ok {
		writeError(w, "invalid accessory name")
		return
	}
	result, err := s.cliKVRun(r.Context(), serverName, appName, accessory, "list", nil, pattern)
	if err != nil {
		writeError(w, err.Error())
		return
	}
	keys := kvParseKeys(result.Stdout)
	var readonly []string
	for _, k := range keys {
		if !validKVKey(k) {
			readonly = append(readonly, k)
		}
	}
	writeData(w, kvListResponse{Keys: keys, Pattern: pattern, Accessory: accessory, Readonly: readonly})
}

// handleKVGet reads one value. GET, viewer.
//
// The key travels in the QUERY STRING, not a path segment: kv keys legitimately
// contain "/" (the CLI's own examples are "flags/beta", "deploys/count"), and
// Go decodes %2F into r.URL.Path, so a path-segment key is ambiguous. The role
// check reads r.URL.Path only, so this does not weaken it.
func (s *Server) handleKVGet(w http.ResponseWriter, r *http.Request, serverName, appName string) {
	if !s.cliInstalled() {
		writeError(w, "teploy CLI not installed")
		return
	}
	key := r.URL.Query().Get("key")
	if !validKVKey(key) {
		writeError(w, "invalid kv key")
		return
	}
	accessory, ok := kvAccessory(r.URL.Query().Get("accessory"))
	if !ok {
		writeError(w, "invalid accessory name")
		return
	}
	result, err := s.cliKVRun(r.Context(), serverName, appName, accessory, "get", nil, key)
	if err != nil {
		// An unset key exits non-zero. That is an answer, not a transport
		// failure — the same call the health case makes for an unhealthy
		// verdict (server.go:1334-1345). Anything else stays an error.
		if kvNotSet(key, result, err) {
			writeData(w, kvValueResponse{Key: key, Accessory: accessory, Exists: false})
			return
		}
		writeError(w, err.Error())
		return
	}
	writeData(w, kvValueResponse{
		Key:       key,
		Accessory: accessory,
		Exists:    true,
		// `kv get` prints the value with fmt.Println, so exactly one trailing
		// newline is the command's, not the value's. Trim that one only —
		// multi-line values survive intact.
		Value: strings.TrimSuffix(result.Stdout, "\n"),
	})
}

// handleKVSet writes a key. POST, editor.
func (s *Server) handleKVSet(w http.ResponseWriter, r *http.Request, serverName, appName string) {
	if !s.cliInstalled() {
		writeError(w, "teploy CLI not installed")
		return
	}
	var body struct {
		Key       string `json:"key"`
		Value     string `json:"value"`
		TTL       int64  `json:"ttl"`
		Accessory string `json:"accessory"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	if !validKVKey(body.Key) {
		writeError(w, "invalid kv key")
		return
	}
	if !validKVValue(body.Value) {
		writeError(w, fmt.Sprintf("value must be at most %d bytes and contain no NUL", kvMaxValueBytes))
		return
	}
	if body.TTL < 0 {
		writeError(w, "ttl must not be negative")
		return
	}
	accessory, ok := kvAccessory(body.Accessory)
	if !ok {
		writeError(w, "invalid accessory name")
		return
	}
	var flagArgs []string
	if body.TTL > 0 {
		flagArgs = []string{"--ttl", strconv.FormatInt(body.TTL, 10)}
	}
	if _, err := s.cliKVRun(r.Context(), serverName, appName, accessory, "set", flagArgs, body.Key, body.Value); err != nil {
		writeError(w, err.Error())
		return
	}
	// `kv set` echoes "key = value"; there is nothing in it dash doesn't
	// already know, and echoing it back would put the value in a second place.
	writeData(w, kvWriteResponse{Key: body.Key, Accessory: accessory, OK: true})
}

// handleKVDelete removes a key. DELETE, editor.
func (s *Server) handleKVDelete(w http.ResponseWriter, r *http.Request, serverName, appName string) {
	if !s.cliInstalled() {
		writeError(w, "teploy CLI not installed")
		return
	}
	key := r.URL.Query().Get("key")
	if !validKVKey(key) {
		writeError(w, "invalid kv key")
		return
	}
	accessory, ok := kvAccessory(r.URL.Query().Get("accessory"))
	if !ok {
		writeError(w, "invalid accessory name")
		return
	}
	if _, err := s.cliKVRun(r.Context(), serverName, appName, accessory, "del", nil, key); err != nil {
		writeError(w, err.Error())
		return
	}
	writeData(w, kvWriteResponse{Key: key, Accessory: accessory, OK: true})
}
