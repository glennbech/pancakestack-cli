// Package archive builds a tarball on the fly from a source directory or
// file. Skips .DS_Store, __MACOSX/, and non-FITS AppleDouble sidecars
// (._foo but NOT ._foo.fit) — a user with a legitimate FITS named with
// a leading dot should still be able to upload it; only true macOS
// resource-fork noise gets dropped.
package archive

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Source describes what to tar and estimated size for progress display.
type Source struct {
	Path      string
	FileCount int
	TotalSize int64 // bytes across all files that will end up in the tar
}

// InspectSource walks the source path (must be a directory of files, or a
// single tar/zip file). For directories, counts eligible files and total
// byte size. For a single archive file, uses its size directly.
func InspectSource(path string) (*Source, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	if !info.IsDir() {
		// Already an archive — will be passed through as-is.
		return &Source{Path: path, FileCount: 1, TotalSize: info.Size()}, nil
	}
	s := &Source{Path: path}
	err = filepath.Walk(path, func(p string, i os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if i.IsDir() {
			if isJunkDir(i.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if isJunkFile(i.Name()) {
			return nil
		}
		s.FileCount++
		s.TotalSize += i.Size()
		return nil
	})
	if err != nil {
		return nil, err
	}
	if s.FileCount == 0 {
		return nil, errors.New("no files to include (all filtered out or directory is empty)")
	}
	return s, nil
}

// TarDirTo streams a plain uncompressed tar of `dir` to `w`. Files land at
// the top level of the tar (no top-level directory), so extraction produces
// a flat set of files that the backend's user-data will then move into
// input/lights/ before Siril runs.
func TarDirTo(dir string, w io.Writer) error {
	tw := tar.NewWriter(w)
	defer tw.Close()
	return filepath.Walk(dir, func(p string, i os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if i.IsDir() {
			if isJunkDir(i.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if isJunkFile(i.Name()) {
			return nil
		}
		// Relative name inside the tar, without leading dir.
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		hdr := &tar.Header{
			Name:    rel,
			Size:    i.Size(),
			Mode:    int64(i.Mode().Perm()),
			ModTime: i.ModTime(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		_, cpErr := io.Copy(tw, f)
		f.Close()
		return cpErr
	})
}

func isJunkFile(name string) bool {
	if name == ".DS_Store" {
		return true
	}
	if strings.HasPrefix(name, "._") {
		// AppleDouble sidecars are typically resource-fork noise, but a
		// user with a legit FITS name starting with `._` should still
		// upload — accept the small risk of some real macOS junk making
		// it through in exchange for not silently dropping user data.
		lower := strings.ToLower(name)
		if strings.HasSuffix(lower, ".fit") ||
			strings.HasSuffix(lower, ".fits") ||
			strings.HasSuffix(lower, ".fz") {
			return false
		}
		return true
	}
	return false
}

func isJunkDir(name string) bool {
	return name == "__MACOSX"
}
