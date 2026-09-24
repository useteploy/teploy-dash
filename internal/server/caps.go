package server

// caps.go — X03 enforcement: the route table that annotates every API route
// class with the capability it requires, and the resolution of a principal
// row (role + capability profile) into an effective capability set.
//
// Enforcement lives at the middleware seam (authGate.wrap) with the
// principal from the existing auth — see the RBAC block there. The
// capability check REPLACES the old role-rank check; parity for existing
// installs is carried by the LEGACY profile (caps.LegacyForRole), which
// reproduces the old role gate exactly, so the swap is invisible to
// pre-X03 accounts until an operator narrows them deliberately.

import (
	"net/http"
	"strings"

	"github.com/useteploy/teploy-dash/internal/caps"
)

// Capability-profile names. The empty string is a pre-X03 account (legacy).
const (
	profileLegacy = "legacy"
	profilePreset = "preset"
	profileCustom = "custom"
)

// capabilitiesForProfile resolves one principal row's effective set:
//
//   - "preset":  accounts created after X03 — the role's preset, read live
//     so preset evolution applies to existing preset accounts;
//   - "custom":  an explicit per-account set (admin-granted grants, or any
//     hand-shaped role). May be empty (a deliberately locked account);
//   - "":        a pre-X03 account — the LEGACY profile: today's effective
//     permissions for the role, frozen. Never silently narrowed, never
//     silently widened; narrowing is the explicit settings act.
//
// An unknown profile value (written by a newer dash) degrades to legacy —
// the conservative reading that can only under-privilege relative to the
// new semantics, never over.
func capabilitiesForProfile(profile string, explicit []string, role string) caps.Set {
	switch profile {
	case profilePreset:
		return caps.PresetForRole(role)
	case profileCustom:
		return caps.NewSet(explicit...)
	default:
		return caps.LegacyForRole(role)
	}
}

// capBasisForProfile maps a stored capability profile onto the audit basis
// recorded on operation actors (D06): how the granted-at set was derived.
// The empty (pre-X03) and unknown profiles read as legacy, mirroring
// capabilitiesForProfile's conservative default.
func capBasisForProfile(profile string) string {
	switch profile {
	case profilePreset:
		return profilePreset
	case profileCustom:
		return profileCustom
	default:
		return profileLegacy
	}
}

// administerCredentialsPrefixes manage credential-bearing configuration.
// Reads are included: their payloads carry secrets (registry passwords,
// SMTP/webhook targets, token metadata, source webhook secrets).
var administerCredentialsPrefixes = []string{
	"/api/mcp-tokens",
	"/api/config/servers",
	"/api/registries",
	"/api/notifications",
	"/api/sources",
}

// requiredCapabilities returns the capability(s) a route requires. It fails
// closed exactly like the role gate it replaced: an unclassified mutating
// route requires execute.mutate (operator-and-up), an unclassified read is
// metadata-visible; reveal.secrets is only ever attached explicitly to the
// value-returning endpoints.
func requiredCapabilities(method, path string) []string {
	if !strings.HasPrefix(path, "/api/") {
		// The SPA shell and pre-auth pages carry no capability requirement;
		// every piece of data they render comes from the API routes below.
		return nil
	}
	// Self-service identity is available to every authenticated principal —
	// gating "who am I" on a capability would brick sessions.
	if path == "/api/auth/me" || path == "/api/auth/password" {
		return nil
	}
	for _, p := range administerCredentialsPrefixes {
		if path == p || strings.HasPrefix(path, p+"/") {
			return []string{caps.AdministerCredentials}
		}
	}
	if path == "/api/users" || strings.HasPrefix(path, "/api/users/") ||
		path == "/api/sso" || strings.HasPrefix(path, "/api/sso/") {
		return []string{caps.AdministerUsers}
	}

	// App-scoped routes classify on the ACTION segment — that is where the
	// metadata/value split lives.
	if path == "/api/apps" {
		return []string{caps.ViewMetadata}
	}
	if strings.HasPrefix(path, "/api/apps/") {
		return appActionCapabilities(method, path)
	}

	if path == "/api/deploy" || path == "/api/templates/install" {
		return []string{caps.ExecuteDeploy}
	}
	if path == "/api/operations" || strings.HasPrefix(path, "/api/operations/") {
		// Listing and event replay are metadata; enqueue/cancel/retry are
		// deploy-flow control.
		if method == http.MethodGet {
			return []string{caps.ViewMetadata}
		}
		return []string{caps.ExecuteDeploy}
	}
	if path == "/api/restore-tests" || strings.HasPrefix(path, "/api/restore-tests/") {
		// Definitions and results are metadata; creating, editing, and
		// RUNNING a verification (which restores backup data on a target)
		// is restore.data.
		if method == http.MethodGet {
			return []string{caps.ViewMetadata}
		}
		return []string{caps.RestoreData}
	}
	if strings.HasPrefix(path, "/api/logs/") {
		return []string{caps.ViewLogs}
	}
	// Onboarding preflight is a read probe; the POST form exists so
	// CORS-safe clients can send a body.
	if path == "/api/onboarding/preflight" || path == "/api/onboarding/entry" {
		return []string{caps.ViewMetadata}
	}

	if isMutating(method) {
		return []string{caps.ExecuteMutate}
	}
	return []string{caps.ViewMetadata}
}

// appActionCapabilities classifies /api/apps/{server}/{app}/{action...}.
func appActionCapabilities(method, path string) []string {
	rest := strings.Trim(strings.TrimPrefix(path, "/api/apps/"), "/")
	parts := strings.Split(rest, "/")
	action := ""
	if len(parts) > 2 {
		action = strings.Join(parts[2:], "/")
	}

	if method == http.MethodGet {
		switch action {
		case "env":
			return []string{caps.RevealSecrets}
		case "env/keys":
			return []string{caps.ViewMetadata}
		case "kv":
			return []string{caps.ViewMetadata}
		case "kv/value":
			return []string{caps.RevealSecrets}
		case "log":
			return []string{caps.ViewLogs}
		default:
			// status, drift, stats, health, accessories, and the bare app
			// record are metadata.
			return []string{caps.ViewMetadata}
		}
	}

	if method == http.MethodPost {
		switch {
		case action == "env", action == "kv":
			return []string{caps.ExecuteMutate}
		case action == "", action == "stop", action == "start", action == "restart",
			action == "rollback", action == "remove", action == "lock", action == "unlock",
			action == "maintenance/on", action == "maintenance/off":
			// Deploy-state changes: lifecycle, rollback, remove, lock,
			// maintenance (the handleAppPost verbs).
			return []string{caps.ExecuteDeploy}
		case strings.HasPrefix(action, "accessories/"):
			sub := strings.Split(strings.TrimPrefix(action, "accessories/"), "/")
			if len(sub) == 2 && sub[1] == "logs" {
				return []string{caps.ViewLogs}
			}
			return []string{caps.ExecuteDeploy}
		default:
			return []string{caps.ExecuteMutate}
		}
	}

	// DELETE (env/{key}, kv) and anything unclassified: fail closed.
	return []string{caps.ExecuteMutate}
}
