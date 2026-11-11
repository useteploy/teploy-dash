package server

// cleanup.go — Slice 6 (plan section 9, Dash half): the fleet cleanup page
// riding `teploy server audit <server> --json` (capability server-audit-v1,
// contracts corpus rev 19). Dash is the face: the CLI owns every judgement
// about what is left over on a server and what fixes it. Dash renders the
// findings, runs the remedies the CLI names (as durable operations, re-built
// from its own allowlist — never the envelope's argv passed through), and
// remembers which findings an operator dismissed.
//
// Honesty rules, mirroring the domains page:
//
//   - An audit is SLOW (the CLI bounds one at 5 minutes over SSH), so it runs
//     as a durable operation; the page reads the cached envelope, with its
//     age, and a failed run keeps the last report instead of clearing it.
//   - state partial/unreachable is never rendered as clean. A partial audit
//     is missing the findings its unknown checks would have produced; an
//     unreachable audit produced an envelope that says so and nothing else.
//   - A remedy button exists only where the CLI set remedy.argv. Report-only
//     findings show their evidence and no button. A remedy dash cannot
//     re-render safely (unknown command shape) is report-only here too, with
//     the argv shown so the operator can run it themselves.
//   - Destructive remedies ask for the finding's app or subject name typed
//     exactly, enforced server-side, not just in the UI.
//   - Dismissals are per finding id (kind:subject, stable across runs),
//     audited, hidden from the default view but counted. A fresh audit that
//     could see the finding's domain (state ok) and did not report it retires
//     the dismissal; a partial or unreachable audit retires nothing — missing
//     is not cleared.
//
// State is one dash-owned file-store record: <data>/server-audit.json.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/useteploy/teploy-dash/internal/cli"
	"github.com/useteploy/teploy-dash/internal/durable"
	"github.com/useteploy/teploy-dash/internal/operation"
	"github.com/useteploy/teploy-dash/internal/store"
)

// cleanupMinCLI is the release that brings `server audit`: a CLI that does
// not advertise server-audit-v1 reads as unsupported, whatever its version.
const cleanupMinCLI = "v0.1.53"

// ── the CLI's envelope (contracts/schema/server-audit-envelope.schema.json) ─

type serverAuditEnvelope struct {
	MachineInterface int                  `json:"machine_interface"`
	Server           string               `json:"server"`
	Host             string               `json:"host"`
	AuditedAt        time.Time            `json:"audited_at"`
	State            string               `json:"state"` // ok | partial | unreachable
	Error            string               `json:"error,omitempty"`
	Checks           []serverAuditCheck   `json:"checks"`
	Disk             *serverAuditDisk     `json:"disk"`
	Findings         []serverAuditFinding `json:"findings"`
}

type serverAuditCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | unknown
	Detail string `json:"detail,omitempty"`
}

type serverAuditDisk struct {
	Path           string                   `json:"path"`
	AvailableBytes uint64                   `json:"available_bytes"`
	UsedPercent    int                      `json:"used_percent"`
	Attribution    []serverAuditAttribution `json:"attribution"`
}

type serverAuditAttribution struct {
	Name      string `json:"name"`
	SizeBytes uint64 `json:"size_bytes"`
}

// serverAuditFinding keeps kind and severity as plain strings on purpose: an
// audit from a newer CLI may name a kind this dash has never heard of, and
// the page renders it with its evidence rather than dropping it.
type serverAuditFinding struct {
	ID        string            `json:"id"` // kind:subject — stable across runs
	Kind      string            `json:"kind"`
	Severity  string            `json:"severity"` // critical | warning | info
	Subject   string            `json:"subject"`
	App       string            `json:"app,omitempty"`
	Evidence  string            `json:"evidence"`
	SizeBytes *uint64           `json:"size_bytes,omitempty"`
	Related   []string          `json:"related,omitempty"`
	Remedy    serverAuditRemedy `json:"remedy"`
}

type serverAuditRemedy struct {
	// Argv is nil when the CLI has no safe fix (report only).
	Argv        []string `json:"argv"`
	Destructive bool     `json:"destructive"`
	Confirm     bool     `json:"confirm"`
}

