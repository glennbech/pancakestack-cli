package seestar

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// State persists what's already been uploaded per (scope serial,
// folder, filename) so a restart never re-downloads and never
// re-uploads a frame that's already in the collection.
//
// Location: ~/.pancakestack/seestar-sync.json. Kept next to the
// CLI's other config (login token, backend URL) so wiping one
// wipes them all. JSON, not binary — Glenn can read and edit it
// if state ever gets confused.
type State struct {
	Version int                  `json:"version"`
	Entries map[string]*SyncEntry `json:"entries"`

	path string
	mu   sync.Mutex
}

// SyncEntry records one successfully-uploaded FITS.
type SyncEntry struct {
	Serial     string    `json:"serial"`
	Folder     string    `json:"folder"`
	Filename   string    `json:"filename"`
	Size       int64     `json:"size"`
	Collection string    `json:"collection"`
	UploadedAt time.Time `json:"uploadedAt"`
}

// LoadState opens (or creates) the state file for this user. Missing
// or unreadable → empty state, no error — new machines just start
// fresh. Corrupt (malformed JSON) → error, so we don't silently
// re-upload thousands of frames.
func LoadState(path string) (*State, error) {
	if path == "" {
		p, err := DefaultStatePath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	s := &State{
		Version: 1,
		Entries: map[string]*SyncEntry{},
		path:    path,
	}
	buf, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	if len(buf) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(buf, s); err != nil {
		return nil, fmt.Errorf("parse state file %s: %w", path, err)
	}
	if s.Entries == nil {
		s.Entries = map[string]*SyncEntry{}
	}
	s.path = path
	return s, nil
}

// DefaultStatePath returns ~/.config/pancakestack/seestar-sync.json,
// the same directory that holds credentials.json and seestar.pem —
// one config dir for everything the CLI needs. Respects XDG_CONFIG_HOME.
// Creates the parent dir on demand.
func DefaultStatePath() (string, error) {
	dir, err := configDirLocal()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "seestar-sync.json"), nil
}

// configDirLocal duplicates the logic in the config package to avoid
// pulling that package into a low-level protocol implementation
// (import cycles + keeps seestar/ self-contained for future re-use).
func configDirLocal() (string, error) {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "pancakestack"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "pancakestack"), nil
}

// entryKey composes the (serial, folder, filename) tuple into one
// map key. Filenames on the Seestar carry a UTC timestamp so
// collisions across sessions don't exist — the size field is a
// cheap extra guard against a truncated/growing file.
func entryKey(serial, folder, filename string) string {
	return serial + "\x00" + folder + "\x00" + filename
}

// Has reports whether this file has already been uploaded at the
// given size. A size mismatch → false, so a truncated / regrown
// file will re-upload.
func (s *State) Has(serial, folder, filename string, size int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.Entries[entryKey(serial, folder, filename)]
	if !ok {
		return false
	}
	return e.Size == size
}

// Mark records a successful upload. Overwrites any existing entry
// (same key), so a re-upload of a resized file replaces its record.
func (s *State) Mark(entry SyncEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Entries[entryKey(entry.Serial, entry.Folder, entry.Filename)] = &entry
	return s.saveLocked()
}

// saveLocked writes the state atomically (write to tmp, rename over
// the target). Caller must hold s.mu.
func (s *State) saveLocked() error {
	tmp := s.path + ".tmp"
	buf, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
