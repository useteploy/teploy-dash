package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// Role names. Canonical across Teploy's self-hosted tools, matching
// teploy-observe (admin/editor/viewer): admin manages users, settings, and
// secrets; editor performs deploys and app actions; viewer is read-only.
const (
	RoleAdmin  = "admin"
	RoleEditor = "editor"
	RoleViewer = "viewer"
)

// maxPasswordBytes is bcrypt's hard input ceiling — GenerateFromPassword errors
// beyond it. Reject longer passwords rather than store the empty hash the error
// path would otherwise produce (which silently locks an account out).
const maxPasswordBytes = 72

// dummyBcryptHash is a valid bcrypt hash (of a random string) used to spend the
// same CPU on a nonexistent-user login as a real one, removing the timing
// side-channel that would otherwise reveal which usernames exist.
const dummyBcryptHash = "$2a$10$N9qo8uLOickgx2ZMRZoMye1J7.6FkVqI3rR0pQ1bQ8XfQ9qK0e2C"

// bcryptCost is the work factor for stored password hashes. It's a var only so
// tests can lower it — production always uses bcrypt.DefaultCost.
var bcryptCost = bcrypt.DefaultCost

// roleRank orders roles for "has at least this role" checks.
func roleRank(role string) int {
	switch role {
	case RoleAdmin:
		return 3
	case RoleEditor:
		return 2
	case RoleViewer:
		return 1
	default:
		return 0
	}
}

// normalizeRole returns a known role, defaulting anything unrecognized to
// viewer (fail-safe: an unknown role gets the least privilege, never more).
func normalizeRole(r string) string {
	switch r {
	case RoleAdmin, RoleEditor, RoleViewer:
		return r
	default:
		return RoleViewer
	}
}

// roleAllows reports whether a user holding `have` may act where `need` is
// required.
func roleAllows(have, need string) bool {
	return roleRank(have) >= roleRank(need)
}

// adminOnlyPrefixes are routes that manage accounts, credentials, or fleet
// config — restricted to admins for both reads and writes because their
// payloads carry secrets (registry passwords, SMTP/webhook targets, tokens).
var adminOnlyPrefixes = []string{
	"/api/users",
	"/api/mcp-tokens",
	"/api/config/servers",
	"/api/registries",
	"/api/notifications",
}

// requiredRole returns the minimum role for a route. Reads default to viewer,
// mutations to editor, and the admin-only prefixes to admin. It fails closed:
// any unclassified mutating route requires editor, never viewer, so a new
// endpoint can't accidentally be viewer-writable.
func requiredRole(method, path string) string {
	for _, p := range adminOnlyPrefixes {
		if path == p || strings.HasPrefix(path, p+"/") {
			return RoleAdmin
		}
	}
	// Changing your own password is self-service — any authenticated user.
	if path == "/api/auth/password" {
		return RoleViewer
	}
	if isMutating(method) {
		return RoleEditor
	}
	return RoleViewer
}

// ── Request-scoped identity ──────────────────────────────────────────────

type ctxKey string

const userCtxKey ctxKey = "dashUser"

// withUser attaches the authenticated session to the request context so
// downstream handlers can attribute actions and enforce self-service scoping.
func withUser(r *http.Request, si *sessionInfo) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), userCtxKey, si))
}

// currentUser returns the authenticated session on the request, if any.
func currentUser(r *http.Request) (*sessionInfo, bool) {
	si, ok := r.Context().Value(userCtxKey).(*sessionInfo)
	return si, ok
}

// ── User store ────────────────────────────────────────────────────────────

// dashUser is one dashboard account. Only the bcrypt hash is persisted.
type dashUser struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
	Role         string `json:"role"`
}

// usersFileFormat is the on-disk shape of users.json.
type usersFileFormat struct {
	Users []dashUser `json:"users"`
}

// legacyCredFile is the pre-RBAC single-user auth.json shape, read once to
// migrate an existing operator into the multi-user store as the first admin.
type legacyCredFile struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
}

