package server

// backupsetup.go — "Protect my data": the simple path to verified, encrypted, off-site
// backups. It is a thin flow over the same engine as the manual form: `server
// backup-target set`, `server backup-policy set`, `backup run --all`. Dash holds no
// secret: the Backblaze key lives in one request, each server's minted key goes to that
// server on stdin and is dropped, and the private recovery key never leaves the browser.
//
// A server with a target is never touched unless the person ticked it, because a push
// replaces its target and redirects its legacy cron lines. Every endpoint names its
// servers, so there is no "all" to get wrong.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/useteploy/teploy-dash/internal/b2"
	"github.com/useteploy/teploy-dash/internal/cli"
	"github.com/useteploy/teploy-dash/internal/monitor"
)

const (
	// backupSetupMinCLI is the first CLI with backup-setup-v1.
	backupSetupMinCLI = "v0.1.55"
	// backupSetupBase is the fixed base prefix of the simple flow.
	backupSetupBase = "teploy"
	// backupProofBytes is how much of an object the browser gets for the proof.
	backupProofBytes = 128 * 1024
	// backupMinFreeBytes: below this a first backup is likely to run out of room.
	backupMinFreeBytes = int64(2) << 30
	backupRulesRetries = 3
	backupStartNow     = "now"
	backupStartNext    = "next"
	backupKeyMinted    = "minted"
	backupKeyPushing   = "pushing"
	backupKeyPushed    = "pushed"
	backupKeyVerified  = "verified"
	// backupKeyAmbiguous: the push failed, and the server may or may not hold this key.
	backupKeyAmbiguous         = "ambiguous"
	backupDefaultSetupInterval = "24h"
	// backupRecoveryDoc is the recovery page the UI links.
	backupRecoveryDoc = "https://github.com/useteploy/teploy-dash/blob/main/docs/recovery.md"
)

var (
	backupAgeRecipientRE = regexp.MustCompile(`^age1[0-9a-z]{58}$`)
	backupFingerprintRE  = regexp.MustCompile(`^[A-Za-z0-9:._+/=-]{1,128}$`)
	backupKeyNameBadRE   = regexp.MustCompile(`[^a-zA-Z0-9-]+`)
)

// backupSetupRecord is what the flow remembers. Key ids identify a key but cannot use
// it; no secret is ever here.
type backupSetupRecord struct {
	// Bucket is recorded before Dash creates it, so a retry never creates a second.
	Bucket    string `json:"bucket,omitempty"`
	BucketID  string `json:"bucket_id,omitempty"`
	HideDays  int    `json:"hide_days,omitempty"`
	S3Managed bool   `json:"s3_managed,omitempty"`
	// Keys is each server's key, by server name.
	Keys            map[string]*backupServerKey        `json:"keys,omitempty"`
	Leftovers       []backupLeftoverKey                `json:"leftovers,omitempty"`
	Preflight       map[string]*backupPreflightSummary `json:"preflight,omitempty"`
	Runs            map[string]*backupSetupRun         `json:"runs,omitempty"`
	AlertsSkippedAt *time.Time                         `json:"alerts_skipped_at,omitempty"`
	TurnedOnAt      *time.Time                         `json:"turned_on_at,omitempty"`
	Proof           *backupProofRecord                 `json:"proof,omitempty"`
	// Skipped is what a person chose to leave out at turn-on, by server.
	Skipped map[string][]string `json:"skipped,omitempty"`
}

type backupServerKey struct {
	KeyID     string     `json:"key_id"`
	KeyName   string     `json:"key_name"`
	Bucket    string     `json:"bucket"`
	Prefix    string     `json:"prefix"`
	State     string     `json:"state"`
	Lifecycle string     `json:"lifecycle,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	Verified  *time.Time `json:"verified_at,omitempty"`
	// Replaces are older keys a server may still be using. They go only after a push
	// whose own call succeeded and then verified, in that same operation: reachability
	// cannot say which key a server holds.
	Replaces []backupLeftoverKey `json:"replaces,omitempty"`
}

// backupLeftoverKey is a key Dash wanted gone and could not delete.
type backupLeftoverKey struct {
	KeyID   string `json:"key_id"`
	KeyName string `json:"key_name"`
	Server  string `json:"server"`
	Reason  string `json:"reason"`
}

type backupSetupRun struct {
	RequestedAt time.Time `json:"requested_at"`
	Status      string    `json:"status"`
	Error       string    `json:"error,omitempty"`
}

type backupProofRecord struct {
	At     time.Time `json:"at"`
	Server string    `json:"server"`
	Object string    `json:"object"`
	Actor  string    `json:"actor"`
}

func (b *backupState) setupLocked() *backupSetupRecord {
	if b.rec.Setup == nil {
		b.rec.Setup = &backupSetupRecord{}
	}
	if b.rec.Setup.Keys == nil {
		b.rec.Setup.Keys = map[string]*backupServerKey{}
	}
	if b.rec.Setup.Preflight == nil {
		b.rec.Setup.Preflight = map[string]*backupPreflightSummary{}
	}
	if b.rec.Setup.Runs == nil {
		b.rec.Setup.Runs = map[string]*backupSetupRun{}
	}
	return b.rec.Setup
}

// ── the CLI's envelopes ──────────────────────────────────────────────────────

type backupPreflightTool struct {
	State   string `json:"state"`
	Version string `json:"version,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

type backupPreflight struct {
	MachineInterface int       `json:"machine_interface"`
	Host             string    `json:"host"`
	ObservedAt       time.Time `json:"observed_at"`
	Reachable        bool      `json:"reachable"`
	Error            string    `json:"error,omitempty"`
	Binary           struct {
		Path        string `json:"path"`
		State       string `json:"state"`
		Version     string `json:"version,omitempty"`
		FleetEngine *bool  `json:"fleet_engine"`
		CLIVersion  string `json:"cli_version"`
		SameAsCLI   *bool  `json:"same_as_cli"`
		Detail      string `json:"detail,omitempty"`
	} `json:"binary"`
	Tools struct {
		AWS     *backupPreflightTool `json:"aws"`
		Age     *backupPreflightTool `json:"age"`
		Python3 *backupPreflightTool `json:"python3"`
	} `json:"tools"`
	Disk struct {
		Path      string `json:"path"`
		FreeBytes *int64 `json:"free_bytes"`
		Detail    string `json:"detail,omitempty"`
	} `json:"disk"`
	Target struct {
		State          string `json:"state"`
		Bucket         string `json:"bucket,omitempty"`
		Endpoint       string `json:"endpoint,omitempty"`
		Prefix         string `json:"prefix,omitempty"`
		HasCredentials bool   `json:"has_credentials"`
		Detail         string `json:"detail,omitempty"`
	} `json:"target"`
	Policy struct {
		State            string   `json:"state"`
		Interval         string   `json:"interval,omitempty"`
		Encrypted        bool     `json:"encrypted"`
		CronInstalled    *bool    `json:"cron_installed"`
		ManagedElsewhere string   `json:"managed_elsewhere"`
		ManagedReasons   []string `json:"managed_reasons,omitempty"`
		Detail           string   `json:"detail,omitempty"`
	} `json:"policy"`
	ToolsInstall []struct {
		Tool   string `json:"tool"`
		Action string `json:"action"`
		Detail string `json:"detail,omitempty"`
	} `json:"tools_install,omitempty"`
	BinaryUpgrade *struct {
		Action string `json:"action"`
		From   string `json:"from,omitempty"`
		To     string `json:"to,omitempty"`
		Detail string `json:"detail,omitempty"`
	} `json:"binary_upgrade,omitempty"`
}

func decodeBackupPreflight(stdout string) (*backupPreflight, error) {
	if _, err := cli.ParseJSON(stdout); err != nil {
		return nil, err
	}
	var env backupPreflight
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		return nil, fmt.Errorf("decoding backup preflight: %w", err)
	}
	if env.Host == "" || env.ObservedAt.IsZero() || env.Binary.State == "" || env.Target.State == "" || env.Policy.State == "" ||
		env.Tools.AWS == nil || env.Tools.Age == nil || env.Tools.Python3 == nil {
		return nil, errors.New("decoding backup preflight: incomplete machine response")
	}
	return &env, nil
}

type backupHead struct {
	MachineInterface int    `json:"machine_interface"`
	Host             string `json:"host"`
	Key              string `json:"key"`
	Encrypted        bool   `json:"encrypted"`
	RequestedBytes   int    `json:"requested_bytes"`
	Length           int    `json:"length"`
	TotalSize        *int64 `json:"total_size"`
	DataBase64       string `json:"data_base64"`
}

func decodeBackupHead(stdout string) (*backupHead, error) {
	if _, err := cli.ParseJSON(stdout); err != nil {
		return nil, err
	}
	var env backupHead
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		return nil, fmt.Errorf("decoding backup head: %w", err)
	}
	if env.Key == "" || env.DataBase64 == "" || env.Length <= 0 {
		return nil, errors.New("decoding backup head: incomplete machine response")
	}
	return &env, nil
}

type backupDetach struct {
	MachineInterface int       `json:"machine_interface"`
	Host             string    `json:"host"`
	RequestedAt      time.Time `json:"requested_at"`
	Status           string    `json:"status"`
	PID              int       `json:"pid,omitempty"`
	Log              string    `json:"log,omitempty"`
	Result           *struct {
		Status string `json:"status"`
		Error  string `json:"error,omitempty"`
	} `json:"result,omitempty"`
	Error string `json:"error,omitempty"`
}

func decodeBackupDetach(stdout string) (*backupDetach, error) {
	if _, err := cli.ParseJSON(stdout); err != nil {
		return nil, err
	}
	var env backupDetach
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		return nil, fmt.Errorf("decoding detached run: %w", err)
	}
	switch env.Status {
	case "started", "already_running", "finished", "failed":
		return &env, nil
	}
	return nil, errors.New("decoding detached run: incomplete machine response")
}

type backupOwnerMarker struct {
	Namespace   string `json:"namespace"`
	Fingerprint string `json:"fingerprint"`
	Key         string `json:"key"`
	Self        bool   `json:"self"`
}

type backupSkippedLocation struct {
	Prefix string `json:"prefix"`
	Reason string `json:"reason"`
}

// ── preflight ────────────────────────────────────────────────────────────────

