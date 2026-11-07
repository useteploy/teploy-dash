package server

// backuppolicy.go — the recovery key (plan 2.1, P2.3) and the one policy (P2.4).
//
// The private recovery key comes from `teploy backup keygen --json`, goes to the
// browser once and is kept nowhere: not in backups.json, the audit log or any log
// line. Only the public half is recorded. Generating and confirming both re-check
// the password; rotation is forward-only, and the page says so.
//
// The policy is written with `server backup-policy set` flags, which change only
// what they name: a server's excludes, adopted containers and any heartbeat a
// person added survive a policy change.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/useteploy/teploy-dash/internal/alert"
	"github.com/useteploy/teploy-dash/internal/cli"
	"github.com/useteploy/teploy-dash/internal/store"
)

// ── recovery key ─────────────────────────────────────────────────────────────

// backupDrillObject is the object the owner is asked to decrypt.
type backupDrillObject struct {
	Server     string    `json:"server"`
	Key        string    `json:"key"`
	SizeBytes  int64     `json:"size_bytes"`
	Endpoint   string    `json:"endpoint,omitempty"`
	Region     string    `json:"region,omitempty"`
	AssignedAt time.Time `json:"assigned_at"`
}

type backupRecovery struct {
	PublicKey     string     `json:"public_key"`
	ConfirmedAt   time.Time  `json:"confirmed_at"`
	RotatedFrom   string     `json:"rotated_from,omitempty"`
	DrillPassedAt *time.Time `json:"drill_passed_at,omitempty"`
	// Drill and PreviousDrill are the objects offered: retention can take an object
	// away during a slow drill, so a fresh one is offered daily and the last stays good.
	Drill         *backupDrillObject `json:"drill,omitempty"`
	PreviousDrill *backupDrillObject `json:"previous_drill,omitempty"`
}

type backupRecoveryView struct {
	PublicKey     string     `json:"public_key,omitempty"`
	ConfirmedAt   *time.Time `json:"confirmed_at,omitempty"`
	DrillPassedAt *time.Time `json:"drill_passed_at,omitempty"`
	// Banner: a key exists (or backups run) and the drill has not been done.
	Banner bool `json:"banner"`
	// DrillKey and DrillCommand are what to run, once an encrypted object exists.
	DrillKey     string `json:"drill_key,omitempty"`
	DrillCommand string `json:"drill_command,omitempty"`
	// RotationNote says what an old key is still needed for.
	RotationNote string `json:"rotation_note,omitempty"`
}

const backupDrillReoffer = 24 * time.Hour

func (r *backupRecord) recoveryViewLocked(now time.Time) backupRecoveryView {
	view := backupRecoveryView{}
	if r.Recovery == nil {
		view.Banner = r.Storage != nil
		return view
	}
	rec := r.Recovery
	view.PublicKey = rec.PublicKey
	t := rec.ConfirmedAt
	view.ConfirmedAt = &t
	view.DrillPassedAt = rec.DrillPassedAt
	view.Banner = rec.DrillPassedAt == nil
	if rec.RotatedFrom != "" {
		view.RotationNote = "Backups taken before this key still need the previous key (" + rec.RotatedFrom + "). Keep it until they age out."
	}
	if rec.DrillPassedAt != nil {
		return view
	}
	if rec.Drill == nil || now.Sub(rec.Drill.AssignedAt) > backupDrillReoffer {
		if next := r.pickDrillObject(now); next != nil {
			rec.PreviousDrill = rec.Drill
			rec.Drill = next
		}
	}
	if rec.Drill != nil {
		view.DrillKey = rec.Drill.Key
		view.DrillCommand = backupDrillCommand(rec.Drill)
	}
	return view
}

// pickDrillObject is the newest object encrypted since this key was set, so the
// drill proves THIS key.
func (r *backupRecord) pickDrillObject(now time.Time) *backupDrillObject {
	var best *backupDrillObject
	var bestAt time.Time
	consider := func(server string, target backupStatTarget, o *backupObject) {
		if o == nil || !o.Encrypted {
			return
		}
		at, err := time.Parse(time.RFC3339, o.CreatedAt)
		if err != nil || at.Before(r.Recovery.ConfirmedAt) || (best != nil && !at.After(bestAt)) {
			return
		}
		bestAt = at
		best = &backupDrillObject{Server: server, Key: o.Key, SizeBytes: o.SizeBytes, Endpoint: target.Endpoint, Region: target.Region, AssignedAt: now}
	}
	names := make([]string, 0, len(r.Servers))
	for name := range r.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		rec := r.Servers[name]
		if rec.Status == nil {
			continue
		}
		for i := range rec.Status.Entries {
			e := &rec.Status.Entries[i]
			consider(name, rec.Status.Target, e.Newest)
			for _, db := range e.Databases {
				consider(name, rec.Status.Target, db.Newest)
			}
		}
	}
	return best
}