// loadUsers populates the in-memory user map. It prefers users.json; failing
// that, it migrates a legacy single-user auth.json into an admin account and
// writes users.json so subsequent loads are canonical. Returns an error when no
// usable credentials exist (the caller decides whether that means setup mode).
func (g *authGate) loadUsers() error {
	if data, err := os.ReadFile(g.usersFile); err == nil {
		var f usersFileFormat
		if err := json.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("parsing %s: %w", g.usersFile, err)
		}
		g.credMu.Lock()
		defer g.credMu.Unlock()
		g.users = make(map[string]*dashUser, len(f.Users))
		for i := range f.Users {
			u := f.Users[i]
			u.Role = normalizeRole(u.Role)
			g.users[u.Username] = &u
		}
		g.setupRequired = len(g.users) == 0
		if len(g.users) == 0 {
			return fmt.Errorf("no users configured in %s", g.usersFile)
		}
		return nil
	}

	// No users.json — try migrating a legacy single-user auth.json.
	data, err := os.ReadFile(g.legacyFile)
	if err != nil {
		return err
	}
	var creds legacyCredFile
	if err := json.Unmarshal(data, &creds); err != nil {
		return fmt.Errorf("parsing legacy %s: %w", g.legacyFile, err)
	}
	if creds.PasswordHash == "" {
		return fmt.Errorf("legacy %s has no password hash", g.legacyFile)
	}
	username := creds.Username
	if username == "" {
		username = "admin"
	}
	g.credMu.Lock()
	defer g.credMu.Unlock()
	g.users = map[string]*dashUser{
		username: {Username: username, PasswordHash: creds.PasswordHash, Role: RoleAdmin},
	}
	g.setupRequired = false
	if err := g.saveUsersLocked(); err != nil {
		// The in-memory admin still works this run; just warn that the on-disk
		// migration didn't persist (it will retry next start).
		log.Printf("auth: migrated legacy %s but could not write %s: %v", g.legacyFile, g.usersFile, err)
	}
	return nil
}

// saveUsersLocked writes users.json atomically. The caller must hold g.credMu
// (read or write).
func (g *authGate) saveUsersLocked() error {
	var f usersFileFormat
	for _, u := range g.users {
		f.Users = append(f.Users, *u)
	}
	sort.Slice(f.Users, func(i, j int) bool { return f.Users[i].Username < f.Users[j].Username })
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(g.usersFile), 0700); err != nil {
		return err
	}
	tmp := g.usersFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, g.usersFile)
}

// authenticate verifies username+password. It always performs a bcrypt compare
// (against a dummy hash for unknown users) so response time doesn't reveal
// which usernames exist. The env-var bootstrap credential is honored as an
// implicit admin when no matching stored user exists.
func (g *authGate) authenticate(username, password string) (*dashUser, bool) {
	g.credMu.RLock()
	u := g.users[username]
	g.credMu.RUnlock()

	if u != nil {
		if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) == nil {
			return &dashUser{Username: u.Username, Role: normalizeRole(u.Role)}, true
		}
		return nil, false
	}

	// Env-var bootstrap: a single implicit admin, used before any user file
	// exists (e.g. TEPLOY_DASH_PASSWORD in Docker). Empty submitted username
	// resolves to the configured env user (or "admin").
	if g.pass != "" {
		envUser := g.user
		if envUser == "" {
			envUser = "admin"
		}
		if username == "" || username == envUser {
			if subtle.ConstantTimeCompare([]byte(password), []byte(g.pass)) == 1 {
				return &dashUser{Username: envUser, Role: RoleAdmin}, true
			}
		}
	}

	// Spend equal CPU on a miss to hide whether the username exists.
	bcrypt.CompareHashAndPassword([]byte(dummyBcryptHash), []byte(password))
	return nil, false
}

// createUser adds a new account. Fails if the username already exists.
func (g *authGate) createUser(username, password, role string) error {
	username = strings.TrimSpace(username)
	if username == "" {
		return fmt.Errorf("username is required")
	}
	if len(password) < 8 {
		return fmt.Errorf("password must be at least 8 characters")
	}
	if len(password) > maxPasswordBytes {
		return fmt.Errorf("password must be at most %d bytes", maxPasswordBytes)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return err
	}
	g.credMu.Lock()
	defer g.credMu.Unlock()
	if _, exists := g.users[username]; exists {
		return fmt.Errorf("user %q already exists", username)
	}
	g.users[username] = &dashUser{Username: username, PasswordHash: string(hash), Role: normalizeRole(role)}
	g.setupRequired = false
	return g.saveUsersLocked()
}

// setPassword replaces a user's password. If the username isn't in the store
// yet but matches the caller's authenticated session (e.g. an env-bootstrap
// admin changing their password), it is created as an admin so the change
// persists.
func (g *authGate) setPassword(username, password string) error {
	if len(password) < 8 {
		return fmt.Errorf("password must be at least 8 characters")
	}
	if len(password) > maxPasswordBytes {
		return fmt.Errorf("password must be at most %d bytes", maxPasswordBytes)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return err
	}
	g.credMu.Lock()
	defer g.credMu.Unlock()
	u := g.users[username]
	if u == nil {
		// Persist the previously-env-only admin.
		u = &dashUser{Username: username, Role: RoleAdmin}
		g.users[username] = u
	}
	u.PasswordHash = string(hash)
	g.setupRequired = false
	return g.saveUsersLocked()
}