// decodeServerAuditEnvelope runs one audit payload through the central MI
// gate (a newer-machine-interface envelope is refused, never interpreted)
// and the completeness rules the page relies on.
func decodeServerAuditEnvelope(raw string) (*serverAuditEnvelope, error) {
	if _, err := cli.ParseJSON(raw); err != nil {
		return nil, err
	}
	var env serverAuditEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		return nil, fmt.Errorf("decoding the server audit: %w", err)
	}
	switch env.State {
	case "ok", "partial", "unreachable":
	default:
		return nil, fmt.Errorf("decoding the server audit: unknown state %q", env.State)
	}
	if env.Server == "" || env.AuditedAt.IsZero() || env.Checks == nil || env.Findings == nil {
		return nil, fmt.Errorf("decoding the server audit: incomplete machine response")
	}
	for _, f := range env.Findings {
		if f.ID == "" || f.Kind == "" || f.Subject == "" || f.Evidence == "" {
			return nil, fmt.Errorf("decoding the server audit: incomplete machine response for finding %q", f.ID)
		}
	}
	return &env, nil
}

// ── persisted state ──────────────────────────────────────────────────────────

// cleanupServerRecord is one server's newest audit outcome. Envelope is the
// last SUCCESSFUL run's report; a failed attempt keeps it, with its age and
// the failure, instead of clearing the page.
type cleanupServerRecord struct {
	Server        string               `json:"server"`
	Host          string               `json:"host,omitempty"`
	Envelope      *serverAuditEnvelope `json:"envelope,omitempty"`
	OperationID   string               `json:"operation_id,omitempty"`
	LastAttemptAt time.Time            `json:"last_attempt_at"`
	LastError     string               `json:"last_error,omitempty"`
}

type cleanupDismissal struct {
	At    time.Time `json:"at"`
	Actor string    `json:"actor"`
}

type cleanupSnapshot struct {
	// Owner marks the file dash-owned (fleet caches under <data>/), so a
	// future reader can tell a dash record from CLI state.
	Owner      string                                 `json:"owner"`
	Servers    map[string]*cleanupServerRecord        `json:"servers"`
	Dismissals map[string]map[string]cleanupDismissal `json:"dismissals"`
}

const cleanupOwner = "teploy-dash"

type cleanupState struct {
	path string
	mu   sync.Mutex
	snap cleanupSnapshot
}

