package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/useteploy/teploy-dash/internal/caps"
	"github.com/useteploy/teploy-dash/internal/operation"
)

// Revocation takes effect on an already admitted replay, including frames
// queued before the principal lost its grant.
func TestOperationReplayRechecksLiveLogGrant(t *testing.T) {
	s := newCapsServer(t, t.TempDir())
	seedPresetUsers(t, s)
	op, _, err := s.operations.Enqueue(operation.Request{Kind: operation.KindDeploy, App: "web", Server: "prod", Image: "img:1"}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		current, err := s.operations.Get(op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status.Terminal() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("operation did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/operations/"+op.ID+"/events", nil).WithContext(context.Background())
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: s.gate.newSession("op", RoleEditor)})
	w := &revokeStreamWriter{ResponseRecorder: httptest.NewRecorder(), revoke: func() {
		s.gate.credMu.Lock()
		defer s.gate.credMu.Unlock()
		s.gate.users["op"].CapabilityProfile = "custom"
		s.gate.users["op"].Capabilities = []string{string(caps.ViewMetadata)}
	}}
	s.handler().ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), ": connected") {
		t.Fatalf("stream did not open: %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "data:") {
		t.Fatal("replay leaked after live grant revocation")
	}
	if w.calls != 1 {
		t.Fatalf("writes after revocation: %d", w.calls)
	}
}

type revokeStreamWriter struct {
	*httptest.ResponseRecorder
	revoke func()
	calls  int
}

func (w *revokeStreamWriter) Write(p []byte) (int, error) {
	w.calls++
	n, err := w.ResponseRecorder.Write(p)
	if w.calls == 1 {
		w.revoke()
	}
	return n, err
}

var _ io.Writer = (*revokeStreamWriter)(nil)
