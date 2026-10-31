package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"time"

	"github.com/useteploy/teploy-dash/internal/durable"
)

// Sessions are 30-day idle-sliding and persisted beside users.json so a
// restart or deploy does not sign anyone out. The file is keyed by SHA-256 of
// the cookie token — a leaked sessions.json yields no usable cookies. Only
// identity + epoch are stored: role and capabilities are rebuilt live from
// the principal row on every request, and the epoch check still revokes a
// persisted session the moment its principal's AuthEpoch moves.
const (
	sessionTTL = 30 * 24 * time.Hour
	// sessionRenewAfter: a request renews the session only once its expiry
	// is closer than sessionTTL-sessionRenewAfter, so an active session is
	// rewritten (and its cookie re-set) at most about once a day.
	sessionRenewAfter = 24 * time.Hour
)

type persistedSession struct {
	Sub   string    `json:"sub"`
	User  string    `json:"user"`
	Role  string    `json:"role"`
	Exp   time.Time `json:"exp"`
	Epoch uint64    `json:"epoch"`
	Local bool      `json:"local"`
}

type sessionsFileFormat struct {
	Sessions map[string]persistedSession `json:"sessions"`
}

func hashSessionToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// loadSessions reads the persisted sessions, dropping expired ones. A missing
// file is a clean start; an unreadable or corrupt one is logged and ignored —
// losing sessions only means users sign in again.
func (g *authGate) loadSessions() {
	if g.sessionsFile == "" {
		return
	}
	data, err := os.ReadFile(g.sessionsFile)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("auth: sessions file unreadable, starting empty: %v", err)
		}
		return
	}
	var f sessionsFileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		log.Printf("auth: sessions file corrupt, starting empty: %v", err)
		return
	}
	now := time.Now()
	g.sessMu.Lock()
	defer g.sessMu.Unlock()
	for k, p := range f.Sessions {
		if len(k) != sha256.Size*2 || !now.Before(p.Exp) {
			continue
		}
		g.sessions[k] = &sessionInfo{sub: p.Sub, user: p.User, role: normalizeRole(p.Role), exp: p.Exp, epoch: p.Epoch, local: p.Local}
	}
}

// saveSessionsLocked persists the live session map. Caller holds g.sessMu.
// A write failure is logged, not surfaced: the in-memory session still works
// and the next write retries the whole file.
func (g *authGate) saveSessionsLocked() {
	if g.sessionsFile == "" {
		return
	}
	f := sessionsFileFormat{Sessions: make(map[string]persistedSession, len(g.sessions))}
	now := time.Now()
	for k, si := range g.sessions {
		if !now.Before(si.exp) {
			continue
		}
		f.Sessions[k] = persistedSession{Sub: si.sub, User: si.user, Role: si.role, Exp: si.exp, Epoch: si.epoch, Local: si.local}
	}
	data, err := json.Marshal(f)
	if err != nil {
		log.Printf("auth: encode sessions: %v", err)
		return
	}
	if err := durable.Replace(g.sessionsFile, data, 0600); err != nil {
		log.Printf("auth: persist sessions: %v", err)
	}
}

// renewSession slides a live session's expiry to now+sessionTTL when less than
// sessionTTL-sessionRenewAfter remains, persisting the change. Reports whether
// it renewed (the caller then re-sets the cookie). Call only after the
// session has passed epoch revalidation.
func (g *authGate) renewSession(token string) bool {
	key := hashSessionToken(token)
	g.sessMu.Lock()
	defer g.sessMu.Unlock()
	si, ok := g.sessions[key]
	if !ok {
		return false
	}
	now := time.Now()
	if !now.Before(si.exp) || si.exp.Sub(now) >= sessionTTL-sessionRenewAfter {
		return false
	}
	si.exp = now.Add(sessionTTL)
	g.saveSessionsLocked()
	return true
}
