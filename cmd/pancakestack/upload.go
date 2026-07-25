package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/glennbech/pancakestack-cli/internal/api"
	"github.com/glennbech/pancakestack-cli/internal/archive"
	"github.com/glennbech/pancakestack-cli/internal/config"
	"github.com/spf13/cobra"
)

func newUploadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "upload <collection-id> <path>",
		Short: "Tar a directory of light frames and upload as a named collection",
		Long: "Tars a directory (or uses a single tar file as-is) and PUTs it to a " +
			"presigned S3 URL the backend generates. The upload lives under your " +
			"user namespace on S3; other users can't touch it.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			collectionID := args[0]
			path := args[1]
			ctx := cmd.Context()

			// Poor-man's collectionId validation client-side. Backend also enforces.
			if !validCollectionID(collectionID) {
				return fmt.Errorf("collectionId %q not allowed — use [a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}", collectionID)
			}

			src, err := archive.InspectSource(path)
			if err != nil {
				return err
			}
			isPreBuilt := isTarOrZipFile(path)
			if isPreBuilt {
				fmt.Fprintf(os.Stderr, "Using existing archive: %s (%.1f MB)\n", path, mib(src.TotalSize))
			} else {
				fmt.Fprintf(os.Stderr, "Tarring %d files (%.1f MB total) from %s\n",
					src.FileCount, mib(src.TotalSize), path)
			}

			client, err := api.New(config.BackendURL(backendURL))
			if err != nil {
				return err
			}

			fmt.Fprintf(os.Stderr, "Requesting upload URL for collection %q...\n", collectionID)
			upResp, err := client.RequestUploadURL(ctx, collectionID)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "  → %s\n", upResp.Key)

			// Upload. For pre-built archives, stream from disk (known length).
			// For directory-tar-on-the-fly, we don't know exact tar size, so we
			// buffer to a temp file first for content-length + retry safety.
			var reader io.Reader
			var length int64
			if isPreBuilt {
				f, err := os.Open(path)
				if err != nil {
					return err
				}
				defer f.Close()
				reader = f
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
				fmt.Fprintf(os.Stderr, "Built local tar: %.1f MB (took %.1fs)\n",
					mib(length), 0.0) // TODO: time the tar step
				reader = tmp
			}

			fmt.Fprintf(os.Stderr, "Uploading %.1f MB to S3...\n", mib(length))
			started := time.Now()
			if err := client.PutToPresignedURL(ctx, upResp.UploadURL, reader, length); err != nil {
				return err
			}
			elapsed := time.Since(started)
			fmt.Fprintf(os.Stderr, "✓ Upload complete in %s (%.1f MiB/s)\n",
				elapsed.Truncate(time.Second),
				mib(length)/elapsed.Seconds())
			fmt.Printf("Uploaded to %s\n", upResp.Key)
			return nil
		},
	}
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

func isTarOrZipFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	l := strings.ToLower(path)
	return strings.HasSuffix(l, ".tar") ||
		strings.HasSuffix(l, ".tar.gz") ||
		strings.HasSuffix(l, ".tgz") ||
		strings.HasSuffix(l, ".tar.zst") ||
		strings.HasSuffix(l, ".zip")
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

func newStackCmd() *cobra.Command {
	var scriptID string
	var instanceType string
	var params []string
	c := &cobra.Command{
		Use:   "stack <collection-id>",
		Short: "Kick off a stack of a previously-uploaded collection",
		Args:  cobra.ExactArgs(1),
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
	c.Flags().StringVar(&scriptID, "script", "", "Catalog script id (seestar | seestar-drizzle | raw-basic)")
	c.Flags().StringSliceVar(&params, "param", nil, "Repeatable. key=value")
	c.Flags().StringVar(&instanceType, "instance", "", "EC2 instance type")
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
