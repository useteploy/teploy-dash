package operation

import (
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
			command, target, err := Build(test.request, testResolver)
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
		if _, _, err := Build(request, testResolver); err == nil {
			t.Fatalf("Build(%+v) accepted unsafe request", request)
		}
	}
}

func TestDeployBuilderPreservesImageOptionalRedeploy(t *testing.T) {
	command, _, err := BuildDeploy(Request{Kind: KindDeploy, Server: "prod", App: "web"}, testResolver)
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
		command, target, err := Build(Request{
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
