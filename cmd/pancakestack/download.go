package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/glennbech/pancakestack-cli/internal/api"
	"github.com/glennbech/pancakestack-cli/internal/config"
	"github.com/spf13/cobra"
)

func newDownloadCmd() *cobra.Command {
	var concurrency int
	var noResume bool
	c := &cobra.Command{
		Use:   "download <collection-id> [dest]",
		Short: "Download every file in a collection to a local directory",
		Long: "Downloads every file in the given collection to a local directory.\n\n" +
			"Default dest is ./<collection-id>/. Files already on disk with a\n" +
			"matching size are skipped, so re-running after a crash resumes\n" +
			"where it left off. Pass --no-resume to force a full re-download.\n\n" +
			"Example:\n" +
			"  pancakestack download m101              # → ./m101/\n" +
			"  pancakestack download m101 /tmp/lights  # → /tmp/lights/",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			collectionID := args[0]
			ctx := cmd.Context()

			if !validCollectionID(collectionID) {
				return fmt.Errorf("collectionId %q not allowed — use [a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}", collectionID)
			}
			dest := collectionID
			if len(args) == 2 {
				dest = args[1]
			}
			absDest, err := filepath.Abs(dest)
			if err != nil {
				return fmt.Errorf("resolve dest %s: %w", dest, err)
			}

			client, err := api.New(config.BackendURL(backendURL))
			if err != nil {
				return err
			}

			fmt.Fprintf(os.Stderr, "Listing files in collection %q...\n", collectionID)
			files, err := client.ListCollectionFiles(ctx, collectionID)
			if err != nil {
				return err
			}
			if len(files) == 0 {
				fmt.Fprintf(os.Stderr, "Collection %q is empty — nothing to download.\n", collectionID)
				return nil
			}

			var totalBytes int64
			for _, f := range files {
				totalBytes += f.SizeBytes
			}
			fmt.Fprintf(os.Stderr, "Downloading %d files (%.1f MB total) → %s, concurrency=%d\n",
				len(files), mib(totalBytes), absDest, concurrency)

			started := time.Now()
			var completedBytes int64
			var lastPrint time.Time
			var printMu sync.Mutex

			results, err := client.DownloadFilesBulk(ctx, api.BulkDownloadOptions{
				CollectionID: collectionID,
				Files:        files,
				DestDir:      absDest,
				Concurrency:  concurrency,
				SkipExisting: !noResume,
				OnFileDone: func(name string, size int64, skipped bool, done, total int) {
					atomic.AddInt64(&completedBytes, size)
					printMu.Lock()
					defer printMu.Unlock()
					now := time.Now()
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

			var okCount, skipCount, failCount int
			for _, r := range results {
				switch {
				case r.Skipped:
					skipCount++
				case r.Error != nil:
					failCount++
					fmt.Fprintf(os.Stderr, "  ✗ %s: %v\n", r.Name, r.Error)
				default:
					okCount++
				}
			}
			elapsed := time.Since(started)
			summary := fmt.Sprintf("✓ %d downloaded", okCount)
			if skipCount > 0 {
				summary += fmt.Sprintf(", %d already present", skipCount)
			}
			if failCount > 0 {
				summary += fmt.Sprintf(", %d failed", failCount)
			}
			rate := 0.0
			if elapsed.Seconds() > 0 {
				rate = mib(totalBytes) / elapsed.Seconds()
			}
			fmt.Fprintf(os.Stderr, "%s in %s (%.1f MiB/s avg)\n",
				summary, elapsed.Truncate(time.Second), rate)

			if failCount > 0 {
				return fmt.Errorf("%d file(s) failed to download", failCount)
			}
			return nil
		},
	}
	c.Flags().IntVar(&concurrency, "concurrency", 4,
		"Max concurrent GETs from S3. Higher = faster on fibre, slower on flaky links.")
	c.Flags().BoolVar(&noResume, "no-resume", false,
		"Re-download files even if a same-size local copy already exists.")
	return c
}
