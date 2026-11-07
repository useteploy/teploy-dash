package server

// backupsetup_status.go — what "Protected" means, and Dash's own backup.
//
// Protected: a verified backup on each covered target, plus a passing restore test for
// each covered database. Volume-only servers need no restore test. A status that could
// not be read proves nothing, so it never reads as protected.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// backupGuardSkipsShown: this many skips in a row and the page says why, rather than
// waiting silently.
const backupGuardSkipsShown = 3

// Per-server setup states.
const (
	setupNotProtected  = "not_protected"
	setupUnknown       = "unknown"
	setupCapped        = "capped"
	setupUnreachable   = "unreachable"
	setupEmpty         = "empty"
	setupWaitingBackup = "waiting_first_backup"
	setupLate          = "late"
	setupFailing       = "failing"
	setupWaitingTest   = "waiting_restore_test"
	setupGuardSkipped  = "guard_skipped"
	setupTestFailed    = "restore_test_failed"
	setupTestOverdue   = "restore_test_overdue"
	// setupNotCovered: a database is not in any backup, and nobody chose that.
	setupNotCovered = "not_covered"
	// setupExcept: protected, apart from what the person chose to leave out.
	setupExcept = "protected_except"
	// setupAllSkipped: everything here was left out on purpose.
	setupAllSkipped = "all_skipped"
	setupProtected  = "protected"
)

// backupRestoreTestMaxAge: an older pass no longer says a backup restores.
const backupRestoreTestMaxAge = 21 * 24 * time.Hour

