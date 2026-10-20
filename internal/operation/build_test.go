package operation

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAllowlistedBuilders(t *testing.T) {
	tests := []struct {
		name       string
		request    Request
		wantPrefix []string
		wantLimit  time.Duration
	}{
		{"deploy", Request{Kind: KindDeploy, Server: "prod", App: "web", Image: "web:1"}, []string{"deploy", "prod"}, 30 * time.Minute},
		{"rollback", Request{Kind: KindRollback, Server: "prod", App: "web"}, []string{"rollback", "--host"}, 10 * time.Minute},
		{"remove", Request{Kind: KindRemove, Server: "prod", App: "web"}, []string{"remove", "--yes"}, 10 * time.Minute},
		{"template", Request{Kind: KindTemplateInstall, Server: "prod", Template: "postgres", Domain: "db.example"}, []string{"template", "install"}, 30 * time.Minute},
		{"lifecycle", Request{Kind: KindAppLifecycle, Server: "prod", App: "web", Action: "restart"}, []string{"restart", "--host"}, 5 * time.Minute},
		{"maintenance", Request{Kind: KindMaintenance, Server: "prod", App: "web", Action: "on"}, []string{"maintenance", "on"}, 5 * time.Minute},
		{"redeploy", Request{Kind: KindRedeploy, Server: "prod", App: "web"}, []string{"redeploy", "--json"}, 30 * time.Minute},
		{"backup_create", Request{Kind: KindBackupCreate, Server: "prod", App: "web"}, []string{"backup", "create"}, 30 * time.Minute},
		{"backup_restore", Request{Kind: KindBackupRestore, Server: "prod", App: "web", BackupID: "20261019-120000"}, []string{"backup", "restore"}, 60 * time.Minute},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command, admitted, target, err := Build(test.request, testResolver)
			_ = admitted
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(command.Args[:len(test.wantPrefix)], test.wantPrefix) || command.Timeout != test.wantLimit || target == "" {
				t.Fatalf("command=%+v target=%q", command, target)
			}
		})
	}
}

// Plan D2/D3: rollback --to, the backup accessory scoping and the backup-id
// grammar. Only ids the surfaces handed dash may reach the CLI argv.
func TestRollbackToAndBackupBuilders(t *testing.T) {
	rollback, _, _, err := Build(Request{Kind: KindRollback, Server: "prod", App: "web", To: "20261019-120000"}, testResolver)
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.Index(rollback.Args, "--to"); i < 0 || rollback.Args[i+1] != "20261019-120000" {
		t.Fatalf("rollback --to args = %v", rollback.Args)
	}
	plain, _, _, err := Build(Request{Kind: KindRollback, Server: "prod", App: "web"}, testResolver)
	if err != nil || slices.Contains(plain.Args, "--to") {
		t.Fatalf("rollback without a target must keep today's argv: %v %v", plain.Args, err)
	}

	accessory, _, _, err := Build(Request{Kind: KindBackupCreate, Server: "prod", App: "web", Accessory: "postgres"}, testResolver)
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.Index(accessory.Args, "--accessory"); i < 0 || accessory.Args[i+1] != "postgres" {
		t.Fatalf("backup create accessory args = %v", accessory.Args)
	}

	restore, _, _, err := Build(Request{Kind: KindBackupRestore, Server: "prod", App: "web", BackupID: "20261019-120000-0123456789abcdef", Accessory: "postgres"}, testResolver)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(restore.Args, "20261019-120000-0123456789abcdef") || !slices.Contains(restore.Args, "--yes") {
		t.Fatalf("restore args = %v", restore.Args)
	}

	for _, request := range []Request{
		{Kind: KindRollback, Server: "prod", App: "web", To: "--image"},
		{Kind: KindRollback, Server: "prod", App: "web", To: strings.Repeat("a", 129)},
		{Kind: KindBackupCreate, Server: "prod", App: "web", Accessory: "-flag"},
		{Kind: KindBackupRestore, Server: "prod", App: "web"},
		{Kind: KindBackupRestore, Server: "prod", App: "web", BackupID: "20261019-120000; rm -rf /"},
		{Kind: KindBackupRestore, Server: "prod", App: "web", BackupID: "not-a-date"},
	} {
		if _, _, _, err := Build(request, testResolver); err == nil {
			t.Fatalf("Build(%+v) accepted an unsafe request", request)
		}
	}
}

func TestBuilderRejectsUnknownCommandsAndFlagLikeIdentifiers(t *testing.T) {
	for _, request := range []Request{
		{Kind: "command", Server: "prod", App: "web"},
		{Kind: KindAppLifecycle, Server: "prod", App: "web", Action: "shell"},
		{Kind: KindDeploy, Server: "--help", App: "web"},
		{Kind: KindTemplateInstall, Server: "prod", Template: "--help", Domain: "example.com"},
	} {
		if _, _, _, err := Build(request, testResolver); err == nil {
			t.Fatalf("Build(%+v) accepted unsafe request", request)
		}
	}
}

func TestDeployBuilderPreservesImageOptionalRedeploy(t *testing.T) {
	command, _, _, err := BuildDeploy(Request{Kind: KindDeploy, Server: "prod", App: "web"}, testResolver)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(command.Args, "--image") {
		t.Fatalf("image-less redeploy args = %v", command.Args)
	}
}

