package manifest

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

type Mode string

const (
	ModeDashManaged Mode = "dash-managed"
	ModeGitManaged  Mode = "git-managed"
)

var (
	ErrNotFound            = errors.New("manifest not found")
	ErrRevisionNotFound    = errors.New("manifest revision not found")
	ErrConflict            = errors.New("manifest revision conflict")
	ErrExpectedRevision    = errors.New("expected_revision is required for an existing manifest")
	ErrSecrets             = errors.New("manifest contains a secret value; store secrets with teploy and use secret:<name>[#field] references")
	identifierPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	revisionPattern        = regexp.MustCompile(`^[a-f0-9]{64}$`)
	secretReferencePattern = regexp.MustCompile(`^secret:[A-Za-z0-9_./-]+(?:#[A-Za-z0-9_.-]*)?$`)
)

type GitReference struct {
	Repository   string `json:"repository"`
	Revision     string `json:"revision"`
	ManifestPath string `json:"manifest_path,omitempty"`
}

type Revision struct {
	SHA256    string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`
	Size      int64     `json:"size"`
}

type Metadata struct {
	Server          string        `json:"server"`
	App             string        `json:"app"`
	Mode            Mode          `json:"mode"`
	Git             *GitReference `json:"git,omitempty"`
	CurrentRevision string        `json:"current_revision"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
	Revisions       []Revision    `json:"revisions"`
}

type Document struct {
	Metadata
	Manifest string `json:"manifest"`
}

type Update struct {
	Mode             Mode
	Git              *GitReference
	Manifest         string
	ExpectedRevision *string
}

type Store struct {
	mu   sync.RWMutex
	root string
}

func New(dataDir string) (*Store, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("manifest data directory is required")
	}
	root, err := filepath.Abs(filepath.Join(dataDir, "manifests"))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, fmt.Errorf("create manifest directory: %w", err)
	}
	return &Store{root: root}, nil
}

func ValidIdentifier(value string) bool {
	return value != "." && value != ".." && !strings.HasPrefix(value, "-") && identifierPattern.MatchString(value)
}

func ValidRevision(value string) bool {
	return revisionPattern.MatchString(value)
}

func (s *Store) Put(server, app string, update Update) (*Document, bool, error) {
	if err := validateIdentity(server, app); err != nil {
		return nil, false, err
	}
	if err := validateMode(update.Mode, update.Git); err != nil {
		return nil, false, err
	}
	content := []byte(update.Manifest)
	if err := Validate(content, app); err != nil {
		return nil, false, err
	}
	digest := sha256.Sum256(content)
	revision := hex.EncodeToString(digest[:])

	s.mu.Lock()
	defer s.mu.Unlock()

	existing, err := s.loadMetadata(server, app)
	created := errors.Is(err, os.ErrNotExist)
	if err != nil && !created {
		return nil, false, err
	}
	if !created {
		if update.ExpectedRevision == nil {
			return nil, false, ErrExpectedRevision
		}
		if *update.ExpectedRevision != existing.CurrentRevision {
			return nil, false, fmt.Errorf("%w: current revision is %s", ErrConflict, existing.CurrentRevision)
		}
	} else if update.ExpectedRevision != nil && *update.ExpectedRevision != "" {
		return nil, false, fmt.Errorf("%w: manifest does not exist", ErrConflict)
	}

	if err := s.writeRevision(server, app, revision, content); err != nil {
		return nil, false, err
	}
	now := time.Now().UTC()
	metadata := existing
	if created {
		metadata = &Metadata{Server: server, App: app, CreatedAt: now}
	}
	metadata.Mode = update.Mode
	metadata.Git = cloneGit(update.Git)
	metadata.CurrentRevision = revision
	metadata.UpdatedAt = now
	if !hasRevision(metadata.Revisions, revision) {
		metadata.Revisions = append(metadata.Revisions, Revision{SHA256: revision, CreatedAt: now, Size: int64(len(content))})
	}
	if err := s.writeMetadata(metadata); err != nil {
		return nil, false, err
	}
	return &Document{Metadata: *cloneMetadata(metadata), Manifest: string(content)}, created, nil
}

func (s *Store) Get(server, app string) (*Document, error) {
	if err := validateIdentity(server, app); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	metadata, err := s.loadMetadata(server, app)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	content, err := s.readRevision(metadata, metadata.CurrentRevision)
	if err != nil {
		return nil, err
	}
	return &Document{Metadata: *cloneMetadata(metadata), Manifest: string(content)}, nil
}