func (s *Server) initCleanup(dataDir string) {
	c := &s.cleanup
	c.snap = cleanupSnapshot{Owner: cleanupOwner, Servers: map[string]*cleanupServerRecord{}, Dismissals: map[string]map[string]cleanupDismissal{}}
	if dataDir == "" {
		return
	}
	c.path = filepath.Join(dataDir, "server-audit.json")
	raw, err := os.ReadFile(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var snap cleanupSnapshot
	if err == nil {
		err = json.Unmarshal(raw, &snap)
	}
	if err != nil {
		// Unreadable state must not take the page down: set aside, start over.
		if aside := setAside(c.path, err); aside != nil {
			log.Printf("[cleanup] %s is unreadable (%v) and could not be set aside: %v", c.path, err, aside)
			c.path = ""
		}
		return
	}
	if snap.Servers == nil {
		snap.Servers = map[string]*cleanupServerRecord{}
	}
	if snap.Dismissals == nil {
		snap.Dismissals = map[string]map[string]cleanupDismissal{}
	}
	snap.Owner = cleanupOwner
	c.snap = snap
}

func (c *cleanupState) saveLocked() {
	if c.path == "" {
		return
	}
	raw, err := json.Marshal(c.snap)
	if err == nil {
		err = durable.Replace(c.path, raw, 0600)
	}
	if err != nil {
		log.Printf("[cleanup] persist failed (state in memory until the next audit): %v", err)
	}
}

// ── the audit operation ──────────────────────────────────────────────────────

// onCleanupOperationFinished harvests a finished server_audit operation's
// report into the cache. Wired beside onOperationFinished (both run; this
// one owns the cleanup state). The envelope IS the operation's stdout, so it
// is read back from the retained event journal — the same bytes the operator
// saw stream past in the operation view.
func (s *Server) onCleanupOperationFinished(op *operation.Operation) {
	if op == nil || op.Request.Kind != operation.KindServerAudit {
		return
	}
	now := time.Now().UTC()
	rec := &cleanupServerRecord{Server: op.Request.Server, LastAttemptAt: now}
	if srv, ok := s.lookupServer(op.Request.Server); ok {
		rec.Host = srv.Host
	}
	switch op.Status {
	case operation.StatusSucceeded:
		env, err := s.harvestServerAudit(op.ID)
		if err != nil {
			rec.LastError = "the audit ran but its report could not be read back: " + err.Error()
			log.Printf("[cleanup] %s", rec.LastError)
		} else {
			rec.Envelope, rec.OperationID = env, op.ID
		}
	default:
		rec.LastError = cleanupFailureMessage(op)
	}
	s.applyServerAuditResult(rec)
}

func cleanupFailureMessage(op *operation.Operation) string {
	why := op.Error
	if why == "" {
		why = string(op.Status)
	}
	return "the audit did not finish: " + why
}

// harvestServerAudit joins the operation's stdout events and decodes the
// envelope. The executor emits line events; the CLI's --json output is the
// whole of stdout.
func (s *Server) harvestServerAudit(opID string) (*serverAuditEnvelope, error) {
	if s.operations == nil {
		return nil, fmt.Errorf("operation service unavailable")
	}
	events, err := s.operations.EventsAfter(opID, 0)
	if err != nil {
		return nil, err
	}
	var out strings.Builder
	for _, e := range events {
		if e.Type == operation.EventStdout {
			out.WriteString(e.Data)
			out.WriteByte('\n')
		}
	}
	return decodeServerAuditEnvelope(out.String())
}

// applyServerAuditResult merges one finished run into the cache: a failure
// keeps the previous report, a success replaces it and retires the
// dismissals a complete audit proved cleared (state ok only — see the file
// comment). Records for servers no longer in the fleet go away here, where
// state is written, never on a read.
func (s *Server) applyServerAuditResult(fresh *cleanupServerRecord) {
	c := &s.cleanup
	c.mu.Lock()
	defer c.mu.Unlock()
	if prev := c.snap.Servers[fresh.Server]; prev != nil && fresh.Envelope == nil {
		fresh.Envelope, fresh.OperationID = prev.Envelope, prev.OperationID
	}
	c.snap.Servers[fresh.Server] = fresh
	if fresh.Envelope != nil && fresh.Envelope.State == "ok" {
		if dis := c.snap.Dismissals[fresh.Server]; dis != nil {
			present := make(map[string]bool, len(fresh.Envelope.Findings))
			for _, f := range fresh.Envelope.Findings {
				present[f.ID] = true
			}
			for id := range dis {
				if !present[id] {
					delete(dis, id)
				}
			}
			if len(dis) == 0 {
				delete(c.snap.Dismissals, fresh.Server)
			}
		}
	}
	for name := range c.snap.Servers {
		if name != fresh.Server && !s.serverConfigured(name) {
			delete(c.snap.Servers, name)
			delete(c.snap.Dismissals, name)
		}
	}
	c.saveLocked()
}

func (s *Server) serverConfigured(name string) bool {
	_, ok := s.lookupServer(name)
	return ok
}

// liveCleanupAudit names the audit operation already queued or running for a
// server, so the page cannot pile a second five-minute SSH sweep onto one.
func (s *Server) liveCleanupAudit(server string) *operation.Operation {
	if s.operations == nil {
		return nil
	}
	for _, op := range s.operations.ListLive(time.Now()) {
		if op.Request.Kind == operation.KindServerAudit && op.Request.Server == server && !op.Status.Terminal() {
			return op
		}
	}
	return nil
}

// ── remedy planning ──────────────────────────────────────────────────────────

// cleanupRemedyPlan is a finding's remedy re-rendered as an operation
// request. Built ONLY from validated pieces of the CLI's argv — the argv
// itself never reaches an exec.
type cleanupRemedyPlan struct {
	Label       string // e.g. "route remove", for the audit trail
	Request     operation.Request
	Destructive bool
}

// remedyAllowlistArgs are the only flags a remedy argv may carry; anything
// else means a newer CLI's remedy shape this dash does not know how to run.
var remedyAllowlistArgs = map[string]bool{
	"--app": true, "--host": true, "--keep": true, "-y": true, "--yes": true,
	"--build-cache": true, "--dangling": true, "--purge": true,
}

// planCleanupRemedy maps one finding's remedy.argv onto an operation
// request. The envelope's argv is the CLI's own suggestion, produced by the
// same engine that would run it — dash treats it as DATA: the command shape
// must be one dash knows how to build, every identifier must pass the
// operation package's grammar, and the host must be the audited server.
func planCleanupRemedy(rec *cleanupServerRecord, f serverAuditFinding) (*cleanupRemedyPlan, error) {
	argv := f.Remedy.Argv
	if len(argv) < 2 || argv[0] != "teploy" {
		return nil, fmt.Errorf("the finding has no remedy dash can run")
	}
	args := argv[1:]
	var command []string
	switch {
	case len(args) >= 2 && args[0] == "route" && args[1] == "remove":
		command, args = []string{"route", "remove"}, args[2:]
	case len(args) >= 2 && args[0] == "releases" && args[1] == "prune":
		command, args = []string{"releases", "prune"}, args[2:]
	case len(args) >= 2 && args[0] == "server" && args[1] == "prune":
		command, args = []string{"server", "prune"}, args[2:]
	case args[0] == "remove", args[0] == "redeploy":
		command, args = args[:1], args[1:]
	default:
		return nil, fmt.Errorf("dash cannot run this remedy itself (unknown command); run it with the CLI: teploy %s", strings.Join(args, " "))
	}

	flags := make(map[string]string)
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !remedyAllowlistArgs[a] {
			return nil, fmt.Errorf("dash cannot run this remedy itself (unrecognized %s); run it with the CLI: teploy %s", a, strings.Join(argv[1:], " "))
		}
		if a == "-y" || a == "--yes" || a == "--build-cache" || a == "--dangling" {
			flags[a] = ""
			continue
		}
		if i+1 >= len(args) {
			return nil, fmt.Errorf("the remedy's %s flag has no value", a)
		}
		i++
		flags[a] = args[i]
	}

	req := operation.Request{Server: rec.Server}
	if app := flags["--app"]; app != "" {
		req.App = app
	} else if f.App != "" {
		req.App = f.App
	}
	if host := flags["--host"]; host != "" && host != rec.Server && host != rec.Host && (rec.Envelope == nil || host != rec.Envelope.Host) {
		return nil, fmt.Errorf("the remedy names host %q, not the audited server", host)
	}

	switch strings.Join(command, " ") {
	case "redeploy":
		req.Kind = operation.KindRedeploy
	case "remove":
		req.Kind = operation.KindRemove
		_, purge := flags["--purge"]
		req.Purge = purge
	case "route remove":
		req.Kind = operation.KindRouteRemove
	case "releases prune":
		req.Kind = operation.KindReleasesPrune
		keep, err := strconv.Atoi(flags["--keep"])
		if err != nil {
			return nil, fmt.Errorf("the remedy did not say how many versions to keep; run it with the CLI")
		}
		req.Keep = keep
	case "server prune":
		req.Kind = operation.KindServerPrune
		// Dash runs the full reclaim the audit offers. A remedy naming only
		// one half is a shape this dash does not build (see AUDIT_OPEN).
		_, buildCache := flags["--build-cache"]
		_, dangling := flags["--dangling"]
		if !buildCache || !dangling {
			return nil, fmt.Errorf("dash runs the full reclaim (--build-cache --dangling); run a narrower prune with the CLI")
		}
	}
	label := strings.Join(command, " ") + " " + req.App
	return &cleanupRemedyPlan{Label: label, Request: req, Destructive: f.Remedy.Destructive}, nil
}

