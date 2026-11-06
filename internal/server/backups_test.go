package server

// backups_test.go — Dash Backups (plan P2): the fleet status sweep and page
// states, storage connect (probe → push → forget → rotate), the recovery key
// drill, the one policy, and the shared helpers, all against a stubbed CLI.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/cli"
	"github.com/useteploy/teploy-dash/internal/s3probe"
	"github.com/useteploy/teploy-dash/internal/store"
)

var backupsNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

// ── the fake CLI ─────────────────────────────────────────────────────────────

type backupCLI struct {
	mu     sync.Mutex
	t      *testing.T
	tokens []string
	// handshake overrides the version reply (an older CLI without the fleet
	// token). Empty uses the default v0.1.52 one.
	handshake string
	// status answers `backup status --host S --json` per server.
	status map[string]func() (*cli.Result, error)
	// pushed tracks what `backup-target set` stored, so the status reply
	// verifies the push like the real CLI does.
	pushed map[string]struct{ bucket, prefix string }
	// policyShow answers `server backup-policy show --host S --json`.
	policyShow map[string]string
	// keygen is the keypair `backup keygen --json` prints.
	keygen struct{ public, private string }
	// keygenErr makes keygen fail (transport).
	keygenErr error
	// verify answers `accessory verify-backup ... --json` per accessory name.
	verify map[string]func(args []string) (*cli.Result, error)
	// every argv seen, with stdin payloads where a command took one.
	calls []backupCLIcall
}

type backupCLIcall struct {
	args  []string
	stdin string
}

func (c *backupCLI) ran(args []string, stdin string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, backupCLIcall{args: append([]string(nil), args...), stdin: stdin})
}

// countOf counts calls whose argv contains each of want in order.
func (c *backupCLI) countOf(want ...string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, call := range c.calls {
		if subsequence(call.args, want) {
			n++
		}
	}
	return n
}

// subsequence reports whether needles appear in hay in order (not
// necessarily adjacent) — CLI flags interleave with values.
func subsequence(hay, needles []string) bool {
	i := 0
	for _, h := range hay {
		if i < len(needles) && h == needles[i] {
			i++
		}
	}
	return i == len(needles)
}

func (c *backupCLI) run(ctx context.Context, args ...string) (*cli.Result, error) {
	stdin := ""
	return c.runStdin(ctx, stdin, args...)
}

func (c *backupCLI) runStdin(ctx context.Context, stdin string, args ...string) (*cli.Result, error) {
	c.ran(args, stdin)
	switch {
	case len(args) >= 1 && args[0] == "version":
		out := c.handshake
		if out == "" {
			raw, _ := json.Marshal(map[string]any{"version": "v0.1.52", "machine_interface": 2,
				"capabilities": append([]string{"backup-fleet-v1"}, c.tokens...)})
			out = string(raw)
		}
		return &cli.Result{Stdout: out}, nil
	case len(args) >= 2 && args[0] == "server" && args[1] == "list":
		return &cli.Result{Stdout: `{"machine_interface":2,"servers":[{"name":"prod","host":"192.0.2.10"},{"name":"stage","host":"192.0.2.11"}],"observed_at":"2026-11-05T12:00:00Z"}`}, nil
	case len(args) >= 3 && args[0] == "backup" && args[1] == "status":
		if fn := c.status[args[3]]; fn != nil {
			res, err := fn()
			if err != nil || res == nil {
				return res, err
			}
			// Fold in what a push stored: the verification read must see it.
			c.mu.Lock()
			p := c.pushed[args[3]]
			c.mu.Unlock()
			if p.bucket != "" {
				var env backupStatusEnvelope
				if json.Unmarshal([]byte(res.Stdout), &env) == nil {
					env.Target.Configured = true
					env.Target.Bucket, env.Target.Prefix = p.bucket, p.prefix
					env.Target.HasCredentials, env.Target.Reachable = true, true
					raw, _ := json.Marshal(env)
					return &cli.Result{Stdout: string(raw)}, nil
				}
			}
			return res, nil
		}
		return &cli.Result{ExitCode: 1, Stderr: "unknown host"}, fmt.Errorf("exit 1: unknown host")
	case len(args) >= 4 && args[0] == "server" && args[1] == "backup-policy" && args[2] == "show":
		if out, ok := c.policyShow[args[4]]; ok {
			return &cli.Result{Stdout: out}, nil
		}
		return &cli.Result{ExitCode: 1, Stderr: "no policy"}, fmt.Errorf("exit 1: no policy")
	case len(args) >= 4 && args[0] == "server" && args[1] == "backup-policy" && args[2] == "set":
		return &cli.Result{Stdout: `{"machine_interface":2,"configured":true}`}, nil
	case len(args) >= 3 && args[0] == "server" && args[1] == "backup-target":
		if args[2] == "set" {
			host, bucket, prefix := "", "", ""
			for i := 0; i < len(args)-1; i++ {
				switch args[i] {
				case "--host":
					host = args[i+1]
				case "--bucket":
					bucket = args[i+1]
				case "--prefix":
					prefix = args[i+1]
				}
			}
			c.mu.Lock()
			if c.pushed == nil {
				c.pushed = map[string]struct{ bucket, prefix string }{}
			}
			c.pushed[host] = struct{ bucket, prefix string }{bucket, prefix}
			c.mu.Unlock()
			return &cli.Result{Stdout: `{"machine_interface":2,"stored":true}`}, nil
		}
		return &cli.Result{Stdout: `{"machine_interface":2,"configured":false}`}, nil
	case len(args) >= 2 && args[0] == "backup" && args[1] == "keygen":
		if c.keygenErr != nil {
			return nil, c.keygenErr
		}
		raw, _ := json.Marshal(map[string]any{"machine_interface": 2, "public_key": c.keygen.public, "private_key": c.keygen.private})
		return &cli.Result{Stdout: string(raw)}, nil
	case len(args) >= 2 && args[0] == "accessory" && args[1] == "verify-backup":
		name := args[2]
		if fn := c.verify[name]; fn != nil {
			return fn(args)
		}
		raw, _ := json.Marshal(map[string]any{"app": "a", "accessory": name, "ok": true, "metric": "tables:12", "date": "20261105-030000", "duration_ms": 4000})
		return &cli.Result{Stdout: string(raw)}, nil
	}
	return &cli.Result{ExitCode: 1, Stderr: "unexpected: " + strings.Join(args, " ")}, fmt.Errorf("exit 1: unexpected")
}