func backupDrillCommand(o *backupDrillObject) string {
	cmd := "TEPLOY_S3_ACCESS_KEY=<key> TEPLOY_S3_SECRET_KEY=<secret> teploy backup fetch " + o.Key + " --identity recovery.txt"
	if o.Endpoint != "" {
		cmd += " --endpoint " + o.Endpoint
	}
	if o.Region != "" {
		cmd += " --region " + o.Region
	}
	return cmd + " --output drill-output --json"
}

// requireReauth re-checks the password of the local account behind this session.
// A single sign-on session has no password here: recovery key actions need a local
// admin. No-auth mode is the operator's deliberate single-user choice.
func (s *Server) requireReauth(w http.ResponseWriter, r *http.Request, password string) bool {
	if s.gate == nil {
		return true
	}
	session, ok := currentUser(r)
	if !ok {
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	if !session.local {
		jsonError(w, "Recovery key actions need a local account's password. Sign in with a local admin account.", http.StatusForbidden)
		return false
	}
	if _, ok := s.gate.authenticate(session.user, password); !ok {
		s.auditf(r, "backups.recovery.reauth_failed", session.user, "wrong password")
		jsonError(w, "That password is not correct.", http.StatusUnauthorized)
		return false
	}
	return true
}

// handleBackupKeyGenerate serves POST /api/backups/recovery-key/generate.
func (s *Server) handleBackupKeyGenerate(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireBackupFleet(w, r) {
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	if !s.requireReauth(w, r, body.Password) {
		return
	}
	args := []string{"backup", "keygen", "--json"}
	result, err := s.runCLI(r.Context(), args...)
	if err == nil {
		err = cli.CheckExit(result)
	}
	if err != nil {
		// Never result.Stdout: a partial reply could hold the key.
		writeErrorStatus(w, "The teploy CLI could not generate a key.", http.StatusBadGateway)
		return
	}
	var out struct {
		PublicKey  string `json:"public_key"`
		PrivateKey string `json:"private_key"`
	}
	if _, err := cli.ParseJSON(result.Stdout); err != nil || json.Unmarshal([]byte(result.Stdout), &out) != nil ||
		!strings.HasPrefix(out.PublicKey, "age1") || !strings.HasPrefix(out.PrivateKey, "AGE-SECRET-KEY-") {
		writeErrorStatus(w, "The teploy CLI returned something other than a key.", http.StatusBadGateway)
		return
	}
	s.backups.mu.Lock()
	s.backups.pendingKey = out.PublicKey
	s.backups.mu.Unlock()
	s.auditf(r, "backups.recovery.generate", out.PublicKey, "recovery key generated and shown once; not stored")
	writeData(w, map[string]string{
		"public_key":  out.PublicKey,
		"private_key": out.PrivateKey,
		"file_text":   "# Teploy backup recovery key. Shown once; stored nowhere. Keep it offline.\n# public key: " + out.PublicKey + "\n" + out.PrivateKey + "\n",
		"file_name":   "teploy-recovery-key.txt",
	})
}

// handleBackupKeyConfirm serves POST /api/backups/recovery-key/confirm: the owner
// saved the private key, so servers start encrypting to the public one.
func (s *Server) handleBackupKeyConfirm(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireBackupFleet(w, r) {
		return
	}
	var body struct {
		Password  string `json:"password"`
		PublicKey string `json:"public_key"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	if !s.requireReauth(w, r, body.Password) {
		return
	}
	b := &s.backups
	b.mu.Lock()
	match := b.pendingKey != "" && b.pendingKey == body.PublicKey
	b.mu.Unlock()
	if !match {
		writeError(w, "That is not the key just generated. Generate a new one.")
		return
	}
	servers, err := s.resolveServers(r.Context())
	if err != nil {
		writeErrorStatus(w, sanitizeDomainError("server discovery failed: "+err.Error()), http.StatusBadGateway)
		return
	}
	ctx := context.WithoutCancel(r.Context())
	results := s.forEachBackupServer(ctx, servers, func(ctx context.Context, name string) error {
		args := []string{"server", "backup-policy", "set", "--host", name, "--recipient", body.PublicKey, "--json"}
		// With a key, Dash's own volumes no longer have to stay out.
		restored := false
		if b.selfExcludedOn(name) {
			if current, err := s.readBackupPolicyShow(ctx, name); err == nil {
				args = append(args, "--exclude="+strings.Join(b.withoutSelfExcludes(name, current.Excludes), ","))
				restored = true
			}
		}
		result, err := s.runCLI(ctx, args...)
		if err != nil {
			return err
		}
		if result.ExitCode != 0 {
			return commandFailure(args, result)
		}
		if restored {
			b.setSelfExcluded(name, nil)
		}
		return nil
	})
	allOK := len(results) > 0
	for _, res := range results {
		allOK = allOK && res.OK
	}
	now := time.Now().UTC()
	b.mu.Lock()
	if allOK {
		b.pendingKey = ""
	}
	prev := ""
	if b.rec.Recovery != nil {
		prev = b.rec.Recovery.PublicKey
	}
	if prev != body.PublicKey {
		b.rec.Recovery = &backupRecovery{PublicKey: body.PublicKey, ConfirmedAt: now, RotatedFrom: prev}
	}
	b.saveLocked()
	b.mu.Unlock()
	s.auditf(r, "backups.recovery.confirm", body.PublicKey, "servers=%d all_ok=%v", len(results), allOK)
	s.kickBackupSweep()
	writeData(w, map[string]any{"servers": results, "all_ok": allOK})
}

// handleBackupDrill serves POST /api/backups/recovery-key/drill {"output": "<the
// JSON teploy backup fetch printed>"}. Dash holds no private key, so it cannot watch
// the decryption. What it checks: the fetch ran with --identity against an object
// Dash assigned, and it reports a plaintext of the size that ciphertext decrypts
// to. A person could forge that; the aim is that the owner actually ran it.
func (s *Server) handleBackupDrill(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireBackupFleet(w, r) {
		return
	}
	var body struct {
		Output string `json:"output"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	var fetched struct {
		Key       string `json:"key"`
		SizeBytes int64  `json:"size_bytes"`
		Encrypted bool   `json:"encrypted"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(body.Output)), &fetched) != nil || fetched.Key == "" {
		writeError(w, "Paste the JSON that teploy backup fetch printed.")
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
		if o != nil && o.Key == fetched.Key {
			object = o
		}
	}
	switch {
	case object == nil:
		writeError(w, "That is not the object this drill asked for. Use the command shown on this page.")
		return
	case !fetched.Encrypted:
		writeError(w, "That fetch did not decrypt anything.")
		return
	case !plausiblePlaintextSize(object.SizeBytes, fetched.SizeBytes):
		writeError(w, fmt.Sprintf("The reported size (%d bytes) does not fit a %d-byte encrypted object. Run the command again and paste its output unchanged.", fetched.SizeBytes, object.SizeBytes))
		return
	}
	now := time.Now().UTC()
	rec.DrillPassedAt = &now
	b.saveLocked()
	s.auditf(r, "backups.recovery.drill", fetched.Key, "decrypt drill recorded: %d bytes", fetched.SizeBytes)
	writeData(w, map[string]any{"ok": true, "drill_passed_at": now})
}

// plausiblePlaintextSize: age adds a header of a few hundred bytes (a stanza per
// recipient), a 16-byte nonce and 16 bytes per 64 KiB chunk, so the difference
// between ciphertext and plaintext is small and known.
func plausiblePlaintextSize(cipher, plain int64) bool {
	if plain <= 0 || plain >= cipher {
		return false
	}
	maxOverhead := 2048 + 16*(plain/65536+2)
	return cipher-plain <= maxOverhead
}

// ── one policy ───────────────────────────────────────────────────────────────

// backupWindow is when automatic restore tests may start, in UTC hours.
type backupWindow struct {
	StartHour int `json:"start_hour"`
	EndHour   int `json:"end_hour"`
}

var defaultBackupWindow = backupWindow{StartHour: 2, EndHour: 6}

// Contains reports whether hour is inside the window, which may wrap midnight.
func (w backupWindow) Contains(hour int) bool {
	if w.StartHour <= w.EndHour {
		return hour >= w.StartHour && hour < w.EndHour
	}
	return hour >= w.StartHour || hour < w.EndHour
}

// The presets the plan names.
const (
	backupPresetSimple   = "simple"
	backupPresetStandard = "standard"
)

func backupPresetNames() []string { return []string{backupPresetSimple, backupPresetStandard} }

func backupPresetRetention(name string) (backupRetention, bool) {
	switch name {
	case backupPresetSimple:
		return backupRetention{MaxAgeDays: 14}, true
	case backupPresetStandard:
		return backupRetention{KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 12}, true
	}
	return backupRetention{}, false
}

// backupIntervals are the frequencies offered, from the CLI's accepted list.
var backupIntervals = []string{"1h", "2h", "3h", "4h", "6h", "8h", "12h", "24h", "7d"}

func backupRunsPerDay(interval string) int64 {
	switch interval {
	case "":
		return 1
	case "15m":
		return 96
	case "30m":
		return 48
	case "1h", "hourly":
		return 24
	case "2h":
		return 12
	case "3h":
		return 8
	case "4h":
		return 6
	case "6h":
		return 4
	case "8h":
		return 3
	case "12h":
		return 2
	}
	return 1
}

func backupIntervalDuration(interval string) time.Duration {
	switch interval {
	case "24h":
		return 24 * time.Hour
	case "7d":
		return 7 * 24 * time.Hour
	}
	if perDay := backupRunsPerDay(interval); perDay > 1 {
		return 24 * time.Hour / time.Duration(perDay)
	}
	return 0
}

// backupPolicySpec is the one policy, as the owner chose it.
type backupPolicySpec struct {
	Interval    string    `json:"interval"`
	Preset      string    `json:"preset"`
	Volumes     bool      `json:"volumes"`
	VolumeMaxGB float64   `json:"volume_max_gb,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
	UpdatedBy   string    `json:"updated_by,omitempty"`
}

type backupServerResult struct {
	Server string `json:"server"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	Note   string `json:"note,omitempty"`
}

// forEachBackupServer runs fn on every server under the push bound and reports
// each one. A failure stays that server's own.
func (s *Server) forEachBackupServer(ctx context.Context, servers []serverName, fn func(context.Context, string) error) []backupServerResult {
	names := backupServerNames(servers)
	results := make([]backupServerResult, len(names))
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
			results[i] = backupServerResult{Server: name, OK: true}
			if err := fn(pctx, name); err != nil {
				results[i].OK, results[i].Error = false, sanitizeDomainError(err.Error())
			}
		}(i, name)
	}
	wg.Wait()
	return results
}

// handleBackupPolicy serves POST /api/backups/policy.
func (s *Server) handleBackupPolicy(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireBackupFleet(w, r) {
		return
	}
	var body struct {
		Interval    string  `json:"interval"`
		Preset      string  `json:"preset"`
		Volumes     bool    `json:"volumes"`
		VolumeMaxGB float64 `json:"volume_max_gb"`
		WindowStart *int    `json:"window_start_hour"`
		WindowEnd   *int    `json:"window_end_hour"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	retention, ok := backupPresetRetention(body.Preset)
	if !ok {
		writeError(w, "retention must be simple or standard")
		return
	}
	valid := false
	for _, v := range backupIntervals {
		valid = valid || v == body.Interval
	}
	if !valid {
		writeError(w, "frequency must be one of "+strings.Join(backupIntervals, ", "))
		return
	}
	if body.VolumeMaxGB < 0 {
		writeError(w, "the volume size cap must be 0 or more")
		return
	}
	window := s.backupWindowFrom(body.WindowStart, body.WindowEnd)
	if window == nil {
		writeError(w, "the restore test window hours must be 0 to 23 and differ")
		return
	}
	servers, err := s.resolveServers(r.Context())
	if err != nil {
		writeErrorStatus(w, sanitizeDomainError("server discovery failed: "+err.Error()), http.StatusBadGateway)
		return
	}
	if len(servers) == 0 {
		writeError(w, "no servers to apply a policy to: add one in Fleet first")
		return
	}
	b := &s.backups
	b.mu.Lock()
	recipient := ""
	if b.rec.Recovery != nil {
		recipient = b.rec.Recovery.PublicKey
	}
	b.mu.Unlock()

	ctx := context.WithoutCancel(r.Context())
	notes := make(map[string]string, len(servers))
	var notesMu sync.Mutex
	results := s.forEachBackupServer(ctx, servers, func(ctx context.Context, name string) error {
		note, err := s.applyBackupPolicy(ctx, name, backupPolicyApply{Interval: body.Interval, Retention: retention, Volumes: body.Volumes, VolumeMaxGB: body.VolumeMaxGB, Recipient: recipient})
		notesMu.Lock()
		notes[name] = note
		notesMu.Unlock()
		return err
	})
	now := time.Now().UTC()
	allOK := true
	b.mu.Lock()
	for i := range results {
		results[i].Note = notes[results[i].Server]
		allOK = allOK && results[i].OK
		rec := b.serverLocked(results[i].Server)
		rec.HeartbeatNote = notes[results[i].Server]
		if results[i].OK {
			rec.PolicyAppliedAt = now
		}
	}
	b.rec.Policy = &backupPolicySpec{Interval: body.Interval, Preset: body.Preset, Volumes: body.Volumes, VolumeMaxGB: body.VolumeMaxGB, UpdatedAt: now, UpdatedBy: requestActor(r)}
	b.rec.Window = *window
	b.saveLocked()
	b.mu.Unlock()
	s.auditf(r, "backups.policy.set", "", "interval=%s preset=%s volumes=%v volume_max_gb=%g servers=%d all_ok=%v", body.Interval, body.Preset, body.Volumes, body.VolumeMaxGB, len(results), allOK)
	s.kickBackupSweep()
	writeData(w, map[string]any{"servers": results, "all_ok": allOK})
}

func (s *Server) backupWindowFrom(start, end *int) *backupWindow {
	s.backups.mu.Lock()
	window := s.backups.rec.Window
	s.backups.mu.Unlock()
	if start != nil {
		window.StartHour = *start
	}
	if end != nil {
		window.EndHour = *end
	}
	if window.StartHour < 0 || window.StartHour > 23 || window.EndHour < 0 || window.EndHour > 23 || window.StartHour == window.EndHour {
		return nil
	}
	return &window
}

// backupPolicyApply is one write of the policy. AllowEmpty and Adopt are only for the
// setup flow: the manual form leaves them as they are.
type backupPolicyApply struct {
	Interval    string
	Retention   backupRetention
	Volumes     bool
	VolumeMaxGB float64
	Recipient   string
	AllowEmpty  *bool
	Adopt       []string
}

// applyBackupPolicy writes the policy to one server. The returned note is what the
// owner should know about its heartbeat.
func (s *Server) applyBackupPolicy(ctx context.Context, name string, p backupPolicyApply) (string, error) {
	interval, ret, volumes, volumeMaxGB, recipient := p.Interval, p.Retention, p.Volumes, p.VolumeMaxGB, p.Recipient
	current, err := s.readBackupPolicyShow(ctx, name)
	if err != nil {
		return "", err
	}
	args := []string{"server", "backup-policy", "set", "--host", name, "--interval", interval,
		"--keep-last=" + strconv.Itoa(ret.KeepLast), "--keep-daily=" + strconv.Itoa(ret.KeepDaily),
		"--keep-weekly=" + strconv.Itoa(ret.KeepWeekly), "--keep-monthly=" + strconv.Itoa(ret.KeepMonthly),
		"--max-age-days=" + strconv.Itoa(ret.MaxAgeDays), "--volumes=" + strconv.FormatBool(volumes),
		"--volume-max-gb=" + strconv.FormatFloat(volumeMaxGB, 'f', -1, 64), "--json"}
	if recipient != "" {
		args = append(args, "--recipient", recipient)
	}
	if p.AllowEmpty != nil {
		args = append(args, "--allow-empty="+strconv.FormatBool(*p.AllowEmpty))
	}
	for _, a := range p.Adopt {
		args = append(args, "--adopt", a)
	}
	// Without any recovery key, a volume backup of Dash would upload its SSH key in
	// plaintext, so its own volumes stay out until a key arrives.
	var selfExcluded []string
	selfChanged := false
	if recipient == "" && current.Recipient == "" {
		merged, added := s.backups.withSelfExcludes(current.Excludes)
		if len(added) > 0 {
			args = append(args, "--exclude="+strings.Join(merged, ","))
			selfExcluded, selfChanged = append(s.backups.selfExcludedList(name), added...), true
		}
	} else if s.backups.selfExcludedOn(name) {
		args = append(args, "--exclude="+strings.Join(s.backups.withoutSelfExcludes(name, current.Excludes), ","))
		selfChanged = true
	}
	heartbeat, note := s.backupHeartbeatFor(name, interval, current)
	var result *cli.Result
	if heartbeat != "" {
		// The URL carries a token, so it only ever travels on stdin.
		result, err = s.runCLIStdin(ctx, heartbeat+"\n", append(args, "--heartbeat-stdin")...)
		if err != nil {
			return note, err
		}
	} else {
		result, err = s.runCLI(ctx, args...)
		if err == nil {
			err = cli.CheckExit(result)
		}
		if err != nil {
			return note, err
		}
	}
	if selfChanged {
		s.backups.setSelfExcluded(name, selfExcluded)
	}
	if _, err := cli.ParseJSON(result.Stdout); err != nil {
		return note, fmt.Errorf("the policy may be stored, but the reply could not be read: %w", err)
	}
	return note, nil
}

// withSelfExcludes adds Dash's own volumes to an exclude list, returning what it added.
func (b *backupState) withSelfExcludes(excludes []string) (merged, added []string) {
	merged = append(merged, excludes...)
	for _, self := range backupSelfExcludes(b.selfApp) {
		have := false
		for _, e := range excludes {
			have = have || e == self
		}
		if !have {
			merged = append(merged, self)
			added = append(added, self)
		}
	}
	return merged, added
}

func (b *backupState) selfExcludedList(name string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.serverLocked(name).SelfExcluded...)
}

func (b *backupState) selfExcludedOn(name string) bool { return len(b.selfExcludedList(name)) > 0 }

// withoutSelfExcludes takes back out only what Dash itself added.
func (b *backupState) withoutSelfExcludes(name string, excludes []string) []string {
	added := map[string]bool{}
	for _, e := range b.selfExcludedList(name) {
		added[e] = true
	}
	out := make([]string, 0, len(excludes))
	for _, e := range excludes {
		if !added[e] {
			out = append(out, e)
		}
	}
	return out
}

func (b *backupState) setSelfExcluded(name string, list []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.serverLocked(name).SelfExcluded = list
	b.saveLocked()
}

type backupPolicyShow struct {
	Configured     bool     `json:"configured"`
	Excludes       []string `json:"excludes"`
	HeartbeatCount int      `json:"heartbeat_count"`
	HeartbeatHosts []string `json:"heartbeat_hosts"`
	Recipient      string   `json:"recipient"`
}

func (s *Server) readBackupPolicyShow(ctx context.Context, name string) (*backupPolicyShow, error) {
	args := []string{"server", "backup-policy", "show", "--host", name, "--json"}
	result, err := s.runCLI(ctx, args...)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, commandFailure(args, result)
	}
	if _, err := cli.ParseJSON(result.Stdout); err != nil {
		return nil, err
	}
	var out backupPolicyShow
	if err := json.Unmarshal([]byte(result.Stdout), &out); err != nil {
		return nil, fmt.Errorf("decoding backup policy: %w", err)
	}
	return &out, nil
}

// ── the heartbeat Dash owns ──────────────────────────────────────────────────

// backupHeartbeatFor makes sure this server has a push monitor at the policy's
// interval, and returns the URL to send to the server ("" when none needs sending).
// The CLI's --heartbeat-stdin replaces the whole list and show masks the tokens, so
// a list holding some other receiver is left alone and the owner is told: Dash
// cannot append (a CLI gap).
func (s *Server) backupHeartbeatFor(name, interval string, current *backupPolicyShow) (string, string) {
	public := s.alertPublicURL()
	if public == "" {
		return "", "Set Dash's public URL in Settings so this server can report in; without it a stopped cron line is only caught by the slower status check."
	}
	if s.store == nil {
		return "", ""
	}
	pub, err := url.Parse(public)
	if err != nil || pub.Host == "" {
		return "", "Dash's public URL is not a valid URL, so no heartbeat was set."
	}
	receiver := pub.Scheme + "://" + pub.Host
	ours, others := false, false
	for _, h := range current.HeartbeatHosts {
		if h == receiver {
			ours = true
		} else {
			others = true
		}
	}
	if others {
		return "", "This server already sends heartbeats elsewhere. The CLI can only replace that list, so Dash left it alone and adds no heartbeat of its own."
	}
	m, err := s.ensureBackupHeartbeat(name, interval)
	if err != nil {
		log.Printf("[backups] heartbeat monitor for %s: %v", name, err)
		return "", "The heartbeat monitor could not be saved, so none was set."
	}
	if ours {
		return "", ""
	}
	return public + "/api/push/" + m.PushToken, ""
}

var backupMonitorIDRE = regexp.MustCompile(`^backups-`)

// ensureBackupHeartbeat creates or updates the push monitor Dash owns for a
// server. A person's edits to it (disabling it, a longer grace) are kept apart
// from the interval, which follows the policy.
func (s *Server) ensureBackupHeartbeat(name, interval string) (store.Monitor, error) {
	b := &s.backups
	b.mu.Lock()
	id := b.serverLocked(name).HeartbeatMonitorID
	b.mu.Unlock()
	every := backupIntervalDuration(interval)
	var m store.Monitor
	if id != "" {
		if cur, err := s.store.GetMonitor(id); err == nil && cur != nil {
			m = *cur
		}
	}
	if m.ID == "" {
		m = store.Monitor{ID: "backups-" + randomHex(8), Name: "Backups on " + name, Type: "push", Enabled: true,
			Server: name, FailAfter: 1, RecoverAfter: 1, PushToken: newPushToken()}
	}
	m.Interval = every
	// Stale is no success in twice the interval, so the grace is one interval.
	m.Grace = every
	if m.Grace > pushMaxGrace {
		m.Grace = pushMaxGrace
	}
	if err := validatePushMonitor(&m); err != nil {
		return m, err
	}
	if err := s.store.SaveMonitor(&m); err != nil {
		return m, err
	}
	if s.monitor != nil {
		s.monitor.Reload(m)
	}
	b.mu.Lock()
	b.serverLocked(name).HeartbeatMonitorID = m.ID
	b.mu.Unlock()
	return m, nil
}

func (s *Server) removeBackupHeartbeat(id string) {
	if s.store == nil || !backupMonitorIDRE.MatchString(id) {
		return
	}
	if err := s.store.DeleteMonitor(id); err != nil {
		log.Printf("[backups] removing heartbeat monitor %s: %v", id, err)
	}
	if s.monitor != nil {
		s.monitor.Remove(id)
	}
}

// isBackupHeartbeatToken reports whether a push token is one of the heartbeats
// Dash owns. Its only job is to prove the server's job ran: a failure the run
// reports is the backup rule's (one owner per signal, plan 2.3), and the status
// read names which target failed.
func (s *Server) isBackupHeartbeatToken(token string) bool {
	if s.store == nil {
		return false
	}
	b := &s.backups
	b.mu.Lock()
	ids := make([]string, 0, len(b.rec.Servers))
	for _, rec := range b.rec.Servers {
		if rec.HeartbeatMonitorID != "" {
			ids = append(ids, rec.HeartbeatMonitorID)
		}
	}
	b.mu.Unlock()
	for _, id := range ids {
		if m, err := s.store.GetMonitor(id); err == nil && m != nil && m.PushToken == token {
			return true
		}
	}
	return false
}

// ── per-app exclude and override ─────────────────────────────────────────────

var backupExcludeRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,254}(/[a-zA-Z0-9][a-zA-Z0-9_.-]{0,254})?$`)

// handleBackupExclude serves POST /api/backups/exclude {server, name, excluded,
// reason}: a read-merge-write of that server's exclude list through the CLI.
func (s *Server) handleBackupExclude(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireBackupFleet(w, r) {
		return
	}
	var body struct {
		Server   string `json:"server"`
		Name     string `json:"name"`
		Excluded bool   `json:"excluded"`
		Reason   string `json:"reason"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	if !backupExcludeRE.MatchString(body.Name) {
		writeError(w, "that name cannot be excluded")
		return
	}
	if body.Excluded && strings.TrimSpace(body.Reason) == "" {
		writeError(w, "say why this is excluded: it goes in the audit log")
		return
	}
	if !body.Excluded {
		for _, self := range backupSelfExcludes(s.backups.selfApp) {
			if body.Name != self {
				continue
			}
			s.backups.mu.Lock()
			hasKey := s.backups.rec.Recovery != nil
			s.backups.mu.Unlock()
			if !hasKey {
				writeError(w, "These volumes hold Dash's SSH key, so they stay out until a recovery key encrypts them.")
				return
			}
		}
	}
	if _, err := s.lookupServerStrict(r.Context(), body.Server); err != nil {
		writeError(w, sanitizeDomainError(err.Error()))
		return
	}
	current, err := s.readBackupPolicyShow(r.Context(), body.Server)
	if err != nil {
		writeErrorStatus(w, sanitizeDomainError(err.Error()), http.StatusBadGateway)
		return
	}
	if !current.Configured {
		writeError(w, "this server has no policy yet: apply the policy first")
		return
	}
	merged := make([]string, 0, len(current.Excludes)+1)
	for _, e := range current.Excludes {
		if e != body.Name {
			merged = append(merged, e)
		}
	}
	if body.Excluded {
		merged = append(merged, body.Name)
	}
	args := []string{"server", "backup-policy", "set", "--host", body.Server, "--exclude=" + strings.Join(merged, ","), "--json"}
	result, err := s.runCLI(r.Context(), args...)
	if err == nil {
		err = cli.CheckExit(result)
	}
	if err != nil {
		writeErrorStatus(w, sanitizeDomainError(err.Error()), http.StatusBadGateway)
		return
	}
	action := "backups.exclude"
	if !body.Excluded {
		action = "backups.include"
	}
	s.auditf(r, action, body.Server+"/"+body.Name, "reason=%s", sanitizeDomainError(body.Reason))
	s.kickBackupSweep()
	writeData(w, map[string]any{"excludes": merged})
}

