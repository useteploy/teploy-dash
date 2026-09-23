package server

// dbactions.go — D05: database actions as DISTINCT operations with distinct
// guarantees. Each class (restart, version upgrade, credential rotation,
// data restore, destructive removal) surfaces its own confirmation,
// blast-radius description and — where the CLI actually supports it — its
// own command. Where the CLI does NOT support an action from dash's
// server-state mode, the entry says so with the remedy (the exact command
// to run and where), NEVER wired to a closest-match command: a database
// that "restarts" via a different operation than labeled is how an
// operator approves one thing and receives another.
//
// The inventory is grounded in the teploy-cli accessory surface as of
// post-v0.1.37 main (teploy-cli/internal/cli/accessory.go):
//
//   - stop / start / logs / list / exec / verify-backup accept --app
//     (+ --host), i.e. server-state mode — dash-reachable;
//   - upgrade / backup / restore load accessory config from teploy.yml in
//     the CURRENT DIRECTORY (loadAppCfgForAccessory) — not dash-reachable
//     without pretending dash has a project checkout it does not have;
//   - there is NO accessory restart and NO credential-rotation command;
//   - destructive removal exists only at app scope (`teploy remove --app
//     <app> --purge` deletes volumes); per-accessory removal does not exist.
//
// When the CLI grows a server-state mode for one of these, flip the entry
// here and wire its dash command in the same change — this file is the
// single place the answer lives.

import (
	"net/http"
)

// dbAction is one database-action class as dash renders it.
type dbAction struct {
	// ID is the stable class identifier (restart, version-upgrade,
	// credential-rotation, data-restore, destructive-removal).
	ID string `json:"id"`
	// Label is the human class name.
	Label string `json:"label"`
	// Supported reports whether the action can be executed FROM DASH.
	// False never means "hidden": the entry renders with its remedy.
	Supported bool `json:"supported"`
	// Command is the exact teploy CLI command for the action when it is
	// supported; for unsupported entries it is the command the operator
	// runs themselves (part of the remedy).
	Command string `json:"command,omitempty"`
	// DashAction is the dash API action that executes it, when supported.
	DashAction string `json:"dash_action,omitempty"`
	// BlastRadius states what the operation does to the data and its
	// consumers — the text an operator is agreeing to.
	BlastRadius string `json:"blast_radius"`
	// Confirmation is the distinct per-class confirmation prompt.
	Confirmation string `json:"confirmation,omitempty"`
	// Remedy names what to do instead when unsupported.
	Remedy string `json:"remedy,omitempty"`
}

// databaseActionInventory returns the five D05 database-action classes.
// Static by design: it describes the CLI's command surface, not per-app
// state; per-app state (which accessory, which server) is composed by the
// caller.
func databaseActionInventory() []dbAction {
	return []dbAction{
		{
			ID:          "restart",
			Label:       "Restart",
			Supported:   false,
			Command:     "teploy accessory stop <name> --app <app> --host <host> && teploy accessory start <name> --app <app> --host <host>",
			BlastRadius: "The database stops and comes back empty-memory: in-flight queries abort, apps using it lose their data connection until it is up again. Persisted data is untouched.",
			Remedy:      "The teploy CLI has no single-command accessory restart. Run Stop, then Start (both are wired below, each with its own confirmation) — or from the app directory: teploy accessory stop <name> && teploy accessory start <name>.",
		},
		{
			ID:          "version-upgrade",
			Label:       "Version upgrade",
			Supported:   false,
			Command:     "teploy accessory upgrade <name> <new_image>",
			BlastRadius: "Replaces the database ENGINE image. Minor upgrades are usually in-place; major-version upgrades (e.g. postgres 16 -> 17) can require a dump/restore and can make existing data files unreadable under the old version if it goes wrong. Take a backup first.",
			Remedy:      "`accessory upgrade` reads the accessory's image config from teploy.yml and must run from the app's project directory on a machine with it — the dashboard has no project checkout. Run it there, after a backup (see Backup scope in the template/catalog docs).",
		},
		{
			ID:          "credential-rotation",
			Label:       "Credential rotation",
			Supported:   false,
			BlastRadius: "Everything authenticating with the rotated credential (apps, scheduled jobs, backup schedules) loses access until it is updated with the new value.",
			Remedy:      "The teploy CLI has no credential-rotation command: accessory credentials come from the app's teploy.yml env. Rotate by changing the value there and redeploying, and update dependents. Generated (`generate`) values are shown once at install time by design.",
		},
		{
			ID:          "data-restore",
			Label:       "Data restore",
			Supported:   false,
			Command:     "teploy accessory restore <name> <date> --bucket <bucket> --region <region>",
			BlastRadius: "REPLACES the database's current contents with the backup's point-in-time state. Everything written since that backup is gone.",
			Remedy:      "`accessory restore` reads the accessory's config from teploy.yml and must run from the app's project directory. The non-destructive cousin IS wired: Restore Tests (see the Restore Tests page) restore the latest backup into an isolated scratch container and verify it, never touching the running database (`teploy accessory verify-backup`, server-state mode).",
		},
		{
			ID:          "destructive-removal",
			Label:       "Destructive removal",
			Supported:   false,
			Command:     "teploy remove --app <app> --purge --yes",
			BlastRadius: "Deletes the app's volumes INCLUDING the database's data directory: permanent, unrecoverable except from backups. The per-accessory scoping the label implies does not exist — removal is app-wide.",
			Remedy:      "The CLI has no per-accessory removal; destructive removal is app-scope only (Remove on the app page, with the purge option, or the command shown). Deleting only the database while keeping the app is not an operation teploy supports.",
		},
	}
}

// handleDatabaseActions serves the inventory for an app's accessories.
// Read-only (view.metadata): the inventory describes the CLI surface, and
// each supported execution path has its own mutating endpoint with its own
// capability check.
func (s *Server) handleDatabaseActions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	writeData(w, map[string]interface{}{
		"actions": databaseActionInventory(),
		"source":  "teploy-cli accessory command surface (see internal/server/dbactions.go for the grounding)",
	})
}
