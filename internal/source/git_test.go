package source

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
