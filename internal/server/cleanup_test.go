package server

// cleanup_test.go — Slice 6: the cleanup page over `teploy server audit`.
// The CLI is a configRunner (token-gated like production) and the operation
// executor emits the audit envelope as stdout lines, exactly how the real
// executor streams them; dash harvests the report from the finished
// operation's journal.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/caps"
	"github.com/useteploy/teploy-dash/internal/operation"
)

const cleanupAuditToken = "server-audit-v1"

// auditEnvelopeJSON builds a minimal but complete server-audit envelope.
func auditEnvelopeJSON(state, findings string) string {
	return fmt.Sprintf(`{
  "machine_interface": 2,
  "server": "prod",
  "host": "192.0.2.10",
  "audited_at": "2026-11-05T12:00:00Z",
  "state": %q,
  "checks": [{"name": "apps", "status": "ok"}],
  "disk": null,
  "findings": [%s]
}`, state, findings)
}

const findingStaleRoute = `{"id":"stale_route:ghost","kind":"stale_route","severity":"warning","subject":"ghost","app":"ghost","evidence":"ghost left a managed block behind","remedy":{"argv":["teploy","route","remove","--app","ghost","--host","prod"],"destructive":false,"confirm":false}}`

const findingOrphan = `{"id":"orphan_state:orphan","kind":"orphan_state","severity":"warning","subject":"orphan","app":"orphan","evidence":"orphan has deploy state but no containers","remedy":{"argv":["teploy","remove","--app","orphan","--host","prod","-y"],"destructive":true,"confirm":true}}`

const findingUnpruned = `{"id":"versions_beyond_policy:unset","kind":"versions_beyond_policy","severity":"warning","subject":"unset","app":"unset","evidence":"9 versions kept, policy says 5","size_bytes":20000000,"remedy":{"argv":["teploy","releases","prune","--app","unset","--host","prod","--keep","5"],"destructive":true,"confirm":true}}`

const findingBuildCache = `{"id":"reclaimable_build_artifacts:docker","kind":"reclaimable_build_artifacts","severity":"info","subject":"docker","evidence":"2.0 GB of unused build cache","size_bytes":2000000000,"remedy":{"argv":["teploy","server","prune","--host","prod","--build-cache","--dangling"],"destructive":false,"confirm":false}}`

const findingReportOnly = `{"id":"disk_pressure:/","kind":"disk_pressure","severity":"critical","subject":"/","evidence":"800 MB available on / (96% used)","related":["reclaimable_build_artifacts:docker"],"remedy":{"argv":null,"destructive":false,"confirm":false}}`

const findingDeadUpstream = `{"id":"managed_route_dead_upstream:blog","kind":"managed_route_dead_upstream","severity":"critical","subject":"blog","app":"blog","evidence":"blog.example.com routes to a container that does not resolve","remedy":{"argv":["teploy","redeploy","--app","blog","--host","prod"],"destructive":false,"confirm":true}}`

const findingFuture = `{"id":"future:x","kind":"future","severity":"warning","subject":"x","evidence":"a future CLI's remedy","remedy":{"argv":["teploy","telemetry","scrub","--app","x"],"destructive":false,"confirm":false}}`

type cleanupServer struct {
	*Server
	runner *configRunner
	// envelope is read by the executor at run time, so a test can change
	// what the next audit reports without rebuilding the server. failRuns
	// makes the next N operations fail at the execution layer.
	envelope *string
	failRuns int
	mu       chan struct{}
	commands []operation.Command
}

