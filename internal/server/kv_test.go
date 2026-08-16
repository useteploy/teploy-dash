package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/useteploy/teploy-dash/internal/cli"
)

// kvRunner records every argv the handler hands the CLI and answers `server
// list` so serverHost/serverUser resolve. Everything else returns `reply`.
type kvRunner struct {
	mu    sync.Mutex
	calls [][]string
	reply func(args []string) (*cli.Result, error)
}

func (k *kvRunner) run(_ context.Context, args ...string) (*cli.Result, error) {
	if strings.HasPrefix(strings.Join(args, " "), "server list") {
		return &cli.Result{Stdout: `{"prod":{"host":"192.0.2.10","user":"deploy"}}`}, nil
	}
	k.mu.Lock()
	k.calls = append(k.calls, append([]string(nil), args...))
	k.mu.Unlock()
	if k.reply != nil {
		return k.reply(args)
	}
	return &cli.Result{}, nil
}

func (k *kvRunner) kvCalls() [][]string {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := [][]string{}
	for _, c := range k.calls {
		if len(c) > 0 && c[0] == "kv" {
			out = append(out, c)
		}
	}
	return out
}

// newKVServer builds a server whose CLI is the given runner. CLIInstalled is
// injected so the tests are hermetic — they must pass with no `teploy` binary
// on PATH.
func newKVServer(t *testing.T, k *kvRunner) *Server {
	t.Helper()
	return New(Config{
		DataDir:      t.TempDir(),
		NoAuth:       true,
		CLIInstalled: func() bool { return true },
		CLIRunner:    k.run,
	})
}

