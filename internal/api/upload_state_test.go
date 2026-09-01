package api

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestUploadStateKey_Stability locks down the fields that go into the
// state-file hash. If any of these change, resume misses across CLI
// versions — every field is either something identifying "the same file"
// (path, size, mtime) or something identifying "the same destination"
// (collection, filename). Adding fields is fine; removing or reordering
// changes existing users' hashes and orphans their state files.
func TestUploadStateKey_Stability(t *testing.T) {
	// Same inputs → same hash. Cache invalidation by change detection
	// depends on this being deterministic.
	a := uploadStateKey("m101", "lights.tar", "/data/lights.tar", 12345, 100)
	b := uploadStateKey("m101", "lights.tar", "/data/lights.tar", 12345, 100)
	if a != b {
		t.Fatalf("same inputs should hash equal: %s vs %s", a, b)
	}

	// Each field must matter — if the hash ignored one, editing that
	// field in the local file wouldn't force a fresh upload and we'd
	// try to resume into a stale multipart on S3.
	base := uploadStateKey("m101", "lights.tar", "/data/lights.tar", 12345, 100)
	variants := []struct {
		name string
		key  string
	}{
		{"collection", uploadStateKey("m102", "lights.tar", "/data/lights.tar", 12345, 100)},
		{"filename", uploadStateKey("m101", "darks.tar", "/data/lights.tar", 12345, 100)},
		{"path", uploadStateKey("m101", "lights.tar", "/other/lights.tar", 12345, 100)},
		{"size", uploadStateKey("m101", "lights.tar", "/data/lights.tar", 99999, 100)},
		{"mtime", uploadStateKey("m101", "lights.tar", "/data/lights.tar", 12345, 200)},
	}
	for _, v := range variants {
		if v.key == base {
			t.Errorf("changing %s should change the hash, but it didn't", v.name)
		}
	}
}

// TestUploadState_RoundTrip covers the save→load→delete cycle with a
// temp XDG_CONFIG_HOME so we don't touch the real user's ~/.config.
func TestUploadState_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	orig := &UploadState{
		CollectionID:  "m101",
		Filename:      "lights.tar",
		AbsPath:       "/data/lights.tar",
		Size:          12345,
		MTimeUnixNano: 42,
		UploadID:      "upload-xyz",
		Key:           "input/sub/m101/lights.tar",
		PartSize:      8 * 1024 * 1024,
		ContentType:   "application/octet-stream",
		CreatedAt:     time.Unix(1700000000, 0).UTC(),
	}
	if err := SaveUploadState(orig); err != nil {
		t.Fatalf("save: %v", err)
	}

	// State file should be under XDG_CONFIG_HOME/pancakestack/uploads/.
	// Existence check catches path bugs (wrong dir, wrong extension).
	entries, err := os.ReadDir(filepath.Join(tmp, "pancakestack", "uploads"))
	if err != nil {
		t.Fatalf("read state dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 state file, got %d", len(entries))
	}

	loaded, err := LoadUploadState(orig.CollectionID, orig.Filename, orig.AbsPath, orig.Size, orig.MTimeUnixNano)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded == nil {
		t.Fatal("load returned nil for existing state")
	}
	if loaded.UploadID != orig.UploadID || loaded.Key != orig.Key || loaded.PartSize != orig.PartSize {
		t.Errorf("round-trip mismatch: got %+v want %+v", loaded, orig)
	}

	// Miss on any field change — this is what stops us from resuming
	// into an S3 multipart that no longer matches the local file.
	miss, err := LoadUploadState(orig.CollectionID, orig.Filename, orig.AbsPath, orig.Size, orig.MTimeUnixNano+1)
	if err != nil {
		t.Fatalf("load miss: %v", err)
	}
	if miss != nil {
		t.Error("changed mtime should have missed lookup")
	}

	if err := DeleteUploadState(orig.CollectionID, orig.Filename, orig.AbsPath, orig.Size, orig.MTimeUnixNano); err != nil {
		t.Fatalf("delete: %v", err)
	}
	loaded, err = LoadUploadState(orig.CollectionID, orig.Filename, orig.AbsPath, orig.Size, orig.MTimeUnixNano)
	if err != nil {
		t.Fatalf("load after delete: %v", err)
	}
	if loaded != nil {
		t.Error("state should be gone after delete")
	}

	// Delete of missing state is a no-op, not an error — the upload
	// path calls it unconditionally after a successful complete.
	if err := DeleteUploadState(orig.CollectionID, orig.Filename, orig.AbsPath, orig.Size, orig.MTimeUnixNano); err != nil {
		t.Errorf("delete of missing state should be no-op: %v", err)
	}
}