// newCleanupServer wires a token-bearing CLI and an executor that streams the
// current audit envelope (as stdout lines) whenever `server audit` runs, and
// records every command it was asked to run.
func newCleanupServer(t *testing.T, envelope string) *cleanupServer {
	t.Helper()
	c := &configRunner{tokens: withScreenTokens(cleanupAuditToken)}
	cs := &cleanupServer{runner: c, envelope: &envelope, mu: make(chan struct{}, 1)}
	cs.mu <- struct{}{}
	s := New(Config{
		DataDir:        t.TempDir(),
		NoAuth:         true,
		CLIInstalled:   func() bool { return true },
		CLIRunner:      c.run,
		CLIStdinRunner: c.runStdin,
		CLIIdentity:    func() string { return "test-cli" },
		OperationResolver: func(name string) (operation.Server, error) {
			return operation.Server{Name: name, Host: "192.0.2.10", User: "deploy"}, nil
		},
		OperationExecutor: func(_ context.Context, command operation.Command, emit func(operation.Stream, string)) (int, error) {
			<-cs.mu
			cs.commands = append(cs.commands, command)
			failing := cs.failRuns > 0
			if failing {
				cs.failRuns--
			}
			cs.mu <- struct{}{}
			if failing {
				return 3, fmt.Errorf("dial tcp: connection refused")
			}
			if len(command.Args) >= 2 && command.Args[0] == "server" && command.Args[1] == "audit" {
				for _, line := range strings.Split(strings.TrimSpace(*cs.envelope), "\n") {
					emit(operation.StreamStdout, line)
				}
			}
			return 0, nil
		},
	})
	t.Cleanup(func() { s.DrainOperations(context.Background()) })
	cs.Server = s
	return cs
}

func (cs *cleanupServer) executed() [][]string {
	<-cs.mu
	out := make([][]string, 0, len(cs.commands))
	for _, c := range cs.commands {
		out = append(out, c.Args)
	}
	cs.mu <- struct{}{}
	return out
}

// failNextAudit makes the next audit operation fail at the execution layer,
// like an SSH break.
func (cs *cleanupServer) failNextAudit() {
	<-cs.mu
	cs.failRuns++
	cs.mu <- struct{}{}
}

// runAudit posts the audit run and waits for the operation to finish, so the
// harvest has landed before the caller looks at the cache.
func runAudit(t *testing.T, cs *cleanupServer, server string) *operation.Operation {
	t.Helper()
	code, body := call(t, cs.Server.handleCleanupServer, "POST", "/api/cleanup/"+server+"/audit", "")
	if code != http.StatusAccepted {
		t.Fatalf("POST audit = %d: %v", code, body)
	}
	opID := body["data"].(map[string]any)["id"].(string)
	return waitForOperation(t, cs.Server, opID)
}

