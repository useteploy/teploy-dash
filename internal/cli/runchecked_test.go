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
		{"generic fallback", &Result{ExitCode: 3}, "teploy rollback exited with code 3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkExit(tc.result, []string{"rollback"})
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