// backupPreflightSummary is one server's preflight in the flow's terms.
type backupPreflightSummary struct {
	Server           string                         `json:"server"`
	ObservedAt       time.Time                      `json:"observed_at"`
	Reachable        bool                           `json:"reachable"`
	Error            string                         `json:"error,omitempty"`
	BinaryState      string                         `json:"binary_state"`
	BinaryVersion    string                         `json:"binary_version,omitempty"`
	CLIVersion       string                         `json:"cli_version,omitempty"`
	Tools            map[string]backupPreflightTool `json:"tools"`
	FreeBytes        *int64                         `json:"free_bytes"`
	TargetState      string                         `json:"target_state"`
	TargetBucket     string                         `json:"target_bucket,omitempty"`
	TargetPrefix     string                         `json:"target_prefix,omitempty"`
	ManagedElsewhere string                         `json:"managed_elsewhere"`
	ManagedReasons   []string                       `json:"managed_reasons,omitempty"`
	// NeedsTools and NeedsUpgrade are consented actions on offer.
	NeedsTools   []string `json:"needs_tools,omitempty"`
	NeedsUpgrade bool     `json:"needs_upgrade,omitempty"`
	// Warnings are worth reading but stop nothing.
	Warnings []string `json:"warnings,omitempty"`
	// Blocked says why this server cannot be set up now.
	Blocked string `json:"blocked,omitempty"`
	// Selected is the default tick: a server with a target, or one whose target could
	// not be read, starts unticked.
	Selected bool `json:"selected"`
	// Actions reports consented installs and upgrades, in plain words.
	Actions []string `json:"actions,omitempty"`
	// OwnKey: the key on this server, if any, is one Dash made, so Dash can replace it
	// safely. An unfinished one starts ticked, so a retry finds it.
	OwnKey bool `json:"own_key,omitempty"`
}

func summarizeBackupPreflight(server string, p *backupPreflight) *backupPreflightSummary {
	sum := &backupPreflightSummary{Server: server, ObservedAt: p.ObservedAt, Reachable: p.Reachable,
		Error: sanitizeDomainError(p.Error), BinaryState: p.Binary.State, BinaryVersion: p.Binary.Version,
		CLIVersion:   p.Binary.CLIVersion,
		Tools:        map[string]backupPreflightTool{"aws": *p.Tools.AWS, "age": *p.Tools.Age, "python3": *p.Tools.Python3},
		FreeBytes:    p.Disk.FreeBytes,
		TargetState:  p.Target.State,
		TargetBucket: p.Target.Bucket, TargetPrefix: p.Target.Prefix,
		ManagedElsewhere: p.Policy.ManagedElsewhere, ManagedReasons: p.Policy.ManagedReasons}
	if !p.Reachable {
		sum.Blocked = "Dash could not reach this server"
		if sum.Error != "" {
			sum.Blocked += ": " + sum.Error
		}
		return sum
	}
	names := make([]string, 0, 3)
	for name := range sum.Tools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		switch sum.Tools[name].State {
		case "missing", "outdated":
			sum.NeedsTools = append(sum.NeedsTools, name)
		case "unknown":
			sum.Warnings = append(sum.Warnings, "Could not check "+name+" on this server.")
		}
	}
	switch p.Binary.State {
	case "missing", "no_fleet_engine":
		sum.NeedsUpgrade = true
	case "unknown":
		sum.Warnings = append(sum.Warnings, "Could not check the teploy version on this server.")
	}
	if p.Disk.FreeBytes == nil {
		sum.Warnings = append(sum.Warnings, "Could not check the free disk space on this server.")
	} else if *p.Disk.FreeBytes < backupMinFreeBytes {
		sum.Warnings = append(sum.Warnings, "This server is low on disk space, and backups are built on its disk first.")
	}
	switch p.Policy.ManagedElsewhere {
	case "yes":
		sum.Warnings = append(sum.Warnings, "Something else seems to manage backups on this server: "+strings.Join(p.Policy.ManagedReasons, "; "))
	case "unknown":
		sum.Warnings = append(sum.Warnings, "Could not tell whether something else manages backups on this server.")
	}
	switch p.Target.State {
	case "absent":
		sum.Selected = p.Policy.ManagedElsewhere != "yes"
	case "configured":
		sum.Warnings = append(sum.Warnings, "This server already sends backups to "+p.Target.Bucket+". It is left alone unless you tick it.")
	default:
		sum.Warnings = append(sum.Warnings, "Could not read this server's backup settings, so it is left alone unless you tick it.")
	}
	return sum
}

// readBackupPreflight reads one server. A flag's failure exits nonzero and still prints
// the report, so the report wins whenever there is one.
func (s *Server) readBackupPreflight(ctx context.Context, name string, install, upgrade bool) (*backupPreflight, error) {
	args := []string{"server", "backup-preflight", "--host", name}
	if install {
		args = append(args, "--install-tools")
	}
	if upgrade {
		args = append(args, "--upgrade-binary")
	}
	args = append(args, "--json")
	result, err := s.runCLI(ctx, args...)
	if result != nil {
		if env, derr := decodeBackupPreflight(result.Stdout); derr == nil {
			return env, nil
		}
		if err == nil && result.ExitCode != 0 {
			err = commandFailure(args, result)
		}
	}
	if err == nil {
		err = errors.New("the teploy CLI did not return a readable report")
	}
	return nil, err
}

func (s *Server) eachBackupName(ctx context.Context, names []string, fn func(context.Context, int, string)) {
	sem := make(chan struct{}, backupPushConcurrency)
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			pctx, cancel := context.WithTimeout(ctx, backupPushTimeout)
			defer cancel()
			fn(pctx, i, name)
		}(i, name)
	}
	wg.Wait()
}