// setRole changes a user's role, refusing to demote the last remaining admin
// (which would leave the dashboard unmanageable).
func (g *authGate) setRole(username, role string) error {
	role = normalizeRole(role)
	g.credMu.Lock()
	defer g.credMu.Unlock()
	u := g.users[username]
	if u == nil {
		return fmt.Errorf("user not found")
	}
	if u.Role == RoleAdmin && role != RoleAdmin && g.countAdminsLocked() <= 1 {
		return fmt.Errorf("cannot demote the last admin")
	}
	u.Role = role
	return g.saveUsersLocked()
}

// deleteUser removes an account, refusing to remove the last remaining admin.
func (g *authGate) deleteUser(username string) error {
	g.credMu.Lock()
	defer g.credMu.Unlock()
	u := g.users[username]
	if u == nil {
		return fmt.Errorf("user not found")
	}
	if u.Role == RoleAdmin && g.countAdminsLocked() <= 1 {
		return fmt.Errorf("cannot remove the last admin")
	}
	delete(g.users, username)
	return g.saveUsersLocked()
}

func (g *authGate) countAdminsLocked() int {
	n := 0
	for _, u := range g.users {
		if u.Role == RoleAdmin {
			n++
		}
	}
	return n
}

// userView is the API projection of an account — never the hash.
type userView struct {
	Username string `json:"username"`
	Role     string `json:"role"`
}

func (g *authGate) listUsers() []userView {
	g.credMu.RLock()
	defer g.credMu.RUnlock()
	out := make([]userView, 0, len(g.users))
	for _, u := range g.users {
		out = append(out, userView{Username: u.Username, Role: u.Role})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out
}

// ── Handlers ────────────────────────────────────────────────────────────

// handleWhoami reports the current user's identity and role so the frontend
// can hide controls the user isn't allowed to use. Any authenticated user.
func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	session, ok := currentUser(r)
	if !ok {
		jsonError(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	writeData(w, userView{Username: session.user, Role: session.role})
}

// handleLoginMethods reports which sign-in methods the login page should offer.
// Unauthenticated (the login page fetches it before there's a session). Password
// login is always available as the break-glass path; SSO appears only when OIDC
// is configured.
func (s *Server) handleLoginMethods(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	resp := map[string]interface{}{"password": true, "oidc": false}
	if s.gate != nil && s.gate.oidc != nil {
		resp["oidc"] = true
		resp["oidc_label"] = s.gate.oidc.label
	}
	writeJSON(w, resp)
}

// handleUsers lists (GET) or creates (POST) accounts. Admin-only (enforced by
// the gate via adminOnlyPrefixes).
func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if s.gate == nil {
		writeError(w, "authentication is disabled")
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeData(w, s.gate.listUsers())
	case http.MethodPost:
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
			Role     string `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, "invalid request body")
			return
		}
		if err := s.gate.createUser(body.Username, body.Password, body.Role); err != nil {
			writeError(w, err.Error())
			return
		}
		writeData(w, map[string]bool{"ok": true})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleUserAction manages one account:
//
//	DELETE /api/users/{username}           remove the account
//	PUT    /api/users/{username}           change role  {"role": "editor"}
//	POST   /api/users/{username}/password  admin reset  {"password": "..."}
//
// Admin-only (enforced by the gate).
func (s *Server) handleUserAction(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if s.gate == nil {
		writeError(w, "authentication is disabled")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/users/")
	parts := strings.SplitN(rest, "/", 2)
	username := parts[0]
	sub := ""
	if len(parts) == 2 {
		sub = parts[1]
	}
	if username == "" {
		writeError(w, "username required")
		return
	}

	switch {
	case r.Method == http.MethodDelete && sub == "":
		// Guard against removing the account you're currently signed in as —
		// that would sign you out of your own admin session mid-request.
		if session, ok := currentUser(r); ok && session.user == username {
			writeError(w, "cannot remove the account you are signed in as")
			return
		}
		if err := s.gate.deleteUser(username); err != nil {
			writeError(w, err.Error())
			return
		}
		s.gate.deleteUserSessions(username)
		writeData(w, map[string]bool{"ok": true})

	case r.Method == http.MethodPut && sub == "":
		var body struct {
			Role string `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, "invalid request body")
			return
		}
		if err := s.gate.setRole(username, body.Role); err != nil {
			writeError(w, err.Error())
			return
		}
		// Force the user to re-authenticate so their new role takes effect in a
		// fresh session rather than lingering at the old privilege.
		s.gate.deleteUserSessions(username)
		writeData(w, map[string]bool{"ok": true})

	case r.Method == http.MethodPost && sub == "password":
		var body struct {
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, "invalid request body")
			return
		}
		if err := s.gate.setPassword(username, body.Password); err != nil {
			writeError(w, err.Error())
			return
		}
		s.gate.deleteUserSessions(username)
		writeData(w, map[string]bool{"ok": true})

	default:
		writeError(w, "unsupported user action")
	}
}
