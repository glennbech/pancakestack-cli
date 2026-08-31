package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/glennbech/pancakestack-cli/internal/api"
	"github.com/glennbech/pancakestack-cli/internal/archive"
	"github.com/glennbech/pancakestack-cli/internal/config"
	"github.com/spf13/cobra"
)

// archiveNameFor picks the S3 basename we'll ask the backend to presign.
// Preserves whatever name the user actually gave us so the S3 object,
// error messages, and pending-uploads UI card all show something the user
// recognises — not a generic `lights.tar` for every upload from every
// telescope on every night. Two cases:
//
//  1. Pre-built archive → preserve `filepath.Base(sourcePath)` verbatim.
//     `~/night1.tar.zst` becomes `night1.tar.zst` on S3.
//
//  2. Directory that we tar locally → use the directory's basename + `.tar`.
//     `~/telescope/2026-08-15/` becomes `2026-08-15.tar` on S3. Falls back
//     to `<collectionId>.tar` if the source path resolves to something
//     without a usable basename (e.g. "." or "/").
//
// Historical: earlier versions hardcoded `lights.tar` for every upload.
// Two consequences that pushed us off that: (a) all archives from the same
// user in the same collection collided on the S3 key (silent overwrite),
// (b) the pending-uploads UI card and STILL_NORMALIZING error messages
// always said "lights.tar" regardless of what the user actually uploaded,
// which was confusing when multiple sessions were in flight.
func archiveNameFor(sourcePath, collectionID string, isPreBuilt bool) string {
	if isPreBuilt {
		return filepath.Base(sourcePath)
	}
	dir := filepath.Base(filepath.Clean(sourcePath))
	if dir == "." || dir == "/" || dir == "" {
		dir = collectionID
	}
	return dir + ".tar"
}

func newUploadCmd() *cobra.Command {
	var concurrency int
	c := &cobra.Command{
		Use:   "upload <collection-id> <path>...",
		Short: "Upload FITS files (or an archive) to a named collection",
		Long: "Uploads N individual files, or a single tar/zip/rar archive, to a\n" +
			"named collection.\n\n" +
			"Modes:\n" +
			"  pancakestack upload m101 *.fits           # multi-file: one object per file\n" +
			"  pancakestack upload m101 lights.tar       # transport: extracts on next job\n" +
			"  pancakestack upload m101 /path/to/dir     # directory: tars locally, uploads once\n\n" +
			"Under the FITS-primary storage model, individual FITS uploads are the\n" +
			"recommended path — no local tar step, per-file retry, resumable across\n" +
			"crashes (files already uploaded skip on re-run).\n\n" +
			"To archive a collection to cold storage after upload:\n" +
			"  pancakestack archive <collection-id>",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			collectionID := args[0]
			paths := args[1:]
			ctx := cmd.Context()

			if !validCollectionID(collectionID) {
				return fmt.Errorf("collectionId %q not allowed — use [a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}", collectionID)
			}

			client, err := api.New(config.BackendURL(backendURL))
			if err != nil {
				return err
			}

			// Multi-file mode: >1 path, OR one path that isn't a dir/archive.
			// Single-path-that-is-dir stays on the tar-and-multipart route
			// because we can't sensibly present N-thousand file uploads
			// without progress + we already have proven directory support.
			if len(paths) > 1 || isFitsFile(paths[0]) {
				return runMultiFileUpload(ctx, client, collectionID, paths, concurrency)
			}
			return runArchiveUpload(ctx, client, collectionID, paths[0])
		},
	}
	c.Flags().IntVar(&concurrency, "concurrency", 4,
		"Max concurrent uploads during multi-file mode. Higher = faster on fibre, slower on flaky links.")
	return c
}

