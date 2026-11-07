package server

// backupsetup_status.go — what "Protected" means, and Dash's own backup.
//
// Protected: a verified backup on each covered target, plus a passing restore test for
// each covered database. Volume-only servers need no restore test. A status that could
// not be read proves nothing, so it never reads as protected.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	setupProtected     = "protected"
)

type backupSetupServerView struct {
	Server string `json:"server"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
	// Uncovered names what is not in any backup here.
	Uncovered []string        `json:"uncovered,omitempty"`
	Run       *backupSetupRun `json:"run,omitempty"`
	Key       string          `json:"key_name,omitempty"`
	Lifecycle string          `json:"lifecycle,omitempty"`
}

type backupSetupView struct {
	Supported bool   `json:"supported"`
	MinCLI    string `json:"min_cli"`
	// State: none | partial | attention | protected.
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

func (b *backupState) setupServerStateLocked(sv backupServerView, rows []backupRowView) backupSetupServerView {
	out := backupSetupServerView{Server: sv.Server}
	if setup := b.rec.Setup; setup != nil {
		out.Run = setup.Runs[sv.Server]
		if k := setup.Keys[sv.Server]; k != nil {
			out.Key, out.Lifecycle = k.KeyName, k.Lifecycle
		}
	}
	if out.Lifecycle == "" {
		out.Lifecycle = sv.Target.IncomingLifecycle
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

	covered := 0
	var firstBackup, late, failing []string
	var waitingTest, failedTest []string
	guardReason := ""
	for i := range rows {
		row := &rows[i]
		switch row.State {
		case "excluded":
			continue
		case "uncovered":
			out.Uncovered = append(out.Uncovered, row.Name)
			continue
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
		if row.Kind != "accessory" && row.Kind != "adopted" {
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
			case run != nil:
				// passed; a later guard skip does not undo it
			case skips >= backupGuardSkipsShown:
				guardReason = reason
				waitingTest = append(waitingTest, row.Name)
			default:
				waitingTest = append(waitingTest, row.Name)
			}
		}
		if seen == 0 && (row.LastRestoreTest == nil || !row.LastRestoreTest.OK) {
			waitingTest = append(waitingTest, row.Name)
		}
	}
	join := func(list []string) string { return strings.Join(list, "; ") }
	switch {
	case covered == 0:
		out.State, out.Detail = setupEmpty, "There is nothing to back up on this server yet."
	case len(failing) > 0:
		out.State, out.Detail = setupFailing, "Backups are failing: "+join(failing)
	case len(failedTest) > 0:
		out.State, out.Detail = setupTestFailed, "A restore test failed: "+join(failedTest)
	case len(late) > 0:
		out.State, out.Detail = setupLate, "Backups are late: "+join(late)
	case len(firstBackup) > 0:
		out.State, out.Detail = setupWaitingBackup, "Waiting for the first backup of "+join(firstBackup)+"."
	case guardReason != "":
		out.State, out.Detail = setupGuardSkipped, "The restore test keeps being put off. "+guardReason
	case len(waitingTest) > 0:
		out.State, out.Detail = setupWaitingTest, "Waiting for a restore test of "+join(waitingTest)+". These run in the off-hours window."
	default:
		out.State = setupProtected
	}
	return out
}

func (b *backupState) setupViewLocked(supported, channels bool, view *backupsPageView) *backupSetupView {
	out := &backupSetupView{Supported: supported, MinCLI: backupSetupMinCLI, Servers: []backupSetupServerView{},
		AlertsConfigured: channels, RecoveryDoc: backupRecoveryDoc, Base: backupSetupBase}
	rowsByServer := map[string][]backupRowView{}
	for _, row := range view.Rows {
		rowsByServer[row.Server] = append(rowsByServer[row.Server], row)
	}
	inFleet := map[string]bool{}
	protected, attention, active := 0, 0, 0
	for _, sv := range view.Servers {
		inFleet[sv.Server] = true
		state := b.setupServerStateLocked(sv, rowsByServer[sv.Server])
		out.Servers = append(out.Servers, state)
		switch state.State {
		case setupProtected:
			protected++
			active++
		case setupEmpty:
		case setupNotProtected:
			if view.Storage != nil {
				out.NewServers = append(out.NewServers, sv.Server)
			}
			active++
		case setupFailing, setupTestFailed, setupLate, setupUnreachable, setupGuardSkipped, setupCapped, setupUnknown:
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
	backupSelfTemplateApp = "dash"
)

// backupSelfApp names Dash's own app: TEPLOY_APP where teploy deployed it (v0.1.49 and
// later), and "" otherwise.
func backupSelfApp() string { return strings.TrimSpace(os.Getenv("TEPLOY_APP")) }

// backupSelfExcludes is what a policy without a recipient must leave out. A Dash
// deployed by an older CLI has no TEPLOY_APP, so the template's name is excluded too:
// a wrong guess skips an app's volumes and says so, a missed one uploads an SSH key.
func backupSelfExcludes(app string) []string {
	if app != "" && app != backupSelfTemplateApp {
		return []string{app + "/volumes", backupSelfTemplateApp + "/volumes"}
	}
	return []string{backupSelfTemplateApp + "/volumes"}
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
	if name == "" {
		name = backupSelfTemplateApp
	}
	var row *backupRowView
	for i := range rows {
		if rows[i].Kind == "volumes" && rows[i].App == name {
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