// remedyConfirmName is what a destructive remedy asks to have typed: the
// finding's app when it belongs to one, else its subject (the schema's own
// rule for the confirmation name).
func remedyConfirmName(f serverAuditFinding) string {
	if f.App != "" {
		return f.App
	}
	return f.Subject
}

// ── views ────────────────────────────────────────────────────────────────────

type cleanupLiveAuditView struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type cleanupCounts struct {
	Critical   int `json:"critical"`
	Warning    int `json:"warning"`
	Info       int `json:"info"`
	Dismissed  int `json:"dismissed"`
	Remediable int `json:"remediable"`
}

type cleanupFleetRow struct {
	Server        string                `json:"server"`
	Configured    bool                  `json:"configured"`
	State         string                `json:"state"` // ok|partial|unreachable|never|stale
	AuditedAt     *time.Time            `json:"audited_at,omitempty"`
	AgeSeconds    *int64                `json:"age_seconds,omitempty"`
	LastError     string                `json:"last_error,omitempty"`
	Error         string                `json:"error,omitempty"`
	ChecksUnknown int                   `json:"checks_unknown"`
	Counts        cleanupCounts         `json:"counts"`
	Disk          *cleanupDiskView      `json:"disk,omitempty"`
	LiveAudit     *cleanupLiveAuditView `json:"live_audit,omitempty"`
}

