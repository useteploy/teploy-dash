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
)

// Token is one MCP access token. Only the SHA-256 of the secret is stored;
// the plaintext is shown once at creation.
type Token struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Hash      string    `json:"hash"` // hex sha256 of the plaintext
	ReadOnly  bool      `json:"read_only"`
	CreatedAt time.Time `json:"created_at"`
	LastUsed  time.Time `json:"last_used,omitempty"`
}

// TokenStore persists MCP tokens as a small JSON file in the dash data dir
// (same category as auth.json — deliberately NOT in the monitor store, which
// may live in Nucleus; auth material stays local to the dash host).
type TokenStore struct {
	path string

	mu     sync.Mutex
	tokens []Token
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

// Create mints a new token and returns its plaintext (shown once) and record.
func (s *TokenStore) Create(name string, readOnly bool) (string, Token, error) {
	if name == "" {
		return "", Token{}, fmt.Errorf("token name is required")
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
	t := Token{
		ID:        hex.EncodeToString(id),
		Name:      name,
		Hash:      hashToken(plaintext),
		ReadOnly:  readOnly,
		CreatedAt: time.Now().UTC(),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = append(s.tokens, t)
	if err := s.saveLocked(); err != nil {
		s.tokens = s.tokens[:len(s.tokens)-1]
		return "", Token{}, err
	}
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

// Delete revokes a token by id.
func (s *TokenStore) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.tokens {
		if t.ID == id {
			s.tokens = append(s.tokens[:i], s.tokens[i+1:]...)
			return s.saveLocked()
		}
	}
	return fmt.Errorf("token not found")
}

// Verify checks a presented plaintext token. Brute force is not a practical
// concern (256-bit secrets, constant-time compare), so there is no lockout.
// A hit updates LastUsed (best-effort persist).
func (s *TokenStore) Verify(plaintext string) (Token, bool) {
	h := hashToken(plaintext)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.tokens {
		if subtle.ConstantTimeCompare([]byte(s.tokens[i].Hash), []byte(h)) == 1 {
			s.tokens[i].LastUsed = time.Now().UTC()
			_ = s.saveLocked()
			return s.tokens[i], true
		}
	}
	return Token{}, false
}

func (s *TokenStore) saveLocked() error {
	data, err := json.MarshalIndent(s.tokens, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func hashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}
