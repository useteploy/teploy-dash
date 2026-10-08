package server

import (
	"github.com/useteploy/teploy-dash/internal/caps"
	"net/http"
)

// streamAuthorized re-reads the live principal rather than the admitted
// request snapshot. Revocation, expiry and custom grants apply to open SSE.
func (s *Server) streamAuthorized(r *http.Request) bool {
	if s.gate == nil {
		return true
	}
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	session, ok := s.gate.lookupSession(cookie.Value)
	if !ok {
		return false
	}
	s.gate.credMu.RLock()
	defer s.gate.credMu.RUnlock()
	if session.local {
		u := s.gate.users[session.sub]
		return u != nil && u.AuthEpoch == session.epoch && capabilitiesForProfile(u.CapabilityProfile, u.Capabilities, u.Role).Allow(caps.ViewLogs)
	}
	p := s.gate.oidcPrincipals[session.sub]
	return p != nil && p.AuthEpoch == session.epoch && capabilitiesForProfile(p.CapabilityProfile, p.Capabilities, p.Role).Allow(caps.ViewLogs)
}