// Spec P2: a new app can be created with its settings in Teploy home; any
// other home value is refused before it reaches the CLI.
func TestDeployAndTemplateHome(t *testing.T) {
	command, _, _, err := BuildDeploy(Request{Kind: KindDeploy, Server: "prod", App: "web", Image: "nginx", Home: "teploy"}, testResolver)
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.Index(command.Args, "--home"); i < 0 || command.Args[i+1] != "teploy" {
		t.Fatalf("deploy --home args = %v", command.Args)
	}
	plain, _, _, err := BuildDeploy(Request{Kind: KindDeploy, Server: "prod", App: "web", Image: "nginx"}, testResolver)
	if err != nil || slices.Contains(plain.Args, "--home") {
		t.Fatalf("no home must keep today's argv: %v %v", plain.Args, err)
	}
	tpl, _, _, err := Build(Request{Kind: KindTemplateInstall, Server: "prod", Template: "postgres", Domain: "db.example", Home: "teploy"}, testResolver)
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.Index(tpl.Args, "--home"); i < 0 || tpl.Args[i+1] != "teploy" {
		t.Fatalf("template --home args = %v", tpl.Args)
	}
	for _, request := range []Request{
		{Kind: KindDeploy, Server: "prod", App: "web", Image: "nginx", Home: "folder"},
		{Kind: KindTemplateInstall, Server: "prod", Template: "postgres", Domain: "db.example", Home: "--purge"},
	} {
		if _, _, _, err := Build(request, testResolver); err == nil {
			t.Fatalf("Build(%+v) accepted an unknown home", request)
		}
	}
}

func TestRemovedKindsAreRefused(t *testing.T) {
	for _, kind := range []Kind{"manifest_apply", "manifest_plan", "manifest_validate", "source_preview", "source_preview_destroy", "source_preview_expire"} {
		if _, _, _, err := Build(Request{Kind: kind, Server: "prod", App: "web", Mode: "git-managed"}, testResolver); err == nil || !strings.Contains(err.Error(), "unsupported operation kind") {
			t.Fatalf("%s: Build err = %v, want unsupported", kind, err)
		}
	}
}

// A10: empty template variable values are preserved as empty in the redacted
// record — a literal [REDACTED] would deploy as the value on retry/recovery,
// and a request whose variables are all empty is not secret-bearing.
func TestRedactedRequestPreservesEmptyVars(t *testing.T) {
	req := Request{Kind: KindTemplateInstall, Server: "prod", Template: "postgres", Domain: "db.example.com",
		Vars: map[string]string{"EMPTY": "", "SECRET": "hunter2"}}
	clean := redactedRequest(req)
	if clean.Vars["EMPTY"] != "" {
		t.Fatalf("empty var redacted to %q, want preserved empty", clean.Vars["EMPTY"])
	}
	if clean.Vars["SECRET"] != "[REDACTED]" {
		t.Fatalf("secret var = %q, want [REDACTED]", clean.Vars["SECRET"])
	}
	// The caller's map is not mutated.
	if req.Vars["SECRET"] != "hunter2" {
		t.Fatalf("original request mutated: %+v", req.Vars)
	}
}

// A11 (UPSTREAM-1 adoption): with --var-stdin support, template variables
// ride on stdin as one JSON object and never appear in the argv; without it,
// the legacy --var path is used.
func TestTemplateInstallVarStdin(t *testing.T) {
	req := Request{Kind: KindTemplateInstall, Server: "prod", Template: "postgres", Domain: "db.example.com",
		Vars: map[string]string{"PASSWORD": "hunter2", "EMPTY": ""}}

	command, _, _, err := BuildTemplateInstall(req, testResolver, true)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(command.Args, "--var") || slices.Contains(command.Args, "PASSWORD=hunter2") {
		t.Fatalf("secret reached the argv with var-stdin support: %v", command.Args)
	}
	if !slices.Contains(command.Args, "--var-stdin") {
		t.Fatalf("missing --var-stdin flag: %v", command.Args)
	}
	var decoded map[string]string
	if err := json.Unmarshal([]byte(command.Stdin), &decoded); err != nil {
		t.Fatalf("stdin payload is not a JSON object: %v", err)
	}
	if decoded["PASSWORD"] != "hunter2" || decoded["EMPTY"] != "" {
		t.Fatalf("stdin payload = %v", decoded)
	}

	// R03: without the opt-in, a nonempty secret on an unsupported CLI is
	// refused outright.
	if _, _, _, err := BuildTemplateInstall(req, testResolver, false); err == nil {
		t.Fatal("legacy argv transport must refuse secret values without the operator opt-in")
	}
	// With the explicit opt-in the documented compatibility path applies.
	SetLegacySecretArgVAllowed(func() bool { return true })
	t.Cleanup(func() { SetLegacySecretArgVAllowed(func() bool { return false }) })
	legacy, _, _, err := BuildTemplateInstall(req, testResolver, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(legacy.Args, "PASSWORD=hunter2") || legacy.Stdin != "" {
		t.Fatalf("legacy path wrong: %v / %q", legacy.Args, legacy.Stdin)
	}
}

// A11: a secret spanning multiple lines is redacted per component — the
// line-based logger never sees the whole value as one line.
func TestRedactHandlesMultilineSecrets(t *testing.T) {
	secret := "line-one\nline-two\r\nline-three"
	got := Redact("prefix "+secret+" suffix\nfirst part alone: line-one", []string{secret})
	if strings.Contains(got, "line-one") || strings.Contains(got, "line-two") || strings.Contains(got, "line-three") {
		t.Fatalf("multiline secret components leaked: %q", got)
	}
}
