package source

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func localGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	b, e := c.CombinedOutput()
	if e != nil {
		t.Fatalf("git fixture: %v %s", e, b)
	}
	return strings.TrimSpace(string(b))
}
func TestImmutableCheckoutLocalGitAndMonorepo(t *testing.T) {
	if _, e := exec.LookPath("git"); e != nil {
		t.Skip("git required")
	}
	repo := t.TempDir()
	localGit(t, repo, "init", "--quiet")
	os.Mkdir(filepath.Join(repo, "app"), 0700)
	os.WriteFile(filepath.Join(repo, "app", "teploy.yml"), []byte("app: fixture\nport: 8080\n"), 0600)
	localGit(t, repo, "add", ".")
	localGit(t, repo, "-c", "user.name=fixture", "-c", "user.email=fixture@invalid", "commit", "--quiet", "-m", "first")
	sha := localGit(t, repo, "rev-parse", "HEAD")
	os.WriteFile(filepath.Join(repo, "app", "teploy.yml"), []byte("app: moved\n"), 0600)
	localGit(t, repo, "add", ".")
	localGit(t, repo, "-c", "user.name=fixture", "-c", "user.email=fixture@invalid", "commit", "--quiet", "-m", "moved")
	dir := t.TempDir()
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
	// file transport is enabled solely for this local fixture; production permits
	// admitted HTTPS/HTTP and does not expose a file-transport setting.
	if e := checkoutGit(context.Background(), dir, repo, sha, []string{"-c", "protocol.file.allow=always"}, env); e != nil {
		t.Fatal(e)
	}
	project, e := ProjectPath(dir, "app/teploy.yml")
	if e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(filepath.Join(project, "teploy.yml"))
	if !strings.Contains(string(b), "fixture") {
		t.Fatalf("moving tip replaced admitted content: %s", b)
	}
	if _, e = ProjectPath(dir, "../../etc/passwd"); e == nil {
		t.Fatal("escaping path passed")
	}
	bad := t.TempDir()
	if e = checkoutGit(context.Background(), bad, repo, strings.Repeat("a", 40), []string{"-c", "protocol.file.allow=always"}, env); e == nil {
		t.Fatal("missing commit passed")
	}
}
func TestCredentialTransportHasNoSecretArgvOrEnvironment(t *testing.T) {
	a := &Access{Provider: Provider{Repository: "http://127.0.0.1/repo", AllowPrivate: true}, Username: "fixture", Token: "secret-must-not-be-in-argv"}
	root, args, env, cleanup, e := a.gitSession(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	defer cleanup()
	if strings.Contains(strings.Join(append(args, env...), " "), a.Token) {
		t.Fatal("credential exposed in process metadata")
	}
	b, e := os.ReadFile(filepath.Join(root, "password"))
	if e != nil || strings.TrimSpace(string(b)) != a.Token {
		t.Fatal("private helper credential missing")
	}
	st, _ := os.Stat(filepath.Join(root, "password"))
	if st.Mode().Perm() != 0600 {
		t.Fatal("credential file not private")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e = gitRun(ctx, root, []string{"status"}, env); e != context.Canceled {
		t.Fatalf("git cancellation lost: %v", e)
	}
}

func TestImmutableCheckoutExplicitlySuppressesTemplatesAndHooks(t *testing.T) {
	repo := t.TempDir()
	localGit(t, repo, "init", "--quiet")
	os.WriteFile(filepath.Join(repo, "teploy.yml"), []byte("app: fixture\n"), 0600)
	localGit(t, repo, "add", ".")
	localGit(t, repo, "-c", "user.name=fixture", "-c", "user.email=fixture@invalid", "commit", "--quiet", "-m", "fixture")
	sha := localGit(t, repo, "rev-parse", "HEAD")
	templates := t.TempDir()
	os.Mkdir(filepath.Join(templates, "hooks"), 0700)
	marker := filepath.Join(t.TempDir(), "hook-ran")
	os.WriteFile(filepath.Join(templates, "hooks", "post-checkout"), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0700)
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TEMPLATE_DIR=" + templates}
	dir := t.TempDir()
	if e := CheckoutRepository(context.Background(), dir, repo, sha, []string{"-c", "protocol.file.allow=always", "-c", "init.templateDir=" + templates}, env); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(marker); !os.IsNotExist(e) {
		t.Fatal("checkout executed injected hook")
	}
	if _, e := os.Stat(filepath.Join(dir, ".git", "hooks", "post-checkout")); !os.IsNotExist(e) {
		t.Fatal("init copied injected template hook")
	}
}
