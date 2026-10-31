package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sessionsGateMux(g *authGate) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/apps", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	return g.wrap(mux)
}

func doWithToken(h http.Handler, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/api/apps", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func setSessionExp(g *authGate, token string, exp time.Time) {
	g.sessMu.Lock()
	g.sessions[hashSessionToken(token)].exp = exp
	g.saveSessionsLocked()
	g.sessMu.Unlock()
}

func TestSessions_SurviveRestartAndStoreOnlyHashes(t *testing.T) {
	dir := t.TempDir()
	credFile := filepath.Join(dir, "auth.json")
	g := newAuthGate("", "", credFile)
	if err := g.createUser("alice", "password123", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	token := g.newSession("alice", RoleAdmin)

	path := filepath.Join(dir, "sessions.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("sessions file not written: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("sessions file mode = %o, want 600", info.Mode().Perm())
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), token) {
		t.Fatal("raw session token stored on disk")
	}
	if !strings.Contains(string(data), hashSessionToken(token)) {
		t.Fatal("token hash missing from sessions file")
	}

	reopened := newAuthGate("", "", credFile)
	if w := doWithToken(sessionsGateMux(reopened), token); w.Code != 200 {
		t.Fatalf("session after restart: got %d, want 200", w.Code)
	}
}

func TestSessions_LogoutAndUserWipePersist(t *testing.T) {
	dir := t.TempDir()
	credFile := filepath.Join(dir, "auth.json")
	g := newAuthGate("", "", credFile)
	if err := g.createUser("alice", "password123", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	a := g.newSession("alice", RoleAdmin)
	b := g.newSession("alice", RoleAdmin)
	g.deleteSession(a)
	reopened := newAuthGate("", "", credFile)
	if w := doWithToken(sessionsGateMux(reopened), a); w.Code != 401 {
		t.Fatalf("logged-out session after restart: got %d, want 401", w.Code)
	}
	g.deleteUserSessions("alice")
	reopened = newAuthGate("", "", credFile)
	if w := doWithToken(sessionsGateMux(reopened), b); w.Code != 401 {
		t.Fatalf("wiped session after restart: got %d, want 401", w.Code)
	}
}

func TestSessions_SlidingRenewal(t *testing.T) {
	dir := t.TempDir()
	credFile := filepath.Join(dir, "auth.json")
	g := newAuthGate("", "", credFile)
	if err := g.createUser("alice", "password123", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	token := g.newSession("alice", RoleAdmin)
	h := sessionsGateMux(g)

	// Fresh session: no renewal, no cookie churn.
	if w := doWithToken(h, token); w.Code != 200 || len(w.Result().Cookies()) != 0 {
		t.Fatalf("fresh session: code %d, cookies %v (want 200, none)", w.Code, w.Result().Cookies())
	}

	// Two days old: renewed to now+30d and the cookie is re-set.
	setSessionExp(g, token, time.Now().Add(sessionTTL-48*time.Hour))
	w := doWithToken(h, token)
	if w.Code != 200 {
		t.Fatalf("aged session: got %d", w.Code)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value != token || cookies[0].MaxAge != int(sessionTTL.Seconds()) {
		t.Fatalf("renewal cookie = %+v, want same token with 30d Max-Age", cookies)
	}
	si, _ := g.lookupSession(token)
	if time.Until(si.exp) < sessionTTL-time.Minute {
		t.Fatalf("expiry not slid: %v left", time.Until(si.exp))
	}
	// The renewal is persisted.
	reopened := newAuthGate("", "", credFile)
	rsi, ok := reopened.lookupSession(token)
	if !ok || time.Until(rsi.exp) < sessionTTL-time.Minute {
		t.Fatal("renewed expiry not persisted")
	}
}

func TestSessions_ExpiredRejectedAndDroppedOnLoad(t *testing.T) {
	dir := t.TempDir()
	credFile := filepath.Join(dir, "auth.json")
	g := newAuthGate("", "", credFile)
	if err := g.createUser("alice", "password123", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	token := g.newSession("alice", RoleAdmin)
	g.sessMu.Lock()
	g.sessions[hashSessionToken(token)].exp = time.Now().Add(-time.Second)
	g.sessMu.Unlock()
	if w := doWithToken(sessionsGateMux(g), token); w.Code != 401 {
		t.Fatalf("expired session: got %d, want 401", w.Code)
	}
	// An expired entry still on disk (written before it lapsed) is dropped
	// at load.
	stale, _ := json.Marshal(sessionsFileFormat{Sessions: map[string]persistedSession{
		hashSessionToken(token): {Sub: "alice", User: "alice", Role: RoleAdmin, Exp: time.Now().Add(-time.Second), Epoch: 1, Local: true},
	}})
	if err := os.WriteFile(filepath.Join(dir, "sessions.json"), stale, 0600); err != nil {
		t.Fatal(err)
	}
	reopened := newAuthGate("", "", credFile)
	reopened.sessMu.Lock()
	n := len(reopened.sessions)
	reopened.sessMu.Unlock()
	if n != 0 {
		t.Fatal("expired session loaded after restart")
	}
}

func TestSessions_RevokedEpochRejectedAfterRestart(t *testing.T) {
	dir := t.TempDir()
	credFile := filepath.Join(dir, "auth.json")
	g := newAuthGate("", "", credFile)
	if err := g.createUser("alice", "password123", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	token := g.newSession("alice", RoleAdmin)

	// Simulate a revocation that bumped the epoch but whose in-memory wipe
	// never reached the sessions file (e.g. a crash): the persisted session
	// must still die on the epoch check.
	data, err := os.ReadFile(filepath.Join(dir, "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := g.setPassword("alice", "newpassword123"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sessions.json"), data, 0600); err != nil {
		t.Fatal(err)
	}

	reopened := newAuthGate("", "", credFile)
	h := sessionsGateMux(reopened)
	// Age it so the renewal path would fire if validation were skipped.
	setSessionExp(reopened, token, time.Now().Add(sessionTTL-48*time.Hour))
	w := doWithToken(h, token)
	if w.Code != 401 {
		t.Fatalf("revoked-epoch session after restart: got %d, want 401", w.Code)
	}
	if len(w.Result().Cookies()) != 0 {
		t.Fatal("revoked session had its cookie renewed")
	}
}