var (
	backupCronRE = regexp.MustCompile(`^[0-9*/,-]+( [0-9*/,-]+){4}$`)
	backupAppRE  = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,254}$`)
)

// handleBackupOverride serves POST /api/backups/override {server, app, cron,
// keep_last, max_age_days, remove}: a per-app schedule for the app's volumes, which
// `backup run --all` then leaves to it. It covers volumes only: from a server's
// state the CLI can schedule and unschedule an app's volumes, not its accessories
// (`accessory backup --schedule` needs a teploy.yml).
func (s *Server) handleBackupOverride(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.requireBackupFleet(w, r) {
		return
	}
	var body struct {
		Server     string `json:"server"`
		App        string `json:"app"`
		Cron       string `json:"cron"`
		KeepLast   int    `json:"keep_last"`
		MaxAgeDays int    `json:"max_age_days"`
		Remove     bool   `json:"remove"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	if !backupAppRE.MatchString(body.App) {
		writeError(w, "invalid app name")
		return
	}
	if _, err := s.lookupServerStrict(r.Context(), body.Server); err != nil {
		writeError(w, sanitizeDomainError(err.Error()))
		return
	}
	var args []string
	detail := "schedule removed: the policy covers it again"
	if body.Remove {
		args = []string{"backup", "unschedule", "--app", body.App, "--host", body.Server}
	} else {
		if !backupCronRE.MatchString(body.Cron) {
			writeError(w, "the schedule must be five cron fields, such as 0 3 * * *")
			return
		}
		if body.KeepLast < 0 || body.MaxAgeDays < 0 {
			writeError(w, "retention values must be 0 or more")
			return
		}
		args = []string{"backup", "schedule", body.Cron, "--app", body.App, "--host", body.Server,
			"--keep-last=" + strconv.Itoa(body.KeepLast), "--max-age-days=" + strconv.Itoa(body.MaxAgeDays)}
		detail = fmt.Sprintf("cron=%q keep_last=%d max_age_days=%d", body.Cron, body.KeepLast, body.MaxAgeDays)
	}
	result, err := s.runCLI(r.Context(), args...)
	if err == nil {
		err = cli.CheckExit(result)
	}
	if err != nil {
		writeErrorStatus(w, sanitizeDomainError(err.Error()), http.StatusBadGateway)
		return
	}
	s.auditf(r, "backups.override", body.Server+"/"+body.App, "%s", detail)
	s.kickBackupSweep()
	writeData(w, map[string]bool{"ok": true})
}

// backupAlertRuleSource keeps the alert package import honest for the rules in
// backupalerts.go; both files describe the same owner.
const backupAlertRuleSource = alert.SourceBackup