// cleanupDiskView is the root-disk line of a server's last audit, so the
// Home and Servers pages can show disk use without opening each report.
type cleanupDiskView struct {
	Path           string `json:"path"`
	AvailableBytes uint64 `json:"available_bytes"`
	UsedPercent    int    `json:"used_percent"`
}

// cleanupStaleAfter: a report older than this reads stale, not current. The
// audit is operator-triggered (no background sweep), so there is no cadence
// to derive this from; a week matches how long a leftover is plausible.
const cleanupStaleAfter = 7 * 24 * time.Hour

func cleanupFleetCounts(env *serverAuditEnvelope, dismissed map[string]cleanupDismissal) (cleanupCounts, int) {
	var counts cleanupCounts
	for _, f := range env.Findings {
		if _, ok := dismissed[f.ID]; ok {
			counts.Dismissed++
			continue
		}
		switch f.Severity {
		case "critical":
			counts.Critical++
		case "warning":
			counts.Warning++
		default:
			// info, and any severity a newer CLI introduced: it renders with
			// the infos until dash learns it.
			counts.Info++
		}
		if f.Remedy.Argv != nil {
			counts.Remediable++
		}
	}
	unknown := 0
	for _, c := range env.Checks {
		if c.Status == "unknown" {
			unknown++
		}
	}
	return counts, unknown
}

