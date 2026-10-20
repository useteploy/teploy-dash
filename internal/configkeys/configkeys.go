// Package configkeys reads the environment keys an app's teploy.yml declares,
// so env edits that would compete with the app's settings can be refused, and
// the git: source block a Teploy-home config declares.
package configkeys

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Bounds on the YAML document walked: depth counts every nesting step
// (mapping, sequence element, alias hop), nodes the total visited.
const (
	maxDepth = 64
	maxNodes = 10000
	maxBytes = 1 << 20
)

// EnvKeys follows the CLI's two flat string maps, including YAML aliases and
// merge precedence. Invalid content is an error, never "no keys".
func EnvKeys(content []byte) ([]string, error) {
	root, err := decodeMapping(content)
	if err != nil {
		return nil, err
	}
	var value struct {
		Env     map[string]string `yaml:"env"`
		Literal map[string]string `yaml:"env_literal"`
	}
	if err := root.Decode(&value); err != nil {
		return nil, fmt.Errorf("config environment invalid")
	}
	keys := map[string]bool{}
	for key := range value.Env {
		keys[key] = true
	}
	for key := range value.Literal {
		keys[key] = true
	}
	var out []string
	if value.Env != nil || value.Literal != nil {
		out = []string{}
	}
	for key := range keys {
		if key == "" {
			return nil, fmt.Errorf("config environment key invalid")
		}
		out = append(out, key)
	}
	sort.Strings(out)
	return out, nil
}

func inspectNode(node *yaml.Node, active map[*yaml.Node]bool, count *int, depth int) error {
	if node == nil || active[node] || depth > maxDepth || *count >= maxNodes {
		return fmt.Errorf("config exceeds bounds")
	}
	*count++
	active[node] = true
	defer delete(active, node)
	if node.Kind == yaml.AliasNode {
		return inspectNode(node.Alias, active, count, depth+1)
	}
	for _, child := range node.Content {
		if err := inspectNode(child, active, count, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// GitSource reads the git: block a Teploy-home config declares (server
// authority D4): the repository an app builds from and the branch it
// admits. ok is false when the config declares no git: source. Invalid
// content is an error, never a silent "no source" — the webhook relay
// must not skip a connected app because its config looked unreadable.
func GitSource(content []byte) (repository, branch string, ok bool, err error) {
	root, err := decodeMapping(content)
	if err != nil {
		return "", "", false, err
	}
	var value struct {
		Git *struct {
			Repository string `yaml:"repository"`
			Branch     string `yaml:"branch"`
		} `yaml:"git"`
	}
	if err := root.Decode(&value); err != nil {
		return "", "", false, fmt.Errorf("config git source invalid")
	}
	if value.Git == nil {
		return "", "", false, nil
	}
	if strings.TrimSpace(value.Git.Repository) == "" {
		return "", "", false, fmt.Errorf("config git source invalid")
	}
	return value.Git.Repository, value.Git.Branch, true, nil
}

// decodeMapping decodes one bounded teploy.yml document into its root
// mapping node (the shared EnvKeys/GitSource walk).
func decodeMapping(content []byte) (*yaml.Node, error) {
	if len(content) > maxBytes {
		return nil, fmt.Errorf("config exceeds bounds")
	}
	var document yaml.Node
	decoder := yaml.NewDecoder(strings.NewReader(string(content)))
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("config invalid")
	}
	var extra yaml.Node
	if decoder.Decode(&extra) != io.EOF || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("config invalid")
	}
	count := 0
	if err := inspectNode(document.Content[0], map[*yaml.Node]bool{}, &count, 0); err != nil {
		return nil, err
	}
	return document.Content[0], nil
}
