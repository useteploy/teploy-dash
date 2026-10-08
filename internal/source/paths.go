package source

import (
	"encoding/json"
	"path"
	"strings"
)

// PushMatchesPaths mirrors the CLI's autodeploy.paths grammar. An incomplete
// file list deploys conservatively; only a complete authenticated push may skip
// a bound app. It does not relax signature, branch or immutable-SHA admission.
func PushMatchesPaths(body []byte, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	var p struct {
		Total   int `json:"total_commits_count"`
		Commits []struct {
			Added    []string `json:"added"`
			Modified []string `json:"modified"`
			Removed  []string `json:"removed"`
		} `json:"commits"`
	}
	// Match the existing consumer's conservative 20-commit truncation fence.
	if json.Unmarshal(body, &p) != nil || len(p.Commits) == 0 || len(p.Commits) >= 20 || p.Total > len(p.Commits) {
		return true
	}
	for _, commit := range p.Commits {
		for _, files := range [][]string{commit.Added, commit.Modified, commit.Removed} {
			for _, file := range files {
				for _, pattern := range patterns {
					pattern = strings.TrimSpace(pattern)
					if pattern == "**" {
						return true
					}
					if prefix, ok := strings.CutSuffix(pattern, "/**"); ok {
						if file == prefix || strings.HasPrefix(file, prefix+"/") {
							return true
						}
						continue
					}
					if ok, e := path.Match(pattern, file); e == nil && ok {
						return true
					}
				}
			}
		}
	}
	return false
}