func (s *Store) List() ([]Metadata, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	servers, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	var result []Metadata
	for _, server := range servers {
		if !server.IsDir() || !ValidIdentifier(server.Name()) {
			continue
		}
		apps, err := os.ReadDir(filepath.Join(s.root, server.Name()))
		if err != nil {
			return nil, err
		}
		for _, app := range apps {
			if !app.IsDir() || !ValidIdentifier(app.Name()) {
				continue
			}
			metadata, err := s.loadMetadata(server.Name(), app.Name())
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			result = append(result, *cloneMetadata(metadata))
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Server != result[j].Server {
			return result[i].Server < result[j].Server
		}
		return result[i].App < result[j].App
	})
	return result, nil
}

func (s *Store) History(server, app string) ([]Revision, error) {
	if err := validateIdentity(server, app); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	metadata, err := s.loadMetadata(server, app)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	history := append([]Revision(nil), metadata.Revisions...)
	sort.Slice(history, func(i, j int) bool { return history[i].CreatedAt.After(history[j].CreatedAt) })
	return history, nil
}

func (s *Store) Export(server, app, revision string) ([]byte, error) {
	if err := validateIdentity(server, app); err != nil {
		return nil, err
	}
	if revision != "" && !ValidRevision(revision) {
		return nil, fmt.Errorf("invalid manifest revision")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	metadata, err := s.loadMetadata(server, app)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if revision == "" {
		revision = metadata.CurrentRevision
	}
	return s.readRevision(metadata, revision)
}

// ProjectDir resolves only a revision registered under server/app. Callers
// never supply or persist an arbitrary filesystem path.
func (s *Store) ProjectDir(server, app, revision string) (string, error) {
	if err := validateIdentity(server, app); err != nil {
		return "", err
	}
	if !ValidRevision(revision) {
		return "", fmt.Errorf("invalid manifest revision")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	metadata, err := s.loadMetadata(server, app)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if _, err := s.readRevision(metadata, revision); err != nil {
		return "", err
	}
	return filepath.Join(s.appDir(server, app), "revisions", revision), nil
}

func (s *Store) Delete(server, app string, expectedRevision *string) error {
	if err := validateIdentity(server, app); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	metadata, err := s.loadMetadata(server, app)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if expectedRevision == nil {
		return ErrExpectedRevision
	}
	if *expectedRevision != metadata.CurrentRevision {
		return fmt.Errorf("%w: current revision is %s", ErrConflict, metadata.CurrentRevision)
	}
	suffix, err := randomSuffix()
	if err != nil {
		return err
	}
	appDir := s.appDir(server, app)
	tombstone := filepath.Join(filepath.Dir(appDir), ".deleted-"+suffix)
	if err := os.Rename(appDir, tombstone); err != nil {
		return fmt.Errorf("remove manifest registration: %w", err)
	}
	if err := syncDir(filepath.Dir(appDir)); err != nil {
		return err
	}
	// This path is always under DataDir/manifests. Deployment state and remote
	// resources are deliberately outside the manifest store and untouched.
	return os.RemoveAll(tombstone)
}

func Validate(content []byte, app string) error {
	if len(content) == 0 {
		return fmt.Errorf("manifest is required")
	}
	var document yaml.Node
	decoder := yaml.NewDecoder(strings.NewReader(string(content)))
	if err := decoder.Decode(&document); err != nil {
		return fmt.Errorf("manifest must be valid YAML")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("manifest must contain exactly one YAML document")
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("manifest root must be a YAML mapping")
	}
	foundApp, err := inspectNode(document.Content[0], nil, false, make(map[*yaml.Node]bool))
	if err != nil {
		return err
	}
	if foundApp != app {
		return fmt.Errorf("manifest app must match URL app %q", app)
	}
	return nil
}

func inspectNode(node *yaml.Node, path []string, inheritedSensitive bool, visiting map[*yaml.Node]bool) (string, error) {
	if node == nil {
		return "", nil
	}
	if visiting[node] {
		return "", fmt.Errorf("manifest contains a recursive YAML alias")
	}
	visiting[node] = true
	defer delete(visiting, node)
	if node.Kind == yaml.AliasNode {
		return inspectNode(node.Alias, path, inheritedSensitive, visiting)
	}
	var foundApp string
	switch node.Kind {
	case yaml.MappingNode:
		seen := make(map[string]bool)
		for i := 0; i < len(node.Content); i += 2 {
			keyNode, valueNode := node.Content[i], node.Content[i+1]
			if keyNode.Kind != yaml.ScalarNode || keyNode.Value == "" {
				return "", fmt.Errorf("manifest mapping keys must be non-empty strings")
			}
			key := keyNode.Value
			if seen[key] {
				return "", fmt.Errorf("manifest contains duplicate key %q", key)
			}
			seen[key] = true
			childPath := append(append([]string(nil), path...), key)
			structuralSecret := len(path) == 0 && strings.EqualFold(key, "secret") && valueNode.Kind == yaml.MappingNode
			sensitive := inheritedSensitive || (!structuralSecret && sensitiveKey(key)) || pathEnds(path, "basic_auth")
			if len(path) == 0 && key == "app" {
				if valueNode.Kind != yaml.ScalarNode {
					return "", fmt.Errorf("manifest app must be a string")
				}
				foundApp = valueNode.Value
			}
			childApp, err := inspectNode(valueNode, childPath, sensitive, visiting)
			if err != nil {
				return "", err
			}
			if foundApp == "" {
				foundApp = childApp
			}
		}
	case yaml.SequenceNode:
		for _, child := range node.Content {
			childApp, err := inspectNode(child, path, inheritedSensitive, visiting)
			if err != nil {
				return "", err
			}
			if foundApp == "" {
				foundApp = childApp
			}
		}
	case yaml.ScalarNode:
		value := strings.TrimSpace(node.Value)
		if secretReferencePattern.MatchString(value) {
			return "", nil
		}
		if inheritedSensitive && value != "" && value != "null" && value != "~" {
			return "", fmt.Errorf("%w at %s", ErrSecrets, strings.Join(path, "."))
		}
		if looksLikeSecret(value) {
			return "", fmt.Errorf("%w at %s", ErrSecrets, strings.Join(path, "."))
		}
	}
	return foundApp, nil
}

func sensitiveKey(key string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), " ", "_"))
	switch normalized {
	case "password", "passwd", "secret", "token", "api_key", "apikey", "client_secret", "private_key", "access_key", "secret_key", "credentials", "credential", "authorization":
		return true
	}
	return strings.HasSuffix(normalized, "_password") || strings.HasSuffix(normalized, "_passwd") ||
		strings.HasSuffix(normalized, "_token") || strings.HasSuffix(normalized, "_secret") ||
		strings.HasSuffix(normalized, "_api_key") || strings.HasSuffix(normalized, "_private_key")
}

