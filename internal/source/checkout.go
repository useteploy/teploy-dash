package source

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func ValidCommit(s string) bool { return commitPattern.MatchString(s) && strings.Trim(s, "0") != "" }

// gitSession owns a private HOME and credential helper. Neither tokens nor
// authorization headers appear in argv, inherited environment, config or logs.
func (a *Access) gitSession(ctx context.Context) (string, []string, []string, func(), error) {
	root, e := os.MkdirTemp("", "teploy-source-")
	if e != nil {
		return "", nil, nil, nil, fmt.Errorf("source workspace unavailable")
	}
	cleanup := func() { os.RemoveAll(root) }
	fail := func(e error) (string, []string, []string, func(), error) { cleanup(); return "", nil, nil, nil, e }
	u, e := admittedURL(a.Provider.Repository, a.Provider.AllowPrivate)
	if e != nil {
		return fail(e)
	}
	ips, e := net.DefaultResolver.LookupIP(ctx, "ip", u.Hostname())
	if e != nil || len(ips) == 0 {
		return fail(fmt.Errorf("repository DNS unavailable"))
	}
	for _, ip := range ips {
		if !allowedIP(ip, a.Provider.AllowPrivate) {
			return fail(fmt.Errorf("repository address not admitted"))
		}
	}
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	addresses := []string{}
	for _, ip := range ips {
		v := ip.String()
		if strings.Contains(v, ":") {
			v = "[" + v + "]"
		}
		addresses = append(addresses, v)
	}
	args := []string{"-c", "core.hooksPath=/dev/null", "-c", "init.templateDir=", "-c", "credential.helper=", "-c", "http.followRedirects=false", "-c", "http.sslVerify=true", "-c", "protocol.allow=never", "-c", "protocol.https.allow=always", "-c", "protocol.http.allow=always", "-c", "http.curloptResolve=" + u.Hostname() + ":" + port + ":" + strings.Join(addresses, ",")}
	if strings.ContainsAny(a.Username, "\r\n\x00") {
		return fail(fmt.Errorf("credential username invalid"))
	}
	if e = os.WriteFile(filepath.Join(root, "username"), []byte(a.Username+"\n"), 0600); e != nil {
		return fail(fmt.Errorf("credential transport unavailable"))
	}
	if e = os.WriteFile(filepath.Join(root, "password"), []byte(a.Token+"\n"), 0600); e != nil {
		return fail(fmt.Errorf("credential transport unavailable"))
	}
	// Helper executes with its own directory as HOME. Git's prompts, traces and
	// system/global configuration are disabled; failure output is never surfaced.
	helper := filepath.Join(root, "askpass")
	if e = os.WriteFile(helper, []byte("#!/bin/sh\ncase \"$1\" in *Username*) cat \"$HOME/username\";; *Password*) cat \"$HOME/password\";; *) exit 1;; esac\n"), 0700); e != nil {
		return fail(fmt.Errorf("credential transport unavailable"))
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "LANG=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=" + helper}
	return root, args, env, cleanup, nil
}
func gitRun(ctx context.Context, dir string, args, env []string) (string, error) {
	c := exec.CommandContext(ctx, "git", args...)
	configureGitProcessGroup(c)
	c.Cancel = func() error { return terminateGitProcessGroup(c) }
	c.Dir = dir
	c.Env = env
	c.WaitDelay = 2 * time.Second
	// Only bounded, locally derived stdout is ever read. Remote diagnostics may
	// contain credentials and are discarded instead of partial-line redaction.
	var out limitedBuffer
	c.Stdout = &out
	if e := c.Run(); e != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("git repository access failed")
	}
	return strings.TrimSpace(out.String()), nil
}