func waitForOperation(t *testing.T, s *Server, id string) *operation.Operation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		op, err := s.operations.Get(id)
		if err != nil {
			t.Fatalf("operation %s vanished: %v", id, err)
		}
		if op.Status.Terminal() {
			return op
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s stuck at %s", id, op.Status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitCleanupRecord waits until the cache reflects a finished audit's merge:
// the operation's terminal record is persisted BEFORE the OnFinish harvest
// runs, so observing "succeeded" is not yet observing the report.
func waitCleanupRecord(t *testing.T, cs *cleanupServer, cond func(*cleanupServerRecord) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		cs.cleanup.mu.Lock()
		rec := cs.cleanup.snap.Servers["prod"]
		ok := rec != nil && cond(rec)
		cs.cleanup.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("cleanup record for prod never settled: %+v", rec)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func cleanupDetail(t *testing.T, cs *cleanupServer, server string) (int, map[string]any) {
	t.Helper()
	return call(t, cs.Server.handleCleanupServer, "GET", "/api/cleanup/"+server, "")
}

func findingByID(t *testing.T, body map[string]any, id string) map[string]any {
	t.Helper()
	findings := body["data"].(map[string]any)["findings"].([]any)
	for _, f := range findings {
		if m := f.(map[string]any); m["id"] == id {
			return m
		}
	}
	t.Fatalf("finding %s not in the audit", id)
	return nil
}

// ── the audit runner ─────────────────────────────────────────────────────────

func TestCleanupAuditRunHarvestsEnvelope(t *testing.T) {
	cs := newCleanupServer(t, auditEnvelopeJSON("ok", strings.Join([]string{findingStaleRoute, findingReportOnly}, ",")))
	if op := runAudit(t, cs, "prod"); op.Status != operation.StatusSucceeded {
		t.Fatalf("audit operation = %s (%s)", op.Status, op.Error)
	}
	waitCleanupRecord(t, cs, func(rec *cleanupServerRecord) bool { return rec.Envelope != nil && rec.LastError == "" })
	code, body := cleanupDetail(t, cs, "prod")
	if code != http.StatusOK {
		t.Fatalf("detail = %d: %v", code, body)
	}
	data := body["data"].(map[string]any)
	if data["state"] != "ok" {
		t.Fatalf("state = %v, want ok", data["state"])
	}
	remedy := findingByID(t, body, "stale_route:ghost")["remedy"].(map[string]any)
	if remedy["available"] != true || remedy["runnable"] != true {
		t.Fatalf("stale route remedy = %v", remedy)
	}
	report := findingByID(t, body, "disk_pressure:/")["remedy"].(map[string]any)
	if report["available"] != false || report["runnable"] != false {
		t.Fatalf("report-only remedy must have no button: %v", report)
	}
	// The fleet view counts severities, dismissals and remediable findings.
	fcode, fleet := call(t, cs.Server.handleCleanup, "GET", "/api/cleanup", "")
	if fcode != http.StatusOK {
		t.Fatalf("fleet = %d", fcode)
	}
	rows := fleet["data"].(map[string]any)["servers"].([]any)
	if len(rows) != 1 {
		t.Fatalf("fleet rows = %v", rows)
	}
	row := rows[0].(map[string]any)
	if row["state"] != "ok" || row["configured"] != true {
		t.Fatalf("row = %v", row)
	}
	if counts := row["counts"].(map[string]any); counts["critical"] != float64(1) || counts["remediable"] != float64(1) {
		t.Fatalf("counts = %v", counts)
	}
}

// The fleet view carries each server's audited root-disk line (no
// attribution), and leaves it out when the audit read no disk.
func TestCleanupFleetRowCarriesDisk(t *testing.T) {
	env := strings.Replace(auditEnvelopeJSON("ok", findingStaleRoute), `"disk": null`,
		`"disk": {"path": "/", "available_bytes": 21000000000, "used_percent": 74, "attribution": [{"name": "docker images", "size_bytes": 31000000000}]}`, 1)
	cs := newCleanupServer(t, env)
	if op := runAudit(t, cs, "prod"); op.Status != operation.StatusSucceeded {
		t.Fatalf("audit operation = %s (%s)", op.Status, op.Error)
	}
	waitCleanupRecord(t, cs, func(rec *cleanupServerRecord) bool { return rec.Envelope != nil })
	_, fleet := call(t, cs.Server.handleCleanup, "GET", "/api/cleanup", "")
	row := fleet["data"].(map[string]any)["servers"].([]any)[0].(map[string]any)
	disk, ok := row["disk"].(map[string]any)
	if !ok {
		t.Fatalf("row has no disk: %v", row)
	}
	if disk["path"] != "/" || disk["used_percent"] != float64(74) || disk["available_bytes"] != float64(21000000000) {
		t.Fatalf("disk = %v", disk)
	}
	if _, leaked := disk["attribution"]; leaked {
		t.Fatalf("the fleet row must not carry the attribution: %v", disk)
	}

	bare := newCleanupServer(t, auditEnvelopeJSON("ok", findingStaleRoute))
	runAudit(t, bare, "prod")
	waitCleanupRecord(t, bare, func(rec *cleanupServerRecord) bool { return rec.Envelope != nil })
	_, fleet = call(t, bare.Server.handleCleanup, "GET", "/api/cleanup", "")
	if _, has := fleet["data"].(map[string]any)["servers"].([]any)[0].(map[string]any)["disk"]; has {
		t.Fatal("an audit without a disk reading must not invent one")
	}
}

// A failed re-audit keeps the last report and names the failure — a broken
// SSH path never clears the page.
func TestCleanupAuditFailureKeepsLastReport(t *testing.T) {
	cs := newCleanupServer(t, auditEnvelopeJSON("ok", findingStaleRoute))
	if op := runAudit(t, cs, "prod"); op.Status != operation.StatusSucceeded {
		t.Fatal("first audit must succeed")
	}
	waitCleanupRecord(t, cs, func(rec *cleanupServerRecord) bool { return rec.Envelope != nil })
	cs.failNextAudit()
	if op := runAudit(t, cs, "prod"); op.Status != operation.StatusFailed {
		t.Fatalf("second audit = %s", op.Status)
	}
	waitCleanupRecord(t, cs, func(rec *cleanupServerRecord) bool { return rec.LastError != "" })
	_, detail := cleanupDetail(t, cs, "prod")
	data := detail["data"].(map[string]any)
	if data["state"] != "ok" || data["last_error"] == "" || !strings.Contains(data["last_error"].(string), "did not finish") {
		t.Fatalf("failed run must keep the last report and name the failure: %v", data)
	}
	if findingByID(t, detail, "stale_route:ghost") == nil {
		t.Fatal("the previous findings must survive a failed re-audit")
	}
}

func TestCleanupSecondAuditRefusedWhileOneRuns(t *testing.T) {
	block := make(chan struct{})
	c := &configRunner{tokens: withScreenTokens(cleanupAuditToken)}
	s := New(Config{
		DataDir: t.TempDir(), NoAuth: true,
		CLIInstalled: func() bool { return true }, CLIRunner: c.run, CLIStdinRunner: c.runStdin,
		CLIIdentity: func() string { return "test-cli" },
		OperationResolver: func(name string) (operation.Server, error) {
			return operation.Server{Name: name, Host: "192.0.2.10"}, nil
		},
		OperationExecutor: func(_ context.Context, _ operation.Command, _ func(operation.Stream, string)) (int, error) {
			<-block
			return 0, nil
		},
	})
	t.Cleanup(func() { s.DrainOperations(context.Background()) })
	code, first := call(t, s.handleCleanupServer, "POST", "/api/cleanup/prod/audit", "")
	if code != http.StatusAccepted {
		t.Fatalf("first audit = %d", code)
	}
	code, second := call(t, s.handleCleanupServer, "POST", "/api/cleanup/prod/audit", "")
	if code != http.StatusConflict {
		t.Fatalf("second concurrent audit = %d: %v", code, second)
	}
	if !strings.Contains(second["error"].(string), "already") {
		t.Fatalf("refusal must say an audit is already running: %v", second)
	}
	close(block)
	waitForOperation(t, s, first["data"].(map[string]any)["id"].(string))
}

// An unreachable audit is a SUCCESSFUL run that says so — never "clean".
func TestCleanupUnreachableAuditIsHonest(t *testing.T) {
	cs := newCleanupServer(t, auditEnvelopeJSON("unreachable", ""))
	if op := runAudit(t, cs, "prod"); op.Status != operation.StatusSucceeded {
		t.Fatal("an unreachable envelope is a successful audit run")
	}
	waitCleanupRecord(t, cs, func(rec *cleanupServerRecord) bool { return rec.Envelope != nil && rec.Envelope.State == "unreachable" })
	_, body := cleanupDetail(t, cs, "prod")
	data := body["data"].(map[string]any)
	if data["state"] != "unreachable" {
		t.Fatalf("unreachable must surface as itself: %v", data)
	}
	if findings := data["findings"].([]any); len(findings) != 0 {
		t.Fatalf("an unreachable audit has no findings to show: %v", findings)
	}
}

// ── envelope decode ──────────────────────────────────────────────────────────

func TestDecodeServerAuditEnvelope(t *testing.T) {
	env, err := decodeServerAuditEnvelope(auditEnvelopeJSON("partial", `{"id":"future_kind:x","kind":"future_kind","severity":"warning","subject":"x","evidence":"a kind this dash has never heard of","remedy":{"argv":null,"destructive":false,"confirm":false}}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.State != "partial" || len(env.Findings) != 1 || env.Findings[0].Kind != "future_kind" {
		t.Fatalf("envelope = %+v", env)
	}
	// A newer machine interface is refused, not interpreted (house rule).
	if _, err := decodeServerAuditEnvelope(strings.Replace(auditEnvelopeJSON("ok", ""), `"machine_interface": 2`, `"machine_interface": 3`, 1)); err == nil {
		t.Fatal("an envelope from MI 3 must be refused")
	}
	// Incomplete payloads are refused rather than half-rendered.
	if _, err := decodeServerAuditEnvelope(`{"machine_interface":2,"server":"prod","state":"ok","checks":[]}`); err == nil {
		t.Fatal("an envelope without audited_at/findings must be refused")
	}
	if _, err := decodeServerAuditEnvelope(auditEnvelopeJSON("ok", `{"id":"","kind":"k","subject":"s","evidence":"e","remedy":{"destructive":false,"confirm":false}}`)); err == nil {
		t.Fatal("a finding without its stable id must be refused")
	}
}

// ── remedies ─────────────────────────────────────────────────────────────────

func TestCleanupRemedyRunsOnlyArgvBearingFindings(t *testing.T) {
	cs := newCleanupServer(t, auditEnvelopeJSON("ok", strings.Join([]string{findingStaleRoute, findingReportOnly, findingBuildCache}, ",")))
	runAudit(t, cs, "prod")
	waitCleanupRecord(t, cs, func(rec *cleanupServerRecord) bool { return rec.Envelope != nil })

	// Report-only: no button server-side either.
	code, body := call(t, cs.Server.handleCleanupServer, "POST", "/api/cleanup/prod/remedy", `{"id":"disk_pressure:/"}`)
	if code != http.StatusBadRequest || !strings.Contains(body["error"].(string), "report-only") {
		t.Fatalf("report-only remedy = %d: %v", code, body)
	}

	// A runnable remedy enqueues the operation the finding names — rebuilt
	// from the allowlist, not the envelope's argv passed through.
	code, body = call(t, cs.Server.handleCleanupServer, "POST", "/api/cleanup/prod/remedy", `{"id":"stale_route:ghost"}`)
	if code != http.StatusAccepted {
		t.Fatalf("route remove = %d: %v", code, body)
	}
	op := waitForOperation(t, cs.Server, body["data"].(map[string]any)["id"].(string))
	if op.Request.Kind != operation.KindRouteRemove || op.Request.App != "ghost" {
		t.Fatalf("operation = %+v", op.Request)
	}
	last := cs.executed()
	ran := last[len(last)-1]
	want := []string{"route", "remove", "--json", "--host", "192.0.2.10", "--app", "ghost", "--user", "deploy"}
	if fmt.Sprint(ran[:len(want)]) != fmt.Sprint(want) {
		t.Fatalf("executed argv = %v", ran)
	}

	// Server-scoped reclaim: server prune with both halves.
	code, body = call(t, cs.Server.handleCleanupServer, "POST", "/api/cleanup/prod/remedy", `{"id":"reclaimable_build_artifacts:docker"}`)
	if code != http.StatusAccepted {
		t.Fatalf("server prune = %d: %v", code, body)
	}
	waitForOperation(t, cs.Server, body["data"].(map[string]any)["id"].(string))
	if argv := cs.executed(); !strings.Contains(strings.Join(argv[len(argv)-1], " "), "server prune --json --host 192.0.2.10 --build-cache --dangling") {
		t.Fatalf("server prune argv = %v", argv[len(argv)-1])
	}
}

// remediableRan counts executed commands that were not the audit itself.
func (cs *cleanupServer) remedyRuns() int {
	n := 0
	for _, argv := range cs.executed() {
		if !(len(argv) >= 2 && argv[0] == "server" && argv[1] == "audit") {
			n++
		}
	}
	return n
}

// The typed confirmation is enforced at the API, not only in the dialog.
func TestCleanupDestructiveRemedyNeedsTypedName(t *testing.T) {
	cs := newCleanupServer(t, auditEnvelopeJSON("ok", strings.Join([]string{findingOrphan, findingUnpruned, findingDeadUpstream}, ",")))
	runAudit(t, cs, "prod")
	waitCleanupRecord(t, cs, func(rec *cleanupServerRecord) bool { return rec.Envelope != nil })

	for _, bad := range []string{`{"id":"orphan_state:orphan"}`, `{"id":"orphan_state:orphan","confirm":"wrong"}`, `{"id":"orphan_state:orphan","confirm":"ORPHAN"}`} {
		code, body := call(t, cs.Server.handleCleanupServer, "POST", "/api/cleanup/prod/remedy", bad)
		if code != http.StatusBadRequest {
			t.Fatalf("destructive remedy without the typed name must be refused: %s -> %d", bad, code)
		}
		if !strings.Contains(body["error"].(string), "type orphan to confirm") {
			t.Fatalf("refusal must name what to type: %v", body["error"])
		}
	}
	if cs.remedyRuns() != 0 {
		t.Fatal("nothing may run before the confirmation matches")
	}

	code, body := call(t, cs.Server.handleCleanupServer, "POST", "/api/cleanup/prod/remedy", `{"id":"orphan_state:orphan","confirm":"orphan"}`)
	if code != http.StatusAccepted {
		t.Fatalf("confirmed orphan removal = %d: %v", code, body)
	}
	op := waitForOperation(t, cs.Server, body["data"].(map[string]any)["id"].(string))
	if op.Request.Kind != operation.KindRemove || op.Request.Purge {
		t.Fatalf("orphan remedy is a remove without purge: %+v", op.Request)
	}

	// The prune's keep comes from the audit's own policy answer.
	code, body = call(t, cs.Server.handleCleanupServer, "POST", "/api/cleanup/prod/remedy", `{"id":"versions_beyond_policy:unset","confirm":"unset"}`)
	if code != http.StatusAccepted {
		t.Fatalf("releases prune = %d: %v", code, body)
	}
	op = waitForOperation(t, cs.Server, body["data"].(map[string]any)["id"].(string))
	if op.Request.Kind != operation.KindReleasesPrune || op.Request.Keep != 5 {
		t.Fatalf("prune request = %+v", op.Request)
	}
	if argv := cs.executed(); !strings.Contains(strings.Join(argv[len(argv)-1], " "), "--keep 5") {
		t.Fatalf("prune argv = %v", argv[len(argv)-1])
	}

	// confirm:true but not destructive (a redeploy): one click, like the
	// app page's redeploy.
	code, body = call(t, cs.Server.handleCleanupServer, "POST", "/api/cleanup/prod/remedy", `{"id":"managed_route_dead_upstream:blog"}`)
	if code != http.StatusAccepted {
		t.Fatalf("redeploy remedy = %d: %v", code, body)
	}
	if op := waitForOperation(t, cs.Server, body["data"].(map[string]any)["id"].(string)); op.Request.Kind != operation.KindRedeploy {
		t.Fatalf("redeploy kind = %s", op.Request.Kind)
	}
}

// A remedy dash cannot re-render safely stays report-only with the argv
// shown — never an unknown command executed.
func TestCleanupRemedyRefusesUnknownCommandShape(t *testing.T) {
	cs := newCleanupServer(t, auditEnvelopeJSON("ok", findingFuture))
	runAudit(t, cs, "prod")
	waitCleanupRecord(t, cs, func(rec *cleanupServerRecord) bool { return rec.Envelope != nil })

	_, detail := cleanupDetail(t, cs, "prod")
	remedy := findingByID(t, detail, "future:x")["remedy"].(map[string]any)
	if remedy["runnable"] != false || remedy["note"] == "" {
		t.Fatalf("unknown remedy must explain itself: %v", remedy)
	}
	code, body := call(t, cs.Server.handleCleanupServer, "POST", "/api/cleanup/prod/remedy", `{"id":"future:x"}`)
	if code != http.StatusBadRequest || !strings.Contains(body["error"].(string), "run it with the CLI") {
		t.Fatalf("unknown command = %d: %v", code, body)
	}
}

// ── dismissals ───────────────────────────────────────────────────────────────

func TestCleanupDismissalLifecycle(t *testing.T) {
	cs := newCleanupServer(t, auditEnvelopeJSON("ok", strings.Join([]string{findingStaleRoute, findingReportOnly}, ",")))
	runAudit(t, cs, "prod")

	waitCleanupRecord(t, cs, func(rec *cleanupServerRecord) bool { return rec.Envelope != nil })
	code, body := call(t, cs.Server.handleCleanupServer, "POST", "/api/cleanup/prod/dismiss", `{"id":"stale_route:ghost"}`)
	if code != http.StatusOK {
		t.Fatalf("dismiss = %d: %v", code, body)
	}
	_, detail := cleanupDetail(t, cs, "prod")
	if f := findingByID(t, detail, "stale_route:ghost"); f["dismissed"] == nil {
		t.Fatalf("dismissed finding must be marked: %v", f)
	}
	if detail["data"].(map[string]any)["dismissed_count"] != float64(1) {
		t.Fatal("and counted")
	}

	// A fresh ok audit that still reports the finding keeps the dismissal.
	cs.cleanup.mu.Lock()
	auditedAt := cs.cleanup.snap.Servers["prod"].LastAttemptAt
	cs.cleanup.mu.Unlock()
	runAudit(t, cs, "prod")
	waitCleanupRecord(t, cs, func(rec *cleanupServerRecord) bool { return !rec.LastAttemptAt.Equal(auditedAt) })
	_, detail = cleanupDetail(t, cs, "prod")
	if findingByID(t, detail, "stale_route:ghost")["dismissed"] == nil {
		t.Fatal("a persisting finding stays dismissed")
	}

	// A fresh PARTIAL audit without the finding retires nothing: the check
	// that would have produced it could not be read, so it is missing, not
	// cleared.
	*cs.envelope = auditEnvelopeJSON("partial", findingReportOnly)
	runAudit(t, cs, "prod")
	waitCleanupRecord(t, cs, func(rec *cleanupServerRecord) bool { return rec.Envelope != nil && rec.Envelope.State == "partial" })
	if _, ok := cs.cleanup.snap.Dismissals["prod"]["stale_route:ghost"]; !ok {
		t.Fatal("a partial audit must not retire dismissals")
	}

	// A fresh ok audit WITHOUT the finding retires the dismissal.
	*cs.envelope = auditEnvelopeJSON("ok", findingReportOnly)
	runAudit(t, cs, "prod")
	waitCleanupRecord(t, cs, func(rec *cleanupServerRecord) bool {
		return rec.Envelope != nil && rec.Envelope.State == "ok" && len(rec.Envelope.Findings) == 1
	})
	if _, ok := cs.cleanup.snap.Dismissals["prod"]["stale_route:ghost"]; ok {
		t.Fatal("the dismissal must retire once an ok audit proves the finding cleared")
	}
	_, detail = cleanupDetail(t, cs, "prod")
	for _, f := range detail["data"].(map[string]any)["findings"].([]any) {
		if f.(map[string]any)["id"] == "stale_route:ghost" {
			t.Fatal("the cleared finding must be gone from a fresh ok audit")
		}
	}

	// Dismissing an unknown finding, and undismissing one not dismissed, 404.
	if code, _ := call(t, cs.Server.handleCleanupServer, "POST", "/api/cleanup/prod/dismiss", `{"id":"nope:x"}`); code != http.StatusNotFound {
		t.Fatalf("unknown finding dismiss = %d", code)
	}
	if code, _ := call(t, cs.Server.handleCleanupServer, "POST", "/api/cleanup/prod/undismiss", `{"id":"disk_pressure:/"}`); code != http.StatusNotFound {
		t.Fatalf("undismiss of a live finding = %d", code)
	}

	// Undismiss restores the finding to the default view, audited.
	call(t, cs.Server.handleCleanupServer, "POST", "/api/cleanup/prod/dismiss", `{"id":"disk_pressure:/"}`)
	if code, _ := call(t, cs.Server.handleCleanupServer, "POST", "/api/cleanup/prod/undismiss", `{"id":"disk_pressure:/"}`); code != http.StatusOK {
		t.Fatal("undismiss failed")
	}
	_, detail = cleanupDetail(t, cs, "prod")
	if f := findingByID(t, detail, "disk_pressure:/"); f["dismissed"] != nil {
		t.Fatalf("undismissed finding must be live again: %v", f)
	}
}

// Dismissals and remedies land in the audit trail with their actor.
func TestCleanupActionsAreAudited(t *testing.T) {
	cs := newCleanupServer(t, auditEnvelopeJSON("ok", strings.Join([]string{findingOrphan, findingStaleRoute}, ",")))
	runAudit(t, cs, "prod")
	waitCleanupRecord(t, cs, func(rec *cleanupServerRecord) bool { return rec.Envelope != nil })
	call(t, cs.Server.handleCleanupServer, "POST", "/api/cleanup/prod/dismiss", `{"id":"stale_route:ghost"}`)
	call(t, cs.Server.handleCleanupServer, "POST", "/api/cleanup/prod/remedy", `{"id":"orphan_state:orphan","confirm":"orphan"}`)
	waitForLastOperation(t, cs)

	var sawDismiss, sawRemedy, sawUndismiss bool
	for _, e := range cs.alerts.audit.Recent(0) {
		switch {
		case e.Action == "cleanup.dismiss" && strings.Contains(e.Target, "stale_route:ghost"):
			sawDismiss = true
		case e.Action == "cleanup.remedy" && strings.Contains(e.Target, "orphan_state:orphan") && strings.Contains(e.Detail, "destructive"):
			sawRemedy = true
		case e.Action == "cleanup.undismiss":
			sawUndismiss = true
		}
	}
	if !sawDismiss || !sawRemedy || sawUndismiss {
		t.Fatalf("audit trail wrong (dismiss=%v remedy=%v undismiss=%v)", sawDismiss, sawRemedy, sawUndismiss)
	}
}

func waitForLastOperation(t *testing.T, cs *cleanupServer) {
	t.Helper()
	ops := cs.operations.List("", "", 10)
	for _, op := range ops {
		waitForOperation(t, cs.Server, op.ID)
	}
}

// ── gates ────────────────────────────────────────────────────────────────────

// Without the token, the page still renders (honestly, with the remedy) but
// nothing runs and no remedy can be admitted.
func TestCleanupGateWithOlderCLI(t *testing.T) {
	c := &configRunner{tokens: allConfigTokens} // no server-audit-v1
	s := newConfigServer(t, c)
	code, body := call(t, s.handleCleanup, "GET", "/api/cleanup", "")
	if code != http.StatusOK {
		t.Fatalf("fleet view = %d", code)
	}
	data := body["data"].(map[string]any)
	if data["cli_supported"] != false || data["min_cli_version"] != cleanupMinCLI {
		t.Fatalf("gate fields = %v", data)
	}
	if note, _ := data["note"].(string); !strings.Contains(note, cleanupMinCLI) {
		t.Fatalf("note must name the CLI release: %v", note)
	}
	for _, tc := range []struct{ path, body string }{
		{"/api/cleanup/prod/audit", ""},
		{"/api/cleanup/prod/remedy", `{"id":"x"}`},
	} {
		code, body := call(t, s.handleCleanupServer, "POST", tc.path, tc.body)
		if code != http.StatusNotImplemented || !strings.Contains(body["error"].(string), "newer teploy CLI") {
			t.Fatalf("POST %s with an older CLI = %d: %v", tc.path, code, body)
		}
	}
	if len(c.calls) != 0 {
		t.Fatalf("an unsupported surface must not run the CLI: %v", c.calls)
	}
}

func TestCleanupRouteCapabilities(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		want         string
	}{
		{http.MethodGet, "/api/cleanup", caps.ViewMetadata},
		{http.MethodGet, "/api/cleanup/prod", caps.ViewMetadata},
		{http.MethodPost, "/api/cleanup/prod/audit", caps.ExecuteDeploy},
		{http.MethodPost, "/api/cleanup/prod/remedy", caps.ExecuteDeploy},
		{http.MethodPost, "/api/cleanup/prod/dismiss", caps.ExecuteMutate},
		{http.MethodPost, "/api/cleanup/prod/undismiss", caps.ExecuteMutate},
	} {
		if got := requiredCapabilities(tc.method, tc.path); len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s %s = %v, want [%s]", tc.method, tc.path, got, tc.want)
		}
	}
}

// The persisted state file is dash-owned and round-trips through a restart.
func TestCleanupStateRoundTrip(t *testing.T) {
	cs := newCleanupServer(t, auditEnvelopeJSON("ok", findingStaleRoute))
	runAudit(t, cs, "prod")
	waitCleanupRecord(t, cs, func(rec *cleanupServerRecord) bool { return rec.Envelope != nil })
	call(t, cs.Server.handleCleanupServer, "POST", "/api/cleanup/prod/dismiss", `{"id":"stale_route:ghost"}`)
	cs.DrainOperations(context.Background())

	restarted := New(Config{
		DataDir: cs.config.DataDir, NoAuth: true,
		CLIInstalled: func() bool { return true },
		CLIRunner:    cs.runner.run, CLIStdinRunner: cs.runner.runStdin,
		CLIIdentity: func() string { return "test-cli" },
	})
	t.Cleanup(func() { restarted.DrainOperations(context.Background()) })
	if restarted.cleanup.snap.Owner != cleanupOwner {
		t.Fatalf("owner marker = %q", restarted.cleanup.snap.Owner)
	}
	if restarted.cleanup.snap.Dismissals["prod"]["stale_route:ghost"].At.IsZero() {
		t.Fatal("dismissal must survive a restart")
	}
	if rec := restarted.cleanup.snap.Servers["prod"]; rec == nil || rec.Envelope == nil || rec.Envelope.State != "ok" {
		t.Fatalf("record after restart = %+v", restarted.cleanup.snap.Servers["prod"])
	}
}
