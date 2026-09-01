package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// UploadState is what we persist between `pancakestack upload` invocations
// so a broken pipe on part 8129 of ~10 000 doesn't discard everything we
// already sent. On the second run we call ListMultipartParts against the
// stored uploadID and re-send only the missing parts.
//
// Written after InitMultipartUpload returns and before we start streaming;
// deleted on successful CompleteMultipartUpload. If the process is killed
// between init and complete, the file survives — that's the whole point.
//
// Location: ~/.config/pancakestack/uploads/<hash>.json. One file per
// in-flight upload, hashed on the tuple that identifies "the same upload":
// (collectionID, filename, absPath, size, mtime). If any of those change
// on the local file (user edited it, replaced it, moved it), we treat
// it as a different upload and start fresh — resuming into an S3 object
// that no longer matches would corrupt the final assembly.
type UploadState struct {
	CollectionID  string    `json:"collectionId"`
	Filename      string    `json:"filename"`
	AbsPath       string    `json:"path"`
	Size          int64     `json:"size"`
	MTimeUnixNano int64     `json:"mtimeUnixNano"`
	UploadID      string    `json:"uploadId"`
	Key           string    `json:"key"`
	PartSize      int       `json:"partSize"`
	ContentType   string    `json:"contentType,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
}

// uploadStateKey derives the filename we save an in-flight upload's state
// under. Combines every field that has to match for a resume to be
// semantically safe:
//   - collectionID + filename → different S3 keys → definitely different upload
//   - absPath → same filename in two dirs would collide on hash otherwise
//   - size + mtime → catch "user edited the file since last attempt"
//
// SHA-256 output keeps the filename ASCII-safe regardless of what the user
// put in the archive name (spaces, unicode, etc.) — the state dir lives in
// XDG config and shouldn't sprout weird filenames.
func uploadStateKey(collectionID, filename, absPath string, size, mtimeUnixNano int64) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%d\x00%d", collectionID, filename, absPath, size, mtimeUnixNano)
	return hex.EncodeToString(h.Sum(nil))
}

// UploadStateDir returns the directory where in-flight upload state files
// live. Mirrors seestar's config dir logic so a `rm -rf ~/.config/pancakestack`
// wipes everything the CLI persists.
func UploadStateDir() (string, error) {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "pancakestack", "uploads"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "pancakestack", "uploads"), nil
}

func uploadStatePath(collectionID, filename, absPath string, size, mtimeUnixNano int64) (string, error) {
	dir, err := UploadStateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, uploadStateKey(collectionID, filename, absPath, size, mtimeUnixNano)+".json"), nil
}

// SaveUploadState atomically writes state to disk. Best-effort: an I/O
// failure here shouldn't fail the upload — worst case is the user loses
// resume for this one upload. Returns the error so the caller can log
// but should not propagate it.
func SaveUploadState(s *UploadState) error {
	dir, err := UploadStateDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path, err := uploadStatePath(s.CollectionID, s.Filename, s.AbsPath, s.Size, s.MTimeUnixNano)
	if err != nil {
		return err
	}
	buf, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadUploadState looks up saved state for the given file. Returns
// (nil, nil) when no resume state exists — a fresh upload, not an error.
// Corrupt state returns an error so we don't silently start a fresh
// upload after a disk gremlin ate the JSON.
func LoadUploadState(collectionID, filename, absPath string, size, mtimeUnixNano int64) (*UploadState, error) {
	path, err := uploadStatePath(collectionID, filename, absPath, size, mtimeUnixNano)
	if err != nil {
		return nil, err
	}
	buf, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var s UploadState
	if err := json.Unmarshal(buf, &s); err != nil {
		return nil, fmt.Errorf("parse upload state %s: %w", path, err)
	}
	return &s, nil
}

// DeleteUploadState removes the state file after a successful upload.
// Not-exist is not an error — we may be cleaning up after a fresh run
// that never wrote state (e.g. small file that skipped the resume path).
func DeleteUploadState(collectionID, filename, absPath string, size, mtimeUnixNano int64) error {
	path, err := uploadStatePath(collectionID, filename, absPath, size, mtimeUnixNano)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
