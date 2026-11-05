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
// The inventory is grounded in the teploy-cli accessory surface as of v0.1.52
// (teploy-cli/internal/cli/accessory.go, backup.go):
//
//   - stop / start / logs / list / exec / verify-backup / backup accept --app
//     (+ --host), i.e. server-state mode — dash-reachable;
//   - `backup restore` runs from server state too (Dash's durable operation),
//     and since v0.1.52 names a single database (--database) and can skip the
//     automatic pre-restore backup (--no-backup);
//   - upgrade loads accessory config from teploy.yml in the CURRENT DIRECTORY
//     (loadAppCfgForAccessory) — not dash-reachable without pretending dash
//     has a project checkout it does not have — but since v0.1.52 it takes
//     the same automatic safety backup a restore does;
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
			BlastRadius: "Replaces the database ENGINE image. Minor upgrades are usually in-place; major-version upgrades (e.g. postgres 16 -> 17) can require a dump/restore and can make existing data files unreadable under the old version if it goes wrong.",
			Remedy:      "`accessory upgrade` reads the accessory's image config from teploy.yml and must run from the app's project directory on a machine with it — the dashboard has no project checkout. Run it there: since teploy v0.1.52 it takes an automatic safety backup first (skip only with --no-backup).",
		},
		{
			ID:          "credential-rotation",
			Label:       "Credential rotation",
			Supported:   false,
			BlastRadius: "Everything authenticating with the rotated credential (apps, scheduled jobs, backup schedules) loses access until it is updated with the new value.",
			Remedy:      "The teploy CLI has no credential-rotation command: accessory credentials come from the app's teploy.yml env. Rotate by changing the value there and redeploying, and update dependents. Generated (`generate`) values are shown once at install time by design.",
		},
		{
			ID:           "data-restore",
			Label:        "Data restore",
			Supported:    true,
			Command:      "teploy backup restore <id> --accessory <name> --app <app> --host <server> [--database <db>] [--no-backup] --yes --json",
			DashAction:   "backups/restore",
			BlastRadius:  "REPLACES the database's current contents with the backup's point-in-time state; everything written since that backup is gone. With one database named, only that database is replaced. The app's containers stop for the restore and start again after. A safety backup of the current state is taken first, and a failed safety backup refuses the restore (--no-backup is the explicit way past that).",
			Confirmation: "Type the app name to confirm.",
			Remedy:       "Run it from the app's Backups tab, which stops the app, runs this command and health-checks it afterwards. The non-destructive cousin is Restore Tests (the Restore Tests page, and the automatic weekly tests on the Backups page): `teploy accessory verify-backup` restores the latest backup into an isolated scratch container and proves it usable, never touching the running database.",
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
