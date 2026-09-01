package seestar

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tempStatePath returns a fresh path in a per-test tempdir (auto-cleaned).
func tempStatePath(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	return filepath.Join(dir, "seestar-sync.json")
}

func TestEntryKey(t *testing.T) {
	// Same inputs → same key.
	if entryKey("M81_sub", "Light_M81_60s_20260101-000000.fit") !=
		entryKey("M81_sub", "Light_M81_60s_20260101-000000.fit") {
		t.Fatal("entryKey not deterministic for identical inputs")
	}
	// Different folder → different key (prevents cross-folder collisions).
	if entryKey("A", "x.fit") == entryKey("B", "x.fit") {
		t.Fatal("entryKey collided across folders")
	}
	// Uses NUL as separator so a folder ending in filename can't collide
	// with a legitimately-named file.
	k := entryKey("foo", "bar")
	if !strings.Contains(k, "\x00") {
		t.Fatalf("entryKey missing NUL separator: %q", k)
	}
}

func TestLoadState_Missing(t *testing.T) {
	s, err := LoadState(tempStatePath(t))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if s.Version != 2 {
		t.Errorf("fresh state version = %d, want 2", s.Version)
	}
	if len(s.Entries) != 0 {
		t.Errorf("fresh state has %d entries, want 0", len(s.Entries))
	}
}

func TestLoadState_EmptyFile(t *testing.T) {
	path := tempStatePath(t)
	if err := os.WriteFile(path, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadState(path)
	if err != nil {
		t.Fatalf("empty file should not error: %v", err)
	}
	if s.Version != 2 || len(s.Entries) != 0 {
		t.Errorf("empty file loaded as version=%d entries=%d, want 2/0", s.Version, len(s.Entries))
	}
}

func TestLoadState_Corrupt(t *testing.T) {
	path := tempStatePath(t)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(path); err == nil {
		t.Fatal("corrupt JSON should error (guards against silent re-upload)")
	}
}

func TestLoadState_MigrateV1(t *testing.T) {
	// Simulate a v1 state file: version=1, keys include the scope serial
	// as a leading segment. LoadState should re-key by (folder, filename)
	// only, bump version to 2, and rewrite the file on disk.
	v1 := map[string]any{
		"version": 1,
		"entries": map[string]any{
			// legacy key format: serial + NUL + folder + NUL + filename
			"scope-A\x00M81_sub\x00a.fit": map[string]any{
				"serial":     "scope-A",
				"folder":     "M81_sub",
				"filename":   "a.fit",
				"size":       1024,
				"collection": "coll-1",
				"uploadedAt": "2026-08-27T10:00:00Z",
			},
			"scope-B\x00M81_sub\x00b.fit": map[string]any{
				"serial":     "scope-B",
				"folder":     "M81_sub",
				"filename":   "b.fit",
				"size":       2048,
				"collection": "coll-1",
				"uploadedAt": "2026-08-27T10:00:00Z",
			},
			// entry missing folder/filename should be dropped during migration
			"scope-C\x00\x00": map[string]any{
				"serial": "scope-C",
			},
		},
	}
	buf, err := json.Marshal(v1)
	if err != nil {
		t.Fatal(err)
	}
	path := tempStatePath(t)
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := LoadState(path)
	if err != nil {
		t.Fatalf("migration should not error: %v", err)
	}
	if s.Version != 2 {
		t.Errorf("post-migration version = %d, want 2", s.Version)
	}
	if len(s.Entries) != 2 {
		t.Errorf("post-migration entries = %d, want 2 (invalid entry dropped)", len(s.Entries))
	}

	// Both legit entries survived and are now keyed without the serial.
	if !s.Has("M81_sub", "a.fit", 1024) {
		t.Error("Has(M81_sub, a.fit, 1024) = false after migration, want true")
	}
	if !s.Has("M81_sub", "b.fit", 2048) {
		t.Error("Has(M81_sub, b.fit, 2048) = false after migration, want true")
	}

	// The migrated state must be persisted back to disk so subsequent loads
	// skip the migration path.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var reload struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &reload); err != nil {
		t.Fatal(err)
	}
	if reload.Version != 2 {
		t.Errorf("on-disk version after migration = %d, want 2", reload.Version)
	}
}