type backupSetupServerView struct {
	Server string `json:"server"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
	// Uncovered names what is not in any backup here and nobody chose that; Skipped is
	// what a person chose to leave out.
	Uncovered []string        `json:"uncovered,omitempty"`
	Skipped   []string        `json:"skipped,omitempty"`
	Run       *backupSetupRun `json:"run,omitempty"`
	Key       string          `json:"key_name,omitempty"`
	Lifecycle string          `json:"lifecycle,omitempty"`
}

type backupSetupView struct {
	Supported bool   `json:"supported"`
	MinCLI    string `json:"min_cli"`
	// State: none | partial | attention | protected_except | protected.
	State    string `json:"state"`
	Headline string `json:"headline"`
	// Configured: storage exists, whichever way it was set up.
	Configured       bool                    `json:"configured"`
	Servers          []backupSetupServerView `json:"servers"`
	NewServers       []string                `json:"new_servers,omitempty"`
	RetiredKeys      []string                `json:"retired_keys,omitempty"`
	Leftovers        []backupLeftoverKey     `json:"leftovers,omitempty"`
	AlertsConfigured bool                    `json:"alerts_configured"`
	AlertsSkipped    bool                    `json:"alerts_skipped"`
	Bucket           string                  `json:"bucket,omitempty"`
	Endpoint         string                  `json:"endpoint,omitempty"`
	Base             string                  `json:"base"`
	HideDays         int                     `json:"hide_days,omitempty"`
	S3Managed        bool                    `json:"s3_managed,omitempty"`
	TurnedOn         bool                    `json:"turned_on"`
	Proof            *backupProofRecord      `json:"proof,omitempty"`
	Multiplier       int64                   `json:"multiplier,omitempty"`
	StoredBytes      int64                   `json:"stored_bytes,omitempty"`
	ProjectedBytes   int64                   `json:"projected_bytes,omitempty"`
	RecoveryDoc      string                  `json:"recovery_doc"`
	// Reauth says how this viewer confirms a sensitive step: password, none, or
	// local_account when a single sign-on session has no password to give.
	Reauth string `json:"reauth,omitempty"`
}

// backupTargetCapped: the store's daily cap, as the CLI or aws report it. It is not an
// outage and is not shown as one.
func backupTargetCapped(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "transaction_cap") || strings.Contains(lower, "transactioncap") || strings.Contains(lower, "cap exceeded")
}

const backupCappedMessage = "Backblaze's daily transaction cap has been reached, so Dash cannot look at this server's backups today. They are still there. Raise the cap in the Backblaze console, or wait for tomorrow."

// lastRealRun is a test's newest run that actually ran, and how many guard skips sit
// after it.
func (t *backupAutoTest) lastRealRun() (*backupTestRun, int, string) {
	skips, reason := 0, ""
	for i := len(t.History) - 1; i >= 0; i-- {
		run := &t.History[i]
		if run.Skipped == "" {
			return run, skips, reason
		}
		if skips == 0 {
			reason = run.Skipped
		}
		skips++
	}
	return nil, skips, reason
}

func (b *backupState) setupServerStateLocked(sv backupServerView, rows []backupRowView, now time.Time) backupSetupServerView {
	out := backupSetupServerView{Server: sv.Server}
	flowBucket := false
	if setup := b.rec.Setup; setup != nil {
		// An old failure stops mattering once a later run has succeeded.
		if run := setup.Runs[sv.Server]; run != nil && !(sv.LastRun != nil && sv.LastRun.Status == "ok" && sv.LastRun.At.After(run.RequestedAt)) {
			out.Run = run
		}
		if k := setup.Keys[sv.Server]; k != nil {
			out.Key = k.KeyName
			// The lifecycle note is for buckets this flow set up. The live read wins; what
			// verification recorded only fills in when the live read cannot say.
			flowBucket = true
			out.Lifecycle = sv.Target.IncomingLifecycle
			if out.Lifecycle == "" || out.Lifecycle == "unknown" {
				if k.Lifecycle != "" {
					out.Lifecycle = k.Lifecycle
				}
			}
		}
	}
	if !flowBucket {
		out.Lifecycle = ""
	}
	if backupTargetCapped(sv.Target.Error) {
		out.State, out.Detail = setupCapped, backupCappedMessage
		return out
	}
	if sv.State != backupServerOK {
		out.State, out.Detail = setupUnknown, "Dash could not read this server, so it does not know how its backups are."
		if sv.Error != "" {
			out.Detail += " " + sv.Error
		}
		return out
	}
	if !sv.Target.Configured || !sv.Policy.Configured {
		out.State, out.Detail = setupNotProtected, sv.Server+" is not backed up."
		return out
	}
	if !sv.Target.Reachable {
		out.State, out.Detail = setupUnreachable, sv.Server+" cannot reach its storage."
		if sv.Target.Error != "" {
			out.Detail += " " + sv.Target.Error
		}
		return out
	}

	chosen := map[string]bool{}
	if setup := b.rec.Setup; setup != nil {
		for _, name := range setup.Skipped[sv.Server] {
			chosen[name] = true
		}
	}
	covered := 0
	var firstBackup, late, failing []string
	var waitingTest, failedTest, overdue []string
	guardReason, overdueReason := "", ""
	for i := range rows {
		row := &rows[i]
		isDatabase := row.Kind != "volumes"
		switch row.State {
		case "excluded":
			if isDatabase {
				out.Skipped = append(out.Skipped, row.Name)
			}
			continue
		case "uncovered":
			switch {
			case chosen[row.Name]:
				out.Skipped = append(out.Skipped, row.Name)
			case !isDatabase && !sv.Policy.Volumes:
				out.Skipped = append(out.Skipped, row.Name)
			default:
				out.Uncovered = append(out.Uncovered, row.Name)
			}
			continue
		}
		for _, db := range row.UncoveredDatabases {
			out.Uncovered = append(out.Uncovered, row.Name+"/"+db)
		}
		covered++
		switch {
		case row.State == "failing":
			detail := row.Name
			if row.LastError != "" {
				detail += ": " + row.LastError
			}
			failing = append(failing, detail)
			continue
		case row.LastGood == nil:
			firstBackup = append(firstBackup, row.Name)
			continue
		case row.State == "stale":
			late = append(late, row.Name)
			continue
		case sv.Encrypted && !row.Encrypted:
			firstBackup = append(firstBackup, row.Name)
			continue
		}
		if !isDatabase {
			continue // volumes need no restore test
		}
		seen := 0
		for _, t := range b.rec.Tests {
			if t.Server != sv.Server || t.RetiredAt != nil || t.Container != row.Name {
				continue
			}
			seen++
			run, skips, reason := t.lastRealRun()
			switch {
			case run != nil && !run.OK:
				failedTest = append(failedTest, row.Name)
			case run != nil && skips >= backupGuardSkipsShown:
				overdue = append(overdue, row.Name)
				overdueReason = reason
			case run != nil && now.Sub(run.At) > backupRestoreTestMaxAge:
				overdue = append(overdue, row.Name)
				if overdueReason == "" {
					overdueReason = fmt.Sprintf("The last one passed %d days ago.", int(now.Sub(run.At).Hours()/24))
				}
			case run != nil:
				// passed recently; one skip since does not undo it
			case skips >= backupGuardSkipsShown:
				guardReason = reason
				waitingTest = append(waitingTest, row.Name)
			default:
				waitingTest = append(waitingTest, row.Name)
			}
		}
		if seen == 0 {
			manual := row.LastRestoreTest
			switch {
			case manual == nil:
				waitingTest = append(waitingTest, row.Name)
			case !manual.OK:
				failedTest = append(failedTest, row.Name)
			case now.Sub(manual.At) > backupRestoreTestMaxAge:
				overdue = append(overdue, row.Name)
				if overdueReason == "" {
					overdueReason = fmt.Sprintf("The last one passed %d days ago.", int(now.Sub(manual.At).Hours()/24))
				}
			}
		}
	}
	join := func(list []string) string { return strings.Join(list, "; ") }
	names := func(list []string) string { return strings.Join(list, ", ") }
	switch {
	case len(failing) > 0:
		out.State, out.Detail = setupFailing, "Backups are failing: "+join(failing)
	case len(failedTest) > 0:
		out.State, out.Detail = setupTestFailed, "A restore test failed: "+join(failedTest)
	case len(out.Uncovered) > 0:
		out.State, out.Detail = setupNotCovered, "Not backed up: "+names(out.Uncovered)+"."
	case len(late) > 0:
		out.State, out.Detail = setupLate, "Backups are late: "+join(late)
	case len(overdue) > 0:
		out.State, out.Detail = setupTestOverdue, "Restore test overdue for "+names(overdue)+". "+overdueReason
	case len(firstBackup) > 0:
		out.State, out.Detail = setupWaitingBackup, "Waiting for the first backup of "+join(firstBackup)+"."
	case guardReason != "":
		out.State, out.Detail = setupGuardSkipped, "The restore test keeps being put off. "+guardReason
	case len(waitingTest) > 0:
		out.State, out.Detail = setupWaitingTest, "Waiting for a restore test of "+join(waitingTest)+". These run in the off-hours window."
	case covered == 0 && len(out.Skipped) > 0:
		out.State, out.Detail = setupAllSkipped, "Nothing here is backed up: you chose to skip "+names(out.Skipped)+"."
	case covered == 0:
		out.State, out.Detail = setupEmpty, "There is nothing to back up on this server yet."
	case len(out.Skipped) > 0:
		out.State, out.Detail = setupExcept, "Protected, except "+names(out.Skipped)+" (you chose to skip)."
	default:
		out.State = setupProtected
	}
	return out
}

func (b *backupState) setupViewLocked(supported, channels bool, view *backupsPageView, now time.Time) *backupSetupView {
	out := &backupSetupView{Supported: supported, MinCLI: backupSetupMinCLI, Servers: []backupSetupServerView{},
		AlertsConfigured: channels, RecoveryDoc: backupRecoveryDoc, Base: backupSetupBase}
	rowsByServer := map[string][]backupRowView{}
	for _, row := range view.Rows {
		rowsByServer[row.Server] = append(rowsByServer[row.Server], row)
	}
	inFleet := map[string]bool{}
	protected, attention, active := 0, 0, 0
	var except []string
	for _, sv := range view.Servers {
		inFleet[sv.Server] = true
		state := b.setupServerStateLocked(sv, rowsByServer[sv.Server], now)
		out.Servers = append(out.Servers, state)
		switch state.State {
		case setupProtected:
			protected++
			active++
		case setupExcept:
			protected++
			active++
			except = append(except, state.Skipped...)
		case setupAllSkipped:
			except = append(except, state.Skipped...)
		case setupEmpty:
		case setupNotProtected:
			if view.Storage != nil {
				out.NewServers = append(out.NewServers, sv.Server)
			}
			active++
		case setupFailing, setupTestFailed, setupTestOverdue, setupNotCovered, setupLate, setupUnreachable, setupGuardSkipped, setupCapped, setupUnknown:
			attention++
			active++
		default:
			active++
		}
	}
	if st := view.Storage; st != nil {
		out.Configured, out.Bucket, out.Endpoint, out.Base = true, st.Bucket, st.Endpoint, st.BasePrefix
	}
	if setup := b.rec.Setup; setup != nil {
		out.AlertsSkipped = setup.AlertsSkippedAt != nil
		out.HideDays = setup.HideDays
		out.S3Managed = setup.S3Managed
		out.TurnedOn = setup.TurnedOnAt != nil
		out.Proof = setup.Proof
		out.Leftovers = setup.Leftovers
		if !b.rec.LastSweepAt.IsZero() {
			for name := range setup.Keys {
				if !inFleet[name] {
					out.RetiredKeys = append(out.RetiredKeys, name)
				}
			}
			sort.Strings(out.RetiredKeys)
		}
	}
	hide := out.HideDays
	if hide == 0 {
		hide = 14
	}
	interval, retention := backupDefaultSetupInterval, backupRetention{KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 12}
	if p := b.rec.Policy; p != nil {
		if r, ok := backupPresetRetention(p.Preset); ok {
			interval, retention = p.Interval, r
		}
	}
	out.Multiplier = backupStorageMultiplier(backupStatPolicy{Interval: interval, Retention: retention}, hide)
	for _, row := range view.Rows {
		if row.State != "excluded" && row.State != "uncovered" && row.LastGood != nil {
			out.StoredBytes += row.SizeBytes
		}
	}
	out.ProjectedBytes = out.StoredBytes * out.Multiplier

	switch {
	case active > 0 && protected == active && len(except) > 0:
		out.State, out.Headline = setupExcept, "Protected, except "+strings.Join(except, ", ")+" (you chose to skip)."
	case active > 0 && protected == active:
		out.State, out.Headline = "protected", "Your data is protected."
	case attention > 0:
		out.State, out.Headline = "attention", "Your backups need attention."
	case out.Configured || out.TurnedOn:
		out.State, out.Headline = "partial", "Your data is not protected yet."
	default:
		out.State, out.Headline = "none", "Your data is not protected yet."
	}
	return out
}

// ── Dash itself ──────────────────────────────────────────────────────────────

// backupSelfView says where Dash's own backup stands. Objects encrypted to the owner
// and the server open with the owner key alone, so its volumes need nothing special,
// except that they must never go up in plaintext: they hold its SSH key.
type backupSelfView struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
	Runbook   string `json:"runbook"`
	// Entry is the row that backs Dash up, such as dash/volumes.
	Entry string `json:"entry,omitempty"`
	// Warning: something here needs doing.
	Warning string `json:"warning,omitempty"`
	// Uncovered lists paths outside the deployment template's volumes, which no backup
	// reaches.
	Uncovered []string `json:"uncovered,omitempty"`
}

const (
	backupRecoveryRunbook = "docs/recovery.md"
	backupSelfProduct     = "dash"
)

// backupSelfApp names Dash's own app: TEPLOY_APP where teploy deployed it (v0.1.49 and
// later), and "" otherwise.
func backupSelfApp() string { return strings.TrimSpace(os.Getenv("TEPLOY_APP")) }

// What a server's relation to this Dash turned out to be.
const (
	selfHostYes     = "host"
	selfHostAbsent  = "absent"
	selfHostUnknown = "unknown"
)

// selfHostOn says whether this server hosts this Dash, without trusting a name: a user
// may have an app called dash. Yes needs the running container's own product label and
// the app name from TEPLOY_APP, as `server siblings` reports them. No volumes under the
// name means there is nothing to protect there. The rest is unknown.
func (s *Server) selfHostOn(ctx context.Context, name string, env *backupStatusEnvelope) string {
	app := s.backups.selfApp
	if env != nil {
		present := false
		for i := range env.Entries {
			e := &env.Entries[i]
			present = present || (e.Kind == "volumes" && (e.App == app || (app == "" && e.App == backupSelfProduct)))
		}
		if !present {
			return selfHostAbsent
		}
	}
	if app == "" || !s.capabilities(ctx).Features.Siblings {
		return selfHostUnknown
	}
	report, err := s.runSiblingsCheck(ctx, name)
	if err != nil {
		return selfHostUnknown
	}
	for _, row := range report.Siblings {
		if row.Product == backupSelfProduct && row.App == app {
			return selfHostYes
		}
	}
	return selfHostUnknown
}

// selfHostsFromSiblings names the servers the last siblings sweep saw running this Dash.
func (s *Server) selfHostsFromSiblings() map[string]bool {
	app := s.backups.selfApp
	if app == "" {
		return nil
	}
	hosts := map[string]bool{}
	s.siblings.mu.Lock()
	defer s.siblings.mu.Unlock()
	for name, rec := range s.siblings.snap.Servers {
		for _, row := range rec.Siblings {
			if row.Product == backupSelfProduct && row.App == app {
				hosts[name] = true
			}
		}
	}
	return hosts
}

// backupSelfRows narrows the rows to the proved host, when one is known, so a user's app
// with the same name on another server is not reported as Dash.
func backupSelfRows(rows []backupRowView, hosts map[string]bool) []backupRowView {
	if len(hosts) == 0 {
		return rows
	}
	out := make([]backupRowView, 0, len(rows))
	for _, row := range rows {
		if hosts[row.Server] {
			out = append(out, row)
		}
	}
	return out
}

// backupSelfUncovered compares where this Dash keeps its state with the template's
// volumes (/var/teploy-dash, /root/.ssh, /root/.teploy).
func backupSelfUncovered(dataDir string) []string {
	var out []string
	if dataDir != "" && filepath.Clean(dataDir) != "/var/teploy-dash" {
		out = append(out, dataDir)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "/root" {
		out = append(out, filepath.Join(home, ".teploy"))
	}
	if key := os.Getenv("TEPLOY_SSH_KEY"); key != "" && !strings.HasPrefix(filepath.Clean(key), "/root/.ssh/") {
		out = append(out, key)
	} else if key == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "/root" {
			out = append(out, filepath.Join(home, ".ssh"))
		}
	}
	return out
}

func backupSelfStatus(app, dataDir string, rows []backupRowView, recipient bool) backupSelfView {
	view := backupSelfView{Runbook: backupRecoveryRunbook, Uncovered: backupSelfUncovered(dataDir)}
	name := app
	var row *backupRowView
	for i := range rows {
		if app != "" && rows[i].Kind == "volumes" && rows[i].App == name {
			row = &rows[i]
			break
		}
	}
	switch {
	case row == nil && app == "":
		view.Reason = "This Dash was not deployed from the teploy template, so no backup reaches its state. Copy it yourself; the recovery page shows how."
	case row == nil:
		view.Reason = "No server reports " + name + "/volumes yet, so Dash's own state is not in a backup."
	case row.State == "excluded":
		view.Entry = row.Name
		if recipient {
			view.Reason = row.Name + " on " + row.Server + " is excluded, so Dash's own state is not in a backup."
		} else {
			view.Reason = row.Name + " on " + row.Server + " is left out because there is no recovery key: it holds Dash's SSH key, which must never be uploaded unencrypted."
		}
	case row.LastGood == nil:
		view.Entry = row.Name
		view.Reason = "Waiting for the first backup of " + row.Name + " on " + row.Server + "."
	case !row.Encrypted:
		view.Entry = row.Name
		view.Reason = row.Name + " on " + row.Server + " is backed up unencrypted."
		view.Warning = "These backups hold Dash's SSH key in the clear. Set a recovery key, or exclude " + row.Name + ", and remove the old copies from your bucket."
	case row.State == "covered" || row.State == "adopted":
		view.Available = true
		view.Entry = row.Name
		view.Reason = fmt.Sprintf("Dash's volumes are backed up as %s on %s. Your recovery key opens them on its own.", row.Name, row.Server)
	default:
		view.Entry = row.Name
		view.Reason = row.Name + " on " + row.Server + " is " + row.State + "."
	}
	if len(view.Uncovered) > 0 {
		view.Reason += " No backup covers " + strings.Join(view.Uncovered, ", ") + ": these are outside the template's volumes, so copy them yourself."
	}
	return view
}

// backupHandwrittenFound reports evidence of jobs the policy does not own. Dash cannot
// list a server's cron and systemd jobs, so this is what it can see: legacy cron lines,
// and policy files something else manages.
func (b *backupState) backupHandwrittenFound(rows []backupRowView) bool {
	for i := range rows {
		if rows[i].LegacySchedule {
			return true
		}
	}
	if b.rec.Setup != nil {
		for _, sum := range b.rec.Setup.Preflight {
			if sum.ManagedElsewhere == "yes" {
				return true
			}
		}
	}
	return false
}
