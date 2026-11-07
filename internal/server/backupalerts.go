package server

// backupalerts.go — Dash-owned backup incidents (plan P2.8), wired into the
// Alerts v2 tracker exactly as monitors and domain health are: one incident
// per cause, opened on evidence, recovered by evidence, adopted on the first
// complete read of a server (what was already broken becomes one summary).
//
// Rules, all source "backup" except restore tests (source "restore-test",
// fed by the automatic test runs in backuptests.go — one owner per signal):
//
//	target   a server's backup target is missing, credential-less or unreachable
//	tools    the aws or age CLI the engine needs is missing on the server
//	fail     an entry's backups are failing (last run failed for it)
//	stale    no success in twice the policy interval (the CLI's coverage word)
//	uncovered a database the engine cannot address appeared (warning: it may be
//	         deliberate, which is what the exclude list is for)
//
// The missed-heartbeat alert is the push monitor's (backuppolicy.go owns it):
// a server whose cron stopped pings nothing, the monitor goes down, source
// "heartbeat". Nothing here duplicates it.
//
// A rule whose subject disappears from a server's COMPLETE fresh read is
// retired with reason "retired"; a partly read server retires nothing.

import (
	"strings"
	"time"

	"github.com/useteploy/teploy-dash/internal/alert"
	"github.com/useteploy/teploy-dash/internal/incident"
)

// backupObservation is one rule's verdict for one sweep.
type backupObservation struct {
	rule    incident.Rule
	ok      bool
	message string
}

func backupRule(server, kind, subject, name, severity string) incident.Rule {
	id := "backup-" + kind + "-" + server
	target := server
	if subject != "" {
		id += "-" + subject
		target = server + "/" + subject
	}
	return incident.Rule{Key: "backup:" + server + ":" + kind + ":" + subject, Source: alert.SourceBackup,
		ID: id, Name: name, Server: server, Target: target, Severity: severity, Path: "/backups",
		Policy: incident.Policy{FailAfter: 1, RecoverAfter: 1}}
}

// backupObservations derives every rule one server's stored status evidences.
// A status that could not be read evidences nothing (its rules are carried,
// not retired). keys are the rules this read still implies.
func backupObservations(rec *backupServerRecord, now time.Time) (obs []backupObservation, keys []string) {
	st := rec.Status
	if st == nil {
		return nil, nil
	}
	add := func(o backupObservation) {
		keys = append(keys, o.rule.Key)
		obs = append(obs, o)
	}

	if st.Target.Configured {
		rule := backupRule(rec.Server, "target", "", "backup target on "+rec.Server, alert.SeverityCritical)
		switch {
		case !st.Target.HasCredentials:
			add(backupObservation{rule, false, "the target has no stored credentials — connect storage again"})
		case !st.Target.Reachable && backupTargetCapped(st.Target.Error):
			// Not an outage, and the server is fine: say what it is.
			add(backupObservation{rule, false, backupCappedMessage})
		case !st.Target.Reachable:
			msg := "the server cannot reach its backup target"
			if st.Target.Error != "" {
				msg += ": " + st.Target.Error
			}
			add(backupObservation{rule, false, msg})
		default:
			add(backupObservation{rule, true, "the backup target answers"})
		}
	}

	if problem := st.AWSCLIProblem; problem != "" {
		rule := backupRule(rec.Server, "tools", "", "backup tooling on "+rec.Server, alert.SeverityWarning)
		add(backupObservation{rule, false, problem})
	} else if st.Target.Configured {
		rule := backupRule(rec.Server, "tools", "", "backup tooling on "+rec.Server, alert.SeverityWarning)
		add(backupObservation{rule, true, "the server's backup tooling is in place"})
	}

	for i := range st.Entries {
		e := &st.Entries[i]
		if e.Kind != "accessory" && e.Kind != "adopted" && e.Kind != "volumes" && e.Kind != "unmanaged" {
			continue
		}
		label := e.Name
		if e.App != "" && e.Accessory != "" {
			label = e.App + "/" + e.Accessory
		}
		fail := backupRule(rec.Server, "fail", e.Name, "backups of "+label+" on "+rec.Server, alert.SeverityCritical)
		stale := backupRule(rec.Server, "stale", e.Name, "backups of "+label+" on "+rec.Server+" are stale", alert.SeverityCritical)
		uncovered := backupRule(rec.Server, "uncovered", e.Name, label+" on "+rec.Server+" is not covered", alert.SeverityWarning)
		switch e.Coverage {
		case "covered", "adopted":
			add(backupObservation{fail, true, "backed up"})
			add(backupObservation{stale, true, "backups are current"})
			if len(e.UncoveredDatabases) > 0 {
				add(backupObservation{uncovered, false, "databases the engine cannot address: " + strings.Join(e.UncoveredDatabases, ", ")})
			}
		case "failing":
			msg := "the last backup failed"
			if e.LastError != "" {
				msg += ": " + e.LastError
			}
			add(backupObservation{fail, false, msg})
		case "stale":
			add(backupObservation{fail, true, "backed up"})
			add(backupObservation{stale, false, "no successful backup within twice the policy interval"})
		case "uncovered":
			add(backupObservation{uncovered, false, "no backup (a new database the policy has not covered yet)"})
		case "excluded":
			// A person's deliberate choice; no rule, no alert.
		}
	}
	return obs, keys
}

// evaluateBackupIncidents feeds one server's newest stored status into the
// tracker, then retires the rules that server's complete read no longer
// implies. Called after every sweep and every storage push verification.
func (s *Server) evaluateBackupIncidents(server string, now time.Time) {
	t := s.alerts.tracker
	if t == nil {
		return
	}
	b := &s.backups
	b.mu.Lock()
	defer b.mu.Unlock()
	rec := b.rec.Servers[server]
	if rec == nil {
		return
	}
	obs, keys := backupObservations(rec, now)
	fresh := backupServerFresh(rec, now)
	adopting := !rec.Adopted && fresh
	adopted := 0
	for _, o := range obs {
		if !fresh && o.ok {
			// Stale evidence recovers nothing: an unread server has proved
			// nothing since its last good status.
			continue
		}
		if !o.ok && adopting {
			// First complete read: what was already broken becomes one summary.
			if t.AdoptFailing(o.rule, now, o.message) {
				adopted++
			} else {
				t.Observe(o.rule, false, now, o.message)
			}
			continue
		}
		t.Observe(o.rule, o.ok, now, o.message)
	}
	if adopted > 0 {
		t.AnnounceWith(incident.Summary{Intro: "First backup results. Already failing:", Path: "/backups", Source: alert.SourceBackup})
	}
	if fresh {
		rec.Adopted = true
		// Retire only on complete evidence: this server was just read fresh
		// and no longer implies these rules.
		have := map[string]bool{}
		for _, k := range keys {
			have[k] = true
		}
		for _, k := range rec.RuleKeys {
			if !have[k] {
				t.Retire(k, "no longer on this server's backup status")
			}
		}
		rec.RuleKeys = keys
	}
	b.saveLocked()
}