// runMultiFileUpload handles N individual files. Backend preserves each
// filename verbatim under the collection prefix.
func runMultiFileUpload(ctx context.Context, client *api.Client, collectionID string, paths []string, concurrency int) error {
	// Validate + stat everything upfront. Total size drives the ETA.
	var totalBytes int64
	seen := make(map[string]string, len(paths))
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("stat %s: %w", p, err)
		}
		if info.IsDir() {
			return fmt.Errorf("%s is a directory — pass individual files, or a single tar/zip/rar", p)
		}
		base := filepath.Base(p)
		if prev, ok := seen[base]; ok {
			return fmt.Errorf("duplicate filename %q (from %s and %s) — S3 keys collide, dedupe before uploading",
				base, prev, p)
		}
		seen[base] = p
		totalBytes += info.Size()
	}
	fmt.Fprintf(os.Stderr, "Uploading %d files (%.1f MB total) to collection %q, concurrency=%d\n",
		len(paths), mib(totalBytes), collectionID, concurrency)

	// Hash every file before we ask the backend for URLs. Backend uses the
	// sha256 to reject duplicates so we never spend bandwidth re-uploading
	// something the collection already has. CPU-bound; use all cores.
	shas, err := hashFilesInParallel(ctx, paths, runtime.NumCPU())
	if err != nil {
		return err
	}

	started := time.Now()
	var completedBytes int64
	var lastPrint time.Time
	var printMu sync.Mutex

	results, err := client.UploadFilesBulk(ctx, api.BulkUploadOptions{
		CollectionID:   collectionID,
		Paths:          paths,
		SHA256s:        shas,
		Concurrency:    concurrency,
		OnFileDone: func(name string, done, total int) {
			if info, err := os.Stat(seen[name]); err == nil {
				atomic.AddInt64(&completedBytes, info.Size())
			}
			printMu.Lock()
			defer printMu.Unlock()
			now := time.Now()
			// Print on last file or throttle to ~4 Hz.
			if done < total && now.Sub(lastPrint) < 250*time.Millisecond {
				return
			}
			lastPrint = now
			elapsed := now.Sub(started).Seconds()
			doneBytes := atomic.LoadInt64(&completedBytes)
			rate := 0.0
			if elapsed > 0 {
				rate = mib(doneBytes) / elapsed
			}
			fmt.Fprintf(os.Stderr, "\r  %d/%d files · %.1f/%.1f MB · %.1f MiB/s   ",
				done, total, mib(doneBytes), mib(totalBytes), rate)
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr)
		return err
	}
	fmt.Fprintln(os.Stderr)

	// Aggregate outcomes: OK, skipped (duplicate, not an error), failed.
	var okCount, failCount, skipCount int
	for _, r := range results {
		switch {
		case r.Skipped:
			skipCount++
			if r.DuplicateOf != "" && r.DuplicateOf != r.Name {
				fmt.Fprintf(os.Stderr, "  · %s: already in this collection (matches %s)\n", r.Name, r.DuplicateOf)
			} else {
				fmt.Fprintf(os.Stderr, "  · %s: already in this collection\n", r.Name)
			}
		case r.Error != nil:
			failCount++
			fmt.Fprintf(os.Stderr, "  ✗ %s: %v\n", r.Name, r.Error)
		default:
			okCount++
		}
	}
	elapsed := time.Since(started)
	summary := fmt.Sprintf("✓ %d uploaded", okCount)
	if skipCount > 0 {
		summary += fmt.Sprintf(", %d duplicate skipped", skipCount)
	}
	if failCount > 0 {
		summary += fmt.Sprintf(", %d failed", failCount)
	}
	fmt.Fprintf(os.Stderr, "%s in %s (%.1f MiB/s avg)\n",
		summary,
		elapsed.Truncate(time.Second),
		mib(totalBytes)/elapsed.Seconds())

	if failCount > 0 {
		return fmt.Errorf("%d file(s) failed to upload", failCount)
	}
	return nil
}

// hashFilesInParallel computes SHA-256 for each path, running up to
// `workers` in parallel. Returns hashes in the same order as paths. On
// any hash failure, returns the first error observed. Progress is
// printed to stderr so long batches (thousands of files) show life.
func hashFilesInParallel(ctx context.Context, paths []string, workers int) ([]string, error) {
	if workers < 1 {
		workers = 1
	}
	if workers > len(paths) {
		workers = len(paths)
	}
	shas := make([]string, len(paths))
	errs := make([]error, len(paths))

	total := int64(len(paths))
	var done int64
	var lastPrint time.Time
	var printMu sync.Mutex
	started := time.Now()

	fmt.Fprintf(os.Stderr, "Hashing %d files (SHA-256)...\n", len(paths))

	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i, p := range paths {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, p string) {
			defer wg.Done()
			defer func() { <-sem }()
			if ctx.Err() != nil {
				errs[i] = ctx.Err()
				return
			}
			sum, err := hashFile(p)
			if err != nil {
				errs[i] = err
				return
			}
			shas[i] = sum

			n := atomic.AddInt64(&done, 1)
			printMu.Lock()
			defer printMu.Unlock()
			now := time.Now()
			if n < total && now.Sub(lastPrint) < 250*time.Millisecond {
				return
			}
			lastPrint = now
			fmt.Fprintf(os.Stderr, "\r  %d/%d files hashed   ", n, total)
		}(i, p)
	}
	wg.Wait()
	fmt.Fprintf(os.Stderr, "\r  %d/%d files hashed in %s\n",
		done, total, time.Since(started).Truncate(time.Millisecond))

	for _, err := range errs {
		if err != nil {
			return nil, fmt.Errorf("hash: %w", err)
		}
	}
	return shas, nil
}