// resolveSetupServers checks named servers against the fleet. Empty means the whole
// fleet only where the caller allows it.
func (s *Server) resolveSetupServers(ctx context.Context, requested []string, allowAll bool) ([]string, []string, error) {
	servers, err := s.resolveServers(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("server discovery failed: %w", err)
	}
	fleet := backupServerNames(servers)
	if len(fleet) == 0 {
		return nil, nil, errors.New("Add a server first: there is nothing to protect yet.")
	}
	if len(requested) == 0 {
		if allowAll {
			return fleet, fleet, nil
		}
		return nil, fleet, errors.New("Tick at least one server.")
	}
	known := map[string]bool{}
	for _, name := range fleet {
		known[name] = true
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(requested))
	for _, name := range requested {
		if !known[name] {
			return nil, fleet, fmt.Errorf("%q is not one of your servers", name)
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, fleet, nil
}

func (s *Server) handleSetupPreflight(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Servers       []string `json:"servers"`
		InstallTools  []string `json:"install_tools"`
		UpgradeBinary []string `json:"upgrade_binary"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	names, _, err := s.resolveSetupServers(r.Context(), body.Servers, true)
	if err != nil {
		writeError(w, sanitizeDomainError(err.Error()))
		return
	}
	contains := func(list []string, v string) bool {
		for _, x := range list {
			if x == v {
				return true
			}
		}
		return false
	}
	ctx := context.WithoutCancel(r.Context())
	results := make([]*backupPreflightSummary, len(names))
	s.eachBackupName(ctx, names, func(ctx context.Context, i int, name string) {
		install, upgrade := contains(body.InstallTools, name), contains(body.UpgradeBinary, name)
		p, err := s.readBackupPreflight(ctx, name, install, upgrade)
		if err != nil {
			results[i] = &backupPreflightSummary{Server: name, BinaryState: "unknown", TargetState: "unknown", ManagedElsewhere: "unknown",
				Tools: map[string]backupPreflightTool{}, Blocked: sanitizeDomainError("the check did not finish: " + err.Error())}
			return
		}
		sum := summarizeBackupPreflight(name, p)
		for _, t := range p.ToolsInstall {
			line := t.Tool + ": " + strings.ReplaceAll(t.Action, "_", " ")
			if t.Detail != "" {
				line += " (" + t.Detail + ")"
			}
			sum.Actions = append(sum.Actions, sanitizeDomainError(line))
		}
		if u := p.BinaryUpgrade; u != nil {
			line := "teploy: " + strings.ReplaceAll(u.Action, "_", " ")
			if u.Detail != "" {
				line += " (" + u.Detail + ")"
			}
			sum.Actions = append(sum.Actions, sanitizeDomainError(line))
		}
		if install || upgrade {
			s.auditf(r, "backups.setup.prepare", name, "install_tools=%v upgrade_binary=%v", install, upgrade)
		}
		results[i] = sum
	})
	b := &s.backups
	b.mu.Lock()
	setup := b.setupLocked()
	for _, sum := range results {
		if k := setup.Keys[sum.Server]; k != nil {
			sum.OwnKey = true
			sum.Selected = sum.Blocked == "" && k.State != backupKeyVerified
		}
		setup.Preflight[sum.Server] = sum
	}
	b.saveLocked()
	b.mu.Unlock()
	s.auditf(r, "backups.setup.preflight", "", "servers=%d", len(names))
	writeData(w, map[string]any{"servers": results})
}

// ── storage: check ───────────────────────────────────────────────────────────

func (s *Server) b2Client() *b2.Client {
	if s.backups.b2Client != nil {
		return s.backups.b2Client()
	}
	return &b2.Client{HTTP: &http.Client{Timeout: 45 * time.Second, Transport: &http.Transport{DialContext: monitor.PolicyDialContext(false)}}}
}

// backupSetupWindow is the recovery window: hidden copies last this long.
func backupSetupWindow(requested, current int) (days int, explicit bool, err error) {
	switch {
	case requested < 0 || requested > 365:
		return 0, false, errors.New("the recovery window must be 1 to 365 days")
	case requested > 0:
		return requested, true, nil
	case current > 0:
		return current, false, nil
	}
	return b2.DefaultHideDays, false, nil
}

type backupBucketFinding struct {
	Name      string   `json:"name"`
	Type      string   `json:"type"`
	Refusals  []string `json:"refusals"`
	Warnings  []string `json:"warnings"`
	S3Managed bool     `json:"s3_managed"`
}

func bucketFinding(b *b2.Bucket, base string, window int) backupBucketFinding {
	a := b2.Analyze(b.Rules, base, window)
	f := backupBucketFinding{Name: b.Name, Type: b.Type, Refusals: a.Refusals, Warnings: a.Warnings, S3Managed: a.S3Managed}
	switch b.Type {
	case b2.BucketPrivate:
	case b2.BucketSnapshot:
		f.Refusals = append(f.Refusals, "This is a snapshot bucket, which Backblaze keeps for its own restores. Choose a private bucket.")
	default:
		f.Refusals = append(f.Refusals, "This bucket is public. Backups need a private bucket.")
	}
	return f
}

// oldPrefixWarnings looks for data Dash would be moving in beside. A key without
// listFiles cannot look, and the warning says so instead of implying a clean bucket.
func (s *Server) oldPrefixWarnings(ctx context.Context, sess *b2.Session, bucket *b2.Bucket, base string, fleet []string) []string {
	out := []string{}
	b := &s.backups
	b.mu.Lock()
	expected := map[string]string{}
	if st := b.rec.Storage; st != nil && st.Bucket == bucket.Name {
		for k, v := range st.Prefixes {
			expected[k] = v
		}
	}
	if b.rec.Setup != nil {
		for name, sum := range b.rec.Setup.Preflight {
			if sum.TargetBucket == bucket.Name && sum.TargetPrefix != "" && !strings.HasPrefix(sum.TargetPrefix+"/", b2.BasePrefix(base)) {
				out = append(out, fmt.Sprintf("%s now stores backups under %s in this bucket. Moving it leaves those copies there, and retention will not touch them.", name, sum.TargetPrefix))
			}
		}
	}
	b.mu.Unlock()
	if !sess.Has(b2.CapListFiles) {
		return append(out, "This key cannot list files, so Dash could not check what this bucket already holds.")
	}
	top, err := sess.ListFolders(ctx, bucket.ID, "", 20)
	if err != nil {
		return append(out, "Dash could not check what this bucket already holds: "+err.Error())
	}
	basePrefix := b2.BasePrefix(base)
	for _, name := range top {
		if basePrefix != "" && name != basePrefix {
			out = append(out, "This bucket already holds "+name+", which Dash's rules do not touch.")
		}
	}
	if basePrefix == "" {
		return out
	}
	known := map[string]bool{}
	for _, name := range fleet {
		known[basePrefix+name+"/"] = true
	}
	for _, p := range expected {
		known[p+"/"] = true
	}
	for _, name := range top {
		if name != basePrefix {
			continue
		}
		inner, err := sess.ListFolders(ctx, bucket.ID, basePrefix, 50)
		if err != nil {
			return append(out, "Dash could not check what this bucket already holds: "+err.Error())
		}
		for _, f := range inner {
			if !known[f] {
				out = append(out, f+" already exists from an older setup. It stays, and retention will not manage it.")
			}
		}
	}
	return out
}

func (s *Server) handleSetupStorageCheck(w http.ResponseWriter, r *http.Request) {
	var body struct {
		KeyID          string `json:"key_id"`
		ApplicationKey string `json:"application_key"`
		Bucket         string `json:"bucket"`
		EmptyBase      bool   `json:"empty_base"`
		HideDays       int    `json:"hide_days"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	if !validSecretField(body.KeyID) || !validSecretField(body.ApplicationKey) {
		writeError(w, "Paste both parts of the key: the key ID and the application key.")
		return
	}
	scrub := func(text string) string { return scrubCredentials(text, body.KeyID, body.ApplicationKey) }
	sess, err := s.b2Client().Authorize(r.Context(), body.KeyID, body.ApplicationKey)
	if err != nil {
		s.auditf(r, "backups.setup.storage_check", "", "the key was not accepted")
		writeErrorStatus(w, scrub(err.Error()), http.StatusUnprocessableEntity)
		return
	}
	base := backupSetupBase
	if body.EmptyBase {
		base = ""
	}
	b := &s.backups
	b.mu.Lock()
	current, recorded := 0, ""
	if setup := b.rec.Setup; setup != nil {
		current, recorded = setup.HideDays, setup.Bucket
	}
	if st := b.rec.Storage; st != nil && st.Provider == backupProviderB2 {
		recorded = st.Bucket
	}
	b.mu.Unlock()
	window, _, err := backupSetupWindow(body.HideDays, current)
	if err != nil {
		writeError(w, err.Error())
		return
	}
	resp := map[string]any{
		"capabilities": sess.Capabilities, "missing": sess.Missing(),
		"can_delete_keys": sess.Has(b2.CapDeleteKeys), "can_delete_files": sess.Has(b2.CapDeleteFiles) && sess.Has(b2.CapListFiles),
		"endpoint": sess.S3URL, "region": sess.Region, "base": base, "hide_days": window, "suggested_bucket": recorded,
		"can_create_bucket": len(sess.Buckets) == 0, "never_expires": sess.ExpiresAt == nil,
	}
	warnings := []string{}
	if sess.ExpiresAt != nil {
		resp["expires_at"] = *sess.ExpiresAt
	} else {
		warnings = append(warnings, "This key never expires. Dash will not delete it for you, so delete it in the Backblaze console when you finish. A key with an expiry is safer.")
	}
	if sess.NamePrefix != "" {
		warnings = append(warnings, "This key is limited to files under "+sess.NamePrefix+", which may stop it from making the bucket's server keys.")
	}
	resp["warnings"] = warnings
	// With nothing chosen, the bucket this flow would use is the one to warn about.
	looked := body.Bucket
	if looked == "" {
		looked = recorded
	}
	findings := []backupBucketFinding{}
	if len(sess.Missing()) == 0 {
		buckets, err := sess.ListBuckets(r.Context())
		switch {
		case b2.IsCapped(err):
			// A capped listing is not an outage: say which it is.
			resp["capped"] = true
			resp["listing_error"] = scrub(err.Error())
		case err != nil:
			resp["listing_error"] = scrub(err.Error())
		default:
			for i := range buckets {
				f := bucketFinding(&buckets[i], base, window)
				if buckets[i].Name == looked {
					servers, serr := s.resolveServers(r.Context())
					if serr == nil {
						f.Warnings = append(f.Warnings, s.oldPrefixWarnings(r.Context(), sess, &buckets[i], base, backupServerNames(servers))...)
					}
					if base == "" {
						if reason := s.rootBucketRefusal(r.Context(), sess, &buckets[i]); reason != "" {
							f.Refusals = append(f.Refusals, reason)
						}
					}
				}
				findings = append(findings, f)
			}
		}
	}
	resp["buckets"] = findings
	s.auditf(r, "backups.setup.storage_check", body.Bucket, "buckets=%d missing=%s", len(findings), strings.Join(sess.Missing(), ","))
	writeData(w, resp)
}

// rootBucketRefusal: backups at the bucket root need the bucket to themselves, because
// nothing separates them from whatever else is there. What this Dash already put there
// does not count.
func (s *Server) rootBucketRefusal(ctx context.Context, sess *b2.Session, bucket *b2.Bucket) string {
	if !sess.Has(b2.CapListFiles) {
		return "Using the bucket root needs an empty bucket, and this key cannot list files to check. Give it listFiles, or use the teploy folder."
	}
	top, err := sess.ListFolders(ctx, bucket.ID, "", 50)
	if err != nil {
		return "Using the bucket root needs an empty bucket, and Dash could not check: " + err.Error()
	}
	known := map[string]bool{}
	b := &s.backups
	b.mu.Lock()
	if st := b.rec.Storage; st != nil && st.Bucket == bucket.Name {
		for _, p := range st.Prefixes {
			known[p+"/"] = true
		}
	}
	b.mu.Unlock()
	for _, name := range top {
		if !known[name] {
			return "Using the bucket root needs an empty bucket, and this one holds " + name + ". Choose an empty bucket, or use the teploy folder."
		}
	}
	return ""
}

// ── storage: apply ───────────────────────────────────────────────────────────

type setupStorageBody struct {
	Password       string   `json:"password"`
	KeyID          string   `json:"key_id"`
	ApplicationKey string   `json:"application_key"`
	Bucket         string   `json:"bucket"`
	Servers        []string `json:"servers"`
	AcceptWarnings bool     `json:"accept_warnings"`
	EmptyBase      bool     `json:"empty_base"`
	HideDays       int      `json:"hide_days"`
	S3Managed      bool     `json:"s3_managed"`
	Rotate         bool     `json:"rotate"`
	// ReplaceConfigured: the person read that a server's current key cannot be put back
	// by Dash once replaced.
	ReplaceConfigured bool `json:"replace_configured"`
}

// setupServerResult reports one server, in the terms of the failure rule: a key is
// deleted only when it was never pushed.
type setupServerResult struct {
	Server   string `json:"server"`
	Prefix   string `json:"prefix"`
	Skipped  string `json:"skipped,omitempty"`
	KeyName  string `json:"key_name,omitempty"`
	Pushed   bool   `json:"pushed"`
	Verified bool   `json:"verified"`
	// KeyKept: pushed but not verified, so the server may be using it.
	KeyKept    bool   `json:"key_kept,omitempty"`
	KeyDeleted bool   `json:"key_deleted,omitempty"`
	Error      string `json:"error,omitempty"`
	Replaced   string `json:"replaced,omitempty"`
	Lifecycle  string `json:"lifecycle,omitempty"`
	Note       string `json:"note,omitempty"`
	// OldOwners are markers another machine left under this server's prefix: a rebuilt
	// server's old self, which stops retention until cleared with consent.
	OldOwners []backupOwnerMarker `json:"old_owners,omitempty"`
}

type setupStorageResponse struct {
	Bucket        string              `json:"bucket"`
	BucketCreated bool                `json:"bucket_created"`
	Endpoint      string              `json:"endpoint"`
	Region        string              `json:"region"`
	Base          string              `json:"base"`
	HideDays      int                 `json:"hide_days"`
	RulesState    string              `json:"rules_state"`
	RulesText     []string            `json:"rules_text,omitempty"`
	Warnings      []string            `json:"warnings"`
	Servers       []setupServerResult `json:"servers"`
	AllOK         bool                `json:"all_ok"`
	Stopped       string              `json:"stopped,omitempty"`
	KeyNote       string              `json:"key_note,omitempty"`
}

func setupKeyName(server string) string {
	slug := strings.Trim(backupKeyNameBadRE.ReplaceAllString(server, "-"), "-")
	if slug == "" {
		slug = "server"
	}
	if len(slug) > 60 {
		slug = slug[:60]
	}
	return "teploy-" + slug + "-" + randomHex(3)
}

// mintServerKey asks for read-lifecycle first and retries without it when Backblaze
// refuses: a retry that also fails was not about the capability.
func mintServerKey(ctx context.Context, sess *b2.Session, name, bucketID, prefix string) (*b2.Key, bool, error) {
	caps := []string{b2.CapListFiles, b2.CapReadFiles, b2.CapWriteFiles}
	key, err := sess.CreateKey(ctx, name, append(append([]string(nil), caps...), b2.CapReadLifecycle), bucketID, prefix)
	if err == nil {
		return key, true, nil
	}
	if !b2.IsCode(err, "bad_request") {
		return nil, false, err
	}
	key, err = sess.CreateKey(ctx, name, caps, bucketID, prefix)
	return key, false, err
}

// resolveSetupBucket finds or creates the bucket. An unchosen bucket's name is saved
// before the create, so a retry finds it rather than making a second.
func (s *Server) resolveSetupBucket(ctx context.Context, sess *b2.Session, chosen string) (*b2.Bucket, bool, error) {
	if chosen != "" {
		bucket, err := sess.FindBucket(ctx, chosen)
		if err != nil {
			return nil, false, err
		}
		if bucket == nil {
			return nil, false, fmt.Errorf("Backblaze does not show a bucket called %s to this key", chosen)
		}
		return bucket, false, nil
	}
	b := &s.backups
	nextName := func() string {
		b.mu.Lock()
		defer b.mu.Unlock()
		setup := b.setupLocked()
		if setup.Bucket == "" {
			setup.Bucket = "teploy-backups-" + randomHex(8)
			b.saveLocked()
		}
		return setup.Bucket
	}
	name := nextName()
	bucket, err := sess.FindBucket(ctx, name)
	if err != nil {
		return nil, false, err
	}
	if bucket != nil {
		return bucket, false, nil
	}
	if len(sess.Buckets) > 0 {
		return nil, false, errors.New("This key is limited to certain buckets, so it cannot create one. Choose one of its buckets.")
	}
	bucket, err = sess.CreatePrivateBucket(ctx, name)
	if err == nil {
		return bucket, true, nil
	}
	if !b2.IsCode(err, "duplicate_bucket_name") {
		return nil, false, err
	}
	// Bucket names are global. Not ours, since the search above did not find it, so
	// choose again and record that before trying.
	b.mu.Lock()
	b.setupLocked().Bucket = ""
	b.mu.Unlock()
	name = nextName()
	bucket, err = sess.CreatePrivateBucket(ctx, name)
	if err != nil {
		return nil, false, err
	}
	return bucket, true, nil
}

// applySetupRules merges the rules, retrying when someone changed the bucket meanwhile.
// It returns the warnings a person must accept, or stops when a rule refuses.
func applySetupRules(ctx context.Context, sess *b2.Session, bucket *b2.Bucket, base string, prefixes []string, window int, explicit, forceS3Managed, accept bool) (state string, warnings []string, refusals []string, err error) {
	current := bucket
	for attempt := 0; attempt < backupRulesRetries; attempt++ {
		merged, changed, merr := b2.Merge(current.Rules, base, prefixes, window, explicit)
		if merr != nil {
			return "", nil, nil, merr
		}
		analysis := b2.Analyze(merged, base, window)
		finding := bucketFinding(&b2.Bucket{Name: current.Name, Type: current.Type, Rules: merged}, base, window)
		if len(finding.Refusals) > 0 {
			return "", nil, finding.Refusals, nil
		}
		warnings = analysis.Warnings
		if forceS3Managed || analysis.S3Managed {
			return "s3_managed", warnings, nil, nil
		}
		if len(warnings) > 0 && !accept {
			return "", warnings, nil, nil
		}
		if !changed {
			return "set", warnings, nil, nil
		}
		err = sess.UpdateRules(ctx, current.ID, merged, current.Revision)
		if err == nil {
			return "set", warnings, nil, nil
		}
		if !b2.IsCode(err, "conflict") {
			return "", nil, nil, err
		}
		fresh, ferr := sess.FindBucket(ctx, current.Name)
		if ferr != nil {
			return "", nil, nil, ferr
		}
		if fresh == nil {
			return "", nil, nil, errors.New("the bucket disappeared while its rules were being set")
		}
		current = fresh
	}
	return "", nil, nil, err
}

func (s *Server) handleSetupStorageApply(w http.ResponseWriter, r *http.Request) {
	var body setupStorageBody
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	if !validSecretField(body.KeyID) || !validSecretField(body.ApplicationKey) {
		writeError(w, "Paste both parts of the key: the key ID and the application key.")
		return
	}
	if !s.requireReauth(w, r, body.Password) {
		return
	}
	if body.Bucket != "" && (!backupNameRE.MatchString(body.Bucket) || len(body.Bucket) > 63) {
		writeError(w, "That bucket name does not look right.")
		return
	}
	names, _, err := s.resolveSetupServers(r.Context(), body.Servers, false)
	if err != nil {
		writeError(w, sanitizeDomainError(err.Error()))
		return
	}
	b := &s.backups
	if !b.setupMu.TryLock() {
		writeErrorStatus(w, "Another setup step is running. Wait for it to finish.", http.StatusConflict)
		return
	}
	defer b.setupMu.Unlock()
	// Started means it finishes: walking away must not leave a fleet half-done with
	// nobody told. The temporary key stays in this function's scope.
	ctx := context.WithoutCancel(r.Context())
	scrub := func(text string) string { return scrubCredentials(text, body.KeyID, body.ApplicationKey) }
	fail := func(err error, status int) {
		s.auditf(r, "backups.setup.storage_failed", body.Bucket, "%s", scrub(err.Error()))
		writeErrorStatus(w, scrub(err.Error()), status)
	}

	sess, err := s.b2Client().Authorize(ctx, body.KeyID, body.ApplicationKey)
	if err != nil {
		fail(err, http.StatusUnprocessableEntity)
		return
	}
	if missing := sess.Missing(); len(missing) > 0 {
		fail(fmt.Errorf("This key is missing these permissions: %s. Make a key that has them, then paste it again.", strings.Join(missing, ", ")), http.StatusUnprocessableEntity)
		return
	}
	// A push replaces what a server already has. Ask before anything is created: Dash
	// never held that server's old key, so it cannot put it back. A key Dash made itself
	// is different, because Dash keeps it until the new one has verified.
	configured := map[string]bool{}
	var needConsent []string
	for _, name := range names {
		show, serr := s.readBackupTargetShow(ctx, name)
		configured[name] = serr != nil || show.Configured
		b.mu.Lock()
		_, ours := b.setupLocked().Keys[name]
		b.mu.Unlock()
		if configured[name] && !ours {
			needConsent = append(needConsent, name)
		}
	}
	if len(needConsent) > 0 && !body.ReplaceConfigured {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{"error": strings.Join(needConsent, ", ") + " may already have a backup key. Replacing it cannot be undone by Dash, which never had a copy of the old one. Confirm to go on.",
			"data": map[string]any{"replace_configured": needConsent}})
		return
	}
	base := backupSetupBase
	if body.EmptyBase {
		base = ""
	}
	b.mu.Lock()
	current := 0
	if b.rec.Setup != nil {
		current = b.rec.Setup.HideDays
	}
	var known *backupStorage
	if st := b.rec.Storage; st != nil {
		c := *st
		known = &c
	}
	b.mu.Unlock()
	window, explicit, err := backupSetupWindow(body.HideDays, current)
	if err != nil {
		writeError(w, err.Error())
		return
	}
	chosen := body.Bucket
	if chosen == "" && known != nil && known.Provider == backupProviderB2 {
		chosen = known.Bucket
	}
	bucket, created, err := s.resolveSetupBucket(ctx, sess, chosen)
	if err != nil {
		fail(err, http.StatusUnprocessableEntity)
		return
	}
	if refusals := bucketFinding(bucket, base, window).Refusals; bucket.Type != b2.BucketPrivate {
		fail(errors.New(refusals[len(refusals)-1]), http.StatusConflict)
		return
	}
	if base == "" && !created {
		if reason := s.rootBucketRefusal(ctx, sess, bucket); reason != "" {
			fail(errors.New(reason), http.StatusConflict)
			return
		}
	}

	// Prefixes stay stable across runs, unless an old one sits outside the base.
	prefixes := map[string]string{}
	taken := map[string]bool{}
	basePrefix := b2.BasePrefix(base)
	if known != nil && known.Bucket == bucket.Name && known.Endpoint == sess.S3URL {
		for k, v := range known.Prefixes {
			prefixes[k] = v
			taken[v] = true
		}
	}
	for _, name := range names {
		if prefixes[name] == "" || !strings.HasPrefix(prefixes[name]+"/", basePrefix) {
			prefixes[name] = backupServerPrefix(base, name, taken)
			taken[prefixes[name]] = true
		}
	}
	selected := make([]string, 0, len(names))
	for _, name := range names {
		selected = append(selected, prefixes[name])
	}

	rulesState, warnings, refusals, err := applySetupRules(ctx, sess, bucket, base, selected, window, explicit, body.S3Managed, body.AcceptWarnings)
	if err != nil {
		fail(err, http.StatusBadGateway)
		return
	}
	if len(refusals) > 0 {
		s.auditf(r, "backups.setup.storage_refused", bucket.Name, "%s", strings.Join(refusals, " "))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{"error": strings.Join(refusals, " "), "data": map[string]any{"refusals": refusals}})
		return
	}
	if rulesState == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{"error": "Read the warnings about this bucket, then confirm.", "data": map[string]any{"warnings": warnings}})
		return
	}
	if warnings == nil {
		warnings = []string{}
	}
	resp := setupStorageResponse{Bucket: bucket.Name, BucketCreated: created, Endpoint: sess.S3URL, Region: sess.Region, Base: base,
		HideDays: window, RulesState: rulesState, Warnings: warnings, Servers: make([]setupServerResult, len(names))}
	if rulesState == "s3_managed" {
		resp.RulesText = b2.RulesText(base, selected, window)
		resp.Warnings = append(resp.Warnings, "This bucket's lifecycle rules look like they are managed through the S3 API, so Dash did not change them. Backblaze gives no sure sign of this, so check that these rules exist.")
	}
	// What is really in force: Merge keeps a window a person set earlier.
	effective := window
	if rulesState == "set" {
		if merged, _, merr := b2.Merge(bucket.Rules, base, selected, window, explicit); merr == nil {
			effective = b2.EffectiveWindow(merged, base, selected, window)
		}
	}
	resp.HideDays = effective
	b.mu.Lock()
	setup := b.setupLocked()
	setup.Bucket, setup.BucketID, setup.HideDays, setup.S3Managed = bucket.Name, bucket.ID, effective, rulesState == "s3_managed"
	b.saveLocked()
	b.mu.Unlock()

	request := func(key *b2.Key) *backupStorageRequest {
		return &backupStorageRequest{Provider: backupProviderB2, Bucket: bucket.Name, Region: sess.Region, Endpoint: sess.S3URL,
			BasePrefix: base, AccessKey: key.ID, SecretKey: key.Secret}
	}
	dropKey := func(server, keyID, keyName, why string) bool {
		if keyID == "" {
			return false
		}
		if sess.Has(b2.CapDeleteKeys) {
			if err := sess.DeleteKey(ctx, keyID); err == nil || b2.IsMissingKey(err) {
				return true
			}
		}
		b.mu.Lock()
		setup := b.setupLocked()
		setup.Leftovers = append(setup.Leftovers, backupLeftoverKey{KeyID: keyID, KeyName: keyName, Server: server, Reason: why})
		b.saveLocked()
		b.mu.Unlock()
		return false
	}

	// Phase one, in order: mint and prove each key before anything reaches a server, so
	// a key that cannot hide stops the run with nothing pushed.
	type pendingPush struct {
		i          int
		key        *b2.Key
		prev       *backupServerKey
		configured bool
	}
	var pending []pendingPush
	stopped := ""
	for i, name := range names {
		res := &resp.Servers[i]
		res.Server, res.Prefix = name, prefixes[name]
		b.mu.Lock()
		var prev *backupServerKey
		if k := b.setupLocked().Keys[name]; k != nil {
			c := *k
			prev = &c
		}
		b.mu.Unlock()
		if stopped != "" {
			res.Skipped = "stopped before this server"
			continue
		}
		// Only a key verified in the operation that pushed it counts as done. Anything
		// else is pushed again: a server answering proves nothing about which key it has.
		if prev != nil && prev.Bucket == bucket.Name && prev.Prefix == prefixes[name] && !body.Rotate && prev.State == backupKeyVerified {
			res.Skipped, res.Verified, res.Pushed, res.KeyName = "already verified", true, true, prev.KeyName
			continue
		}
		// A key left at minted never reached a server, and its secret is gone. What it
		// was replacing may still be in use, so that stays on the record.
		if prev != nil && prev.State == backupKeyMinted {
			dropKey(name, prev.KeyID, prev.KeyName, "made by an earlier attempt and never pushed")
			prev = chainRecord(prev)
		}
		keyName := setupKeyName(name)
		key, lifecycleRead, err := mintServerKey(ctx, sess, keyName, bucket.ID, prefixes[name]+"/")
		if err != nil {
			res.Error = scrub(err.Error())
			if b2.IsCapped(err) {
				stopped = res.Error
			}
			continue
		}
		res.KeyName = keyName
		if !lifecycleRead {
			res.Lifecycle = "unknown"
			res.Note = "This key cannot read the bucket's lifecycle rules, so whether they still hold cannot be rechecked."
		}
		now := time.Now().UTC()
		record := &backupServerKey{KeyID: key.ID, KeyName: keyName, Bucket: bucket.Name, Prefix: prefixes[name], State: backupKeyMinted, CreatedAt: now}
		if !lifecycleRead {
			record.Lifecycle = "unknown"
		}
		if prev != nil {
			record.Replaces = append([]backupLeftoverKey(nil), prev.Replaces...)
			if prev.KeyID != "" {
				record.Replaces = append(record.Replaces, backupLeftoverKey{KeyID: prev.KeyID, KeyName: prev.KeyName, Server: name})
			}
		}
		b.mu.Lock()
		b.setupLocked().Keys[name] = record
		b.saveLocked()
		b.mu.Unlock()

		req := request(key)
		probe, _, ok := s.probeBackupStorage(ctx, req, prefixes[name], true)
		if !ok {
			msg := scrubCredentials(scrub(probe.Error), key.ID, key.Secret)
			res.Error = msg
			res.KeyDeleted = dropKey(name, key.ID, keyName, "failed its test and never pushed")
			restoreSetupKey(b, name, prev)
			if probe.Write && probe.Read {
				// The key works and cannot hide: every other key would be the same.
				stopped = msg
			}
			continue
		}
		pending = append(pending, pendingPush{i: i, key: key, prev: prev, configured: configured[name]})
	}
	if stopped != "" {
		// Nothing reaches any server: drop what this run minted.
		for _, p := range pending {
			name := names[p.i]
			res := &resp.Servers[p.i]
			res.Skipped = "stopped before this server"
			res.KeyDeleted = dropKey(name, p.key.ID, p.key.Name, "stopped before it was pushed")
			restoreSetupKey(b, name, p.prev)
		}
		pending = nil
		resp.Stopped = stopped
	}

	// Phase two: push and verify, then retire any key this one replaces. Servers with no
	// target go first, because they lose nothing if the key turns out bad. With none of
	// those, one configured server goes alone. An auth failure there stops the rest.
	pushOne := func(p pendingPush) {
		pctx, cancel := context.WithTimeout(ctx, backupPushTimeout)
		defer cancel()
		name := names[p.i]
		res := &resp.Servers[p.i]
		b.mu.Lock()
		b.setupLocked().Keys[name].State = backupKeyPushing
		b.saveLocked()
		b.mu.Unlock()
		pushed := s.pushBackupTargetOne(pctx, name, prefixes[name], request(p.key))
		res.Pushed, res.Verified, res.Replaced = pushed.Pushed, pushed.Verified, pushed.Replaced
		res.Error = scrub(pushed.Error)
		if pushed.Lifecycle != "" && res.Lifecycle == "" {
			res.Lifecycle = pushed.Lifecycle
		}
		now := time.Now().UTC()
		switch {
		case pushed.Verified:
			// This key's own push succeeded and verified, so the server holds it.
			b.mu.Lock()
			k := b.setupLocked().Keys[name]
			k.State, k.Verified = backupKeyVerified, &now
			if pushed.Lifecycle != "" && k.Lifecycle == "" {
				k.Lifecycle = pushed.Lifecycle
			}
			replaced := k.Replaces
			k.Replaces = nil
			b.saveLocked()
			b.mu.Unlock()
			for _, old := range replaced {
				if !dropKey(name, old.KeyID, old.KeyName, "replaced by a new key") {
					res.Note = strings.TrimSpace(res.Note + " The old key " + old.KeyName + " could not be deleted, so delete it in the Backblaze console.")
				}
			}
			res.OldOwners = s.readOldOwners(pctx, name, res)
		case pushed.Pushed:
			// It landed and did not verify. Older keys stay until a later push verifies.
			res.KeyKept = true
			b.mu.Lock()
			b.setupLocked().Keys[name].State = backupKeyPushed
			b.saveLocked()
			b.mu.Unlock()
		case s.pushProvablyMissed(pctx, name, bucket.Name, prefixes[name]):
			res.KeyDeleted = dropKey(name, p.key.ID, p.key.Name, "the push did not reach the server")
			restoreSetupKey(b, name, p.prev)
		default:
			// The call failed and the server may or may not hold this key: keep every
			// key, and do not say it was pushed.
			res.KeyKept = true
			b.mu.Lock()
			b.setupLocked().Keys[name].State = backupKeyAmbiguous
			b.saveLocked()
			b.mu.Unlock()
		}
	}
	pushGroup := func(group []pendingPush) {
		var wg sync.WaitGroup
		sem := make(chan struct{}, backupPushConcurrency)
		for _, p := range group {
			wg.Add(1)
			go func(p pendingPush) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				pushOne(p)
			}(p)
		}
		wg.Wait()
	}
	var first, rest []pendingPush
	for _, p := range pending {
		if p.configured {
			rest = append(rest, p)
		} else {
			first = append(first, p)
		}
	}
	if len(first) == 0 && len(rest) > 0 {
		first, rest = rest[:1], rest[1:]
	}
	pushGroup(first)
	authFailed := ""
	for _, p := range first {
		if res := &resp.Servers[p.i]; !res.Verified && backupAuthClassError(res.Error) {
			authFailed = res.Error
		}
	}
	if authFailed != "" && len(rest) > 0 {
		for _, p := range rest {
			name := names[p.i]
			res := &resp.Servers[p.i]
			res.Skipped = "left as it was, because a new key failed on another server"
			res.KeyDeleted = dropKey(name, p.key.ID, p.key.Name, "stopped before it was pushed")
			restoreSetupKey(b, name, p.prev)
		}
		resp.Stopped = "A new key was refused on another server, so Dash did not touch the servers that already have a key. " + authFailed
	} else {
		pushGroup(rest)
	}

	resp.AllOK = resp.Stopped == ""
	pushedAny := false
	verified := 0
	for i := range resp.Servers {
		res := &resp.Servers[i]
		resp.AllOK = resp.AllOK && res.Verified
		pushedAny = pushedAny || res.Pushed
		if res.Verified {
			verified++
		}
	}
	if pushedAny {
		now := time.Now().UTC()
		b.mu.Lock()
		st := b.rec.Storage
		if st == nil || st.Bucket != bucket.Name || st.Endpoint != sess.S3URL {
			fresh := &backupStorage{Provider: backupProviderB2, Bucket: bucket.Name, Region: sess.Region, Endpoint: sess.S3URL,
				BasePrefix: base, Prefixes: map[string]string{}, ConnectedAt: now}
			// Servers not ticked this time still write where they did.
			if st != nil {
				for k, v := range st.Prefixes {
					fresh.Prefixes[k] = v
				}
			}
			st = fresh
		} else if body.Rotate {
			st.RotatedAt = &now
		}
		st.Provider = backupProviderB2
		for _, name := range names {
			st.Prefixes[name] = prefixes[name]
		}
		b.rec.Storage = st
		b.saveLocked()
		b.mu.Unlock()
	}
	// The temporary key goes last, only once every server is done: a partial run needs
	// it again, and a key without an expiry may not be temporary at all.
	if resp.AllOK {
		switch {
		case sess.ExpiresAt == nil:
			resp.KeyNote = "Your key never expires, so Dash left it. Delete it in the Backblaze console."
		case !sess.Has(b2.CapDeleteKeys):
			resp.KeyNote = "Your key cannot delete keys, so it stays until it expires. You can delete it in the Backblaze console."
		default:
			if err := sess.DeleteKey(ctx, body.KeyID); err != nil {
				resp.KeyNote = "Dash could not delete your temporary key, so delete it in the Backblaze console. " + scrub(err.Error())
			} else {
				resp.KeyNote = "Dash deleted your temporary key."
			}
		}
	} else {
		resp.KeyNote = "Your key is still in place, because not every server finished. Dash did not save it, so paste it again to retry."
	}
	s.auditf(r, "backups.setup.storage", bucket.Name, "created=%v rules=%s servers=%d verified=%d stopped=%v", created, rulesState, len(names), verified, resp.Stopped != "")
	s.kickBackupSweep()
	writeData(w, resp)
}

