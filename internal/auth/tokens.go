// Package auth handles the Cognito OAuth 2.0 PKCE flow and local token storage.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Tokens is the persisted OAuth token bundle. mode 0600 on disk.
type Tokens struct {
	IDToken      string    `json:"id_token"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	TokenType    string    `json:"token_type"`

	// Discovery values captured at login time so refresh works without
	// re-hitting /login.
	Issuer   string `json:"issuer"`
	ClientID string `json:"client_id"`
	TokenURL string `json:"token_url"`
}

// Load reads tokens from path. Returns (nil, nil) if the file doesn't exist —
// caller decides whether that's an error.
func Load(path string) (*Tokens, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read credentials %s: %w", path, err)
	}
	var t Tokens
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("parse credentials %s: %w", path, err)
	}
	return &t, nil
}

// Save writes tokens to path with mode 0600, creating parent dirs as needed.
func Save(path string, t *Tokens) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal tokens: %w", err)
	}
	// O_TRUNC + 0600 so we don't leave stale bytes around.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(b); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// Delete removes the credentials file. Not an error if it doesn't exist.
func Delete(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// IsExpired returns true if the access token is past ExpiresAt (with a small
// buffer so we don't hand out a token that's about to expire mid-request).
func (t *Tokens) IsExpired() bool {
	if t.ExpiresAt.IsZero() {
		return false
	}
	return time.Now().After(t.ExpiresAt.Add(-30 * time.Second))
}