type limitedBuffer struct{ b []byte }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if len(b.b)+len(p) > 1<<20 {
		return 0, fmt.Errorf("git output exceeds bounds")
	}
	b.b = append(b.b, p...)
	return len(p), nil
}
func (b *limitedBuffer) String() string { return string(b.b) }
func (a *Access) verifyGit(ctx context.Context) error {
	root, args, env, cleanup, e := a.gitSession(ctx)
	if e != nil {
		return e
	}
	defer cleanup()
	_, e = gitRun(ctx, root, append(args, "ls-remote", "--exit-code", a.Provider.Repository, "HEAD"), env)
	return e
}
func (a *Access) Checkout(ctx context.Context, sha string) (string, func(), error) {
	if !ValidCommit(sha) {
		return "", nil, fmt.Errorf("immutable source commit required")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	root, args, env, cleanup, e := a.gitSession(ctx)
	if e != nil {
		return "", nil, e
	}
	dir := filepath.Join(root, "repo")
	if e = os.Mkdir(dir, 0700); e != nil {
		cleanup()
		return "", nil, fmt.Errorf("checkout workspace unavailable")
	}
	if e = CheckoutRepository(ctx, dir, a.Provider.Repository, sha, args, env); e != nil {
		cleanup()
		return "", nil, e
	}
	// Credentials are destroyed before any checked-out content is consumed.
	for _, name := range []string{"password", "username", "askpass"} {
		if e = os.Remove(filepath.Join(root, name)); e != nil {
			cleanup()
			return "", nil, fmt.Errorf("credential cleanup failed")
		}
	}
	return dir, cleanup, nil
}

// CheckoutRepository fetches and verifies an immutable revision using only the
// caller-admitted transport. Production passes isolated gitSession arguments.
func CheckoutRepository(ctx context.Context, dir, repo, sha string, args, env []string) error {
	// Defense applies even to controlled transports used by wrapper tests.
	args = append(append([]string{}, args...), "-c", "core.hooksPath=/dev/null", "-c", "init.templateDir=")
	for _, step := range [][]string{{"init", "--quiet", "--template="}, {"remote", "add", "origin", repo}, {"fetch", "--quiet", "--no-tags", "--depth=1", "origin", sha}} {
		if _, e := gitRun(ctx, dir, append(append([]string{}, args...), step...), env); e != nil {
			return e
		}
	}
	got, e := gitRun(ctx, dir, append(append([]string{}, args...), "rev-parse", "FETCH_HEAD^{commit}"), env)
	if e != nil || got != sha {
		return fmt.Errorf("fetched commit does not match admitted SHA")
	}
	if _, e = gitRun(ctx, dir, append(append([]string{}, args...), "checkout", "--quiet", "--detach", sha), env); e != nil {
		return e
	}
	realDir, e := filepath.EvalSymlinks(dir)
	if e != nil {
		return fmt.Errorf("checkout root unavailable")
	}
	return filepath.WalkDir(realDir, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return fmt.Errorf("checkout path unreadable")
		}
		if d.IsDir() && path == filepath.Join(realDir, ".git") {
			return filepath.SkipDir
		}
		if d.Type()&os.ModeSymlink != 0 {
			target, e := filepath.EvalSymlinks(path)
			if e != nil || !within(realDir, target) {
				return fmt.Errorf("checkout symlink escapes repository")
			}
		}
		return nil
	})
}
func within(root, path string) bool {
	rel, e := filepath.Rel(root, path)
	return e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
func ProjectPath(root, manifestPath string) (string, error) {
	realRoot, e := filepath.EvalSymlinks(root)
	if e != nil {
		return "", fmt.Errorf("checkout root unavailable")
	}
	root = realRoot
	if manifestPath == "" {
		manifestPath = "teploy.yml"
	}
	if filepath.IsAbs(manifestPath) {
		return "", fmt.Errorf("source manifest path must be relative")
	}
	path := filepath.Join(root, manifestPath)
	if !within(root, path) {
		return "", fmt.Errorf("source manifest path escapes repository")
	}
	actual, e := filepath.EvalSymlinks(path)
	if e != nil || !within(root, actual) {
		return "", fmt.Errorf("source manifest path unavailable or unsafe")
	}
	st, e := os.Stat(actual)
	if e != nil || !st.Mode().IsRegular() || st.Size() > 1<<20 {
		return "", fmt.Errorf("source manifest is not a bounded regular file")
	}
	// CLI has a fixed teploy.yml entrypoint. A monorepo selects its directory,
	// retaining the relative build context instead of flattening the tree.
	if filepath.Base(path) != "teploy.yml" {
		return "", fmt.Errorf("source manifest path must end in teploy.yml")
	}
	return filepath.Dir(path), nil
}

func checkoutGit(ctx context.Context, dir, repo, sha string, args, env []string) error {
	return CheckoutRepository(ctx, dir, repo, sha, args, env)
}