// chainRecord is what is left of a record once its newest key has gone: the key it
// replaced, which a server may still hold, and so never reads as verified.
func chainRecord(k *backupServerKey) *backupServerKey {
	if len(k.Replaces) == 0 {
		return nil
	}
	last := k.Replaces[len(k.Replaces)-1]
	return &backupServerKey{KeyID: last.KeyID, KeyName: last.KeyName, Bucket: k.Bucket, Prefix: k.Prefix, State: backupKeyAmbiguous,
		CreatedAt: k.CreatedAt, Replaces: append([]backupLeftoverKey(nil), k.Replaces[:len(k.Replaces)-1]...)}
}

// backupAuthClassError: the store refused the key, as opposed to a server being down.
func backupAuthClassError(text string) bool {
	lower := strings.ToLower(text)
	for _, sign := range []string{"invalidaccesskeyid", "signaturedoesnotmatch", "accessdenied", "access denied", "forbidden", "unauthorized", "invalid credentials", "(403)", " 403"} {
		if strings.Contains(lower, sign) {
			return true
		}
	}
	return false
}

func restoreSetupKey(b *backupState, name string, prev *backupServerKey) {
	b.mu.Lock()
	defer b.mu.Unlock()
	setup := b.setupLocked()
	if prev != nil {
		setup.Keys[name] = prev
	} else {
		delete(setup.Keys, name)
	}
	b.saveLocked()
}