// hashFile returns the lowercase-hex SHA-256 of the file at path.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// runArchiveUpload is the legacy single-archive path — kept because a
// user with an existing tar (or a directory to tar) shouldn't be forced
// to expand it locally first.
func runArchiveUpload(ctx context.Context, client *api.Client, collectionID, path string) error {
	src, err := archive.InspectSource(path)
	if err != nil {
		return err
	}
	isPreBuilt := isArchiveFile(path)
	if isPreBuilt {
		fmt.Fprintf(os.Stderr, "Using existing archive: %s (%.1f MB)\n", path, mib(src.TotalSize))
	} else {
		fmt.Fprintf(os.Stderr, "Tarring %d files (%.1f MB total) from %s\n",
			src.FileCount, mib(src.TotalSize), path)
	}

	archiveName := archiveNameFor(path, collectionID, isPreBuilt)

	var data *os.File
	var length int64
	if isPreBuilt {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		data = f
		length = src.TotalSize
	} else {
		tmp, err := tarToTempFile(path)
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		defer tmp.Close()
		info, _ := tmp.Stat()
		length = info.Size()
		fmt.Fprintf(os.Stderr, "Built local tar: %.1f MB\n", mib(length))
		data = tmp
	}

	// Skip client-side dedup for archive uploads. Tar/zip output rarely
	// hashes identically across runs (mtimes, file ordering), so the
	// SHA-256 is ~pure overhead — for a 38 GB tar it's 5+ min of CPU
	// just to send a value the backend won't dedup on.
	fmt.Fprintf(os.Stderr, "Uploading %.1f MB (multipart, archive %s)...\n",
		mib(length), archiveName)
	started := time.Now()
	progress := newProgressPrinter(started)
	done, err := client.UploadFileMultipart(ctx, api.MultipartUploadOptions{
		CollectionID: collectionID,
		Filename:     archiveName,
		Data:         data,
		Size:         length,
		OnProgress:   progress.update,
	})
	if err != nil {
		progress.finish()
		return err
	}
	progress.finish()
	if done.Skipped {
		if done.DuplicateOf != "" && done.DuplicateOf != archiveName {
			fmt.Fprintf(os.Stderr, "· skipped: this archive is already in collection %q (matches %s)\n",
				collectionID, done.DuplicateOf)
		} else {
			fmt.Fprintf(os.Stderr, "· skipped: this archive is already in collection %q\n", collectionID)
		}
		return nil
	}
	elapsed := time.Since(started)
	fmt.Fprintf(os.Stderr, "✓ Upload complete in %s (%.1f MiB/s)\n",
		elapsed.Truncate(time.Second),
		mib(length)/elapsed.Seconds())
	fmt.Printf("Uploaded to %s\n", done.Key)
	return nil
}

// isFitsFile returns true for a path ending in .fit/.fits/.fz. Used to
// route "single-file" uploads: a lone `.fit` should take the multi-file
// path (individual upload), not the archive path.
func isFitsFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	l := strings.ToLower(path)
	return strings.HasSuffix(l, ".fit") ||
		strings.HasSuffix(l, ".fits") ||
		strings.HasSuffix(l, ".fz")
}

// tarToTempFile builds a tarball of `dir` into a new tempfile and returns
// the open handle rewound to the start.
func tarToTempFile(dir string) (*os.File, error) {
	f, err := os.CreateTemp("", "pancakestack-*.tar")
	if err != nil {
		return nil, err
	}
	if err := archive.TarDirTo(dir, f); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	return f, nil
}

func isArchiveFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	l := strings.ToLower(path)
	return strings.HasSuffix(l, ".tar") ||
		strings.HasSuffix(l, ".tar.gz") ||
		strings.HasSuffix(l, ".tgz") ||
		strings.HasSuffix(l, ".tar.zst") ||
		strings.HasSuffix(l, ".zip") ||
		strings.HasSuffix(l, ".rar")
}

func validCollectionID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	first := s[0]
	if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z') || (first >= '0' && first <= '9')) {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '_' || c == '.' || c == '-':
		default:
			return false
		}
	}
	return true
}

func mib(bytes int64) float64 { return float64(bytes) / (1024 * 1024) }

// progressPrinter renders a single self-overwriting stderr line as multipart
// parts complete. Throttled so a fast link doesn't spam the terminal.
type progressPrinter struct {
	started time.Time
	mu      sync.Mutex
	lastAt  time.Time
	printed bool
}