func (s *Server) cleanupFleetView(ctx context.Context, now time.Time) map[string]any {
	caps := s.capabilities(ctx)
	supported := caps.CLI.Installed && caps.Features.ServerAudit

	configured := make(map[string]bool)
	discoveryError := ""
	listCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	servers, err := s.resolveServers(listCtx)
	cancel()
	if err != nil {
		discoveryError = err.Error()
	} else {
		for _, srv := range servers {
			configured[srv.Name] = true
		}
	}

	c := &s.cleanup
	c.mu.Lock()
	defer c.mu.Unlock()

	names := make([]string, 0, len(configured)+len(c.snap.Servers))
	for name := range configured {
		names = append(names, name)
	}
	for name := range c.snap.Servers {
		if !configured[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	rows := make([]cleanupFleetRow, 0, len(names))
	for _, name := range names {
		row := cleanupFleetRow{Server: name, Configured: configured[name], State: "never"}
		if op := s.liveCleanupAudit(name); op != nil {
			row.LiveAudit = &cleanupLiveAuditView{ID: op.ID, Status: string(op.Status)}
		}
		if rec := c.snap.Servers[name]; rec != nil {
			row.LastError = rec.LastError
			if env := rec.Envelope; env != nil {
				row.State = env.State
				row.Error = env.Error
				at := env.AuditedAt
				age := int64(now.Sub(at) / time.Second)
				row.AuditedAt, row.AgeSeconds = &at, &age
				if now.Sub(at) > cleanupStaleAfter && row.LastError == "" {
					row.State = "stale"
				}
				row.ChecksUnknown = countUnknownChecks(env)
				row.Counts, _ = cleanupFleetCounts(env, c.snap.Dismissals[name])
				if d := env.Disk; d != nil {
					row.Disk = &cleanupDiskView{Path: d.Path, AvailableBytes: d.AvailableBytes, UsedPercent: d.UsedPercent}
				}
			}
		}
		rows = append(rows, row)
	}

	view := map[string]any{
		"servers":         rows,
		"cli_supported":   supported,
		"cli_version":     caps.CLI.Version,
		"min_cli_version": cleanupMinCLI,
	}
	if discoveryError != "" {
		view["discovery_error"] = "the server list could not be read: " + discoveryError + "; rows below are from the last audit"
	}
	if !supported {
		view["note"] = "Server audits need teploy " + cleanupMinCLI + " or later on the dashboard host."
	}
	return view
}

func countUnknownChecks(env *serverAuditEnvelope) int {
	n := 0
	for _, c := range env.Checks {
		if c.Status == "unknown" {
			n++
		}
	}
	return n
}

type cleanupRemedyView struct {
	Available   bool     `json:"available"` // the CLI named a command
	Runnable    bool     `json:"runnable"`  // dash can build it
	Note        string   `json:"note,omitempty"`
	Argv        []string `json:"argv,omitempty"` // shown, never executed as-is
	Destructive bool     `json:"destructive"`
	Confirm     bool     `json:"confirm"`
}

type cleanupFindingView struct {
	serverAuditFinding
	Remedy    cleanupRemedyView `json:"remedy"`
	Dismissed *cleanupDismissal `json:"dismissed,omitempty"`
}

type cleanupServerView struct {
	Server         string                `json:"server"`
	Configured     bool                  `json:"configured"`
	State          string                `json:"state"`
	Error          string                `json:"error,omitempty"`
	LastError      string                `json:"last_error,omitempty"`
	AuditedAt      *time.Time            `json:"audited_at,omitempty"`
	AgeSeconds     *int64                `json:"age_seconds,omitempty"`
	OperationID    string                `json:"operation_id,omitempty"`
	Checks         []serverAuditCheck    `json:"checks"`
	Disk           *serverAuditDisk      `json:"disk,omitempty"`
	Findings       []cleanupFindingView  `json:"findings"`
	DismissedCount int                   `json:"dismissed_count"`
	LiveAudit      *cleanupLiveAuditView `json:"live_audit,omitempty"`
	CLISupported   bool                  `json:"cli_supported"`
	CLIVersion     string                `json:"cli_version"`
	MinCLIVersion  string                `json:"min_cli_version"`
}

func (s *Server) cleanupServerView(ctx context.Context, name string, now time.Time) (*cleanupServerView, bool) {
	caps := s.capabilities(ctx)
	_, configured := s.lookupServer(name)

	c := &s.cleanup
	c.mu.Lock()
	defer c.mu.Unlock()
	rec := c.snap.Servers[name]
	if rec == nil && !configured {
		return nil, false
	}
	view := &cleanupServerView{
		Server:        name,
		Configured:    configured,
		State:         "never",
		Checks:        []serverAuditCheck{},
		Findings:      []cleanupFindingView{},
		CLISupported:  caps.CLI.Installed && caps.Features.ServerAudit,
		CLIVersion:    caps.CLI.Version,
		MinCLIVersion: cleanupMinCLI,
	}
	if op := s.liveCleanupAudit(name); op != nil {
		view.LiveAudit = &cleanupLiveAuditView{ID: op.ID, Status: string(op.Status)}
	}
	if rec == nil {
		return view, true
	}
	view.LastError = rec.LastError
	view.OperationID = rec.OperationID
	if rec.Envelope == nil {
		return view, true
	}
	env := rec.Envelope
	view.State, view.Error = env.State, env.Error
	at := env.AuditedAt
	age := int64(now.Sub(at) / time.Second)
	view.AuditedAt, view.AgeSeconds = &at, &age
	view.Checks, view.Disk = env.Checks, env.Disk

	dismissed := c.snap.Dismissals[name]
	for _, f := range env.Findings {
		fv := cleanupFindingView{serverAuditFinding: f, Remedy: cleanupRemedyView{
			Available: f.Remedy.Argv != nil, Destructive: f.Remedy.Destructive, Confirm: f.Remedy.Confirm,
		}}
		if f.Remedy.Argv != nil {
			fv.Remedy.Argv = f.Remedy.Argv
			if _, err := planCleanupRemedy(rec, f); err != nil {
				fv.Remedy.Note = err.Error()
			} else {
				fv.Remedy.Runnable = true
			}
		}
		if d, ok := dismissed[f.ID]; ok {
			d := d
			fv.Dismissed = &d
			view.DismissedCount++
		}
		view.Findings = append(view.Findings, fv)
	}
	return view, true
}

// ── the API ──────────────────────────────────────────────────────────────────

func (s *Server) handleCleanup(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeData(w, s.cleanupFleetView(r.Context(), time.Now().UTC()))
}

func (s *Server) handleCleanupServer(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/cleanup/"), "/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		writeErrorStatus(w, "cleanup route not found", http.StatusNotFound)
		return
	}
	serverName, action := parts[0], ""
	if len(parts) > 2 {
		writeErrorStatus(w, "cleanup route not found", http.StatusNotFound)
		return
	}
	if len(parts) == 2 {
		action = parts[1]
	}
	if !store.ValidID(serverName) {
		writeError(w, "invalid server name")
		return
	}

	switch {
	case action == "" && r.Method == http.MethodGet:
		view, ok := s.cleanupServerView(r.Context(), serverName, time.Now().UTC())
		if !ok {
			writeErrorStatus(w, "server not found: "+serverName, http.StatusNotFound)
			return
		}
		writeData(w, view)
	case action == "audit" && r.Method == http.MethodPost:
		s.handleCleanupAuditRun(w, r, serverName)
	case action == "remedy" && r.Method == http.MethodPost:
		s.handleCleanupRemedy(w, r, serverName)
	case action == "dismiss" && r.Method == http.MethodPost:
		s.handleCleanupDismissal(w, r, serverName, true)
	case action == "undismiss" && r.Method == http.MethodPost:
		s.handleCleanupDismissal(w, r, serverName, false)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleCleanupAuditRun queues the durable `server audit` operation for one
// server. One at a time per server: the audit SSHes a dozen reads, and a
// second concurrent sweep proves nothing the first will not.
func (s *Server) handleCleanupAuditRun(w http.ResponseWriter, r *http.Request, serverName string) {
	if !s.requireFeatureMin(w, r, func(f machineFeatures) bool { return f.ServerAudit }, "Server audit", cleanupMinCLI) {
		return
	}
	if _, ok := s.lookupServer(serverName); !ok {
		writeErrorStatus(w, "server not found: "+serverName, http.StatusNotFound)
		return
	}
	if live := s.liveCleanupAudit(serverName); live != nil {
		writeErrorStatus(w, fmt.Sprintf("an audit of %s is already %s (operation %s)", serverName, live.Status, live.ID), http.StatusConflict)
		return
	}
	s.enqueueOperation(w, r, operation.Request{Kind: operation.KindServerAudit, Server: serverName})
}

// handleCleanupRemedy runs one finding's remedy: the finding must exist in
// the cached report, its remedy must be one dash can build, and a
// destructive remedy must arrive with the app/subject name typed exactly —
// enforced here, so the UI's dialog is convenience, not the fence.
func (s *Server) handleCleanupRemedy(w http.ResponseWriter, r *http.Request, serverName string) {
	if !s.requireFeatureMin(w, r, func(f machineFeatures) bool { return f.ServerAudit }, "Server audit", cleanupMinCLI) {
		return
	}
	var body struct {
		ID      string `json:"id"`
		Confirm string `json:"confirm"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body: "+err.Error())
		return
	}
	finding, rec, ok := s.cleanupFinding(serverName, body.ID)
	if !ok {
		writeErrorStatus(w, "no such finding in "+serverName+"'s last audit", http.StatusNotFound)
		return
	}
	if finding.Remedy.Argv == nil {
		writeError(w, "this finding is report-only; the CLI named no command that fixes it")
		return
	}
	plan, err := planCleanupRemedy(rec, *finding)
	if err != nil {
		writeError(w, err.Error())
		return
	}
	if plan.Destructive {
		if want := remedyConfirmName(*finding); strings.TrimSpace(body.Confirm) != want {
			writeErrorStatus(w, "type "+want+" to confirm", http.StatusBadRequest)
			return
		}
	}
	s.auditf(r, "cleanup.remedy", "server/"+serverName+" finding/"+body.ID, "%s (the audit's remedy%s)", plan.Label, map[bool]string{true: ", destructive, confirmed by typed name", false: ""}[plan.Destructive])
	s.enqueueOperation(w, r, plan.Request)
}

// handleCleanupDismissal hides (or restores) one finding. Dismissals are
// per finding id and audited; a fresh audit that proves the finding cleared
// retires the dismissal (see applyServerAuditResult).
func (s *Server) handleCleanupDismissal(w http.ResponseWriter, r *http.Request, serverName string, dismiss bool) {
	var body struct {
		ID string `json:"id"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body: "+err.Error())
		return
	}
	c := &s.cleanup
	c.mu.Lock()
	rec := c.snap.Servers[serverName]
	var finding *serverAuditFinding
	if rec != nil && rec.Envelope != nil {
		for i := range rec.Envelope.Findings {
			if rec.Envelope.Findings[i].ID == body.ID {
				finding = &rec.Envelope.Findings[i]
				break
			}
		}
	}
	if finding == nil {
		c.mu.Unlock()
		writeErrorStatus(w, "no such finding in "+serverName+"'s last audit", http.StatusNotFound)
		return
	}
	if dismiss {
		if c.snap.Dismissals[serverName] == nil {
			c.snap.Dismissals[serverName] = map[string]cleanupDismissal{}
		}
		c.snap.Dismissals[serverName][body.ID] = cleanupDismissal{At: time.Now().UTC(), Actor: requestActor(r)}
	} else {
		if _, ok := c.snap.Dismissals[serverName][body.ID]; !ok {
			c.mu.Unlock()
			writeErrorStatus(w, "finding "+body.ID+" is not dismissed", http.StatusNotFound)
			return
		}
		delete(c.snap.Dismissals[serverName], body.ID)
		if len(c.snap.Dismissals[serverName]) == 0 {
			delete(c.snap.Dismissals, serverName)
		}
	}
	c.saveLocked()
	c.mu.Unlock()

	action := "cleanup.undismiss"
	if dismiss {
		action = "cleanup.dismiss"
	}
	s.auditf(r, action, "server/"+serverName+" finding/"+body.ID, "%s: %s (%s)", map[bool]string{true: "dismissed", false: "restored"}[dismiss], finding.Evidence, finding.ID)
	writeData(w, map[string]bool{"ok": true})
}

// cleanupFinding looks one finding up in the server's cached report.
func (s *Server) cleanupFinding(serverName, id string) (*serverAuditFinding, *cleanupServerRecord, bool) {
	c := &s.cleanup
	c.mu.Lock()
	defer c.mu.Unlock()
	rec := c.snap.Servers[serverName]
	if rec == nil || rec.Envelope == nil {
		return nil, nil, false
	}
	for i := range rec.Envelope.Findings {
		if rec.Envelope.Findings[i].ID == id {
			f := rec.Envelope.Findings[i]
			return &f, rec, true
		}
	}
	return nil, nil, false
}