// pushProvablyMissed is what lets a failed push count as never pushed: the server
// answered and stores something else. If it does not answer, or stores this very
// bucket and prefix, the key may be in use.
func (s *Server) pushProvablyMissed(ctx context.Context, name, bucket, prefix string) bool {
	args := []string{"server", "backup-target", "show", "--host", name, "--json"}
	result, err := s.runCLI(ctx, args...)
	if err != nil || result == nil || result.ExitCode != 0 {
		return false
	}
	var out struct {
		Configured bool   `json:"configured"`
		Bucket     string `json:"bucket"`
		Prefix     string `json:"prefix"`
	}
	if json.Unmarshal([]byte(result.Stdout), &out) != nil {
		return false
	}
	return !out.Configured || out.Bucket != bucket || out.Prefix != prefix
}

func (s *Server) readOldOwners(ctx context.Context, name string, res *setupServerResult) []backupOwnerMarker {
	args := []string{"server", "backup-target", "owners", "--host", name, "--json"}
	result, err := s.runCLI(ctx, args...)
	if err != nil || result == nil || result.ExitCode != 0 {
		res.Note = strings.TrimSpace(res.Note + " Could not check whether an older machine left its mark here.")
		return nil
	}
	var out struct {
		Markers []backupOwnerMarker `json:"markers"`
	}
	if json.Unmarshal([]byte(result.Stdout), &out) != nil {
		return nil
	}
	seen := map[string]bool{}
	var others []backupOwnerMarker
	for _, m := range out.Markers {
		if !m.Self && !seen[m.Fingerprint] {
			seen[m.Fingerprint] = true
			others = append(others, m)
		}
	}
	return others
}