func newProgressPrinter(started time.Time) *progressPrinter {
	return &progressPrinter{started: started}
}

func (p *progressPrinter) update(uploaded, total int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	// Always render the last part (100%). Otherwise throttle to ~4 Hz.
	if uploaded < total && now.Sub(p.lastAt) < 250*time.Millisecond {
		return
	}
	p.lastAt = now
	elapsed := now.Sub(p.started).Seconds()
	rate := 0.0
	if elapsed > 0 {
		rate = mib(uploaded) / elapsed
	}
	pct := 100.0 * float64(uploaded) / float64(total)
	fmt.Fprintf(os.Stderr, "\r  %.1f / %.1f MiB (%.1f%%) at %.1f MiB/s   ",
		mib(uploaded), mib(total), pct, rate)
	p.printed = true
}

func (p *progressPrinter) finish() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.printed {
		fmt.Fprintln(os.Stderr)
	}
}

func newStackCmd() *cobra.Command {
	var scriptID string
	var instanceType string
	var params []string
	var files []string
	c := &cobra.Command{
		Use:   "stack <collection-id>",
		Short: "Kick off a stack of a previously-uploaded collection",
		Long: "Kick off a stack of a previously-uploaded collection.\n\n" +
			"Under frame-based pricing the backend picks the compute tier from\n" +
			"the workload shape (frame count + drizzle) — same as the SaaS\n" +
			"webapp. --instance is an admin-privileged override; the backend\n" +
			"rejects it for non-admin users.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			collectionID := args[0]
			ctx := cmd.Context()

			paramMap, err := parseParams(params)
			if err != nil {
				return err
			}

			client, err := api.New(config.BackendURL(backendURL))
			if err != nil {
				return err
			}
			resp, err := client.Stack(ctx, api.StackRequest{
				CollectionID: collectionID,
				ScriptID:     scriptID,
				Params:       paramMap,
				InstanceType: instanceType,
				IncludeFiles: files,
			})
			if err != nil {
				return err
			}
			fmt.Printf("✓ Job launched\n")
			fmt.Printf("  jobId:      %s\n", resp.JobID)
			fmt.Printf("  runId:      %s\n", resp.RunID)
			fmt.Printf("  instance:   %s\n", resp.InstanceID)
			fmt.Printf("  script:     %s\n", resp.ScriptID)
			fmt.Printf("  output:     %s\n", resp.OutputPrefix)
			return nil
		},
	}
	c.Flags().StringVar(&scriptID, "script", "", "Catalog script id (seestar | seestar-advanced)")
	c.Flags().StringSliceVar(&params, "param", nil, "Repeatable. key=value")
	c.Flags().StringVar(&instanceType, "instance", "", "[admin-privileged] EC2 instance type override. Backend picks the tier from workload shape for non-admin users and rejects the override.")
	c.Flags().StringSliceVar(&files, "files", nil, "Comma-separated filename allowlist — only these frames get stacked (default: whole collection). Filenames must match what's on S3 exactly.")
	return c
}

// parseParams turns ["drizzle=2","pixelFraction=1.0","rgbEqualize=true"] into
// a typed map[string]any where numbers are numbers, bools are bools, else strings.
func parseParams(kvs []string) (map[string]any, error) {
	if len(kvs) == 0 {
		return nil, nil
	}
	out := make(map[string]any, len(kvs))
	for _, kv := range kvs {
		eq := strings.Index(kv, "=")
		if eq < 1 {
			return nil, fmt.Errorf("--param %q: expected key=value", kv)
		}
		k := kv[:eq]
		v := kv[eq+1:]
		out[k] = coerceParam(v)
	}
	return out, nil
}

// coerceParam picks a JSON type for a raw string value: int → int, float → float,
// "true"/"false" → bool, everything else → string.
func coerceParam(v string) any {
	if v == "true" {
		return true
	}
	if v == "false" {
		return false
	}
	// int first (whole numbers), then float.
	if isInt(v) {
		n := int64(0)
		fmt.Sscanf(v, "%d", &n)
		return n
	}
	if isFloat(v) {
		var f float64
		fmt.Sscanf(v, "%f", &f)
		return f
	}
	return v
}

func isInt(s string) bool {
	if s == "" {
		return false
	}
	i := 0
	if s[0] == '-' {
		if len(s) == 1 {
			return false
		}
		i = 1
	}
	for ; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isFloat(s string) bool {
	if s == "" {
		return false
	}
	dot := false
	i := 0
	if s[0] == '-' {
		if len(s) == 1 {
			return false
		}
		i = 1
	}
	for ; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c == '.' && !dot:
			dot = true
		default:
			return false
		}
	}
	return dot
}
