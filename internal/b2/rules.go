package b2

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// MaxRules is Backblaze's limit per bucket.
const MaxRules = 100

// DefaultHideDays is how long a hidden backup stays recoverable.
const DefaultHideDays = 14

const incomingSuffix = "/.incoming/"

// Rule is one lifecycle rule. Rules Dash does not own go back byte for byte, so a field
// this code has never heard of survives a merge.
type Rule struct {
	FileNamePrefix        string `json:"fileNamePrefix"`
	HideToDelete          *int   `json:"daysFromHidingToDeleting"`
	UploadToHide          *int   `json:"daysFromUploadingToHiding"`
	CancelUnfinishedLarge *int   `json:"daysFromStartingToCancelingUnfinishedLargeFiles,omitempty"`
	// Foreign is true when the rule carries a field the native API does not document:
	// the sign of a rule managed some other way.
	Foreign bool `json:"-"`
	raw     json.RawMessage
}

// UnmarshalJSON keeps the original bytes and notes any undocumented field.
func (r *Rule) UnmarshalJSON(data []byte) error {
	type plain Rule
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	*r = Rule(p)
	r.raw = append(json.RawMessage(nil), data...)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for name := range fields {
		switch name {
		case "fileNamePrefix", "daysFromHidingToDeleting", "daysFromUploadingToHiding", "daysFromStartingToCancelingUnfinishedLargeFiles":
		default:
			r.Foreign = true
		}
	}
	return nil
}

// MarshalJSON sends an untouched rule back as it came.
func (r Rule) MarshalJSON() ([]byte, error) {
	if len(r.raw) > 0 {
		return r.raw, nil
	}
	type plain Rule
	return json.Marshal(plain(r))
}

// Rules is a bucket's list. Backblaze's schema calls the field an object and its
// examples use an array, so both read.
type Rules []Rule

// UnmarshalJSON accepts an array, null or a single rule.
func (rs *Rules) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var one Rule
		if err := json.Unmarshal(trimmed, &one); err != nil {
			return err
		}
		*rs = Rules{one}
		return nil
	}
	var many []Rule
	if err := json.Unmarshal(trimmed, &many); err != nil {
		return err
	}
	*rs = many
	return nil
}

func intp(v int) *int { return &v }

func set(v *int) bool { return v != nil && *v > 0 }

// BasePrefix turns a base into the prefix rules use: "teploy/", or "" for no base.
func BasePrefix(base string) string {
	if base == "" {
		return ""
	}
	return base + "/"
}

// IncomingPrefix is a server's pending-upload tree, from its full prefix.
func IncomingPrefix(serverPrefix string) string { return serverPrefix + incomingSuffix }

func overlaps(a, b string) bool { return strings.HasPrefix(a, b) || strings.HasPrefix(b, a) }

func isIncoming(prefix string) bool {
	return strings.HasSuffix(prefix, incomingSuffix) || prefix == strings.TrimPrefix(incomingSuffix, "/")
}

// Analysis is what the rules mean for backups under a base.
type Analysis struct {
	// Refusals: a rule would hide live backups, so setup must stop.
	Refusals []string `json:"refusals"`
	// Warnings: a person should read these and decide.
	Warnings []string `json:"warnings"`
	// S3Managed: a rule looks like it is managed through the S3 API, so Dash must not
	// send lifecycleRules. Backblaze gives no sure sign, so this is a best effort.
	S3Managed bool `json:"s3_managed"`
}

// Analyze checks a bucket's rules against a base and the recovery window wanted. An
// empty base means the bucket root, where every rule counts.
func Analyze(rules Rules, base string, windowDays int) Analysis {
	out := Analysis{Refusals: []string{}, Warnings: []string{}}
	basePrefix := BasePrefix(base)
	for _, r := range rules {
		if r.Foreign {
			out.S3Managed = true
		}
		if !overlaps(r.FileNamePrefix, basePrefix) || isIncoming(r.FileNamePrefix) {
			continue
		}
		name := r.FileNamePrefix
		if name == "" {
			name = "the whole bucket"
		}
		if set(r.UploadToHide) {
			out.Refusals = append(out.Refusals, fmt.Sprintf("A lifecycle rule on %s hides files %d days after upload, which would make live backups disappear. Remove that rule or choose another bucket.", name, *r.UploadToHide))
			continue
		}
		if set(r.HideToDelete) && *r.HideToDelete < windowDays {
			if *r.HideToDelete == 1 && r.FileNamePrefix == "" {
				out.Warnings = append(out.Warnings, fmt.Sprintf("This bucket keeps only the last version of each file, so a backup hidden by retention or by a compromised server is gone after a day instead of %d.", windowDays))
			} else {
				out.Warnings = append(out.Warnings, fmt.Sprintf("A lifecycle rule on %s deletes hidden files after %d days, which shortens the recovery window from %d days.", name, *r.HideToDelete, windowDays))
			}
		}
	}
	return out
}