// handleSetupOwners clears a rebuilt server's old marker, with the person's consent.
func (s *Server) handleSetupOwners(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password    string `json:"password"`
		Server      string `json:"server"`
		Fingerprint string `json:"fingerprint"`
		Force       bool   `json:"force"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	if !backupFingerprintRE.MatchString(body.Fingerprint) {
		writeError(w, "That marker does not look right.")
		return
	}
	if _, err := s.lookupServerStrict(r.Context(), body.Server); err != nil {
		writeError(w, sanitizeDomainError(err.Error()))
		return
	}
	// Forcing lets a prune delete what that machine wrote, so it asks again.
	if body.Force && !s.requireReauth(w, r, body.Password) {
		return
	}
	args := []string{"server", "backup-target", "owners", "--host", body.Server, "--clear", body.Fingerprint}
	if body.Force {
		args = append(args, "--force")
	}
	args = append(args, "--json")
	result, err := s.runCLI(context.WithoutCancel(r.Context()), args...)
	if result == nil {
		writeErrorStatus(w, sanitizeDomainError(fmt.Sprintf("clearing the marker: %v", err)), http.StatusBadGateway)
		return
	}
	var out struct {
		Fingerprint string                  `json:"fingerprint"`
		Removed     int                     `json:"removed"`
		Skipped     []backupSkippedLocation `json:"skipped_locations"`
	}
	decoded := json.Unmarshal([]byte(result.Stdout), &out) == nil
	failed := err != nil || result.ExitCode != 0
	s.auditf(r, "backups.setup.clear_owner", body.Server, "fingerprint=%s removed=%d force=%v failed=%v", body.Fingerprint, out.Removed, body.Force, failed)
	if failed {
		msg := strings.TrimSpace(result.Stderr)
		if msg == "" && err != nil {
			msg = err.Error()
		}
		if strings.Contains(msg, "last week") || strings.Contains(strings.ToLower(msg), "recent") {
			writeErrorStatus(w, "This location was written to in the last week. If the old server is really gone, confirm that and try again.", http.StatusConflict)
			return
		}
		writeErrorStatus(w, sanitizeDomainError("Dash could not clear the marker: "+msg), http.StatusBadGateway)
		return
	}
	if !decoded {
		writeErrorStatus(w, "The teploy CLI did not return a readable report.", http.StatusBadGateway)
		return
	}
	s.kickBackupSweep()
	writeData(w, map[string]any{"removed": out.Removed, "skipped_locations": out.Skipped})
}

// ── recovery key ─────────────────────────────────────────────────────────────

// handleSetupRecovery records a public key the browser made and proved against the
// saved file. It reaches the servers when backups are turned on.
func (s *Server) handleSetupRecovery(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password  string `json:"password"`
		PublicKey string `json:"public_key"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	if !backupAgeRecipientRE.MatchString(body.PublicKey) {
		writeError(w, "That is not a public recovery key. It starts with age1.")
		return
	}
	if !s.requireReauth(w, r, body.Password) {
		return
	}
	b := &s.backups
	b.mu.Lock()
	defer b.mu.Unlock()
	if rec := b.rec.Recovery; rec != nil {
		if rec.PublicKey != body.PublicKey {
			writeErrorStatus(w, "A recovery key is already set. Backups encrypted to it still need it.", http.StatusConflict)
			return
		}
		writeData(w, map[string]string{"public_key": rec.PublicKey})
		return
	}
	b.rec.Recovery = &backupRecovery{PublicKey: body.PublicKey, ConfirmedAt: time.Now().UTC()}
	b.saveLocked()
	s.auditf(r, "backups.setup.recovery", body.PublicKey, "public key recorded; the private key stayed in the browser")
	writeData(w, map[string]string{"public_key": body.PublicKey})
}

func (s *Server) handleSetupAlertsSkip(w http.ResponseWriter, r *http.Request) {
	var body struct{}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	now := time.Now().UTC()
	b := &s.backups
	b.mu.Lock()
	b.setupLocked().AlertsSkippedAt = &now
	b.saveLocked()
	b.mu.Unlock()
	s.auditf(r, "backups.setup.alerts_skip", "", "alert channel skipped for now")
	writeData(w, map[string]bool{"ok": true})
}

// ── coverage and turning on ──────────────────────────────────────────────────

type setupCoverageServer struct {
	Server    string              `json:"server"`
	Error     string              `json:"error,omitempty"`
	Items     []setupCoverageItem `json:"items"`
	Unmanaged []setupCoverageItem `json:"unmanaged"`
	// NotCoverable are databases teploy has no way to back up: said, not offered.
	NotCoverable []setupCoverageItem  `json:"not_coverable"`
	Target       *setupCoverageTarget `json:"target,omitempty"`
}

type setupCoverageItem struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Engine    string `json:"engine,omitempty"`
	App       string `json:"app,omitempty"`
	SizeBytes int64  `json:"size_bytes"`
}

type setupCoverageTarget struct {
	Bucket string `json:"bucket"`
	Prefix string `json:"prefix"`
}

func backupAdoptable(e *backupEntry) bool {
	return e.Unmanaged && e.Coverage == "uncovered" && e.Engine != "" && !strings.Contains(e.Reason, "no backup method")
}

func setupCoverageOf(env *backupStatusEnvelope) (items, unmanaged, notCoverable []setupCoverageItem) {
	items, unmanaged, notCoverable = []setupCoverageItem{}, []setupCoverageItem{}, []setupCoverageItem{}
	for i := range env.Entries {
		e := &env.Entries[i]
		item := setupCoverageItem{Name: e.Name, Kind: e.Kind, Engine: e.Engine, App: e.App, SizeBytes: entryStoredBytes(e)}
		switch {
		case backupAdoptable(e):
			unmanaged = append(unmanaged, item)
		case e.Coverage == "uncovered":
			notCoverable = append(notCoverable, item)
		case e.Coverage == "excluded":
		default:
			items = append(items, item)
		}
	}
	return items, unmanaged, notCoverable
}