func looksLikeSecret(value string) bool {
	lower := strings.ToLower(value)
	if strings.Contains(value, "-----BEGIN ") && strings.Contains(value, "PRIVATE KEY-----") {
		return true
	}
	for _, prefix := range []string{"ghp_", "github_pat_", "xoxb-", "xoxp-", "sk_live_", "rk_live_", "akia", "bearer "} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.User != nil
}

func validateIdentity(server, app string) error {
	if !ValidIdentifier(server) {
		return fmt.Errorf("invalid server")
	}
	if !ValidIdentifier(app) {
		return fmt.Errorf("invalid app")
	}
	return nil
}

func validateMode(mode Mode, git *GitReference) error {
	switch mode {
	case ModeDashManaged:
		if git != nil {
			return fmt.Errorf("git must be omitted for dash-managed manifests")
		}
	case ModeGitManaged:
		if git == nil || git.Repository == "" || git.Revision == "" {
			return fmt.Errorf("git.repository and git.revision are required for git-managed manifests")
		}
		if len(git.Repository) > 2048 || len(git.Revision) > 255 || strings.ContainsAny(git.Repository, "\r\n") || strings.ContainsAny(git.Revision, " \t\r\n") {
			return fmt.Errorf("invalid git reference")
		}
		if parsed, err := url.Parse(git.Repository); err == nil && (parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "") {
			return fmt.Errorf("git repository URL must not contain credentials, query parameters, or fragments")
		}
		if git.ManifestPath != "" && !safeRelativePath(git.ManifestPath) {
			return fmt.Errorf("git.manifest_path must be a relative path without traversal")
		}
	default:
		return fmt.Errorf("mode must be dash-managed or git-managed")
	}
	return nil
}

