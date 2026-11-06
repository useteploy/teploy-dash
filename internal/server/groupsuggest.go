package server

// groupsuggest.go — Programme B item 7 (plan section 10.7): group
// suggestions. Apps that visibly belong together — they share a registrable
// domain — are offered as a DIFF the owner accepts ONE suggestion at a time
// through the existing groups API (POST /api/groups, then POST
// /api/groups/{name}/apps with the server binding). Dash never writes a
// group here; it only proposes.
//
// Rules (binding, from the plan):
//   - the grouping key is the registrable domain of the union of an app's
//     domains (golang.org/x/net/publicsuffix, vendoring the PSL through the
//     module); apps with no usable domain fall back to the first hyphen
//     token of their name; an app with neither gets no suggestion. An app
//     serving several registrable domains evidences each — accepting one
//     suggestion groups it and it drops from the rest on the next read;
//   - an app already in ANY group (bare or server-scoped) is never suggested
//     — never move an already-grouped app;
//   - singletons are dropped: a suggestion always names ≥ 2 apps;
//   - a suggestion may ADD to an existing group (the diff against today's
//     groups.json); it still never touches a grouped app.

import (
	"net"
	"net/http"
	"sort"
	"strings"

	"golang.org/x/net/publicsuffix"
)

type groupSuggestionApp struct {
	App    string `json:"app"`
	Server string `json:"server"`
	Domain string `json:"domain,omitempty"`
}

type groupSuggestion struct {
	Name   string               `json:"name"`
	Exists bool                 `json:"exists"` // the group already exists; this adds to it
	Basis  string               `json:"basis"`  // "domain" | "name"
	Apps   []groupSuggestionApp `json:"apps"`
}

// appGrouped reports whether the app name is already in any group (either
// list — a bare name or a server-scoped ref both count: membership is
// membership, and a suggestion must never move or duplicate it).
func appGrouped(groups []groupEntry, app string) bool {
	for _, g := range groups {
		for _, a := range g.Apps {
			if a == app {
				return true
			}
		}
		for _, r := range g.ServerApps {
			if r.App == app {
				return true
			}
		}
		for _, p := range g.Projects {
			for _, a := range p.Apps {
				if a == app {
					return true
				}
			}
			for _, r := range p.ServerApps {
				if r.App == app {
					return true
				}
			}
		}
	}
	return false
}

// registrableDomainOf resolves a host to its registrable domain (eTLD+1).
// Empty when the host has none (an IP literal, a placeholder, an unknown or
// non-public suffix) — those are not grouping evidence.
func registrableDomainOf(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" || isPlaceholderDomain(host) {
		return ""
	}
	if ip := strings.Trim(host, "[]"); strings.Contains(ip, ":") || !strings.ContainsAny(host, ".") {
		return "" // v6 literal / single label
	}
	if net.ParseIP(strings.Trim(host, "[]")) != nil {
		return "" // an address is not a name (the PSL would happily answer e.g. "2.10")
	}
	reg, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil || reg == "" {
		return ""
	}
	return reg
}

// appDomainRegistrables returns the sorted distinct registrable domains of
// an app's domain list (comma-separated, as the fleet reports it).
func appDomainRegistrables(domainField string) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range strings.Split(domainField, ",") {
		if reg := registrableDomainOf(d); reg != "" && !seen[reg] {
			seen[reg] = true
			out = append(out, reg)
		}
	}
	sort.Strings(out)
	return out
}

// appHyphenToken is the fallback key: the first hyphen token of the app
// name ("fylun-web" → "fylun"). Empty when the name has no hyphen.
func appHyphenToken(app string) string {
	if i := strings.Index(app, "-"); i > 0 {
		return app[:i]
	}
	return ""
}

// groupSuggestions is the pure core: fleet apps + today's groups → the diff.
// Deterministic: keys sorted, apps sorted within a key.
func groupSuggestions(apps []fleetAppLite, groups []groupEntry) []groupSuggestion {
	groupedNames := map[string]bool{}
	for _, g := range groups {
		for _, a := range g.Apps {
			groupedNames[a] = true
		}
		for _, r := range g.ServerApps {
			groupedNames[r.App] = true
		}
		for _, p := range g.Projects {
			for _, a := range p.Apps {
				groupedNames[a] = true
			}
			for _, r := range p.ServerApps {
				groupedNames[r.App] = true
			}
		}
	}
	existing := map[string]bool{}
	for _, g := range groups {
		existing[g.Name] = true
	}

	type bucket struct {
		basis string
		apps  []groupSuggestionApp
	}
	byKey := map[string]*bucket{}
	for _, a := range apps {
		if groupedNames[a.App] {
			continue // never move an already-grouped app
		}
		regs := appDomainRegistrables(a.Domain)
		for _, key := range regs {
			// An app evidences every registrable domain it serves; accepting
			// one suggestion groups it and it drops from the rest on the
			// next read (the diff is advisory until accepted).
			b := byKey[key]
			if b == nil {
				b = &bucket{basis: "domain"}
				byKey[key] = b
			}
			b.apps = append(b.apps, groupSuggestionApp{App: a.App, Server: a.Server, Domain: key})
		}
		if len(regs) > 0 {
			continue
		}
		// No usable domain: the first hyphen token of the name is the key.
		if tok := appHyphenToken(a.App); tok != "" && groupNameRE.MatchString(tok) {
			b := byKey[tok]
			if b == nil {
				b = &bucket{basis: "name"}
				byKey[tok] = b
			}
			b.apps = append(b.apps, groupSuggestionApp{App: a.App, Server: a.Server})
		}
	}

	out := make([]groupSuggestion, 0, len(byKey))
	for key, b := range byKey {
		// The suggested name must be creatable through the groups API.
		if _, err := checkedGroupName(key); err != nil {
			continue
		}
		// Singletons are dropped: a suggestion always names at least two.
		if len(b.apps) < 2 {
			continue
		}
		sort.Slice(b.apps, func(i, j int) bool {
			if b.apps[i].App != b.apps[j].App {
				return b.apps[i].App < b.apps[j].App
			}
			return b.apps[i].Server < b.apps[j].Server
		})
		out = append(out, groupSuggestion{Name: key, Exists: existing[key], Basis: b.basis, Apps: b.apps})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// fleetAppLite is the app shape the suggestion core needs.
type fleetAppLite struct {
	App    string
	Server string
	Domain string
}

// handleGroupSuggestions serves GET /api/group-suggestions: the diff, read
// from the warm fleet cache (never triggers a sweep) and today's groups.
func (s *Server) handleGroupSuggestions(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	groups, err := loadGroupsFile()
	if err != nil {
		writeError(w, "the groups file could not be read: "+err.Error())
		return
	}
	fleet := s.fleet.snapshot()
	apps := make([]fleetAppLite, 0, len(fleet))
	for _, a := range fleet {
		apps = append(apps, fleetAppLite{App: a.App, Server: a.Server, Domain: a.Domain})
	}
	writeData(w, map[string]any{"suggestions": groupSuggestions(apps, groups.Groups)})
}