func (s *Server) handleSetupCoverage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Servers []string `json:"servers"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	names, _, err := s.resolveSetupServers(r.Context(), body.Servers, false)
	if err != nil {
		writeError(w, sanitizeDomainError(err.Error()))
		return
	}
	out := make([]setupCoverageServer, len(names))
	s.eachBackupName(r.Context(), names, func(ctx context.Context, i int, name string) {
		out[i] = setupCoverageServer{Server: name, Items: []setupCoverageItem{}, Unmanaged: []setupCoverageItem{}, NotCoverable: []setupCoverageItem{}}
		env, err := s.readBackupStatus(ctx, name)
		s.recordBackupStatus(name, env, err, time.Now().UTC())
		if err != nil {
			out[i].Error = sanitizeDomainError(err.Error())
			return
		}
		out[i].Items, out[i].Unmanaged, out[i].NotCoverable = setupCoverageOf(env)
		if env.Target.Configured {
			out[i].Target = &setupCoverageTarget{Bucket: env.Target.Bucket, Prefix: env.Target.Prefix}
		}
	})
	preset, interval, days := s.setupPolicyDefaults()
	ret, _ := backupPresetRetention(preset)
	volumes, maxGB := true, 0.0
	s.backups.mu.Lock()
	if p := s.backups.rec.Policy; p != nil {
		volumes, maxGB = p.Volumes, p.VolumeMaxGB
	}
	s.backups.mu.Unlock()
	writeData(w, map[string]any{"servers": out, "interval": interval, "preset": preset, "hide_days": days, "volumes": volumes, "volume_max_gb": maxGB,
		"multiplier": backupStorageMultiplier(backupStatPolicy{Interval: interval, Retention: ret}, days)})
}

func (s *Server) setupPolicyDefaults() (preset, interval string, hideDays int) {
	preset, interval, hideDays = backupPresetStandard, backupDefaultSetupInterval, b2.DefaultHideDays
	b := &s.backups
	b.mu.Lock()
	defer b.mu.Unlock()
	if p := b.rec.Policy; p != nil {
		preset, interval = p.Preset, p.Interval
	}
	if setup := b.rec.Setup; setup != nil && setup.HideDays > 0 {
		hideDays = setup.HideDays
	}
	return preset, interval, hideDays
}

// backupStorageMultiplier is how many times one backup's size the bucket holds at
// steady state: the copies retention keeps, the copies it has hidden and that wait out
// the recovery window, and two days of pending uploads under .incoming/.
func backupStorageMultiplier(p backupStatPolicy, hideDays int) int64 {
	return retainedCopies(p) + backupRunsPerDay(p.Interval)*(int64(hideDays)+2)
}

type setupTurnOnResult struct {
	Server     string          `json:"server"`
	OK         bool            `json:"ok"`
	Error      string          `json:"error,omitempty"`
	Note       string          `json:"note,omitempty"`
	AllowEmpty bool            `json:"allow_empty"`
	Adopted    []string        `json:"adopted,omitempty"`
	Run        *backupSetupRun `json:"run,omitempty"`
	// Skipped: everything here was unticked, so no policy or cron line was written.
	Skipped bool `json:"skipped,omitempty"`
}

func (s *Server) handleSetupTurnOn(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string   `json:"password"`
		Servers  []string `json:"servers"`
		// Skip names unmanaged databases, by server, left unticked. Everything else
		// found is adopted: turning on is the consent.
		Skip     map[string][]string `json:"skip"`
		Start    string              `json:"start"`
		Interval string              `json:"interval"`
		Preset   string              `json:"preset"`
		// SkipVolumes and VolumeMaxGB keep what is already set when they are left out.
		SkipVolumes *bool    `json:"skip_volumes"`
		VolumeMaxGB *float64 `json:"volume_max_gb"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	if !s.requireReauth(w, r, body.Password) {
		return
	}
	if body.Start != backupStartNow && body.Start != backupStartNext {
		writeError(w, `Choose "Back up now" or "Start with the next scheduled run".`)
		return
	}
	preset, interval, _ := s.setupPolicyDefaults()
	if body.Preset != "" {
		preset = body.Preset
	}
	if body.Interval != "" {
		interval = body.Interval
	}
	retention, ok := backupPresetRetention(preset)
	if !ok {
		writeError(w, "retention must be simple or standard")
		return
	}
	valid := false
	for _, v := range backupIntervals {
		valid = valid || v == interval
	}
	if !valid {
		writeError(w, "frequency must be one of "+strings.Join(backupIntervals, ", "))
		return
	}
	if body.VolumeMaxGB != nil && *body.VolumeMaxGB < 0 {
		writeError(w, "the volume size cap must be 0 or more")
		return
	}
	names, _, err := s.resolveSetupServers(r.Context(), body.Servers, false)
	if err != nil {
		writeError(w, sanitizeDomainError(err.Error()))
		return
	}
	b := &s.backups
	b.mu.Lock()
	recipient := ""
	if b.rec.Recovery != nil {
		recipient = b.rec.Recovery.PublicKey
	}
	volumes, fleetMaxGB := true, 0.0
	if p := b.rec.Policy; p != nil {
		volumes, fleetMaxGB = p.Volumes, p.VolumeMaxGB
	}
	b.mu.Unlock()
	if recipient == "" {
		writeError(w, "Save a recovery key first: backups are encrypted to it.")
		return
	}
	if body.SkipVolumes != nil {
		volumes = !*body.SkipVolumes
	}
	if body.VolumeMaxGB != nil {
		fleetMaxGB = *body.VolumeMaxGB
	}
	if !b.setupMu.TryLock() {
		writeErrorStatus(w, "Another setup step is running. Wait for it to finish.", http.StatusConflict)
		return
	}
	defer b.setupMu.Unlock()
	ctx := context.WithoutCancel(r.Context())
	results := make([]setupTurnOnResult, len(names))
	s.eachBackupName(ctx, names, func(ctx context.Context, i int, name string) {
		res := setupTurnOnResult{Server: name}
		defer func() { results[i] = res }()
		env, err := s.readBackupStatus(ctx, name)
		s.recordBackupStatus(name, env, err, time.Now().UTC())
		if err != nil {
			res.Error = sanitizeDomainError(err.Error())
			return
		}
		if !env.Target.Configured || !env.Target.HasCredentials {
			res.Error = "This server has no storage yet, so it was skipped."
			return
		}
		skip := map[string]bool{}
		for _, n := range body.Skip[name] {
			skip[n] = true
		}
		var adopt, skipped []string
		targets, databases, notChosen := 0, 0, 0
		for j := range env.Entries {
			e := &env.Entries[j]
			if e.Kind != "volumes" {
				databases++
			}
			switch {
			case backupAdoptable(e):
				if skip[e.Name] {
					skipped = append(skipped, e.Name)
				} else {
					adopt = append(adopt, e.Name+":"+e.Engine+":all")
					res.Adopted = append(res.Adopted, e.Name)
					targets++
				}
			case e.Coverage == "uncovered":
				notChosen++
			case e.Coverage == "excluded":
			case e.Kind == "volumes" && !volumes:
			default:
				targets++
			}
		}
		// Everything unticked is a decision, not a failure: nothing is written, so no run
		// reports one. An existing policy is not this flow's to remove.
		if targets == 0 && len(skipped) > 0 && notChosen == 0 {
			res.Skipped = true
			res.Note = "Skipped: you chose not to back up this server."
			if env.Policy.Configured {
				res.Note += " It already has a backup policy, which Dash left as it was."
			}
			b.mu.Lock()
			b.serverLocked(name).SkippedByChoice = skipped
			setup := b.setupLocked()
			if setup.Skipped == nil {
				setup.Skipped = map[string][]string{}
			}
			setup.Skipped[name] = skipped
			delete(setup.Runs, name)
			b.saveLocked()
			b.mu.Unlock()
			return
		}
		// Only a server with really nothing on it gets allow_empty. One holding a database
		// is not empty, whatever was chosen for that database, and losing every target on
		// one with targets should stay a failure.
		res.AllowEmpty = targets == 0 && databases == 0
		// A server's own cap stays unless the person set one here.
		maxGB := fleetMaxGB
		if body.VolumeMaxGB == nil && env.Policy.Configured {
			maxGB = env.Policy.VolumeMaxGB
		}
		note, err := s.applyBackupPolicy(ctx, name, backupPolicyApply{Interval: interval, Retention: retention, Volumes: volumes,
			VolumeMaxGB: maxGB, Recipient: recipient, AllowEmpty: &res.AllowEmpty, Adopt: adopt})
		res.Note = note
		if err != nil {
			res.Error = sanitizeDomainError(err.Error())
			return
		}
		res.OK = true
		now := time.Now().UTC()
		b.mu.Lock()
		rec := b.serverLocked(name)
		rec.HeartbeatNote = note
		rec.PolicyAppliedAt = now
		rec.SkippedByChoice = nil
		setup := b.setupLocked()
		if setup.Skipped == nil {
			setup.Skipped = map[string][]string{}
		}
		if len(skipped) > 0 {
			setup.Skipped[name] = skipped
		} else {
			delete(setup.Skipped, name)
		}
		b.saveLocked()
		b.mu.Unlock()
		if body.Start == backupStartNow {
			res.Run = s.startDetachedBackup(ctx, name)
		}
	})
	now := time.Now().UTC()
	allOK, anyOK := true, false
	for _, res := range results {
		allOK = allOK && (res.OK || res.Skipped)
		anyOK = anyOK || res.OK
	}
	// Turned on means at least one server really is.
	if anyOK {
		b.mu.Lock()
		b.rec.Policy = &backupPolicySpec{Interval: interval, Preset: preset, Volumes: volumes, VolumeMaxGB: fleetMaxGB, UpdatedAt: now, UpdatedBy: requestActor(r)}
		b.setupLocked().TurnedOnAt = &now
		b.saveLocked()
		b.mu.Unlock()
	}
	s.auditf(r, "backups.setup.turn_on", "", "interval=%s preset=%s volumes=%v start=%s servers=%d all_ok=%v any_ok=%v", interval, preset, volumes, body.Start, len(names), allOK, anyOK)
	s.kickBackupSweep()
	writeData(w, map[string]any{"servers": results, "all_ok": allOK, "any_ok": anyOK, "interval": interval})
}

// startDetachedBackup starts the same run a cron line starts, and records how it went.
// finished and failed exit nonzero with a report, so the report wins.
func (s *Server) startDetachedBackup(ctx context.Context, name string) *backupSetupRun {
	run := &backupSetupRun{RequestedAt: time.Now().UTC()}
	args := []string{"backup", "run", "--all", "--detach", "--host", name, "--json"}
	result, err := s.runCLI(ctx, args...)
	var env *backupDetach
	if result != nil {
		env, _ = decodeBackupDetach(result.Stdout)
	}
	switch {
	case env != nil:
		run.Status, run.Error = env.Status, env.Error
		if env.Result != nil {
			if env.Result.Status != "ok" {
				run.Status = "failed"
			}
			if run.Error == "" {
				run.Error = env.Result.Error
			}
		}
	case err != nil:
		run.Status, run.Error = "failed", err.Error()
	case result != nil && result.ExitCode != 0:
		run.Status, run.Error = "failed", commandFailure(args, result).Error()
	default:
		run.Status, run.Error = "failed", "the teploy CLI did not return a readable report"
	}
	run.Error = sanitizeDomainError(run.Error)
	b := &s.backups
	b.mu.Lock()
	b.setupLocked().Runs[name] = run
	b.saveLocked()
	b.mu.Unlock()
	return run
}

func (s *Server) handleSetupRun(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Servers []string `json:"servers"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	names, _, err := s.resolveSetupServers(r.Context(), body.Servers, false)
	if err != nil {
		writeError(w, sanitizeDomainError(err.Error()))
		return
	}
	runs := map[string]*backupSetupRun{}
	var mu sync.Mutex
	s.eachBackupName(context.WithoutCancel(r.Context()), names, func(ctx context.Context, _ int, name string) {
		run := s.startDetachedBackup(ctx, name)
		mu.Lock()
		runs[name] = run
		mu.Unlock()
		s.auditf(r, "backups.setup.run", name, "status=%s", run.Status)
	})
	s.kickBackupSweep()
	writeData(w, map[string]any{"runs": runs})
}

// ── proving recovery ─────────────────────────────────────────────────────────

func (s *Server) handleSetupProofStart(w http.ResponseWriter, r *http.Request) {
	var body struct{}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	b := &s.backups
	now := time.Now().UTC()
	b.mu.Lock()
	rec := b.rec.Recovery
	if rec == nil {
		b.mu.Unlock()
		writeError(w, "No recovery key has been set yet.")
		return
	}
	// The newest object this key can open: retention may have taken an older one.
	if next := b.rec.pickDrillObject(now); next != nil && (rec.Drill == nil || rec.Drill.Key != next.Key) {
		rec.PreviousDrill, rec.Drill = rec.Drill, next
		b.saveLocked()
	}
	var object backupDrillObject
	found := rec.Drill != nil
	if found {
		object = *rec.Drill
	}
	publicKey := rec.PublicKey
	b.mu.Unlock()
	if !found {
		writeErrorStatus(w, "No backup encrypted to this key has finished yet. Try again after the first backup.", http.StatusConflict)
		return
	}
	args := []string{"backup", "head", object.Key, "--bytes", strconv.Itoa(backupProofBytes), "--host", object.Server, "--json"}
	result, err := s.runCLI(r.Context(), args...)
	if err == nil {
		err = cli.CheckExit(result)
	}
	if err != nil {
		writeErrorStatus(w, sanitizeDomainError("Dash could not read the backup from "+object.Server+": "+err.Error()), http.StatusBadGateway)
		return
	}
	head, err := decodeBackupHead(result.Stdout)
	if err != nil {
		writeErrorStatus(w, sanitizeDomainError(err.Error()), http.StatusBadGateway)
		return
	}
	if !head.Encrypted {
		writeErrorStatus(w, "That backup is not encrypted, so it proves nothing about this key.", http.StatusConflict)
		return
	}
	s.auditf(r, "backups.setup.proof_read", head.Key, "server=%s bytes=%d", object.Server, head.Length)
	writeData(w, map[string]any{"server": object.Server, "key": head.Key, "length": head.Length, "total_size": head.TotalSize,
		"data_base64": head.DataBase64, "public_key": publicKey})
}

func (s *Server) handleSetupProofResult(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key    string `json:"key"`
		Passed bool   `json:"passed"`
		Detail string `json:"detail"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	b := &s.backups
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.rec.Recovery
	if rec == nil {
		writeError(w, "No recovery key has been set yet.")
		return
	}
	var object *backupDrillObject
	for _, o := range []*backupDrillObject{rec.Drill, rec.PreviousDrill} {
		if o != nil && o.Key == body.Key {
			object = o
		}
	}
	if object == nil {
		writeError(w, "That is not the backup this proof read.")
		return
	}
	if !body.Passed {
		s.auditf(r, "backups.setup.proof_failed", object.Key, "server=%s detail=%s", object.Server, sanitizeDomainError(body.Detail))
		writeData(w, map[string]bool{"recorded": true})
		return
	}
	now := time.Now().UTC()
	rec.DrillPassedAt = &now
	b.setupLocked().Proof = &backupProofRecord{At: now, Server: object.Server, Object: object.Key, Actor: requestActor(r)}
	b.saveLocked()
	s.auditf(r, "backups.setup.proof_passed", object.Key, "server=%s", object.Server)
	writeData(w, map[string]any{"recorded": true, "drill_passed_at": now})
}