func safeRelativePath(path string) bool {
	if filepath.IsAbs(path) || strings.Contains(path, "\\") {
		return false
	}
	clean := filepath.Clean(path)
	return clean != "." && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func (s *Store) appDir(server, app string) string {
	return filepath.Join(s.root, server, app)
}

func (s *Store) loadMetadata(server, app string) (*Metadata, error) {
	data, err := os.ReadFile(filepath.Join(s.appDir(server, app), "metadata.json"))
	if err != nil {
		return nil, err
	}
	var metadata Metadata
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metadata); err != nil {
		return nil, fmt.Errorf("load manifest metadata: %w", err)
	}
	if metadata.Server != server || metadata.App != app || !ValidRevision(metadata.CurrentRevision) {
		return nil, fmt.Errorf("invalid manifest metadata for %s/%s", server, app)
	}
	if err := validateMode(metadata.Mode, metadata.Git); err != nil {
		return nil, fmt.Errorf("invalid manifest metadata for %s/%s: %w", server, app, err)
	}
	seen := make(map[string]bool)
	for _, revision := range metadata.Revisions {
		if !ValidRevision(revision.SHA256) || revision.Size < 0 {
			return nil, fmt.Errorf("invalid revision metadata for %s/%s", server, app)
		}
		if seen[revision.SHA256] {
			return nil, fmt.Errorf("duplicate revision metadata for %s/%s", server, app)
		}
		seen[revision.SHA256] = true
	}
	if !seen[metadata.CurrentRevision] {
		return nil, fmt.Errorf("current revision missing from metadata for %s/%s", server, app)
	}
	return &metadata, nil
}

func (s *Store) writeMetadata(metadata *Metadata) error {
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	dir := s.appDir(metadata.Server, metadata.App)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return atomicWrite(filepath.Join(dir, "metadata.json"), append(data, '\n'), 0600)
}

func (s *Store) writeRevision(server, app, revision string, content []byte) error {
	revisionsDir := filepath.Join(s.appDir(server, app), "revisions")
	if err := os.MkdirAll(revisionsDir, 0700); err != nil {
		return err
	}
	target := filepath.Join(revisionsDir, revision)
	if existing, err := os.ReadFile(filepath.Join(target, "teploy.yml")); err == nil {
		if string(existing) != string(content) {
			return fmt.Errorf("revision digest collision")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tempDir, err := os.MkdirTemp(revisionsDir, ".tmp-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)
	file, err := os.OpenFile(filepath.Join(tempDir, "teploy.yml"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0400)
	if err != nil {
		return err
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := syncDir(tempDir); err != nil {
		return err
	}
	if err := os.Rename(tempDir, target); err != nil {
		if existing, readErr := os.ReadFile(filepath.Join(target, "teploy.yml")); readErr == nil && string(existing) == string(content) {
			return nil
		}
		return err
	}
	return syncDir(revisionsDir)
}

func (s *Store) readRevision(metadata *Metadata, revision string) ([]byte, error) {
	if !hasRevision(metadata.Revisions, revision) {
		return nil, ErrRevisionNotFound
	}
	data, err := os.ReadFile(filepath.Join(s.appDir(metadata.Server, metadata.App), "revisions", revision, "teploy.yml"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrRevisionNotFound
	}
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != revision {
		return nil, fmt.Errorf("manifest revision %s failed integrity check", revision)
	}
	if err := Validate(data, metadata.App); err != nil {
		return nil, fmt.Errorf("stored manifest revision rejected: %w", err)
	}
	return data, nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(mode); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func randomSuffix() (string, error) {
	data := make([]byte, 8)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func hasRevision(revisions []Revision, revision string) bool {
	for _, item := range revisions {
		if item.SHA256 == revision {
			return true
		}
	}
	return false
}

func cloneGit(git *GitReference) *GitReference {
	if git == nil {
		return nil
	}
	copy := *git
	return &copy
}

func cloneMetadata(metadata *Metadata) *Metadata {
	copy := *metadata
	copy.Git = cloneGit(metadata.Git)
	copy.Revisions = append([]Revision(nil), metadata.Revisions...)
	return &copy
}

func pathEnds(path []string, value string) bool {
	return len(path) > 0 && strings.EqualFold(path[len(path)-1], value)
}