// statusesFromTestdata answers every host with a corpus envelope.
func statusesFromTestdata(t *testing.T) map[string]func() (*cli.Result, error) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "backup-contracts", "status-fleet.json"))
	if err != nil {
		t.Fatal(err)
	}
	one := func() (*cli.Result, error) { return &cli.Result{Stdout: string(raw)}, nil }
	return map[string]func() (*cli.Result, error){"prod": one, "stage": one}
}

func newBackupsServer(t *testing.T, c *backupCLI) *Server {
	t.Helper()
	withTempGroupsFile(t)
	writeNotifications(t, `{}`)
	return New(Config{DataDir: t.TempDir(), NoAuth: true,
		CLIInstalled:           func() bool { return true },
		CLIRunner:              c.run,
		CLIStdinRunner:         c.runStdin,
		OperationMaxConcurrent: 4})
}

// ── decode + page states ─────────────────────────────────────────────────────

func TestBackupStatusDecode_RefusesIncomplete(t *testing.T) {
	for _, name := range []string{"status-fleet.json", "status-unconfigured.json"} {
		raw, err := os.ReadFile(filepath.Join("testdata", "backup-contracts", name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeBackupStatus(string(raw)); err != nil {
			t.Fatalf("%s must decode: %v", name, err)
		}
	}
	if _, err := decodeBackupStatus(`{"host":"x"}`); err == nil {
		t.Fatal("an incomplete envelope must be refused")
	}
}

// Simulated status envelopes drive every page state: a fresh server's rows
// carry the CLI's coverage word; a server that could not be read reads
// unknown everywhere, never healthy.
func TestBackupsView_RowStatesAndProjection(t *testing.T) {
	c := &backupCLI{status: statusesFromTestdata(t)}
	s := newBackupsServer(t, c)
	now := backupsNow
	s.recordBackupStatus("prod", mustDecodeStatus(t, "status-fleet.json"), nil, now)
	// stage answered once, long ago: its rows read unknown, never healthy.
	s.recordBackupStatus("stage", mustDecodeStatus(t, "status-fleet.json"), nil, now.Add(-3*backupSweepInterval-2*backupServerTimeout))

	view := s.backupsView(context.Background(), now)
	if !view.CLISupported || view.Note != "" {
		t.Fatalf("view gate: %+v", view)
	}
	rows := map[string]backupRowView{}
	for _, r := range view.Rows {
		rows[r.Server+"/"+r.Name] = r
	}
	want := map[string]string{
		"prod/blog-db":                "covered",
		"prod/wordpress-shared-mysql": "adopted",
		"prod/omni_postgres":          "failing",
		"prod/shop-db":                "stale",
		"prod/redis-cache":            "uncovered",
		"prod/analytics-ch":           "uncovered",
		"prod/blog/volumes":           "excluded",
		"stage/blog-db":               "unknown",
	}
	for key, state := range want {
		if rows[key].State != state {
			t.Errorf("%s state = %s, want %s", key, rows[key].State, state)
		}
	}
	if rows["prod/shop-db"].LegacySchedule != true {
		t.Error("a legacy cron line must be marked")
	}
	// Projection: sum of newest objects × retained copies; excluded and
	// uncovered rows project nothing.
	if rows["prod/blog/volumes"].ProjectedBytes != 0 {
		t.Error("an excluded row must not project storage")
	}
	retained := retainedCopies(mustDecodeStatus(t, "status-fleet.json").Policy)
	if retained <= 0 {
		t.Fatalf("retainedCopies = %d", retained)
	}
	if view.ProjectedBytes <= 0 {
		t.Fatalf("the page must project monthly storage: %d", view.ProjectedBytes)
	}
	// omni_postgres carries an uncovered database inside a failing entry.
	if len(rows["prod/omni_postgres"].UncoveredDatabases) != 1 || rows["prod/omni_postgres"].UncoveredDatabases[0] != "reports" {
		t.Errorf("uncovered databases = %v", rows["prod/omni_postgres"].UncoveredDatabases)
	}
}

func mustDecodeStatus(t *testing.T, name string) *backupStatusEnvelope {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "backup-contracts", name))
	if err != nil {
		t.Fatal(err)
	}
	env, err := decodeBackupStatus(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func TestRetainedCopies_NoRetentionKeepsAMonth(t *testing.T) {
	p := backupStatPolicy{Interval: "1h"}
	if got := retainedCopies(p); got != 30*24 {
		t.Fatalf("no retention must project a month of runs: %d", got)
	}
	p.Retention = backupRetention{KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 12}
	if got := retainedCopies(p); got != 23 {
		t.Fatalf("GFS union upper bound: %d", got)
	}
	p = backupStatPolicy{Interval: "24h", Retention: backupRetention{MaxAgeDays: 14}}
	if got := retainedCopies(p); got != 14 {
		t.Fatalf("keep-within: %d", got)
	}
}

func TestBackupsSweep_PerServerReadAndAlertEvaluation(t *testing.T) {
	c := &backupCLI{status: statusesFromTestdata(t)}
	s := newBackupsServer(t, c)
	// One server answers, one fails: both are listed with their own state.
	c.status["stage"] = func() (*cli.Result, error) { return nil, fmt.Errorf("exit 1: ssh: connect timeout") }
	s.sweepBackups(context.Background())

	view := s.backupsView(context.Background(), backupsNow)
	servers := map[string]backupServerView{}
	for _, sv := range view.Servers {
		servers[sv.Server] = sv
	}
	if len(servers) != 2 {
		t.Fatalf("both servers must be listed: %+v", view.Servers)
	}
	if servers["prod"].State != backupServerOK {
		t.Errorf("prod = %+v", servers["prod"])
	}
	if servers["stage"].State != backupServerFailed {
		t.Errorf("stage = %+v", servers["stage"])
	}
	if strings.Contains(servers["stage"].Error, "://") && strings.Contains(servers["stage"].Error, "@") {
		t.Errorf("errors are sanitized: %s", servers["stage"].Error)
	}
	// The sweep asked for a sweep record.
	s.backups.mu.Lock()
	last := s.backups.rec.LastSweepAt
	s.backups.mu.Unlock()
	if last.IsZero() {
		t.Error("the sweep must be recorded")
	}
}

// An older CLI (no backup-fleet-v1 token) reads as unsupported: the page says
// so, the CLI is never asked for status.
func TestBackupsGate_OlderCLIIsANote(t *testing.T) {
	c := &backupCLI{handshake: `{"version":"v0.1.50","machine_interface":2,"capabilities":["backup-json-v1"]}`}
	c.status = map[string]func() (*cli.Result, error){
		"prod": func() (*cli.Result, error) {
			t.Error("an unsupported CLI must not be asked for fleet status")
			return &cli.Result{}, nil
		},
	}
	s := newBackupsServer(t, c)
	s.sweepBackups(context.Background())
	view := s.backupsView(context.Background(), backupsNow)
	if view.CLISupported || !strings.Contains(view.Note, backupFleetMinCLI) {
		t.Fatalf("view = %+v", view)
	}
	s.backups.mu.Lock()
	unsupported := s.backups.rec.Unsupported
	s.backups.mu.Unlock()
	if !unsupported {
		t.Error("the record must say the CLI cannot do this yet")
	}
}

// ── the checklist (P2.1) ─────────────────────────────────────────────────────

func TestBackupsChecklist_ProgressesToDone(t *testing.T) {
	c := &backupCLI{status: statusesFromTestdata(t)}
	s := newBackupsServer(t, c)
	env := mustDecodeStatus(t, "status-fleet.json")
	s.recordBackupStatus("prod", env, nil, backupsNow)

	s.backups.mu.Lock()
	s.backups.rec.Recovery = &backupRecovery{PublicKey: "age1lggyhqrw2nlhcxprm67z43rta597azn8gknawjehu9d9dl0jq3yqqvfafg", ConfirmedAt: backupsNow}
	s.backups.mu.Unlock()

	view := s.backupsView(context.Background(), backupsNow)
	states := map[string]string{}
	for _, item := range view.Checklist {
		states[item.ID] = item.State
	}
	if states["storage"] != "done" || states["alerts"] != "todo" || states["recovery"] != "todo" || states["verified"] != "todo" {
		t.Fatalf("checklist = %+v", view.Checklist)
	}
	if !view.Recovery.Banner {
		t.Error("the drill banner must show until the drill passes")
	}

	// Configure a channel, pass the drill, and give one row a green test.
	writeNotifications(t, `{"channels":[{"id":"ntfy","type":"ntfy","url":"https://ntfy.example/topic","token":"t"}]}`)
	s.backups.mu.Lock()
	passed := backupsNow
	s.backups.rec.Recovery.DrillPassedAt = &passed
	tid := "backups-test1"
	s.backups.rec.Tests[tid] = &backupAutoTest{ID: tid, Server: "prod", Container: "blog-db", App: "blog", Accessory: "db", Database: "", Engine: "postgres", DashOwned: true,
		LastRun: &backupTestRun{At: backupsNow, OK: true}}
	s.backups.mu.Unlock()

	view = s.backupsView(context.Background(), backupsNow)
	for _, item := range view.Checklist {
		if item.State != "done" {
			t.Fatalf("item %s = %s", item.ID, item.State)
		}
	}
	if view.Recovery.Banner {
		t.Error("the banner must clear once the drill passed")
	}
	// Done is sticky: the checklist is not rebuilt after completion.
	if view.ChecklistDone != true {
		t.Fatal("checklist must be marked done")
	}
	view = s.backupsView(context.Background(), backupsNow.Add(time.Hour))
	if len(view.Checklist) != 0 {
		t.Fatalf("a done checklist disappears: %+v", view.Checklist)
	}
}

// ── storage connect (P2.2) ───────────────────────────────────────────────────

func storageBody() map[string]any {
	return map[string]any{"provider": "b2", "bucket": "teploy-backups", "region": "us-west-004",
		"access_key": "KEYID", "secret_key": "SECRET", "base_prefix": "teploy"}
}

func postJSON(t *testing.T, s *Server, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, req)
	return rec
}

func decodeData(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	var envelope struct {
		Data  json.RawMessage `json:"data"`
		Error string          `json:"error,omitempty"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decoding %s: %v", rec.Body.String(), err)
	}
	if envelope.Error != "" {
		t.Fatalf("unexpected error envelope: %s", envelope.Error)
	}
	if err := json.Unmarshal(envelope.Data, dst); err != nil {
		t.Fatal(err)
	}
}

func TestBackupStorageConnect_ProbePushForgetVerify(t *testing.T) {
	c := &backupCLI{status: statusesFromTestdata(t)}
	s := newBackupsServer(t, c)
	var probeCalls int
	s.backups.probe = func(ctx context.Context, cfg s3probe.Config) s3probe.Result {
		probeCalls++
		if cfg.Bucket != "teploy-backups" || cfg.AccessKey != "KEYID" || cfg.SecretKey != "SECRET" {
			t.Errorf("probe config = %+v", cfg)
		}
		if !strings.Contains(cfg.Key, "/.incoming/dash-probe-") {
			t.Errorf("the probe object must sit under .incoming/: %s", cfg.Key)
		}
		return s3probe.Result{Write: true, Read: true, Delete: s3probe.DeleteRemoved}
	}

	rec := postJSON(t, s, "/api/backups/storage", storageBody())
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body.String())
	}
	var resp backupStorageResponse
	decodeData(t, rec, &resp)
	if !resp.AllOK || len(resp.Servers) != 2 {
		t.Fatalf("resp = %+v", resp)
	}
	for _, sv := range resp.Servers {
		if !sv.Pushed || !sv.Verified {
			t.Errorf("server push = %+v", sv)
		}
		if sv.Prefix == "" || sv.Prefix == resp.Servers[0].Prefix {
			if sv.Server == resp.Servers[0].Server {
				continue
			}
			t.Errorf("each server needs its own prefix: %+v", resp.Servers)
		}
	}
	if probeCalls != 1 {
		t.Errorf("probe calls = %d", probeCalls)
	}
	// The push carried the credentials on stdin exactly once per server.
	if n := c.countOf("backup-target", "set"); n != 2 {
		t.Fatalf("backup-target set calls = %d", n)
	}
	c.mu.Lock()
	for _, call := range c.calls {
		if subsequence(call.args, []string{"backup-target", "set"}) {
			if call.stdin != "KEYID\nSECRET\n" {
				t.Fatalf("credentials must ride stdin: %q", call.stdin)
			}
			if strings.Contains(strings.Join(call.args, " "), "KEYID") || strings.Contains(strings.Join(call.args, " "), "SECRET") {
				t.Fatal("credentials must never appear in argv")
			}
		}
	}
	c.mu.Unlock()
	// Dash remembers the bucket, never a credential.
	s.backups.mu.Lock()
	st := s.backups.rec.Storage
	raw, _ := json.Marshal(s.backups.rec)
	s.backups.mu.Unlock()
	if st == nil || st.Bucket != "teploy-backups" || len(st.Prefixes) != 2 {
		t.Fatalf("storage record = %+v", st)
	}
	for _, secret := range []string{"KEYID", "SECRET"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("a credential was persisted: %s", raw)
		}
	}
}

func TestBackupStorageConnect_ProbeFailurePushesNothing(t *testing.T) {
	c := &backupCLI{status: statusesFromTestdata(t)}
	s := newBackupsServer(t, c)
	s.backups.probe = func(context.Context, s3probe.Config) s3probe.Result {
		return s3probe.Result{Delete: s3probe.DeleteNotReached, Error: "the store refused the credentials (InvalidAccessKeyId)"}
	}
	rec := postJSON(t, s, "/api/backups/storage", storageBody())
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code = %d", rec.Code)
	}
	if n := c.countOf("backup-target", "set"); n != 0 {
		t.Fatalf("a failed probe must push nothing: %d", n)
	}
	s.backups.mu.Lock()
	nilStorage := s.backups.rec.Storage == nil
	s.backups.mu.Unlock()
	if !nilStorage {
		t.Fatal("nothing may be recorded on a failed probe")
	}
}

// A B2 key without deleteFiles is the recommended setup (plan D2): the probe
// notes it, connects, and says why the object stays.
func TestBackupStorageConnect_B2DeleteRefusalIsFine(t *testing.T) {
	c := &backupCLI{status: statusesFromTestdata(t)}
	s := newBackupsServer(t, c)
	s.backups.probe = func(context.Context, s3probe.Config) s3probe.Result {
		return s3probe.Result{Write: true, Read: true, Delete: s3probe.DeleteRefused}
	}
	rec := postJSON(t, s, "/api/backups/storage", storageBody())
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body.String())
	}
	var resp backupStorageResponse
	decodeData(t, rec, &resp)
	if resp.ProbeNote == "" || !strings.Contains(resp.ProbeNote, "cannot delete files") {
		t.Fatalf("probe note = %q", resp.ProbeNote)
	}
}

func TestBackupStorageRotation_OnlyRevokeWhenEveryServerVerified(t *testing.T) {
	c := &backupCLI{status: statusesFromTestdata(t)}
	s := newBackupsServer(t, c)
	s.backups.probe = func(context.Context, s3probe.Config) s3probe.Result {
		return s3probe.Result{Write: true, Read: true, Delete: s3probe.DeleteRemoved}
	}
	if rec := postJSON(t, s, "/api/backups/storage", storageBody()); rec.Code != http.StatusOK {
		t.Fatalf("connect failed: %s", rec.Body.String())
	}
	// Rotate with one server now failing its verification read.
	c.status["stage"] = func() (*cli.Result, error) { return nil, fmt.Errorf("exit 1: timeout") }
	body := storageBody()
	body["rotate"] = true
	body["access_key"] = "NEWKEY"
	body["secret_key"] = "NEWSECRET"
	rec := postJSON(t, s, "/api/backups/storage", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body.String())
	}
	var resp backupStorageResponse
	decodeData(t, rec, &resp)
	if !resp.Rotated || resp.AllOK {
		t.Fatalf("resp = %+v", resp)
	}
	if !strings.Contains(resp.RevokeNote, "Do not revoke") {
		t.Fatalf("the owner must be told to keep the old key: %q", resp.RevokeNote)
	}
	// Both servers were pushed the new key regardless (rotation must not
	// leave a mixed fleet), and the new prefixes stayed stable.
	if n := c.countOf("backup-target", "set"); n != 4 {
		t.Fatalf("two connects' worth of pushes expected: %d", n)
	}
	s.backups.mu.Lock()
	prefixes := s.backups.rec.Storage.Prefixes
	s.backups.mu.Unlock()
	if len(prefixes) != 2 {
		t.Fatalf("prefixes = %+v", prefixes)
	}
}

func TestBackupServerPrefix_UniquePerServer(t *testing.T) {
	taken := map[string]bool{}
	a := backupServerPrefix("base", "deploy-home", taken)
	taken[a] = true
	b := backupServerPrefix("base", "deploy home", taken) // space becomes a dash
	if a == b || !strings.HasPrefix(a, "base/deploy-home") || !strings.HasPrefix(b, "base/deploy") {
		t.Fatalf("a=%s b=%s", a, b)
	}
	taken[b] = true
	if c := backupServerPrefix("base", "deploy-home", taken); c == a {
		t.Fatal("a collision must be disambiguated")
	}
}

func TestScrubCredentials(t *testing.T) {
	out := scrubCredentials("put failed for KEYID (secret SECRET) at https://user:pw@host", "KEYID", "SECRET")
	if strings.Contains(out, "KEYID") || strings.Contains(out, "SECRET") || strings.Contains(out, "user:pw") {
		t.Fatalf("scrubbed = %q", out)
	}
}

// ── recovery key + drill (P2.3) ──────────────────────────────────────────────

const (
	testPublicKey  = "age1lggyhqrw2nlhcxprm67z43rta597azn8gknawjehu9d9dl0jq3yqqvfafg"
	testPrivateKey = "AGE-SECRET-KEY-1QYPVQQQDWK7W2VFJGJQQQQCJZS8S4N3NZQPQWPD5Z6Q9Z8PCQWQS3Y9Q7Z"
)

func TestBackupKeyGenerate_ShownOnceNeverStored(t *testing.T) {
	c := &backupCLI{status: statusesFromTestdata(t)}
	c.keygen.public, c.keygen.private = testPublicKey, testPrivateKey
	s := newBackupsServer(t, c)

	rec := postJSON(t, s, "/api/backups/recovery-key/generate", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		PublicKey  string `json:"public_key"`
		PrivateKey string `json:"private_key"`
		FileText   string `json:"file_text"`
	}
	decodeData(t, rec, &out)
	if out.PublicKey != testPublicKey || out.PrivateKey != testPrivateKey || !strings.Contains(out.FileText, testPrivateKey) {
		t.Fatalf("out = %+v", out)
	}
	s.backups.mu.Lock()
	raw, _ := json.Marshal(s.backups.rec)
	pending := s.backups.pendingKey
	s.backups.mu.Unlock()
	if pending != testPublicKey {
		t.Fatalf("pending key = %q", pending)
	}
	if strings.Contains(string(raw), testPrivateKey) {
		t.Fatal("the private key must never be persisted")
	}

	// Confirm pushes the recipient to every server and records only the public half.
	rec = postJSON(t, s, "/api/backups/recovery-key/confirm", map[string]any{"public_key": testPublicKey})
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body.String())
	}
	if n := c.countOf("backup-policy", "set", "--recipient"); n != 2 {
		t.Fatalf("recipient pushes = %d", n)
	}
	s.backups.mu.Lock()
	recovery := s.backups.rec.Recovery
	pending = s.backups.pendingKey
	s.backups.mu.Unlock()
	if recovery == nil || recovery.PublicKey != testPublicKey || recovery.DrillPassedAt != nil {
		t.Fatalf("recovery = %+v", recovery)
	}
	if pending != "" {
		t.Fatal("confirm clears the pending key")
	}
	// The drill banner shows until the drill passes.
	view := s.backupsView(context.Background(), backupsNow)
	if !view.Recovery.Banner {
		t.Fatal("banner must show")
	}
}

func TestBackupKeyConfirm_WrongKeyRefused(t *testing.T) {
	c := &backupCLI{status: statusesFromTestdata(t)}
	s := newBackupsServer(t, c)
	rec := postJSON(t, s, "/api/backups/recovery-key/confirm", map[string]any{"public_key": "age1someotherkey"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d", rec.Code)
	}
	if n := c.countOf("backup-policy", "set"); n != 0 {
		t.Fatal("nothing may be pushed for an unconfirmed key")
	}
}

func TestBackupDrill_ChecksTheRealThing(t *testing.T) {
	c := &backupCLI{status: statusesFromTestdata(t)}
	s := newBackupsServer(t, c)
	s.backups.mu.Lock()
	s.backups.rec.Recovery = &backupRecovery{PublicKey: testPublicKey, ConfirmedAt: backupsNow,
		Drill: &backupDrillObject{Server: "prod", Key: "s3://teploy-backups/prod/blog/app/accessories/db/20261105-030000-ab.pgdump.age",
			SizeBytes: 4096, AssignedAt: backupsNow}}
	s.backups.mu.Unlock()

	drill := func(output string) int {
		rec := postJSON(t, s, "/api/backups/recovery-key/drill", map[string]any{"output": output})
		return rec.Code
	}
	fetch := func(key string, size int64) string {
		raw, _ := json.Marshal(map[string]any{"key": key, "size_bytes": size, "encrypted": true})
		return string(raw)
	}
	if code := drill(`not json`); code != http.StatusBadRequest {
		t.Fatalf("non-JSON refused: %d", code)
	}
	if code := drill(fetch("s3://other-bucket/other-object", 1000)); code != http.StatusBadRequest {
		t.Fatalf("wrong object refused: %d", code)
	}
	if code := drill(fetch("s3://teploy-backups/prod/blog/app/accessories/db/20261105-030000-ab.pgdump.age", 999999)); code != http.StatusBadRequest {
		t.Fatalf("implausible size refused: %d", code)
	}
	if code := drill(fetch("s3://teploy-backups/prod/blog/app/accessories/db/20261105-030000-ab.pgdump.age", 4000)); code != http.StatusOK {
		t.Fatalf("a real drill passes: %d", code)
	}
	s.backups.mu.Lock()
	passed := s.backups.rec.Recovery.DrillPassedAt
	s.backups.mu.Unlock()
	if passed == nil {
		t.Fatal("the drill must be recorded")
	}
}

func TestPlausiblePlaintextSize(t *testing.T) {
	cases := []struct {
		cipher, plain int64
		ok            bool
	}{
		{4096, 4000, true},
		{4096, 4096, false}, // ciphertext ≥ plaintext is impossible
		{4096, 0, false},
		{4096, 100, false}, // a 4 KB ciphertext of a 100-byte plaintext is not age
		{1 << 30, (1 << 30) - 70000, true},
		{1 << 30, (1 << 30) - 5000000, false}, // 5 MB of "overhead" is not age
	}
	for _, c := range cases {
		if got := plausiblePlaintextSize(c.cipher, c.plain); got != c.ok {
			t.Errorf("plausiblePlaintextSize(%d,%d) = %v", c.cipher, c.plain, got)
		}
	}
}

// ── the one policy (P2.4) ────────────────────────────────────────────────────

func TestBackupPolicy_PresetsAndValidation(t *testing.T) {
	simple, ok := backupPresetRetention(backupPresetSimple)
	if !ok || simple.MaxAgeDays != 14 || simple.KeepDaily != 0 {
		t.Fatalf("simple = %+v", simple)
	}
	standard, ok := backupPresetRetention(backupPresetStandard)
	if !ok || standard.KeepDaily != 7 || standard.KeepWeekly != 4 || standard.KeepMonthly != 12 {
		t.Fatalf("standard = %+v", standard)
	}
	if _, ok := backupPresetRetention("custom"); ok {
		t.Fatal("only the two presets exist")
	}
}

func TestBackupWindow_WrapsMidnight(t *testing.T) {
	w := backupWindow{StartHour: 22, EndHour: 3}
	for h := 22; h <= 23; h++ {
		if !w.Contains(h) {
			t.Errorf("hour %d must be inside", h)
		}
	}
	for h := 0; h <= 2; h++ {
		if !w.Contains(h) {
			t.Errorf("hour %d must be inside", h)
		}
	}
	if w.Contains(3) || w.Contains(12) {
		t.Error("end hour is exclusive; midday is outside")
	}
}

func policyBody() map[string]any {
	return map[string]any{"interval": "1h", "preset": "standard", "volumes": true, "volume_max_gb": 20}
}

func TestBackupPolicyApply_WritesEveryServerAndHeartbeat(t *testing.T) {
	c := &backupCLI{status: statusesFromTestdata(t)}
	c.policyShow = map[string]string{
		"prod":  `{"machine_interface":2,"configured":true,"excludes":["blog/volumes"],"heartbeat_count":0}`,
		"stage": `{"machine_interface":2,"configured":true,"excludes":[],"heartbeat_count":0}`,
	}
	s := newBackupsServer(t, c)
	writeNotifications(t, `{"channels":[],"public_url":"https://dash.example.internal"}`)

	rec := postJSON(t, s, "/api/backups/policy", policyBody())
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Servers []backupServerResult `json:"servers"`
		AllOK   bool                 `json:"all_ok"`
	}
	decodeData(t, rec, &out)
	if !out.AllOK || len(out.Servers) != 2 {
		t.Fatalf("out = %+v", out)
	}
	// Both servers got the full policy flags.
	if n := c.countOf("--keep-daily=7"); n != 2 {
		t.Fatalf("keep-daily writes = %d", n)
	}
	if n := c.countOf("--volumes=true"); n != 2 {
		t.Fatalf("volumes writes = %d", n)
	}
	s.backups.mu.Lock()
	policy := s.backups.rec.Policy
	window := s.backups.rec.Window
	s.backups.mu.Unlock()
	if policy == nil || policy.Interval != "1h" || policy.Preset != "standard" {
		t.Fatalf("policy = %+v", policy)
	}
	if window != defaultBackupWindow {
		t.Fatalf("window = %+v", window)
	}

	// A bad preset or interval is refused before anything runs.
	c2 := &backupCLI{}
	s2 := newBackupsServer(t, c2)
	bad := policyBody()
	bad["preset"] = "custom"
	if rec := postJSON(t, s2, "/api/backups/policy", bad); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad preset: %d", rec.Code)
	}
	bad = policyBody()
	bad["interval"] = "9h"
	if rec := postJSON(t, s2, "/api/backups/policy", bad); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad interval: %d", rec.Code)
	}
	if n := c2.countOf("backup-policy", "set"); n != 0 {
		t.Fatal("nothing is written for an invalid policy")
	}
}

// The heartbeat URL carries a token: it only ever travels on stdin, via
// --heartbeat-stdin, and a server that already sends heartbeats elsewhere is
// left alone with a note (the CLI can only replace the list).
func TestBackupHeartbeatFor(t *testing.T) {
	// With a public URL, a server that already sends heartbeats elsewhere is
	// left alone with a note (the CLI can only replace the list).
	c := &backupCLI{}
	s := newBackupsServer(t, c)
	s.store = store.NewFileStore(t.TempDir())
	writeNotifications(t, `{"channels":[],"public_url":"https://dash.example.internal"}`)
	current := &backupPolicyShow{Configured: true, HeartbeatHosts: []string{"https://elsewhere.example"}}
	url, note := s.backupHeartbeatFor("prod", "1h", current)
	if url != "" || !strings.Contains(note, "left it alone") {
		t.Fatalf("url=%q note=%q", url, note)
	}
	// No public URL: a note, not a silent skip.
	writeNotifications(t, `{}`)
	url, note = s.backupHeartbeatFor("prod", "1h", &backupPolicyShow{})
	if url != "" || note == "" {
		t.Fatalf("url=%q note=%q", url, note)
	}
}

func TestBackupRunsPerDay(t *testing.T) {
	cases := map[string]int64{"1h": 24, "12h": 2, "24h": 1, "7d": 1, "": 1, "30m": 48}
	for in, want := range cases {
		if got := backupRunsPerDay(in); got != want {
			t.Errorf("backupRunsPerDay(%q) = %d, want %d", in, got, want)
		}
	}
	if d := backupIntervalDuration("7d"); d != 7*24*time.Hour {
		t.Errorf("7d = %v", d)
	}
}

// ── per-app exclude (P2.4) ───────────────────────────────────────────────────

func TestBackupExclude_ReadMergeWrite(t *testing.T) {
	c := &backupCLI{status: statusesFromTestdata(t)}
	c.policyShow = map[string]string{"prod": `{"machine_interface":2,"configured":true,"excludes":["blog/volumes"],"heartbeat_count":0}`}
	s := newBackupsServer(t, c)

	body := map[string]any{"server": "prod", "name": "shop/db", "excluded": true, "reason": "moved to RDS"}
	rec := postJSON(t, s, "/api/backups/exclude", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body.String())
	}
	// The write merged the person's existing exclude list, not replaced it.
	c.mu.Lock()
	merged := false
	for _, call := range c.calls {
		for _, a := range call.args {
			if a == "--exclude=blog/volumes,shop/db" {
				merged = true
			}
		}
	}
	c.mu.Unlock()
	if !merged {
		t.Fatal("the merged exclude list must be written (want --exclude=blog/volumes,shop/db)")
	}

	// A reason is required to exclude.
	body["reason"] = ""
	if rec := postJSON(t, s, "/api/backups/exclude", body); rec.Code != http.StatusBadRequest {
		t.Fatalf("reasonless exclude: %d", rec.Code)
	}
}

// ── version gate on the mutating endpoints ───────────────────────────────────

func TestBackupMutations_GatedOnFleetCLI(t *testing.T) {
	c := &backupCLI{handshake: `{"version":"v0.1.51","machine_interface":2,"capabilities":["backup-json-v1"]}`}
	s := newBackupsServer(t, c)
	for _, path := range []string{"/api/backups/storage", "/api/backups/policy"} {
		rec := postJSON(t, s, path, storageBody())
		if rec.Code != http.StatusNotImplemented {
			body := rec.Body.String()
			t.Errorf("%s on an older CLI: code=%d body=%s", path, rec.Code, body)
		} else if !strings.Contains(rec.Body.String(), backupFleetMinCLI) {
			t.Errorf("the refusal must name the remedy: %s", rec.Body.String())
		}
	}
}

// ── capabilities (binding principle: every action gets a capability) ────────

func TestBackupRouteCapabilities(t *testing.T) {
	cases := []struct {
		method, path string
		want         string
	}{
		{http.MethodGet, "/api/backups", "view.metadata"},
		{http.MethodPost, "/api/backups/policy", "execute.mutate"},
		{http.MethodPost, "/api/backups/exclude", "execute.mutate"},
		{http.MethodPost, "/api/backups/storage", "administer.credentials"},
		{http.MethodPost, "/api/backups/recovery-key/generate", "administer.credentials"},
		{http.MethodPost, "/api/backups/recovery-key/confirm", "administer.credentials"},
	}
	for _, c := range cases {
		got := requiredCapabilities(c.method, c.path)
		if len(got) != 1 || got[0] != c.want {
			t.Errorf("%s %s = %v, want [%s]", c.method, c.path, got, c.want)
		}
	}
}

// B2 keeps hidden versions forever by default, so the guidance must also expire what
// retention hides, and that rule must never hide live backups.
func TestB2LifecycleRuleCoversHiddenBackups(t *testing.T) {
	rule := backupLifecycleRule(&backupStorage{Provider: backupProviderB2}, backupStatTarget{Prefix: "teploy/prod"})
	for _, want := range []string{
		"fileNamePrefix teploy/prod/.incoming/, daysFromUploadingToHiding 1, daysFromHidingToDeleting 1",
		"fileNamePrefix teploy/prod/, daysFromHidingToDeleting 30, daysFromUploadingToHiding empty",
		"never set daysFromUploadingToHiding on this one",
	} {
		if !strings.Contains(rule, want) {
			t.Errorf("rule %q is missing %q", rule, want)
		}
	}
}