func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder, data any) string {
	t.Helper()
	var envelope struct {
		Data  json.RawMessage `json:"data"`
		Error string          `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decoding response: %v (body=%s)", err, rec.Body.String())
	}
	if envelope.Error == "" && data != nil && len(envelope.Data) > 0 {
		if err := json.Unmarshal(envelope.Data, data); err != nil {
			t.Fatalf("decoding data: %v (body=%s)", err, rec.Body.String())
		}
	}
	return envelope.Error
}

// ── Seam tests: these fail when the wiring is removed ─────────────────────

// Without --json the CLI prints "Connecting to <user>@<host>..." to STDOUT
// ahead of the payload (teploy-cli/internal/cli/connect.go:62-64), so a `kv
// get` would return that banner as the key's VALUE. Verified live against
// v0.1.26: with --json stdout is empty for a failed connect, without it the
// banner is there. A response-shape assertion cannot catch this, so assert on
// the argv itself.
func TestAppKV_PassesJSONSoStdoutIsOnlyTheValue(t *testing.T) {
	k := &kvRunner{reply: func([]string) (*cli.Result, error) {
		return &cli.Result{Stdout: "on\n"}, nil
	}}
	s := newKVServer(t, k)

	rec := httptest.NewRecorder()
	s.handleAppAction(rec, httptest.NewRequest(http.MethodGet, "/api/apps/prod/web/kv/value?key=flags/beta", nil))

	calls := k.kvCalls()
	if len(calls) != 1 {
		t.Fatalf("expected one kv call, got %d (%v)", len(calls), calls)
	}
	if indexOf(calls[0], "--json") < 0 {
		t.Fatalf("--json missing from argv %v — the CLI's connect banner would be returned as the value", calls[0])
	}
}

// The `--` terminator is what stops a kv key or value being parsed as a flag —
// including the global `--key`, which is the SSH PRIVATE KEY PATH. Verified
// live: `kv get --host H --app A --json -- --host` reaches the SSH dial, while
// the same call without `--` dies with "unknown flag".
//
// This is also the test that fails if someone folds cliKVRun back onto
// cliAppRun: that helper APPENDS --host/--app/--user after the caller's parts,
// which would land them after `--` as positionals and trip cobra.ExactArgs.
func TestAppKV_TerminatesFlagsBeforePositionals(t *testing.T) {
	k := &kvRunner{}
	s := newKVServer(t, k)

	rec := httptest.NewRecorder()
	s.handleAppAction(rec, httptest.NewRequest(http.MethodGet, "/api/apps/prod/web/kv?pattern=flags/*", nil))

	calls := k.kvCalls()
	if len(calls) != 1 {
		t.Fatalf("expected one kv call, got %d (%v)", len(calls), calls)
	}
	args := calls[0]
	dash := indexOf(args, "--")
	if dash < 0 {
		t.Fatalf("`--` missing from argv %v — a flag-shaped key or value would be parsed as a flag", args)
	}
	for _, flag := range []string{"--host", "--app", "--user", "--accessory", "--json"} {
		i := indexOf(args, flag)
		if i < 0 {
			t.Errorf("%s missing from argv %v", flag, args)
			continue
		}
		if i > dash {
			t.Errorf("%s at index %d is AFTER the `--` at %d — it would be a positional, not a flag (argv=%v)", flag, i, dash, args)
		}
	}
	if args[len(args)-1] != "flags/*" {
		t.Errorf("the pattern must be the trailing positional, got argv=%v", args)
	}
}

// The CLI's global --key is the SSH private key path. A kv key must never
// travel as a named flag, or reading one would repoint the SSH identity.
func TestAppKV_KeyIsNeverPassedAsAFlag(t *testing.T) {
	k := &kvRunner{reply: func([]string) (*cli.Result, error) {
		return &cli.Result{Stdout: "v\n"}, nil
	}}
	s := newKVServer(t, k)

	for _, target := range []string{
		"/api/apps/prod/web/kv/value?key=flags/beta",
		"/api/apps/prod/web/kv?key=flags/beta",
	} {
		method := http.MethodGet
		if strings.HasSuffix(target, "/kv?key=flags/beta") {
			method = http.MethodDelete
		}
		rec := httptest.NewRecorder()
		s.handleAppAction(rec, httptest.NewRequest(method, target, nil))
	}
	for _, args := range k.kvCalls() {
		if i := indexOf(args, "--key"); i >= 0 {
			t.Errorf("kv key leaked onto the SSH --key flag at index %d: %v", i, args)
		}
	}
}

// _internal/PARITY-PLAN.md section 0: dash must never cache authoritative
// state. Two identical reads must produce two CLI invocations — the executable
// form of that invariant. If a cache is ever added here, this fails.
func TestAppKV_NeverCachesAuthoritativeState(t *testing.T) {
	k := &kvRunner{reply: func([]string) (*cli.Result, error) {
		return &cli.Result{Stdout: "flags/beta\n"}, nil
	}}
	s := newKVServer(t, k)

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		s.handleAppAction(rec, httptest.NewRequest(http.MethodGet, "/api/apps/prod/web/kv", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("read %d: code=%d body=%s", i, rec.Code, rec.Body.String())
		}
	}
	if got := len(k.kvCalls()); got != 2 {
		t.Fatalf("two reads must be two CLI invocations, got %d — dash is caching authoritative state", got)
	}
}

// ── Behaviour ────────────────────────────────────────────────────────────

func TestAppKV_ListParsesOneKeyPerLine(t *testing.T) {
	k := &kvRunner{reply: func([]string) (*cli.Result, error) {
		// `teploy kv list` prints one key per line and nothing else
		// (teploy-cli/internal/cli/kv.go:209-211).
		return &cli.Result{Stdout: "flags/beta\nflags/dark-mode\ndeploys/count\n"}, nil
	}}
	s := newKVServer(t, k)

	rec := httptest.NewRecorder()
	s.handleAppAction(rec, httptest.NewRequest(http.MethodGet, "/api/apps/prod/web/kv?pattern=*", nil))

	var out kvListResponse
	if msg := decodeEnvelope(t, rec, &out); msg != "" {
		t.Fatalf("unexpected error: %s", msg)
	}
	want := []string{"flags/beta", "flags/dark-mode", "deploys/count"}
	if len(out.Keys) != len(want) {
		t.Fatalf("got %v, want %v", out.Keys, want)
	}
	for i := range want {
		if out.Keys[i] != want[i] {
			t.Fatalf("got %v, want %v", out.Keys, want)
		}
	}
	if out.Accessory != "nucleus" {
		t.Errorf("accessory should default to the CLI's own default, got %q", out.Accessory)
	}
}

func TestAppKV_EmptyListIsAnEmptyArrayNotNull(t *testing.T) {
	k := &kvRunner{}
	s := newKVServer(t, k)

	rec := httptest.NewRecorder()
	s.handleAppAction(rec, httptest.NewRequest(http.MethodGet, "/api/apps/prod/web/kv", nil))

	if !strings.Contains(rec.Body.String(), `"keys":[]`) {
		t.Errorf("an empty store must serialise as [], not null: %s", rec.Body.String())
	}
}

// An unset key exits non-zero with a specific message. That is an answer, not
// a transport failure — the same distinction the health endpoint makes. The
// stderr below is VERBATIM what the CLI emits: SilenceErrors is set on the
// root command and Execute prints the bare error (teploy-cli/internal/cli/root.go:89-95),
// so there is no "Error:" prefix.
func TestAppKV_MissingKeyIsAnAnswerNotAnError(t *testing.T) {
	k := &kvRunner{reply: func([]string) (*cli.Result, error) {
		return &cli.Result{Stderr: "key \"flags/beta\" is not set\n", ExitCode: 1}, nil
	}}
	s := newKVServer(t, k)

	rec := httptest.NewRecorder()
	s.handleAppAction(rec, httptest.NewRequest(http.MethodGet, "/api/apps/prod/web/kv/value?key=flags/beta", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("an unset key should still be a 200 answer; code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out kvValueResponse
	if msg := decodeEnvelope(t, rec, &out); msg != "" {
		t.Fatalf("an unset key must not be an API error, got %q", msg)
	}
	if out.Exists {
		t.Error("exists should be false for an unset key")
	}
}

// Anything OTHER than the exact not-set message must stay an error. If the CLI
// ever rewords that message this test still passes and the one above fails —
// the coupling fails closed, which is the point.
func TestAppKV_TransportFailureIsAnError(t *testing.T) {
	k := &kvRunner{reply: func([]string) (*cli.Result, error) {
		return nil, errors.New("ssh: connect to host 192.0.2.10 port 22: connection refused")
	}}
	s := newKVServer(t, k)

	rec := httptest.NewRecorder()
	s.handleAppAction(rec, httptest.NewRequest(http.MethodGet, "/api/apps/prod/web/kv/value?key=flags/beta", nil))

	if msg := decodeEnvelope(t, rec, nil); msg == "" {
		t.Errorf("an unreachable server must be an error, not exists=false: %s", rec.Body.String())
	}
}

// A kv query failure inside the container (bad accessory, Nucleus down) is
// also non-zero, and must NOT be mistaken for an absent key.
func TestAppKV_ContainerFailureIsNotMistakenForAnAbsentKey(t *testing.T) {
	k := &kvRunner{reply: func([]string) (*cli.Result, error) {
		return &cli.Result{Stderr: "kv query failed in web-nucleus: no such container\n", ExitCode: 1}, nil
	}}
	s := newKVServer(t, k)

	rec := httptest.NewRecorder()
	s.handleAppAction(rec, httptest.NewRequest(http.MethodGet, "/api/apps/prod/web/kv/value?key=flags/beta", nil))

	if msg := decodeEnvelope(t, rec, nil); msg == "" {
		t.Errorf("a container failure must surface as an error, not exists=false: %s", rec.Body.String())
	}
}

// `kv get` prints the value with fmt.Println, so exactly one trailing newline
// belongs to the command. Multi-line values must survive.
func TestAppKV_GetTrimsOnlyTheCommandsOwnNewline(t *testing.T) {
	k := &kvRunner{reply: func([]string) (*cli.Result, error) {
		return &cli.Result{Stdout: "line one\nline two\n"}, nil
	}}
	s := newKVServer(t, k)

	rec := httptest.NewRecorder()
	s.handleAppAction(rec, httptest.NewRequest(http.MethodGet, "/api/apps/prod/web/kv/value?key=motd", nil))

	var out kvValueResponse
	if msg := decodeEnvelope(t, rec, &out); msg != "" {
		t.Fatalf("unexpected error: %s", msg)
	}
	if out.Value != "line one\nline two" {
		t.Errorf("value = %q, want %q", out.Value, "line one\nline two")
	}
	if !out.Exists {
		t.Error("exists should be true")
	}
}

func TestAppKV_SetPassesTTLAndPositionalsInOrder(t *testing.T) {
	k := &kvRunner{}
	s := newKVServer(t, k)

	req := httptest.NewRequest(http.MethodPost, "/api/apps/prod/web/kv",
		strings.NewReader(`{"key":"flags/beta","value":"on","ttl":60}`))
	rec := httptest.NewRecorder()
	s.handleAppAction(rec, req)

	calls := k.kvCalls()
	if len(calls) != 1 {
		t.Fatalf("expected one kv call, got %v", calls)
	}
	args := calls[0]
	if args[1] != "set" {
		t.Fatalf("expected the set subcommand, got %v", args)
	}
	ttl := indexOf(args, "--ttl")
	dash := indexOf(args, "--")
	if ttl < 0 || args[ttl+1] != "60" {
		t.Fatalf("--ttl 60 missing from %v", args)
	}
	if ttl > dash {
		t.Fatalf("--ttl must precede the `--` terminator: %v", args)
	}
	if got := args[len(args)-2:]; got[0] != "flags/beta" || got[1] != "on" {
		t.Fatalf("key and value must be the trailing positionals in order, got %v", args)
	}
}

func TestAppKV_SetWithoutTTLOmitsTheFlag(t *testing.T) {
	k := &kvRunner{}
	s := newKVServer(t, k)

	req := httptest.NewRequest(http.MethodPost, "/api/apps/prod/web/kv",
		strings.NewReader(`{"key":"flags/beta","value":"on"}`))
	rec := httptest.NewRecorder()
	s.handleAppAction(rec, req)

	calls := k.kvCalls()
	if len(calls) != 1 {
		t.Fatalf("expected one kv call, got %v", calls)
	}
	if indexOf(calls[0], "--ttl") >= 0 {
		t.Errorf("--ttl must be omitted when unset (0 means no expiry to the CLI): %v", calls[0])
	}
}

func TestAppKV_DeleteUsesTheDelSubcommand(t *testing.T) {
	k := &kvRunner{}
	s := newKVServer(t, k)

	rec := httptest.NewRecorder()
	s.handleAppAction(rec, httptest.NewRequest(http.MethodDelete, "/api/apps/prod/web/kv?key=flags/beta", nil))

	calls := k.kvCalls()
	if len(calls) != 1 || calls[0][1] != "del" {
		t.Fatalf("expected one `kv del` call, got %v", calls)
	}
	if calls[0][len(calls[0])-1] != "flags/beta" {
		t.Fatalf("key must be the trailing positional: %v", calls[0])
	}
}

// Bad input must be rejected at the boundary, before the CLI is ever invoked.
func TestAppKV_RejectsBadInputWithoutInvokingTheCLI(t *testing.T) {
	cases := []struct {
		name, method, target, body string
	}{
		{"flag-shaped key", http.MethodGet, "/api/apps/prod/web/kv/value?key=--host", ""},
		{"empty key", http.MethodGet, "/api/apps/prod/web/kv/value?key=", ""},
		{"key with a shell metacharacter", http.MethodGet, "/api/apps/prod/web/kv/value?key=a;rm+-rf+/", ""},
		{"key with a quote", http.MethodDelete, "/api/apps/prod/web/kv?key=a%27b", ""},
		// %3B, not a literal ";": Go's url.Values.Get drops the ENTIRE query
		// when it sees a bare semicolon, so a literal one would silently
		// exercise the default rather than the validator.
		{"bad accessory", http.MethodGet, "/api/apps/prod/web/kv?accessory=a%3Bb", ""},
		{"uppercase accessory", http.MethodGet, "/api/apps/prod/web/kv?accessory=Nucleus", ""},
		{"flag-shaped pattern", http.MethodGet, "/api/apps/prod/web/kv?pattern=--host", ""},
		{"negative ttl", http.MethodPost, "/api/apps/prod/web/kv", `{"key":"a","value":"b","ttl":-1}`},
		{"unknown body field", http.MethodPost, "/api/apps/prod/web/kv", `{"key":"a","value":"b","nope":1}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := &kvRunner{}
			s := newKVServer(t, k)
			var req *http.Request
			if c.body == "" {
				req = httptest.NewRequest(c.method, c.target, nil)
			} else {
				req = httptest.NewRequest(c.method, c.target, strings.NewReader(c.body))
			}
			rec := httptest.NewRecorder()
			s.handleAppAction(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("want 400, got %d (body=%s)", rec.Code, rec.Body.String())
			}
			if got := k.kvCalls(); len(got) != 0 {
				t.Errorf("the CLI must not be invoked for rejected input, got %v", got)
			}
		})
	}
}

// An oversized value is rejected here rather than pushed through an SSH exec
// and a SQL literal.
func TestAppKV_RejectsOversizedValue(t *testing.T) {
	k := &kvRunner{}
	s := newKVServer(t, k)

	big, _ := json.Marshal(map[string]any{"key": "big", "value": strings.Repeat("x", kvMaxValueBytes+1)})
	rec := httptest.NewRecorder()
	s.handleAppAction(rec, httptest.NewRequest(http.MethodPost, "/api/apps/prod/web/kv", strings.NewReader(string(big))))

	if rec.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if got := k.kvCalls(); len(got) != 0 {
		t.Errorf("the CLI must not be invoked for an oversized value, got %v", got)
	}
}

// kv values are secret-shaped; they must never land in an intermediary cache.
func TestAppKV_ResponsesAreNoStore(t *testing.T) {
	k := &kvRunner{reply: func([]string) (*cli.Result, error) {
		return &cli.Result{Stdout: "on\n"}, nil
	}}
	s := newKVServer(t, k)

	for _, target := range []string{
		"/api/apps/prod/web/kv",
		"/api/apps/prod/web/kv/value?key=flags/beta",
	} {
		rec := httptest.NewRecorder()
		s.handleAppAction(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", target, got)
		}
	}
}

// A non-default accessory must reach the CLI as given.
func TestAppKV_AccessoryFlagIsForwarded(t *testing.T) {
	k := &kvRunner{}
	s := newKVServer(t, k)

	rec := httptest.NewRecorder()
	s.handleAppAction(rec, httptest.NewRequest(http.MethodGet, "/api/apps/prod/web/kv?accessory=nucleus-2", nil))

	calls := k.kvCalls()
	if len(calls) != 1 {
		t.Fatalf("expected one kv call, got %v", calls)
	}
	i := indexOf(calls[0], "--accessory")
	if i < 0 || calls[0][i+1] != "nucleus-2" {
		t.Fatalf("--accessory nucleus-2 missing from %v", calls[0])
	}
}

// kvNotSet is the one string coupling to the CLI. It must match the CLI's
// exact message and nothing else.
func TestKVNotSet_MatchesOnlyTheExactCLIMessage(t *testing.T) {
	cases := []struct {
		name, stderr string
		want         bool
	}{
		{"exact", "key \"flags/beta\" is not set\n", true},
		{"wrong key", "key \"other\" is not set\n", false},
		{"reworded", "key flags/beta is not set\n", false},
		{"different failure", "kv query failed in web-nucleus: no such container\n", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := kvNotSet("flags/beta", &cli.Result{Stderr: c.stderr, ExitCode: 1}, errors.New(strings.TrimSpace(c.stderr)))
			if got != c.want {
				t.Errorf("kvNotSet(%q) = %v, want %v", c.stderr, got, c.want)
			}
		})
	}
}

// SEAM: every kv route must actually dispatch to a kv handler.
//
// The input-validation tests above assert a 400, but an unrouted action also
// answers 400 — so they pass with the `case action == "kv"` arms deleted from
// server.go and are not evidence that anything is wired. This test sends a
// VALID request on each route and asserts the CLI was invoked with the right
// kv verb, which is false the moment a route stops being dispatched.
func TestAppKV_SEAM_EveryRouteReachesItsHandler(t *testing.T) {
	cases := []struct {
		name, method, target, body string
		wantVerb                   string
	}{
		{"list", http.MethodGet, "/api/apps/prod/web/kv?pattern=*", "", "list"},
		{"get", http.MethodGet, "/api/apps/prod/web/kv/value?key=flags/beta", "", "get"},
		{"set", http.MethodPost, "/api/apps/prod/web/kv", `{"key":"flags/beta","value":"on"}`, "set"},
		{"delete", http.MethodDelete, "/api/apps/prod/web/kv?key=flags/beta", "", "del"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := &kvRunner{}
			s := newKVServer(t, k)
			var req *http.Request
			if c.body == "" {
				req = httptest.NewRequest(c.method, c.target, nil)
			} else {
				req = httptest.NewRequest(c.method, c.target, strings.NewReader(c.body))
			}
			rec := httptest.NewRecorder()
			s.handleAppAction(rec, req)

			calls := k.kvCalls()
			if len(calls) == 0 {
				t.Fatalf("route %s %s never reached a kv handler (status %d) — the dispatch arm is missing", c.method, c.target, rec.Code)
			}
			joined := strings.Join(calls[0], " ")
			if !strings.Contains(joined, "kv "+c.wantVerb) && !strings.Contains(joined, "kv") {
				t.Fatalf("expected a kv %s invocation, got %v", c.wantVerb, calls[0])
			}
			if indexOf(calls[0], c.wantVerb) < 0 {
				t.Errorf("expected verb %q in the CLI args, got %v", c.wantVerb, calls[0])
			}
		})
	}
}
