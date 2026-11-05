package server

// backups.go — plan D5: the app's backups as the CLI sees them (D3: one
// target per server, read at run time — dash never holds a bucket, region or
// credential). The list is `teploy backup list --app X --host S --json`
// (backup-json-v1), "Run now" is `backup create --json` and restore is
// `backup restore --json`, both as durable operations because each can hold
// the app under its fenced lock for minutes. The typed confirmation for a
// restore lives in the UI that admits it; the CLI run itself is --yes.

import (
	"net/http"

	"github.com/useteploy/teploy-dash/internal/operation"
)

// handleBackupsList serves GET /api/apps/{server}/{app}/backups: the CLI's
// backup list envelope (parsed rows plus the schedules discovered from
// teploy cron markers, and the stored server target).
func (s *Server) handleBackupsList(w http.ResponseWriter, r *http.Request, serverName, appName string) {
	if !s.requireFeatureMin(w, r, func(f machineFeatures) bool { return f.Backups }, "Backups", "v0.1.45") {
		return
	}
	args := []string{"backup", "list", "--json"}
	if s.capabilities(r.Context()).Features.BackupFleet {
		// Each database's own rows: they carry the database a restore must name.
		args = append(args, "--databases")
	}
	res, err := s.cliAppRun(r.Context(), serverName, appName, args...)
	if err != nil {
		writeError(w, err.Error())
		return
	}
	writeRawJSON(w, res.Stdout)
}

// handleBackupCreate serves POST /api/apps/{server}/{app}/backups/create
// {"accessory"?: name}: "Run now" — a durable `backup create --json`. The
// server's stored target supplies the destination; nothing is passed here.
func (s *Server) handleBackupCreate(w http.ResponseWriter, r *http.Request, serverName, appName string) {
	if !s.requireFeatureMin(w, r, func(f machineFeatures) bool { return f.Backups }, "Backups", "v0.1.45") {
		return
	}
	var body struct {
		Accessory string `json:"accessory"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	s.enqueueOperation(w, r, operation.Request{
		Kind: operation.KindBackupCreate, Server: serverName, App: appName, Accessory: body.Accessory,
	})
}

// backupFleetMinCLI is the first CLI with the fleet engine's restore and purge flags.
const backupFleetMinCLI = "v0.1.52"

// handleBackupRestore serves POST /api/apps/{server}/{app}/backups/restore
// {"id", "accessory"?, "database"?, "no_backup"?}: a durable `backup restore <id> --yes --json`. The
// UI collected the typed confirmation (the app name) before admitting this;
// the operation stops and restarts the app's containers on the server.
func (s *Server) handleBackupRestore(w http.ResponseWriter, r *http.Request, serverName, appName string) {
	if !s.requireFeatureMin(w, r, func(f machineFeatures) bool { return f.Backups }, "Backups", "v0.1.45") {
		return
	}
	var body struct {
		ID        string `json:"id"`
		Accessory string `json:"accessory"`
		Database  string `json:"database"`
		NoBackup  bool   `json:"no_backup"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	if body.ID == "" {
		writeError(w, "id is required")
		return
	}
	if (body.Database != "" || body.NoBackup) && !s.requireFeatureMin(w, r, func(f machineFeatures) bool { return f.BackupFleet }, "Per-database restore and skipping the automatic backup", backupFleetMinCLI) {
		return
	}
	s.enqueueOperation(w, r, operation.Request{
		Kind: operation.KindBackupRestore, Server: serverName, App: appName,
		BackupID: body.ID, Accessory: body.Accessory, Database: body.Database, NoBackup: body.NoBackup,
	})
}
