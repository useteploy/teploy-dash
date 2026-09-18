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

func TestManifestCommandConstructionUsesInternalProjectResolver(t *testing.T) {
	projectDir := "/var/teploy-dash/manifests/prod/web/revisions/" + strings.Repeat("a", 64)
	resolver := func(server, app, revision string) (string, error) {
		if server != "prod" || app != "web" || revision != strings.Repeat("a", 64) {
			t.Fatalf("unexpected project lookup: %s/%s@%s", server, app, revision)
		}
		return projectDir, nil
	}
	tests := []struct {
		kind Kind
		want []string
	}{
		{KindManifestApply, []string{"--project-dir", projectDir, "deploy", "--host", "prod.example", "--user", "deploy"}},
		{KindManifestPlan, []string{"--project-dir", projectDir, "plan", "--host", "prod.example", "--user", "deploy", "--json"}},
		{KindManifestValidate, []string{"--project-dir", projectDir, "validate", "--host", "prod.example", "--user", "deploy", "--json"}},
	}
	for _, test := range tests {
		command, _, target, err := Build(Request{
			Kind: test.kind, Server: "prod", App: "web", Mode: "dash-managed", ManifestRevision: strings.Repeat("a", 64),
		}, testResolver, resolver)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(command.Args, test.want) || target != "server:prod/app:web" {
			t.Fatalf("%s command=%v target=%q", test.kind, command.Args, target)
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
