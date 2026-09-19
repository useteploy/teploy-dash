package cli

import "testing"

// checkExit must turn a non-zero CLI exit into an error (so mutating delegate
// calls fail through the caller's normal err path) and a zero exit into nil.
func TestCheckExit(t *testing.T) {
	cases := []struct {
		name    string
		result  *Result
		wantErr string // "" means expect nil error
	}{
		{"success", &Result{ExitCode: 0, Stdout: "ok"}, ""},
		{"stderr surfaced", &Result{ExitCode: 1, Stderr: "deploy failed: image not found"}, "deploy failed: image not found"},
		{"stdout fallback", &Result{ExitCode: 2, Stdout: "boom"}, "boom"},
		{"generic fallback", &Result{ExitCode: 3}, "teploy exited with code 3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkExit(tc.result)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected nil error, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error %q, got nil", tc.wantErr)
			}
			if err.Error() != tc.wantErr {
				t.Errorf("got %q, want %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// F015: an unverified probe outcome is not cached and fails closed for the
// secret-bearing call sites; a verified answer (either way) is cached.
func TestSecretFlagProbeVerifiedCaching(t *testing.T) {
	// Hermetic: no teploy on PATH, so the probe cannot establish anything.
	t.Setenv("PATH", t.TempDir())
	p := &secretFlagProbe{flag: "--stdin", args: []string{"env", "set"}}
	supported, err := p.check()
	if err == nil {
		t.Fatal("unverified probe must surface an error")
	}
	if supported {
		t.Fatal("unverified probe must not claim support")
	}
	if p.decided {
		t.Fatal("unverified outcome must not be cached")
	}
	// A verified outcome is cached and returned on subsequent calls.
	p.decided, p.supported = true, true
	if s, err := p.check(); err != nil || !s {
		t.Fatalf("cached verified answer: %v %v", s, err)
	}
}
