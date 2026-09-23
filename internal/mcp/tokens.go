// Package mcp implements teploy-dash's Model Context Protocol server: a
// bearer-token-authenticated JSON-RPC endpoint exposing a curated set of
// read and action tools to AI clients (Claude Code, Cursor, any MCP client).
//
// Sync-safety design: MCP introduces NO deployment state of its own. Read
// tools consult the same server state files the dashboard reads; action
// tools delegate to the teploy CLI binary exactly like the UI buttons do,
// so the CLI's deploy lock and server-side state files remain the single
// source of truth for terminal, UI, webhook, and MCP alike. The only state
// owned here is the token file — auth material, not deployment state.
package mcp

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/useteploy/teploy-dash/internal/caps"
	"github.com/useteploy/teploy-dash/internal/durable"
)

// Token is one MCP access token. Only the SHA-256 of the secret is stored;
// the plaintext is shown once at creation.
//
// X03: Capabilities is the token's explicit capability set. nil marks a
// pre-X03 token, whose permissions derive from ReadOnly exactly as before
// (a non-read-only token could use the whole MCP surface, a read-only token
// only the read tools) — never silently narrowed, never silently widened.
// Tokens minted after X03 always record their set.
type Token struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Hash         string    `json:"hash"` // hex sha256 of the plaintext
	ReadOnly     bool      `json:"read_only"`
	Capabilities []string  `json:"capabilities,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	LastUsed     time.Time `json:"last_used,omitempty"`
}

// CapabilitySet resolves the token's effective capabilities. The legacy
// (nil Capabilities) full surface equals the operator preset minus secrets —
// which is exactly what the MCP tool surface ever offered.
func (t Token) CapabilitySet() caps.Set {
	if t.Capabilities == nil {
		if t.ReadOnly {
			return caps.ViewerDefault()
		}
		return caps.OperatorDefault()
	}
	return caps.NewSet(t.Capabilities...)
}

// TokenStore persists MCP tokens as a small JSON file in the dash data dir
// (same category as auth.json — deliberately NOT in the monitor store, which
// may live in Nucleus; auth material stays local to the dash host).
type TokenStore struct {
	path string

	mu            sync.Mutex
	tokens        []Token
	lastUsedFlush time.Time
}

// NewTokenStore loads (or lazily creates) the token file.
func NewTokenStore(dataDir string) (*TokenStore, error) {
	s := &TokenStore{path: filepath.Join(dataDir, "mcp-tokens.json")}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("reading %s: %w", s.path, err)
	}
	if err := json.Unmarshal(data, &s.tokens); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", s.path, err)
	}
	return s, nil
}

const tokenPrefix = "tpd_"

// Create mints a new token and returns its plaintext (shown once) and
// record. X03 default capability sets are recorded explicitly at mint:
// the operator preset minus secrets for full tokens (metadata reads, deploy
// and mutate actions, logs — NO value revelation; no MCP tool returns
// secret values), metadata-only for read-only tokens.
func (s *TokenStore) Create(name string, readOnly bool) (string, Token, error) {
	defaults := caps.OperatorDefault()
	if readOnly {
		defaults = caps.ViewerDefault()
	}
	return s.CreateWithCapabilities(name, defaults.Sorted())
}

// CreateWithCapabilities mints a token with an EXPLICIT capability set.
// An empty set is rejected: it would round-trip through the omitted JSON
// field back to the legacy full default — a silent widening.
func (s *TokenStore) CreateWithCapabilities(name string, capabilities []string) (string, Token, error) {
	if name == "" {
		return "", Token{}, fmt.Errorf("token name is required")
	}
	if len(capabilities) == 0 {
		return "", Token{}, fmt.Errorf("a token needs at least one capability")
	}
	if err := caps.Validate(capabilities); err != nil {
		return "", Token{}, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", Token{}, err
	}
	plaintext := tokenPrefix + base64.RawURLEncoding.EncodeToString(raw)

	id := make([]byte, 6)
	if _, err := rand.Read(id); err != nil {
		return "", Token{}, err
	}
	granted := caps.NewSet(capabilities...)
	t := Token{
		ID:           hex.EncodeToString(id),
		Name:         name,
		Hash:         hashToken(plaintext),
		ReadOnly:     !granted.Allow(caps.ExecuteDeploy) && !granted.Allow(caps.ExecuteMutate),
		Capabilities: caps.Normalize(capabilities),
		CreatedAt:    time.Now().UTC(),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	candidate := append(append([]Token(nil), s.tokens...), t)
	if err := s.saveLocked(candidate); err != nil {
		return "", Token{}, err
	}
	s.tokens = candidate
	return plaintext, t, nil
}

// List returns the token records (no secrets — only hashes, which are not
// reversible; the UI shows name/created/last-used).
func (s *TokenStore) List() []Token {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Token, len(s.tokens))
	copy(out, s.tokens)
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Delete revokes a token by id. The candidate slice is persisted BEFORE the
// live state changes (A20): the old order removed the token first and saved
// second, so a failed save left the process denying a token that a restart
// would happily accept again.
func (s *TokenStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.tokens {
		if t.ID == id {
			candidate := make([]Token, 0, len(s.tokens)-1)
			candidate = append(candidate, s.tokens[:i]...)
			candidate = append(candidate, s.tokens[i+1:]...)
			if err := s.saveLocked(candidate); err != nil {
				return err
			}
			s.tokens = candidate
			return nil
		}
	}
	return fmt.Errorf("token not found")
}

// lastUsedFlushInterval bounds how often Verify persists usage telemetry.
// Writing the whole token file on every successful MCP request turned
// authentication traffic into disk traffic under the store mutex (A20);
// LastUsed is best-effort, so it is flushed at most this often.
const lastUsedFlushInterval = time.Minute

// Verify checks a presented plaintext token. Brute force is not a practical
// concern (256-bit secrets, constant-time compare), so there is no lockout.
// A hit updates LastUsed in memory and flushes it to disk at most once per
// minute.
func (s *TokenStore) Verify(plaintext string) (Token, bool) {
	h := hashToken(plaintext)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.tokens {
		if subtle.ConstantTimeCompare([]byte(s.tokens[i].Hash), []byte(h)) == 1 {
			s.tokens[i].LastUsed = time.Now().UTC()
			if time.Since(s.lastUsedFlush) >= lastUsedFlushInterval {
				s.lastUsedFlush = time.Now().UTC()
				_ = s.saveLocked(s.tokens)
			}
			return s.tokens[i], true
		}
	}
	return Token{}, false
}

func (s *TokenStore) saveLocked(tokens []Token) error {
	data, err := json.MarshalIndent(tokens, "", "  ")
	if err != nil {
		return err
	}
	// F004: durable unique-temp replacement (sync + rename + dir sync) — a
	// revoked token could previously reappear after a crash, and the fixed
	// .tmp name let concurrent saves clobber each other's temporary file.
	return durable.Replace(s.path, data, 0600)
}

func hashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}
