package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/useteploy/teploy-dash/internal/operation"
)

// Role names. Canonical across Teploy's self-hosted tools, matching
// teploy-observe (admin/editor/viewer): admin manages users, settings, and
// secrets; editor performs deploys and app actions; viewer is read-only.
const (
	RoleAdmin  = "admin"
	RoleEditor = "editor"
	RoleViewer = "viewer"
)

// usernameRE is the creation grammar for new accounts (A07): a route-safe,
// bounded name that can never collide with the slash-separated management
// routes or exceed filename limits. Existing accounts outside the grammar
// keep working; only NEW names are rejected.
var usernameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@-]{0,63}$`)

func validateUsername(name string) error {
	if !usernameRE.MatchString(name) {
		return fmt.Errorf("username must start with a letter or digit and use only letters, digits, and _ . @ - (max 64 characters)")
	}
	return nil
}

// validRole reports whether r names a known role exactly.
func validRole(r string) bool {
	switch r {
	case RoleAdmin, RoleEditor, RoleViewer:
		return true
	}
	return false
}

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
	"/api/sso",
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

// actorFromRequest projects the authenticated session onto an operation
// Actor (A27): which principal enqueued the work. Local sessions key on the
// username; SSO sessions on the issuer-namespaced subject. Nil when the
// request carries no session (internal callers).
func actorFromRequest(r *http.Request) *operation.Actor {
	si, ok := currentUser(r)
	if !ok {
		return nil
	}
	kind := "sso"
	if si.local {
		kind = "local"
	}
	return &operation.Actor{Kind: kind, Subject: si.sub, Label: si.user}
}

// ── User store ────────────────────────────────────────────────────────────

// dashUser is one dashboard account. Only the bcrypt hash is persisted.
// AuthEpoch is a per-account revision of the credential/role state: it is
// assigned from a store-wide monotonic counter on every create, password
// change, or role change, and embedded in sessions at issuance. A session
// whose epoch no longer matches the account was issued against revoked
// state and fails validation on its next request (A03).
type dashUser struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
	Role         string `json:"role"`
	AuthEpoch    uint64 `json:"auth_epoch,omitempty"`
}

// dashPrincipal is one external (SSO) identity, persisted in users.json
// alongside local accounts (A02/A08 identity work, following teploy-observe's
// unified principal store). Subject is the issuer-namespaced identity
// (oidc:<sha256(issuer)[:16]>:<sub>) — display-name claims are recorded for
// attribution but never key the identity. AuthEpoch is drawn from the same
// store-wide monotonic counter as local accounts: sessions embed it at
// issuance and every request revalidates it unconditionally, so a revoked or
// deleted principal's sessions die on their next use. A missing row (e.g. a
// session from an install upgraded from pre-principal dash) is equally dead.
type dashPrincipal struct {
	Subject    string `json:"subject"`
	Username   string `json:"username"`
	Email      string `json:"email,omitempty"`
	Role       string `json:"role"`
	AuthEpoch  uint64 `json:"auth_epoch"`
	LastSignIn string `json:"last_sign_in,omitempty"`
}

// usersFileFormat is the on-disk shape of users.json. EpochCounter is the
// store-wide monotonic source of AuthEpoch values; advancing it on deletion
// (with no account to carry the number) guarantees a recreated username gets
// an epoch strictly higher than any session the previous account issued.
// OIDCPrincipals is additive: installs migrating from the pre-principal
// format simply lack the field and load with an empty principal set.
type usersFileFormat struct {
	Users          []dashUser      `json:"users"`
	EpochCounter   uint64          `json:"epoch_counter,omitempty"`
	OIDCPrincipals []dashPrincipal `json:"oidc_principals,omitempty"`
}

// legacyCredFile is the pre-RBAC single-user auth.json shape, read once to
// migrate an existing operator into the multi-user store as the first admin.
type legacyCredFile struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
}

// errNoUsers reports that no credential store exists at all (fresh install):
// the only condition under which setup mode or the legacy migration may run.
// Any other load failure is an authentication outage, not a new install.
var errNoUsers = errors.New("no credential store found")

// loadUsers populates the in-memory user map. It prefers users.json. Only when
// that file is genuinely ABSENT may a legacy single-user auth.json be migrated
// into an admin account (and users.json written so subsequent loads are
// canonical). An unreadable or corrupt users.json is returned as an error so
// the caller can fail closed — previously any read failure fell through to the
// legacy file, which could reactivate obsolete credentials.
func (g *authGate) loadUsers() error {
	data, err := os.ReadFile(g.usersFile)
	switch {
	case err == nil:
		var f usersFileFormat
		if err := json.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("parsing %s: %w", g.usersFile, err)
		}
		g.credMu.Lock()
		defer g.credMu.Unlock()
		g.users = make(map[string]*dashUser, len(f.Users))
		for i := range f.Users {
			u := f.Users[i]
			if u.Username == "" {
				return fmt.Errorf("parsing %s: entry %d has an empty username", g.usersFile, i)
			}
			if _, dup := g.users[u.Username]; dup {
				return fmt.Errorf("parsing %s: duplicate username %q", g.usersFile, u.Username)
			}
			u.Role = normalizeRole(u.Role)
			g.users[u.Username] = &u
		}
		g.oidcPrincipals = make(map[string]*dashPrincipal, len(f.OIDCPrincipals))
		for i := range f.OIDCPrincipals {
			p := f.OIDCPrincipals[i]
			if p.Subject == "" {
				return fmt.Errorf("parsing %s: oidc principal %d has an empty subject", g.usersFile, i)
			}
			if _, dup := g.oidcPrincipals[p.Subject]; dup {
				return fmt.Errorf("parsing %s: duplicate oidc principal %q", g.usersFile, p.Subject)
			}
			p.Role = normalizeRole(p.Role)
			g.oidcPrincipals[p.Subject] = &p
		}
		g.epochCounter = f.EpochCounter
		g.setupRequired = len(g.users) == 0
		if len(g.users) == 0 {
			return fmt.Errorf("no users configured in %s", g.usersFile)
		}
		return nil
	case errors.Is(err, fs.ErrNotExist):
		// Absent — the only case allowed to try the legacy migration.
	default:
		return fmt.Errorf("reading %s: %w", g.usersFile, err)
	}

	// No users.json — try migrating a legacy single-user auth.json.
	legacyData, err := os.ReadFile(g.legacyFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return errNoUsers
		}
		return fmt.Errorf("reading legacy %s: %w", g.legacyFile, err)
	}
	var creds legacyCredFile
	if err := json.Unmarshal(legacyData, &creds); err != nil {
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

// saveUsersLocked writes users.json atomically from the live maps. The caller
// must hold g.credMu (read or write). Only safe to call when the live maps are
// already the state that should be durable — see saveUsersFile for the
// copy-on-write path every mutating handler below actually uses.
func (g *authGate) saveUsersLocked() error {
	return saveUsersFile(g.usersFile, g.users, g.oidcPrincipals, g.epochCounter)
}

// saveUsersFile writes the given user and principal maps and epoch counter to
// path atomically. Free-standing (doesn't touch authGate state) so a mutation
// can persist a CANDIDATE map and only publish it into the live gate after
// the write succeeds — otherwise a failed rename/write left the in-memory
// mutation applied while the handler reported an error, so the running
// process and users.json silently diverged (DASH-005). For createUser
// specifically this also keeps g.setupRequired from flipping to false before
// the first account is durably saved (DASH-006).
func saveUsersFile(path string, users map[string]*dashUser, principals map[string]*dashPrincipal, epochCounter uint64) error {
	var f usersFileFormat
	f.EpochCounter = epochCounter
	for _, u := range users {
		f.Users = append(f.Users, *u)
	}
	sort.Slice(f.Users, func(i, j int) bool { return f.Users[i].Username < f.Users[j].Username })
	for _, p := range principals {
		f.OIDCPrincipals = append(f.OIDCPrincipals, *p)
	}
	sort.Slice(f.OIDCPrincipals, func(i, j int) bool { return f.OIDCPrincipals[i].Subject < f.OIDCPrincipals[j].Subject })
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// cloneUsersLocked returns an independent copy of the live user map (new map,
// new *dashUser pointers) so a mutation can be built and persisted without
// any live state changing until the write succeeds. Caller must hold
// g.credMu.
func cloneUsersLocked(users map[string]*dashUser) map[string]*dashUser {
	out := make(map[string]*dashUser, len(users))
	for k, v := range users {
		cp := *v
		out[k] = &cp
	}
	return out
}

// clonePrincipalsLocked is cloneUsersLocked for the OIDC principal map.
func clonePrincipalsLocked(principals map[string]*dashPrincipal) map[string]*dashPrincipal {
	out := make(map[string]*dashPrincipal, len(principals))
	for k, v := range principals {
		cp := *v
		out[k] = &cp
	}
	return out
}

// upsertOIDCPrincipal records an SSO sign-in against the principal store.
// First sign-in creates the row with the next store epoch; later ones refresh
// the display fields and role from the IdP (the IdP stays authoritative for
// role) while PRESERVING the epoch, so one device signing in does not retire
// another device's still-valid session. The epoch is captured and the
// candidate persisted within the same critical section — the session minted
// with the returned epoch can never predate the row it validates against
// (A02 for external identities).
func (g *authGate) upsertOIDCPrincipal(subject, username, email, role string) (epoch uint64, liveRole string, err error) {
	if subject == "" || username == "" {
		return 0, "", fmt.Errorf("principal subject and username are required")
	}
	g.credMu.Lock()
	defer g.credMu.Unlock()
	liveRole = normalizeRole(role)
	candidate := clonePrincipalsLocked(g.oidcPrincipals)
	if existing := candidate[subject]; existing != nil {
		existing.Username = username
		existing.Email = email
		existing.Role = liveRole
		existing.LastSignIn = time.Now().UTC().Format(time.RFC3339)
		if err := saveUsersFile(g.usersFile, g.users, candidate, g.epochCounter); err != nil {
			return 0, "", err
		}
		g.oidcPrincipals = candidate
		return existing.AuthEpoch, liveRole, nil
	}
	epoch = g.epochCounter + 1
	candidate[subject] = &dashPrincipal{
		Subject:    subject,
		Username:   username,
		Email:      email,
		Role:       liveRole,
		AuthEpoch:  epoch,
		LastSignIn: time.Now().UTC().Format(time.RFC3339),
	}
	if err := saveUsersFile(g.usersFile, g.users, candidate, epoch); err != nil {
		return 0, "", err
	}
	g.oidcPrincipals = candidate
	g.epochCounter = epoch
	return epoch, liveRole, nil
}

// revokePrincipalSessions bumps an external principal's epoch, retiring every
// live session for that identity on its next request. Re-signing in mints a
// fresh session against the new epoch; it cannot resurrect the old ones.
func (g *authGate) revokePrincipalSessions(subject string) error {
	g.credMu.Lock()
	defer g.credMu.Unlock()
	existing := g.oidcPrincipals[subject]
	if existing == nil {
		return fmt.Errorf("principal not found")
	}
	candidate := clonePrincipalsLocked(g.oidcPrincipals)
	epoch := g.epochCounter + 1
	candidate[subject].AuthEpoch = epoch
	if err := saveUsersFile(g.usersFile, g.users, candidate, epoch); err != nil {
		return err
	}
	g.oidcPrincipals = candidate
	g.epochCounter = epoch
	return nil
}

// revokeSessions durably revokes every session of a LOCAL account by bumping
// its epoch (the in-memory session wipe callers also perform is just
// cleanup). Refuses unknown users.
func (g *authGate) revokeSessions(username string) error {
	g.credMu.Lock()
	defer g.credMu.Unlock()
	if g.users[username] == nil {
		return fmt.Errorf("user not found")
	}
	candidate := cloneUsersLocked(g.users)
	epoch := g.epochCounter + 1
	candidate[username].AuthEpoch = epoch
	if err := saveUsersFile(g.usersFile, candidate, g.oidcPrincipals, epoch); err != nil {
		return err
	}
	g.users = candidate
	g.epochCounter = epoch
	return nil
}

// principalView is the API projection of an SSO identity — never a secret
// (principals hold no credentials).
type principalView struct {
	Subject    string `json:"subject"`
	Username   string `json:"username"`
	Email      string `json:"email,omitempty"`
	Role       string `json:"role"`
	LastSignIn string `json:"last_sign_in,omitempty"`
}

func (g *authGate) listSSOPrincipals() []principalView {
	g.credMu.RLock()
	defer g.credMu.RUnlock()
	out := make([]principalView, 0, len(g.oidcPrincipals))
	for _, p := range g.oidcPrincipals {
		out = append(out, principalView{Subject: p.Subject, Username: p.Username, Email: p.Email, Role: p.Role, LastSignIn: p.LastSignIn})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Subject < out[j].Subject })
	return out
}

// authenticate verifies username+password. It always performs a bcrypt compare
// (against a dummy hash for unknown users) so response time doesn't reveal
// which usernames exist. A blank username resolves to the configured
// environment user BEFORE the stored-account lookup, so a stored account
// shadowing the env name always wins (A01).
//
// There is deliberately NO environment-password fallback anymore (A02): the
// env credential is materialized into a stored account once at startup (see
// newAuthGate), so deleting that account removes the identity entirely
// instead of resurrecting a retired bootstrap password. The returned dashUser
// carries the account's AuthEpoch so login can embed it in the session (A03).
func (g *authGate) authenticate(username, password string) (*dashUser, bool) {
	envUser := g.user
	if envUser == "" {
		envUser = "admin"
	}
	if username == "" {
		username = envUser
	}

	g.credMu.RLock()
	u := g.users[username]
	g.credMu.RUnlock()

	if u != nil {
		if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) == nil {
			return &dashUser{Username: u.Username, Role: normalizeRole(u.Role), AuthEpoch: u.AuthEpoch}, true
		}
		return nil, false
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
	if err := validateUsername(username); err != nil {
		return err
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
	candidate := cloneUsersLocked(g.users)
	epoch := g.epochCounter + 1
	candidate[username] = &dashUser{Username: username, PasswordHash: string(hash), Role: normalizeRole(role), AuthEpoch: epoch}
	if err := saveUsersFile(g.usersFile, candidate, g.oidcPrincipals, epoch); err != nil {
		return err
	}
	g.epochCounter = epoch
	g.users = candidate
	g.setupRequired = false
	return nil
}

// setPassword replaces an EXISTING user's password. An unknown username is an
// error — the old behavior synthesized a missing account as an admin, which
// turned an ordinary admin reset against a typo'd name into accidental admin
// creation (A03). Env-bootstrap migration has its own method below.
func (g *authGate) setPassword(username, password string) error {
	return g.setPasswordCAS(username, 0, password, false)
}

// ErrStaleEpoch reports a compare-and-swap password change whose captured
// account epoch no longer matches — an administrative reset (or another
// self-change) landed between verification and commit (A03).
var ErrStaleEpoch = errors.New("account changed since the password was verified; try again")

// setPasswordCAS replaces an existing user's password, requiring the account's
// current AuthEpoch to equal expected when checkEpoch is set. The caller
// captures the epoch from the SAME authenticated read that verified the
// current password, so an interleaved reset fails the change instead of
// silently overwriting the newer credential.
func (g *authGate) setPasswordCAS(username string, expected uint64, password string, checkEpoch bool) error {
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
		return fmt.Errorf("user not found")
	}
	if checkEpoch && u.AuthEpoch != expected {
		return ErrStaleEpoch
	}
	candidate := cloneUsersLocked(g.users)
	epoch := g.epochCounter + 1
	candidate[username].PasswordHash = string(hash)
	candidate[username].AuthEpoch = epoch
	if err := saveUsersFile(g.usersFile, candidate, g.oidcPrincipals, epoch); err != nil {
		return err
	}
	g.epochCounter = epoch
	g.users = candidate
	return nil
}

// setPasswordMigratingEnv persists the environment-bootstrap admin's first
// password change as a real stored account. It refuses to create anything
// unless the named identity IS the canonical env user and the env credential
// is configured — the caller has already verified the current password through
// authenticate, which for a shadow-free name means the env password.
func (g *authGate) setPasswordMigratingEnv(username, password string) error {
	envUser := g.user
	if envUser == "" {
		envUser = "admin"
	}
	if g.pass == "" || username != envUser {
		return fmt.Errorf("user not found")
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
	if u := g.users[username]; u != nil {
		// A stored account appeared between authenticate and now — plain reset.
		candidate := cloneUsersLocked(g.users)
		epoch := g.epochCounter + 1
		candidate[username].PasswordHash = string(hash)
		candidate[username].AuthEpoch = epoch
		if err := saveUsersFile(g.usersFile, candidate, g.oidcPrincipals, epoch); err != nil {
			return err
		}
		g.epochCounter = epoch
		g.users = candidate
		return nil
	}
	candidate := cloneUsersLocked(g.users)
	epoch := g.epochCounter + 1
	candidate[username] = &dashUser{Username: username, PasswordHash: string(hash), Role: RoleAdmin, AuthEpoch: epoch}
	if err := saveUsersFile(g.usersFile, candidate, g.oidcPrincipals, epoch); err != nil {
		return err
	}
	g.epochCounter = epoch
	g.users = candidate
	g.setupRequired = false
	return nil
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
	candidate := cloneUsersLocked(g.users)
	epoch := g.epochCounter + 1
	candidate[username].Role = role
	candidate[username].AuthEpoch = epoch
	if err := saveUsersFile(g.usersFile, candidate, g.oidcPrincipals, epoch); err != nil {
		return err
	}
	g.epochCounter = epoch
	g.users = candidate
	return nil
}

// deleteUser removes an account, refusing to remove the last remaining admin.
// The epoch counter still advances so a later account created under the same
// name gets an epoch no earlier session could carry (A03).
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
	candidate := cloneUsersLocked(g.users)
	delete(candidate, username)
	epoch := g.epochCounter + 1
	if err := saveUsersFile(g.usersFile, candidate, g.oidcPrincipals, epoch); err != nil {
		return err
	}
	g.epochCounter = epoch
	g.users = candidate
	return nil
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
		if err := strictDecode(r, &body); err != nil {
			writeError(w, "invalid request body")
			return
		}
		// A07: a typo'd role must be rejected, not silently mapped to viewer.
		if body.Role != "" && !validRole(body.Role) {
			writeError(w, "role must be admin, editor, or viewer")
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
//	DELETE /api/users/{username}              remove the account
//	PUT    /api/users/{username}              change role  {"role": "editor"}
//	POST   /api/users/{username}/password     admin reset  {"password": "..."}
//	POST   /api/users/{username}/revoke-sessions  retire all live sessions
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
		if err := strictDecode(r, &body); err != nil {
			writeError(w, "invalid request body")
			return
		}
		// A07: reject unknown roles at the boundary (empty keeps the old
		// normalize-to-viewer behavior for hand-rolled callers).
		if body.Role != "" && !validRole(body.Role) {
			writeError(w, "role must be admin, editor, or viewer")
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
		if err := strictDecode(r, &body); err != nil {
			writeError(w, "invalid request body")
			return
		}
		if err := s.gate.setPassword(username, body.Password); err != nil {
			if strings.Contains(err.Error(), "user not found") {
				writeErrorStatus(w, err.Error(), http.StatusNotFound)
			} else {
				writeError(w, err.Error())
			}
			return
		}
		s.gate.deleteUserSessions(username)
		writeData(w, map[string]bool{"ok": true})

	case r.Method == http.MethodPost && sub == "revoke-sessions":
		// A02: durable revocation — the epoch bump retires every live
		// session for the account on its next request; the in-memory wipe
		// is immediate cleanup. The user simply signs in again.
		if err := s.gate.revokeSessions(username); err != nil {
			if strings.Contains(err.Error(), "user not found") {
				writeErrorStatus(w, err.Error(), http.StatusNotFound)
			} else {
				writeError(w, err.Error())
			}
			return
		}
		s.gate.deleteUserSessions(username)
		writeData(w, map[string]bool{"ok": true})

	default:
		writeError(w, "unsupported user action")
	}
}

// handleSSOPrincipals lists the external (SSO) identities known to this
// instance (GET, admin-only via adminOnlyPrefixes).
func (s *Server) handleSSOPrincipals(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if s.gate == nil {
		writeError(w, "authentication is disabled")
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeData(w, s.gate.listSSOPrincipals())
}

// handleSSORevoke durably revokes every live session for one external
// identity (POST, admin-only): the principal's epoch is bumped, so its
// sessions die on their next request and a re-sign-in mints a fresh session
// against the new epoch. The subject travels in the body because principal
// ids are opaque issuer-derived strings, not route-safe names.
func (s *Server) handleSSORevoke(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if s.gate == nil {
		writeError(w, "authentication is disabled")
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Subject string `json:"subject"`
	}
	if err := strictDecode(r, &body); err != nil {
		writeError(w, "invalid request body")
		return
	}
	if body.Subject == "" {
		writeError(w, "subject is required")
		return
	}
	if err := s.gate.revokePrincipalSessions(body.Subject); err != nil {
		writeErrorStatus(w, err.Error(), http.StatusNotFound)
		return
	}
	s.gate.deleteUserSessions(body.Subject)
	writeData(w, map[string]bool{"ok": true})
}
