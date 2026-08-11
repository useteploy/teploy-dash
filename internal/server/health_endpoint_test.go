package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/useteploy/teploy-dash/internal/cli"
)

// `teploy health` exits NON-ZERO when the app is unhealthy, and prints its JSON
// verdict anyway. That is an answer, not a transport failure — surfacing it as
// an error banner would make an unhealthy app look like a broken dashboard,
// which is the opposite of what this panel exists to tell you.
func TestAppHealth_UnhealthyVerdictIsAnAnswerNotAnError(t *testing.T) {
	verdict := `{"app":"blog","host":"192.0.2.10","port":3000,"healthy":false,` +
		`"error":"timeout after 30s waiting for health check on localhost:3000",` +
		`"observed_at":"2026-08-11T12:00:00Z"}`

	runner := func(_ context.Context, args ...string) (*cli.Result, error) {
		joined := strings.Join(args, " ")
		if strings.HasPrefix(joined, "server list") {
			return &cli.Result{Stdout: `{"prod":{"host":"192.0.2.10","user":"deploy"}}`}, nil
		}
		if strings.HasPrefix(joined, "health") {
			// Non-zero exit AND a payload — exactly what the CLI does.
			return &cli.Result{Stdout: verdict, ExitCode: 1}, errors.New("exit status 1")
		}
		return nil, errors.New("unexpected command: " + joined)
	}
	s := New(Config{DataDir: t.TempDir(), NoAuth: true, CLIInstalled: func() bool { return true }, CLIRunner: runner})

	rec := httptest.NewRecorder()
	s.handleAppAction(rec, httptest.NewRequest(http.MethodGet, "/api/apps/prod/blog/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("an unhealthy verdict should still be a 200 answer; code=%d body=%s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Data struct {
			Healthy bool   `json:"healthy"`
			Error   string `json:"error"`
			Port    int    `json:"port"`
		} `json:"data"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decoding response: %v (body=%s)", err, rec.Body.String())
	}
	if envelope.Error != "" {
		t.Errorf("should not be reported as an API error, got %q", envelope.Error)
	}
	if envelope.Data.Healthy {
		t.Error("verdict should carry healthy=false")
	}
	if envelope.Data.Port != 3000 {
		t.Errorf("verdict detail should survive, got port=%d", envelope.Data.Port)
	}
}

// A real transport failure (no output at all) must still surface as an error —
// otherwise an unreachable server renders as a blank, healthy-looking panel.
func TestAppHealth_TransportFailureIsAnError(t *testing.T) {
	runner := func(_ context.Context, args ...string) (*cli.Result, error) {
		joined := strings.Join(args, " ")
		if strings.HasPrefix(joined, "server list") {
			return &cli.Result{Stdout: `{"prod":{"host":"192.0.2.10","user":"deploy"}}`}, nil
		}
		return nil, errors.New("ssh: connect to host 192.0.2.10 port 22: connection refused")
	}
	s := New(Config{DataDir: t.TempDir(), NoAuth: true, CLIInstalled: func() bool { return true }, CLIRunner: runner})

	rec := httptest.NewRecorder()
	s.handleAppAction(rec, httptest.NewRequest(http.MethodGet, "/api/apps/prod/blog/health", nil))

	var envelope struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decoding response: %v (body=%s)", err, rec.Body.String())
	}
	if envelope.Error == "" {
		t.Errorf("an unreachable server must be an error, not a silent empty verdict: %s", rec.Body.String())
	}
}