func TestHas_SizeMismatch(t *testing.T) {
	s, err := LoadState(tempStatePath(t))
	if err != nil {
		t.Fatal(err)
	}
	entry := SyncEntry{
		Folder:     "F",
		Filename:   "x.fit",
		Size:       1000,
		UploadedAt: time.Now().UTC(),
	}
	if err := s.Mark(entry); err != nil {
		t.Fatal(err)
	}
	if !s.Has("F", "x.fit", 1000) {
		t.Error("exact-size match should return true")
	}
	if s.Has("F", "x.fit", 999) {
		t.Error("size mismatch should return false — truncated/regrown files need re-upload")
	}
	if s.Has("F", "y.fit", 1000) {
		t.Error("unknown filename should return false")
	}
	if s.Has("OTHER", "x.fit", 1000) {
		t.Error("different folder should return false")
	}
}

func TestMark_PersistsAcrossReload(t *testing.T) {
	path := tempStatePath(t)
	s1, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.Mark(SyncEntry{Folder: "F", Filename: "x.fit", Size: 100}); err != nil {
		t.Fatal(err)
	}
	// Fresh load must see the persisted entry.
	s2, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.Has("F", "x.fit", 100) {
		t.Fatal("Mark did not persist across reload")
	}
}

func TestMarkBatch_SingleWrite(t *testing.T) {
	path := tempStatePath(t)
	s, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	batch := []SyncEntry{
		{Folder: "F", Filename: "a.fit", Size: 1},
		{Folder: "F", Filename: "b.fit", Size: 2},
		{Folder: "G", Filename: "c.fit", Size: 3},
	}
	if err := s.MarkBatch(batch); err != nil {
		t.Fatal(err)
	}
	s2, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.Has("F", "a.fit", 1) || !s2.Has("F", "b.fit", 2) || !s2.Has("G", "c.fit", 3) {
		t.Fatal("MarkBatch did not persist all entries")
	}
	// Empty batch is a no-op, no error.
	if err := s.MarkBatch(nil); err != nil {
		t.Errorf("empty MarkBatch should not error, got %v", err)
	}
}

func TestCountFolder(t *testing.T) {
	s, err := LoadState(tempStatePath(t))
	if err != nil {
		t.Fatal(err)
	}
	entries := []SyncEntry{
		{Folder: "A", Filename: "1.fit", Size: 1},
		{Folder: "A", Filename: "2.fit", Size: 1},
		{Folder: "A", Filename: "3.fit", Size: 1},
		{Folder: "B", Filename: "1.fit", Size: 1},
	}
	if err := s.MarkBatch(entries); err != nil {
		t.Fatal(err)
	}
	if got := s.CountFolder("A"); got != 3 {
		t.Errorf("CountFolder(A) = %d, want 3", got)
	}
	if got := s.CountFolder("B"); got != 1 {
		t.Errorf("CountFolder(B) = %d, want 1", got)
	}
	if got := s.CountFolder("MISSING"); got != 0 {
		t.Errorf("CountFolder(MISSING) = %d, want 0", got)
	}
}

func TestMark_OverwriteSameKey(t *testing.T) {
	// Re-uploading a resized file should replace the entry, not add a
	// second one — same (folder, filename) key.
	s, err := LoadState(tempStatePath(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Mark(SyncEntry{Folder: "F", Filename: "x.fit", Size: 100}); err != nil {
		t.Fatal(err)
	}
	if err := s.Mark(SyncEntry{Folder: "F", Filename: "x.fit", Size: 200}); err != nil {
		t.Fatal(err)
	}
	if s.CountFolder("F") != 1 {
		t.Errorf("re-Mark same key produced %d entries, want 1", s.CountFolder("F"))
	}
	if !s.Has("F", "x.fit", 200) {
		t.Error("re-Mark did not update size to 200")
	}
	if s.Has("F", "x.fit", 100) {
		t.Error("old size still matches after re-Mark — overwrite failed")
	}
}