// Merge returns the bucket's rules with the base rule and each server's .incoming/ rule
// in place, matched by prefix since rule ids change. Rules it does not own are kept
// untouched. An existing base rule's window stays unless windowExplicit says a person
// chose one. changed is false when nothing needs sending.
func Merge(existing Rules, base string, serverPrefixes []string, windowDays int, windowExplicit bool) (Rules, bool, error) {
	out := make(Rules, len(existing))
	copy(out, existing)
	changed := false
	find := func(prefix string) int {
		for i := range out {
			if out[i].FileNamePrefix == prefix {
				return i
			}
		}
		return -1
	}
	sorted := append([]string(nil), serverPrefixes...)
	sort.Strings(sorted)
	// With no base, each server gets its own window rule. A rule with an empty prefix
	// would act on the whole bucket, and Dash never sends one.
	windowPrefixes := []string{BasePrefix(base)}
	if base == "" {
		windowPrefixes = windowPrefixes[:0]
		for _, sp := range sorted {
			windowPrefixes = append(windowPrefixes, sp+"/")
		}
	}
	for _, prefix := range windowPrefixes {
		if i := find(prefix); i < 0 {
			out = append(out, Rule{FileNamePrefix: prefix, HideToDelete: intp(windowDays), CancelUnfinishedLarge: intp(7)})
			changed = true
		} else {
			r := out[i]
			update := false
			if !set(r.HideToDelete) || (windowExplicit && *r.HideToDelete != windowDays) {
				r.HideToDelete = intp(windowDays)
				update = true
			}
			if !set(r.CancelUnfinishedLarge) {
				r.CancelUnfinishedLarge = intp(7)
				update = true
			}
			if update {
				r.raw = nil
				out[i] = r
				changed = true
			}
		}
	}
	for _, sp := range sorted {
		prefix := IncomingPrefix(sp)
		if i := find(prefix); i < 0 {
			out = append(out, Rule{FileNamePrefix: prefix, HideToDelete: intp(1), UploadToHide: intp(1)})
			changed = true
		} else if !set(out[i].UploadToHide) || !set(out[i].HideToDelete) || *out[i].UploadToHide != 1 || *out[i].HideToDelete != 1 {
			r := out[i]
			r.UploadToHide, r.HideToDelete, r.raw = intp(1), intp(1), nil
			out[i] = r
			changed = true
		}
	}
	if changed && len(out) > MaxRules {
		return nil, false, fmt.Errorf("this bucket would need %d lifecycle rules and Backblaze allows %d. Choose another bucket", len(out), MaxRules)
	}
	return out, changed, nil
}

// WithoutPrefixes drops the rules with these exact prefixes.
func WithoutPrefixes(rules Rules, prefixes ...string) (Rules, bool) {
	out := make(Rules, 0, len(rules))
	removed := false
	for _, r := range rules {
		drop := false
		for _, p := range prefixes {
			drop = drop || r.FileNamePrefix == p
		}
		if drop {
			removed = true
			continue
		}
		out = append(out, r)
	}
	return out, removed
}

// EffectiveWindow is how long hidden copies really last for this base: an existing rule
// that Merge kept wins over the window asked for. With no base it is the shortest of
// the servers' rules.
func EffectiveWindow(rules Rules, base string, serverPrefixes []string, asked int) int {
	wanted := []string{BasePrefix(base)}
	if base == "" {
		wanted = wanted[:0]
		for _, sp := range serverPrefixes {
			wanted = append(wanted, sp+"/")
		}
	}
	found := 0
	for _, r := range rules {
		for _, w := range wanted {
			if r.FileNamePrefix == w && set(r.HideToDelete) && (found == 0 || *r.HideToDelete < found) {
				found = *r.HideToDelete
			}
		}
	}
	if found == 0 {
		return asked
	}
	return found
}

// RulesText is the rules in words, for a bucket whose rules a person keeps themselves.
func RulesText(base string, serverPrefixes []string, windowDays int) []string {
	sorted := append([]string(nil), serverPrefixes...)
	sort.Strings(sorted)
	names := []string{BasePrefix(base)}
	if base == "" {
		names = names[:0]
		for _, sp := range sorted {
			names = append(names, sp+"/")
		}
	}
	out := []string{}
	for _, name := range names {
		out = append(out, fmt.Sprintf("On %s: delete hidden files after %d days, cancel unfinished large files after 7 days, and do not hide on upload.", name, windowDays))
	}
	for _, sp := range sorted {
		out = append(out, fmt.Sprintf("On %s: hide files 1 day after upload and delete hidden files 1 day later.", IncomingPrefix(sp)))
	}
	return out
}
