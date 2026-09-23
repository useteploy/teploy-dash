// Package caps is the X03 capability registry: stable, resource-scoped
// tokens that roles map onto. It is shared by the dashboard's session gate
// (internal/server) and the MCP token surface (internal/mcp) so the two
// principal kinds enforce the same vocabulary.
//
// Tokens are STABLE identifiers — never rename or reuse one. Adding a token
// is additive; removing one requires a migration.
package caps

import "sort"

// The capability tokens.
const (
	// ViewMetadata reads non-secret state: app lists, statuses, drift,
	// stats, health, env/KV KEY listings, operations, monitors.
	ViewMetadata = "view.metadata"
	// RevealSecrets reads secret VALUES (env variable contents, KV values).
	// The one deliberately-not-viewer read.
	RevealSecrets = "reveal.secrets"
	// ExecuteDeploy changes deploy state: deploys, rollbacks, container
	// lifecycle, maintenance, removes, lock/unlock, template installs,
	// operation cancel/retry.
	ExecuteDeploy = "execute.deploy"
	// ExecuteMutate writes configuration: env/KV writes and dashboard-side
	// config mutations (monitors, groups, homepage, manifests).
	ExecuteMutate = "execute.mutate"
	// RestoreData runs restore verifications and cutovers — destructive
	// operations against backup data.
	RestoreData = "restore.data"
	// AdministerCredentials manages credential-bearing configuration:
	// MCP tokens, server registry config, container registries,
	// notification channels.
	AdministerCredentials = "administer.credentials"
	// AdministerUsers manages accounts and SSO principals.
	AdministerUsers = "administer.users"
	// ViewLogs reads container/service logs and replay (X03: "logs and
	// replay... require deliberate access").
	ViewLogs = "view.logs"
)

var all = []string{
	ViewMetadata,
	RevealSecrets,
	ExecuteDeploy,
	ExecuteMutate,
	RestoreData,
	AdministerCredentials,
	AdministerUsers,
	ViewLogs,
}

// All returns every capability token, sorted. The sorted order is the
// canonical display order everywhere (settings, listings, 403 messages).
func All() []string {
	out := append([]string(nil), all...)
	sort.Strings(out)
	return out
}

// Valid reports whether c names a known capability token.
func Valid(c string) bool {
	for _, t := range all {
		if t == c {
			return true
		}
	}
	return false
}

// Validate rejects any unknown token in list (fail closed at write
// boundaries — a typo must not silently become a missing permission).
func Validate(list []string) error {
	for _, c := range list {
		if !Valid(c) {
			return &UnknownCapabilityError{Capability: c}
		}
	}
	return nil
}

// UnknownCapabilityError names the rejected token.
type UnknownCapabilityError struct{ Capability string }

func (e *UnknownCapabilityError) Error() string {
	return "unknown capability " + e.Capability
}

// Normalize dedupes, drops unknown tokens, and sorts. Used when LOADING
// stores: a token removed by a future dash degrades the set instead of
// bricking the account.
func Normalize(list []string) []string {
	seen := make(map[string]bool, len(list))
	out := make([]string, 0, len(list))
	for _, c := range list {
		if !Valid(c) || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// Set is one principal's effective capabilities.
type Set map[string]bool

// NewSet builds a Set, dropping unknown tokens (loaded stores are
// normalized already; this is defense in depth).
func NewSet(list ...string) Set {
	s := make(Set, len(list))
	for _, c := range list {
		if Valid(c) {
			s[c] = true
		}
	}
	return s
}

// Allow reports whether the set grants one capability.
func (s Set) Allow(c string) bool { return s[c] }

// Missing returns every required capability the set lacks, in the order
// given — the first element names the capability a 403 should report.
func (s Set) Missing(required []string) []string {
	var missing []string
	for _, c := range required {
		if !s[c] {
			missing = append(missing, c)
		}
	}
	return missing
}

// Sorted returns the set's tokens in canonical order.
func (s Set) Sorted() []string {
	out := make([]string, 0, len(s))
	for c := range s {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// PresetForRole returns the capabilities a role grants accounts created
// after X03. "editor" and "operator" name the same preset (the storage
// string is "editor", canonical across teploy-observe; the programme text
// says "operator"). Unknown roles get the viewer preset — least privilege.
func PresetForRole(role string) Set {
	switch role {
	case "admin":
		return NewSet(all...)
	case "editor", "operator":
		return OperatorDefault()
	default:
		return ViewerDefault()
	}
}

// LegacyForRole records the effective permissions each role had BEFORE X03
// — the profile pre-existing accounts carry until an operator narrows them:
//
//   - viewer read everything GET endpoints returned, including env/KV
//     VALUES and logs (the documented viewer contract at the time);
//   - editor additionally performed every non-admin mutation, including
//     restore-test runs;
//   - admin held the admin-only prefixes too (users, SSO, MCP tokens,
//     server config, registries, notifications).
//
// These sets are FROZEN — they describe history, not intent. Never widen
// them; never silently apply them to new accounts.
func LegacyForRole(role string) Set {
	switch role {
	case "admin":
		return NewSet(all...)
	case "editor", "operator":
		return NewSet(ViewMetadata, RevealSecrets, ViewLogs, ExecuteDeploy, ExecuteMutate, RestoreData)
	default:
		return NewSet(ViewMetadata, RevealSecrets, ViewLogs)
	}
}

// OperatorDefault is the default capability set minted for MCP tokens:
// the operator preset — metadata reads, deploy and mutate actions, logs —
// and deliberately NO secret revelation (operator-minus-secrets; no MCP
// tool returns secret values), no restore, no administration.
func OperatorDefault() Set {
	return NewSet(ViewMetadata, ExecuteDeploy, ExecuteMutate, ViewLogs)
}

// ViewerDefault is the minimal MCP mint: metadata reads only.
func ViewerDefault() Set {
	return NewSet(ViewMetadata)
}
