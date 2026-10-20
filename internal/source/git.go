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
