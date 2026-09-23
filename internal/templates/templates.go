// Package templates is dash's side of D05's "templates as reviewed versioned
// packages": the D05 manifest shape (name, version, architecture, required
// secrets, upgrade notes, backup scope, fixture check reference) validated
// and rendered here. Templates are CLI-owned artifacts — teploy-cli fetches
// the catalog (index.json from the community repo) and `teploy template
// list --json` re-encodes it — so dash NEVER edits, mints or back-fills
// manifest fields; it validates what the CLI returned and renders the
// truth, including "unversioned" for today's version-less catalog. A
// version field the catalog does not carry must never be invented here
// (the layer rule: a made-up version in dash is a desync against every
// other consumer of the same catalog).
package templates

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Version states for a rendered entry.
const (
	VersionStateVersioned   = "versioned"
	VersionStateUnversioned = "unversioned"
)

// versionPattern is deliberately narrower than "any string": reviewed
// versioned packages carry semver (MAJOR.MINOR.PATCH with optional
// prerelease). Anything else fails validation instead of rendering a
// version-ish label the upgrade comparison cannot honestly order.
var versionPattern = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?$`)

// Manifest is the D05 template manifest shape. Every field beyond the
// historical name/description/accessories/variables is optional: the
// current catalog carries none of them, and an absent field renders as an
// explicit gap (VersionState, the UI's "unversioned" badge), never as a
// default value that pretends a review happened.
type Manifest struct {
	Name            string   `json:"name"`
	Description     string   `json:"description,omitempty"`
	Version         string   `json:"version,omitempty"`
	Architecture    string   `json:"architecture,omitempty"`
	RequiredSecrets []string `json:"required_secrets,omitempty"`
	UpgradeNotes    string   `json:"upgrade_notes,omitempty"`
	BackupScope     string   `json:"backup_scope,omitempty"`
	FixtureCheck    string   `json:"fixture_check,omitempty"`
	Accessories     []string `json:"accessories,omitempty"`
	Variables       []string `json:"variables,omitempty"`
}

// Rendered is one catalog entry as dash serves it: the validated manifest
// plus the derived state the UI renders.
type Rendered struct {
	Manifest
	// VersionState is "versioned" or "unversioned" — the honest answer for
	// today's catalog is unversioned, and the UI must show it.
	VersionState string `json:"version_state"`
	// Installed is the newest succeeded template_install dash recorded for
	// this template (any server), nil when none exists.
	Installed *Installed `json:"installed,omitempty"`
	// Upgrade is set when an installed version exists, the catalog carries
	// versions, and the catalog version is newer than the installed one.
	Upgrade *Upgrade `json:"upgrade,omitempty"`
}

// Installed records what dash knows about a template's instantiation.
type Installed struct {
	Server      string `json:"server"`
	Version     string `json:"version,omitempty"` // empty = recorded before the catalog carried versions
	OperationID string `json:"operation_id"`
}

// Upgrade surfaces the reviewed package's own upgrade guidance when a
// newer template version exists (D05).
type Upgrade struct {
	From        string `json:"from"`
	To          string `json:"to"`
	Notes       string `json:"notes,omitempty"`
	BackupScope string `json:"backup_scope,omitempty"`
}

// DecodeCatalog validates a `teploy template list --json` payload and
// returns the rendered entries. Unknown ADDITIVE fields are tolerated (a
// newer CLI's catalog must not brick an older dash), but every known field
// is type- and value-checked, and any invalid entry fails the WHOLE
// listing: a catalog that cannot be validated is a dependency failure, not
// a shorter list — rendering the valid remainder would advertise a
// reviewed catalog that was not reviewed whole.
func DecodeCatalog(raw string) ([]Rendered, error) {
	var entries []Manifest
	dec := json.NewDecoder(strings.NewReader(raw))
	if err := dec.Decode(&entries); err != nil {
		return nil, fmt.Errorf("decoding template catalog: %w", err)
	}
	seen := make(map[string]bool, len(entries))
	rendered := make([]Rendered, 0, len(entries))
	for _, m := range entries {
		if err := validateManifest(m); err != nil {
			return nil, fmt.Errorf("template catalog entry %q invalid: %w", m.Name, err)
		}
		if seen[m.Name] {
			return nil, fmt.Errorf("template catalog contains duplicate entry %q", m.Name)
		}
		seen[m.Name] = true
		state := VersionStateUnversioned
		if m.Version != "" {
			state = VersionStateVersioned
		}
		rendered = append(rendered, Rendered{Manifest: m, VersionState: state})
	}
	return rendered, nil
}

func validateManifest(m Manifest) error {
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if m.Version != "" && !versionPattern.MatchString(m.Version) {
		return fmt.Errorf("version %q is not semver (MAJOR.MINOR.PATCH[+prerelease])", m.Version)
	}
	if m.RequiredSecrets != nil {
		declared := make(map[string]bool, len(m.Variables))
		for _, v := range m.Variables {
			declared[v] = true
		}
		for _, s := range m.RequiredSecrets {
			if !declared[s] {
				return fmt.Errorf("required secret %q is not a declared template variable — the install form could never collect it", s)
			}
		}
	}
	return nil
}

// Find returns the validated entry for name, or nil when the catalog does
// not carry it.
func Find(entries []Rendered, name string) *Rendered {
	for i := range entries {
		if entries[i].Name == name {
			return &entries[i]
		}
	}
	return nil
}

// CompareVersions orders two semver versions. Returns -1 when a < b, 0
// when equal, +1 when a > b. A version is "newer" only under this order —
// string comparison is how 0.10.0 silently reads older than 0.9.0.
func CompareVersions(a, b string) int {
	am, err := versionParts(a)
	if err != nil {
		return 0
	}
	bm, err := versionParts(b)
	if err != nil {
		return 0
	}
	for i := 0; i < 3; i++ {
		an, aerr := strconv.Atoi(am[i])
		bn, berr := strconv.Atoi(bm[i])
		if aerr != nil || berr != nil {
			return 0 // guarded by versionPattern; unreachable in practice
		}
		if an != bn {
			if an < bn {
				return -1
			}
			return 1
		}
	}
	// Equal core: a prerelease sorts BEFORE the release it belongs to
	// (1.0.0-rc1 < 1.0.0). Prerelease identifiers compare lexically — the
	// numeric-identifier rule of full semver is not implemented because no
	// catalog carries versions yet; documented limit, revisit with the
	// first versioned catalog.
	ap, bp := am[3], bm[3]
	if ap == bp {
		return 0
	}
	if ap == "" {
		return 1
	}
	if bp == "" {
		return -1
	}
	return strings.Compare(ap, bp)
}

func versionParts(v string) ([4]string, error) {
	var out [4]string
	m := versionPattern.FindStringSubmatch(v)
	if m == nil {
		return out, fmt.Errorf("not semver: %q", v)
	}
	copy(out[:], m[1:])
	return out, nil
}
