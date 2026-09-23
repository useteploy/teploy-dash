package source

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/useteploy/teploy-dash/internal/durable"
)

var (
	ErrNotFound        = errors.New("source not found")
	ErrDuplicate       = errors.New("source already registered for this forge and repository")
	idPattern          = regexp.MustCompile(`^src-[a-f0-9]{16}$`)
	branchPattern      = regexp.MustCompile(`^[^\s\x00]{1,128}$`)
	referencePattern   = regexp.MustCompile(`^[^\s\x00]{0,128}$`)
	deliveryIDMaxLen   = 255
	maxRetainedRecords = 250
)

func IsNotFound(err error) bool  { return errors.Is(err, ErrNotFound) }
func IsDuplicate(err error) bool { return errors.Is(err, ErrDuplicate) }

// Source is one registered forge repository. Identity (Forge + CloneURL +
// ID) is immutable; DisplayName, CredentialRef and DefaultBranch are
// mutable configuration. The credential is a REFERENCE into wherever the
// operator keeps scoped tokens (vault path, CLI secret name) — never a
// token value.
type Source struct {
	ID            string    `json:"id"`
	Forge         Forge     `json:"forge"`
	CloneURL      string    `json:"clone_url"`
	DefaultBranch string    `json:"default_branch,omitempty"`
	CredentialRef string    `json:"credential_ref,omitempty"`
	DisplayName   string    `json:"display_name,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	// Degraded is the D01-convention visibility envelope for revoked or
	// failing credentials: a degraded source stays in every listing with
	// the exact reason; nothing about it is silently dropped.
	Degraded         bool      `json:"degraded,omitempty"`
	DegradeReason    string    `json:"degrade_reason,omitempty"`
	DegradeCheckedAt time.Time `json:"degrade_checked_at,omitempty"`
}

type CreateInput struct {
	Forge         Forge
	CloneURL      string
	DefaultBranch string
	CredentialRef string
	DisplayName   string
}

type UpdateInput struct {
	DefaultBranch *string
	CredentialRef *string
	DisplayName   *string
}

// CreatedSource is the create/rotate result: the source plus the webhook
// secret, which is returned EXACTLY ONCE and never appears in any later
// read.
type CreatedSource struct {
	Source
	WebhookSecret string `json:"webhook_secret"`
}

// Delivery dispositions. Everything except "refused" joins the delivery-id
// seen index: a refused delivery admitted nothing, so the forge's retry
// must re-run admission instead of being swallowed as a duplicate (C02's
// rollback rule).
const (
	DispositionAdmitted   = "admitted"
	DispositionDuplicate  = "duplicate"
	DispositionIgnored    = "ignored"
	DispositionDegraded   = "degraded"
	DispositionSuperseded = "superseded"
	DispositionRefused    = "refused"
)

func dispositionSeen(disposition string) bool {
	switch disposition {
	case DispositionAdmitted, DispositionDuplicate, DispositionIgnored, DispositionDegraded, DispositionSuperseded:
		return true
	}
	return false
}

// Delivery is one authenticated webhook delivery record (the delivery
// identity D04 asks for): provider delivery id, normalized event, branch,
// the authenticated commit pin, and what dash did with it.
type Delivery struct {
	ID          string    `json:"id"`
	Event       string    `json:"event"`
	Branch      string    `json:"branch,omitempty"`
	Commit      string    `json:"commit,omitempty"`
	Disposition string    `json:"disposition"`
	Reason      string    `json:"reason,omitempty"`
	OperationID string    `json:"operation_id,omitempty"`
	ReceivedAt  time.Time `json:"received_at"`
}

// Store persists sources under <dataDir>/sources/<id>/ — metadata.json and
// webhook-secret via durable whole-file replacement, deliveries as an
// append-only JSONL ledger. Like the manifest store, it is a data-dir
// store in BOTH backend modes.
type Store struct {
	mu         sync.Mutex
	root       string
	deliveries map[string][]Delivery
	seen       map[string]map[string]bool
}

func New(dataDir string) (*Store, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("source data directory is required")
	}
	root, err := filepath.Abs(filepath.Join(dataDir, "sources"))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, fmt.Errorf("create source directory: %w", err)
	}
	store := &Store{
		root:       root,
		deliveries: make(map[string][]Delivery),
		seen:       make(map[string]map[string]bool),
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !ValidID(entry.Name()) {
			continue
		}
		if _, err := store.loadSource(entry.Name()); err != nil {
			return nil, fmt.Errorf("load source %s: %w", entry.Name(), err)
		}
	}
	return store, nil
}

func ValidID(id string) bool { return idPattern.MatchString(id) }

func (s *Store) dir(id string) string { return filepath.Join(s.root, id) }

// Create registers one source. Identity collisions (same forge + canonical
// URL under any spelling) answer ErrDuplicate; the webhook secret is
// generated here and persisted in its own 0600 file.
func (s *Store) Create(input CreateInput) (*CreatedSource, error) {
	if !ValidForge(input.Forge) {
		return nil, fmt.Errorf("unknown forge kind %q", string(input.Forge))
	}
	normalized, err := NormalizeCloneURL(input.CloneURL)
	if err != nil {
		return nil, err
	}
	if input.DefaultBranch != "" && !branchPattern.MatchString(input.DefaultBranch) {
		return nil, fmt.Errorf("invalid default branch")
	}
	if !referencePattern.MatchString(input.CredentialRef) {
		return nil, fmt.Errorf("invalid credential reference")
	}
	if len(input.DisplayName) > 128 {
		return nil, fmt.Errorf("display name exceeds 128 characters")
	}
	id, err := DeriveID(input.Forge, input.CloneURL)
	if err != nil {
		return nil, err
	}
	secret, err := generateWebhookSecret()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	source := Source{
		ID:            id,
		Forge:         input.Forge,
		CloneURL:      normalized,
		DefaultBranch: input.DefaultBranch,
		CredentialRef: input.CredentialRef,
		DisplayName:   input.DisplayName,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	metadataPath := filepath.Join(s.dir(id), "metadata.json")
	if _, err := os.Stat(metadataPath); err == nil {
		return nil, ErrDuplicate
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := durable.Replace(filepath.Join(s.dir(id), "webhook-secret"), []byte(secret+"\n"), 0600); err != nil {
		return nil, fmt.Errorf("persist webhook secret: %w", err)
	}
	if err := s.persist(source); err != nil {
		return nil, err
	}
	return &CreatedSource{Source: source, WebhookSecret: secret}, nil
}

func (s *Store) Get(id string) (*Source, error) {
	if !ValidID(id) {
		return nil, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadSource(id)
}

// loadSource reads and validates one metadata record. Caller holds s.mu
// (or is the constructor).
func (s *Store) loadSource(id string) (*Source, error) {
	data, err := os.ReadFile(filepath.Join(s.dir(id), "metadata.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var source Source
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&source); err != nil {
		return nil, fmt.Errorf("parse source metadata: %w", err)
	}
	if source.ID != id || !ValidForge(source.Forge) {
		return nil, fmt.Errorf("invalid source metadata for %s", id)
	}
	if normalized, err := NormalizeCloneURL(source.CloneURL); err != nil || normalized != source.CloneURL {
		return nil, fmt.Errorf("non-canonical clone URL for %s", id)
	}
	if err := s.loadDeliveries(id); err != nil {
		return nil, err
	}
	return &source, nil
}

func (s *Store) List() ([]Source, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	var result []Source
	for _, entry := range entries {
		if !entry.IsDir() || !ValidID(entry.Name()) {
			continue
		}
		source, err := s.loadSource(entry.Name())
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result = append(result, *source)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].CloneURL != result[j].CloneURL {
			return result[i].CloneURL < result[j].CloneURL
		}
		return result[i].Forge < result[j].Forge
	})
	return result, nil
}

// Update mutates only the mutable fields; identity (forge, URL, ID) is
// immutable by construction — a repository that moved is a NEW source.
func (s *Store) Update(id string, input UpdateInput) (*Source, error) {
	if !ValidID(id) {
		return nil, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	source, err := s.loadSource(id)
	if err != nil {
		return nil, err
	}
	if input.DefaultBranch != nil {
		if *input.DefaultBranch != "" && !branchPattern.MatchString(*input.DefaultBranch) {
			return nil, fmt.Errorf("invalid default branch")
		}
		source.DefaultBranch = *input.DefaultBranch
	}
	if input.CredentialRef != nil {
		if !referencePattern.MatchString(*input.CredentialRef) {
			return nil, fmt.Errorf("invalid credential reference")
		}
		source.CredentialRef = *input.CredentialRef
	}
	if input.DisplayName != nil {
		if len(*input.DisplayName) > 128 {
			return nil, fmt.Errorf("display name exceeds 128 characters")
		}
		source.DisplayName = *input.DisplayName
	}
	source.UpdatedAt = time.Now().UTC()
	if err := s.persist(*source); err != nil {
		return nil, err
	}
	return source, nil
}

// MarkCredentialState records a credential verification verdict (D04
// revoked-permission handling): failure marks the source degraded with the
// exact reason — visible on every read, never a silent drop; success
// clears it.
func (s *Store) MarkCredentialState(id string, degraded bool, reason string) error {
	if !ValidID(id) {
		return ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	source, err := s.loadSource(id)
	if err != nil {
		return err
	}
	source.Degraded = degraded
	source.DegradeReason = ""
	if degraded {
		if reason == "" {
			reason = "credential verification failed"
		}
		if len(reason) > 512 {
			reason = reason[:512]
		}
		source.DegradeReason = reason
	}
	source.DegradeCheckedAt = time.Now().UTC()
	return s.persist(*source)
}

func (s *Store) Delete(id string) error {
	if !ValidID(id) {
		return ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.loadSource(id); err != nil {
		return err
	}
	suffix, err := randomSuffix()
	if err != nil {
		return err
	}
	tombstone := filepath.Join(s.root, ".deleted-"+suffix)
	if err := os.Rename(s.dir(id), tombstone); err != nil {
		return fmt.Errorf("remove source: %w", err)
	}
	delete(s.deliveries, id)
	delete(s.seen, id)
	return os.RemoveAll(tombstone)
}

func (s *Store) WebhookSecret(id string) (string, error) {
	if !ValidID(id) {
		return "", ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.loadSource(id); err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(s.dir(id), "webhook-secret"))
	if err != nil {
		return "", fmt.Errorf("read webhook secret: %w", err)
	}
	secret := strings.TrimSpace(string(data))
	if secret == "" {
		return "", fmt.Errorf("webhook secret is missing for %s", id)
	}
	return secret, nil
}

// RotateWebhookSecret replaces the secret and returns the new value
// exactly once.
func (s *Store) RotateWebhookSecret(id string) (string, error) {
	if !ValidID(id) {
		return "", ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.loadSource(id); err != nil {
		return "", err
	}
	secret, err := generateWebhookSecret()
	if err != nil {
		return "", err
	}
	if err := durable.Replace(filepath.Join(s.dir(id), "webhook-secret"), []byte(secret+"\n"), 0600); err != nil {
		return "", fmt.Errorf("persist webhook secret: %w", err)
	}
	return secret, nil
}

// RecordDelivery durably appends one delivery to the source's ledger. The
// caller acknowledges the delivery to the forge only after this returns
// nil (the C02 ack-after-fsync discipline).
func (s *Store) RecordDelivery(id string, delivery Delivery) error {
	if !ValidID(id) {
		return ErrNotFound
	}
	if delivery.ID == "" || len(delivery.ID) > deliveryIDMaxLen || strings.ContainsAny(delivery.ID, "\r\n\x00") {
		return fmt.Errorf("invalid delivery id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.loadSource(id); err != nil {
		return err
	}
	line, err := json.Marshal(delivery)
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir(id), "deliveries.jsonl")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0600)
	if err != nil {
		return fmt.Errorf("open delivery ledger: %w", err)
	}
	// F017 discipline: a torn final record (crash mid-append, no newline)
	// is repaired by appending the newline BEFORE the next record, so the
	// torn line stays its own (already-dropped-on-load) line instead of
	// merging with this one.
	if size := fileSize(file); size > 0 {
		if lastByte(file, size-1) != '\n' {
			if _, err := file.Write([]byte("\n")); err != nil {
				file.Close()
				return fmt.Errorf("repair delivery ledger: %w", err)
			}
		}
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		file.Close()
		return fmt.Errorf("append delivery: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return fmt.Errorf("sync delivery ledger: %w", err)
	}
	if err := file.Close(); err != nil {
		return err
	}
	s.deliveries[id] = append(s.deliveries[id], delivery)
	if dispositionSeen(delivery.Disposition) {
		s.addSeenLocked(id, delivery.ID)
	}
	// Bound the retained ledger the way the alert outbox bounds its
	// journal: compact to the newest records, never dropping the tail
	// append a concurrent reader could depend on (s.mu serializes).
	if len(s.deliveries[id]) > maxRetainedRecords*2 {
		records := append([]Delivery(nil), s.deliveries[id][len(s.deliveries[id])-maxRetainedRecords:]...)
		var buf strings.Builder
		for _, record := range records {
			line, err := json.Marshal(record)
			if err != nil {
				return err
			}
			buf.Write(line)
			buf.WriteByte('\n')
		}
		if err := durable.Replace(path, []byte(buf.String()), 0600); err != nil {
			return fmt.Errorf("compact delivery ledger: %w", err)
		}
		s.deliveries[id] = records
	}
	return nil
}

func (s *Store) addSeenLocked(id, deliveryID string) {
	if s.seen[id] == nil {
		s.seen[id] = make(map[string]bool)
	}
	s.seen[id][deliveryID] = true
}

// DeliverySeen answers whether a delivery id was already durably recorded
// with an admitting disposition.
func (s *Store) DeliverySeen(id, deliveryID string) bool {
	if !ValidID(id) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadDeliveries(id); err != nil {
		return false
	}
	return s.seen[id][deliveryID]
}

// RecentDeliveries returns up to limit delivery records, newest first.
func (s *Store) RecentDeliveries(id string, limit int) []Delivery {
	if !ValidID(id) || limit <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadDeliveries(id); err != nil {
		return nil
	}
	records := s.deliveries[id]
	if len(records) == 0 {
		return nil
	}
	if len(records) > limit {
		records = records[len(records)-limit:]
	}
	out := make([]Delivery, len(records))
	for i, record := range records {
		out[len(records)-1-i] = record
	}
	return out
}

// loadDeliveries folds the ledger into memory once per process (caller
// holds s.mu). A torn FINAL line is dropped (crash mid-append = never
// fsynced = never acked); any other damage fails loudly — history is never
// silently shortened.
func (s *Store) loadDeliveries(id string) error {
	if _, ok := s.deliveries[id]; ok {
		return nil
	}
	path := filepath.Join(s.dir(id), "deliveries.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.deliveries[id] = nil
			return nil
		}
		return fmt.Errorf("read delivery ledger for %s: %w", id, err)
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	var records []Delivery
	for i, line := range lines {
		if strings.TrimSpace(line) == "" && i != len(lines)-1 {
			return fmt.Errorf("delivery ledger for %s has a blank record at line %d", id, i+1)
		}
		var delivery Delivery
		if err := json.Unmarshal([]byte(line), &delivery); err != nil {
			if i == len(lines)-1 {
				break // torn tail: never fsynced, never acked
			}
			return fmt.Errorf("delivery ledger for %s is corrupt at line %d: %w", id, i+1, err)
		}
		records = append(records, delivery)
	}
	s.deliveries[id] = records
	for _, record := range records {
		if record.ID != "" && dispositionSeen(record.Disposition) {
			s.addSeenLocked(id, record.ID)
		}
	}
	return nil
}

func (s *Store) persist(source Source) error {
	data, err := json.MarshalIndent(source, "", "  ")
	if err != nil {
		return err
	}
	return durable.Replace(filepath.Join(s.dir(source.ID), "metadata.json"), append(data, '\n'), 0600)
}

func generateWebhookSecret() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func fileSize(file *os.File) int64 {
	info, err := file.Stat()
	if err != nil {
		return 0
	}
	return info.Size()
}

func lastByte(file *os.File, offset int64) byte {
	var buf [1]byte
	if _, err := file.ReadAt(buf[:], offset); err != nil {
		return '\n' // unreadable tail: assume terminated rather than double-repair
	}
	return buf[0]
}

func randomSuffix() (string, error) {
	data := make([]byte, 8)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}