// ── retiring a server ────────────────────────────────────────────────────────

func (s *Server) handleSetupRetire(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password       string `json:"password"`
		KeyID          string `json:"key_id"`
		ApplicationKey string `json:"application_key"`
		Server         string `json:"server"`
		Data           string `json:"data"`
		Confirm        string `json:"confirm"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	if body.Data != "keep" && body.Data != "remove" {
		writeError(w, "Choose whether to keep or remove this server's backups.")
		return
	}
	if body.Data == "remove" && body.Confirm != body.Server {
		writeError(w, "Type the server's name to remove its backups.")
		return
	}
	if !validSecretField(body.KeyID) || !validSecretField(body.ApplicationKey) {
		writeError(w, "Paste both parts of the key: the key ID and the application key.")
		return
	}
	if !s.requireReauth(w, r, body.Password) {
		return
	}
	b := &s.backups
	if !b.setupMu.TryLock() {
		writeErrorStatus(w, "Another setup step is running. Wait for it to finish.", http.StatusConflict)
		return
	}
	defer b.setupMu.Unlock()
	ctx := context.WithoutCancel(r.Context())
	scrub := func(text string) string { return scrubCredentials(text, body.KeyID, body.ApplicationKey) }
	b.mu.Lock()
	var key *backupServerKey
	if b.rec.Setup != nil {
		if k := b.rec.Setup.Keys[body.Server]; k != nil {
			c := *k
			key = &c
		}
	}
	prefix, bucketName := "", ""
	if st := b.rec.Storage; st != nil {
		prefix, bucketName = st.Prefixes[body.Server], st.Bucket
	}
	b.mu.Unlock()
	if key != nil {
		prefix, bucketName = key.Prefix, key.Bucket
	}
	if key == nil && prefix == "" {
		writeError(w, "Dash has no storage on record for this server.")
		return
	}
	sess, err := s.b2Client().Authorize(ctx, body.KeyID, body.ApplicationKey)
	if err != nil {
		writeErrorStatus(w, scrub(err.Error()), http.StatusUnprocessableEntity)
		return
	}
	need := []string{}
	if key != nil && (key.KeyID != "" || len(key.Replaces) > 0) && !sess.Has(b2.CapDeleteKeys) {
		need = append(need, b2.CapDeleteKeys)
	}
	if body.Data == "remove" {
		for _, c := range []string{b2.CapListBuckets, b2.CapListFiles, b2.CapDeleteFiles} {
			if !sess.Has(c) {
				need = append(need, c)
			}
		}
	}
	if len(need) > 0 {
		writeErrorStatus(w, "This key is missing these permissions: "+strings.Join(need, ", ")+".", http.StatusUnprocessableEntity)
		return
	}
	notes := []string{}
	// A server still in the fleet stops trying first, so it does not fail every night
	// on a deleted key. One that is gone cannot, and that is fine.
	if _, err := s.lookupServerStrict(ctx, body.Server); err == nil {
		for _, args := range [][]string{
			{"server", "backup-policy", "set", "--host", body.Server, "--interval", "none", "--json"},
			{"server", "backup-target", "unset", "--host", body.Server},
		} {
			result, err := s.runCLI(ctx, args...)
			if err == nil {
				err = cli.CheckExit(result)
			}
			if err != nil {
				notes = append(notes, sanitizeDomainError("Could not stop backups on the server: "+err.Error()))
				break
			}
		}
	}
	// Every key this server may hold goes, older ones included. A key that will not go
	// stays on the record, so another try can finish.
	if key != nil {
		pendingKeys := append([]backupLeftoverKey(nil), key.Replaces...)
		if key.KeyID != "" {
			pendingKeys = append(pendingKeys, backupLeftoverKey{KeyID: key.KeyID, KeyName: key.KeyName, Server: body.Server})
		}
		var remaining []backupLeftoverKey
		var lastErr error
		for _, k := range pendingKeys {
			if err := sess.DeleteKey(ctx, k.KeyID); err != nil && !b2.IsMissingKey(err) {
				remaining = append(remaining, k)
				lastErr = err
			}
		}
		b.mu.Lock()
		if k := b.setupLocked().Keys[body.Server]; k != nil {
			// No longer a key anything can count on, whatever happens next.
			k.KeyID, k.Replaces, k.State = "", remaining, backupKeyAmbiguous
		}
		b.saveLocked()
		b.mu.Unlock()
		if lastErr != nil {
			s.auditf(r, "backups.setup.retire_failed", body.Server, "%s", scrub(lastErr.Error()))
			writeErrorStatus(w, "Dash could not delete this server's key: "+scrub(lastErr.Error())+" Try again.", http.StatusBadGateway)
			return
		}
	}
	removed := 0
	if body.Data == "remove" && prefix != "" {
		bucket, err := sess.FindBucket(ctx, bucketName)
		if err == nil && bucket == nil {
			err = errors.New("Backblaze does not show this bucket to this key")
		}
		if err == nil {
			removed, err = sess.DeleteAllVersions(ctx, bucket.ID, prefix+"/")
		}
		if err != nil {
			// Keep what a retry needs: the record still names the bucket and folder.
			s.auditf(r, "backups.setup.retire_partial", body.Server, "removed=%d %s", removed, scrub(err.Error()))
			msg := fmt.Sprintf("Removed %d files, then stopped: %s The rest is still there. Try again to finish.", removed, scrub(err.Error()))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]any{"error": msg, "data": map[string]any{"partial": true, "files_removed": removed}})
			return
		}
		ruleNames := []string{b2.IncomingPrefix(prefix)}
		if !strings.Contains(prefix, "/") {
			ruleNames = append(ruleNames, prefix+"/") // a server at the bucket root has its own window rule
		}
		b.mu.Lock()
		s3Managed := b.rec.Setup != nil && b.rec.Setup.S3Managed
		b.mu.Unlock()
		rules, changed := b2.WithoutPrefixes(bucket.Rules, ruleNames...)
		switch {
		case !changed:
		case s3Managed || b2.Analyze(bucket.Rules, "", b2.DefaultHideDays).S3Managed:
			// Never send rules to a bucket managed through S3. Say what to remove.
			notes = append(notes, "This bucket's rules are managed through the S3 API, so Dash left them. Remove the rules on "+strings.Join(ruleNames, " and ")+" yourself.")
		case sess.Has(b2.CapWriteBuckets):
			if uerr := sess.UpdateRules(ctx, bucket.ID, rules, bucket.Revision); uerr != nil {
				notes = append(notes, "Could not remove this server's lifecycle rule: "+scrub(uerr.Error()))
			}
		default:
			notes = append(notes, "This key cannot change the bucket, so remove the rules on "+strings.Join(ruleNames, " and ")+" in the Backblaze console.")
		}
	}
	b.mu.Lock()
	setup := b.setupLocked()
	delete(setup.Keys, body.Server)
	delete(setup.Preflight, body.Server)
	delete(setup.Runs, body.Server)
	delete(setup.Skipped, body.Server)
	if rec := b.rec.Servers[body.Server]; rec != nil {
		rec.SkippedByChoice = nil
	}
	if body.Data == "remove" && b.rec.Storage != nil {
		delete(b.rec.Storage.Prefixes, body.Server)
	}
	b.saveLocked()
	b.mu.Unlock()
	s.auditf(r, "backups.setup.retire", body.Server, "data=%s files_removed=%d", body.Data, removed)
	s.kickBackupSweep()
	writeData(w, map[string]any{"server": body.Server, "data": body.Data, "files_removed": removed, "notes": notes})
}

// handleSetupLeftoverDismiss clears a notice about a key once the person dealt with it.
func (s *Server) handleSetupLeftoverDismiss(w http.ResponseWriter, r *http.Request) {
	var body struct {
		KeyID string `json:"key_id"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	b := &s.backups
	b.mu.Lock()
	setup := b.setupLocked()
	kept := setup.Leftovers[:0]
	removed := false
	for _, left := range setup.Leftovers {
		if left.KeyID == body.KeyID {
			removed = true
			continue
		}
		kept = append(kept, left)
	}
	setup.Leftovers = kept
	b.saveLocked()
	b.mu.Unlock()
	if !removed {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	s.auditf(r, "backups.setup.leftover_dismissed", body.KeyID, "")
	writeData(w, map[string]bool{"ok": true})
}

// ── routing ──────────────────────────────────────────────────────────────────

func (s *Server) handleBackupSetup(w http.ResponseWriter, r *http.Request, sub string) {
	noStore(w)
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireFeatureMin(w, r, func(f machineFeatures) bool { return f.BackupSetup }, "Protect my data", backupSetupMinCLI) {
		return
	}
	switch sub {
	case "preflight":
		s.handleSetupPreflight(w, r)
	case "storage/check":
		s.handleSetupStorageCheck(w, r)
	case "storage/apply":
		s.handleSetupStorageApply(w, r)
	case "owners":
		s.handleSetupOwners(w, r)
	case "recovery":
		s.handleSetupRecovery(w, r)
	case "alerts/skip":
		s.handleSetupAlertsSkip(w, r)
	case "coverage":
		s.handleSetupCoverage(w, r)
	case "turn-on":
		s.handleSetupTurnOn(w, r)
	case "run":
		s.handleSetupRun(w, r)
	case "proof/start":
		s.handleSetupProofStart(w, r)
	case "proof/result":
		s.handleSetupProofResult(w, r)
	case "retire":
		s.handleSetupRetire(w, r)
	case "leftovers/dismiss":
		s.handleSetupLeftoverDismiss(w, r)
	default:
		jsonError(w, "not found", http.StatusNotFound)
	}
}
