package source

import (
	"encoding/json"
	"regexp"
	"strings"
)

var (
	commitPattern   = regexp.MustCompile(`^[0-9a-f]{40}$|^[0-9a-f]{64}$`)
	zeroCommitValue = strings.Repeat("0", 40)
)

// NormalizeEvent maps the forge event headers onto one vocabulary:
// X-GitHub-Event (github/gitea/forgejo) is already lowercase;
// X-Gitlab-Event uses Title Case ("Push Hook"); everything lowercases,
// spaces become underscores, and the GitLab titles fold onto the shared
// names ("Push Hook" -> "push").
func NormalizeEvent(raw string) string {
	normalized := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(raw, " ", "_")))
	switch normalized {
	case "push_hook":
		normalized = "push"
	case "tag_push_hook":
		normalized = "tag_push"
	}
	if len(normalized) > 64 {
		return ""
	}
	return normalized
}

// ParsePush extracts the branch and the authenticated commit from a push
// payload (the payload pin). It mirrors teploy-cli's autodeploy.PushCommit
// contract (C02): GitLab's checkout_sha is preferred, else after; tag refs
// (whose "after" names the tag object), explicit deletions, the all-zero
// deletion marker, and malformed hashes pin NOTHING — "" means "no commit
// this delivery can pin", never "pin to garbage".
func ParsePush(body []byte) (branch, commit string) {
	var payload struct {
		Ref         string  `json:"ref"`
		After       *string `json:"after"`
		CheckoutSHA *string `json:"checkout_sha"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	if err := decoder.Decode(&payload); err != nil {
		return "", ""
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err == nil {
		return "", ""
	}
	if payload.Ref == "" {
		return "", ""
	}
	switch {
	case strings.HasPrefix(payload.Ref, "refs/heads/"):
		branch = strings.TrimPrefix(payload.Ref, "refs/heads/")
	case strings.HasPrefix(payload.Ref, "refs/tags/"):
		// A tag's after names the tag object, not a buildable commit —
		// record the tag as the branch, pin nothing.
		return strings.TrimPrefix(payload.Ref, "refs/tags/"), ""
	default:
		return payload.Ref, ""
	}
	candidate := ""
	if payload.CheckoutSHA != nil {
		candidate = *payload.CheckoutSHA
	} else if payload.After != nil {
		candidate = *payload.After
	}
	if !commitPattern.MatchString(candidate) || strings.Trim(candidate, "0") == "" {
		return branch, ""
	}
	return branch, candidate
}
