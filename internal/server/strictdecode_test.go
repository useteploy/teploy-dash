package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestStrictDecode_RejectsUnknownFieldsAndTrailingValues(t *testing.T) {
	type body struct {
		Username string `json:"username"`
	}
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"valid single object", `{"username":"alice"}`, false},
		{"unknown field", `{"username":"alice","admin":true}`, true},
		{"trailing value", `{"username":"alice"}{"username":"bob"}`, true},
		{"truncated", `{"username":`, true},
		{"empty body", ``, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/x", bytes.NewReader([]byte(c.raw)))
			var b body
			err := strictDecode(req, &b)
			if (err != nil) != c.wantErr {
				t.Errorf("raw %q: err = %v, wantErr = %v", c.raw, err, c.wantErr)
			}
		})
	}
}

// DASH-008: confirm the fix actually reached a real handler, not just the
// helper in isolation. handleLogin is the highest-value auth boundary.
func TestHandleLogin_RejectsUnknownFieldsAndTrailingValues(t *testing.T) {
	dir := t.TempDir()
	g := newAuthGate("", "", filepath.Join(dir, "auth.json"))
	if err := g.createUser("alice", "alicepassword", RoleViewer); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	cases := []struct {
		name string
		raw  string
	}{
		{"unknown field", `{"username":"alice","password":"alicepassword","role":"admin"}`},
		{"trailing value", `{"username":"alice","password":"alicepassword"}{"x":1}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewReader([]byte(c.raw)))
			w := httptest.NewRecorder()
			g.handleLogin(w, req)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, body = %s, want 400", w.Code, w.Body.String())
			}
		})
	}

	// A clean, single-object body with correct credentials still works.
	req := httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewReader([]byte(`{"username":"alice","password":"alicepassword"}`)))
	w := httptest.NewRecorder()
	g.handleLogin(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("valid login: status = %d, body = %s, want 200", w.Code, w.Body.String())
	}
}
