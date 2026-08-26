// Command pancakestack — CLI for the pancakestack astro-stacking service.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/glennbech/pancakestack-cli/internal/api"
	"github.com/glennbech/pancakestack-cli/internal/config"
	"github.com/spf13/cobra"
)

// Version is set at build time via -ldflags "-X main.Version=..."
var Version = "dev"

// backendURL is a package-level flag captured by the root command's persistent
// flag, read by subcommands via config.BackendURL(backendURL).
var backendURL string

// jsonErrors is set by --json at the root command level; changes error
// rendering from human-friendly stderr text to a structured JSON payload
// on stdout for scripting. Currently only STORAGE_QUOTA_EXCEEDED emits a
// structured shape; other errors fall back to a small `{error, code}` JSON
// envelope so callers can rely on the shape being valid JSON when the
// flag is set.
var jsonErrors bool

func main() {
	root := &cobra.Command{
		Use:     "pancakestack",
		Short:   "Stack your astro photos in the cloud",
		Long:    "pancakestack — point it at a folder of light frames, wait a bit, get a stacked FITS back.",
		Version: Version,
		// PersistentPreRun runs after flag parsing, before every subcommand's
		// RunE. Fetches the ops banner (unstability warnings, downtime) from
		// GET /stats and prints it in yellow on stderr. Best-effort with a
		// hard 1.5s timeout inside FetchOpsBanner — a slow or unreachable
		// backend never blocks the actual command.
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			banner, _ := api.FetchOpsBanner(cmd.Context(), config.BackendURL(backendURL))
			if banner == "" {
				return
			}
			printOpsBanner(banner)
		},
	}
	root.PersistentFlags().StringVar(&backendURL, "url", "", "Backend URL (overrides PANCAKESTACK_URL and compiled default)")
	root.PersistentFlags().BoolVar(&jsonErrors, "json", false, "Emit errors as JSON on stdout instead of human text on stderr (for scripting)")

	root.AddCommand(newLoginCmd())
	root.AddCommand(newLogoutCmd())
	root.AddCommand(newWhoamiCmd())
	root.AddCommand(newUploadCmd())
	root.AddCommand(newDownloadCmd())
	root.AddCommand(newStackCmd())
	root.AddCommand(newJobsCmd())
	root.AddCommand(newCancelCmd())
	root.AddCommand(newLogsCmd())
	root.AddCommand(newMetricsCmd())
	root.AddCommand(newAskCmd())
	root.AddCommand(newArchiveCmd())
	root.AddCommand(newUnarchiveCmd())
	root.AddCommand(newSeestarCmd())

	// Suppress cobra's own "Error:" line so the not-activated case can
	// print the exact one-liner the product wants, and every other error
	// still routes through our single fmt.Fprintln below.
	root.SilenceErrors = true

	if err := root.Execute(); err != nil {
		var quotaErr *api.StorageQuotaExceededError
		switch {
		case errors.As(err, &quotaErr):
			printStorageQuotaError(quotaErr)
		case errors.Is(err, api.ErrNotActivated):
			if jsonErrors {
				printJSONError("NOT_ACTIVATED", err.Error())
			} else {
				fmt.Fprintln(os.Stderr, err.Error())
			}
		default:
			if jsonErrors {
				printJSONError("ERROR", err.Error())
			} else {
				fmt.Fprintln(os.Stderr, "error:", err)
			}
		}
		os.Exit(1)
	}
}

// printStorageQuotaError renders the 402 quota error in the appropriate
// form: human string on stderr by default, structured JSON on stdout when
// --json is set. Copy locked in pancakestack repo issue #7 UX section.
func printStorageQuotaError(e *api.StorageQuotaExceededError) {
	if jsonErrors {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(struct {
			Code      string `json:"code"`
			Error     string `json:"error"`
			Used      int64  `json:"used"`
			Quota     int64  `json:"quota"`
			Requested int64  `json:"requested"`
			Shortfall int64  `json:"shortfall"`
		}{
			Code:      "STORAGE_QUOTA_EXCEEDED",
			Error:     e.Message,
			Used:      e.Used,
			Quota:     e.Quota,
			Requested: e.Requested,
			Shortfall: e.Shortfall,
		})
		return
	}
	fmt.Fprintf(os.Stderr,
		"Upload blocked: this would exceed your storage cap by %s "+
			"(currently using %s of %s). Free space with `pancakestack collection delete <name>` "+
			"or upgrade at https://pancakestack.net/profile\n",
		humanBytes(e.Shortfall), humanBytes(e.Used), humanBytes(e.Quota),
	)
}

// printJSONError emits a minimal JSON envelope for non-quota errors when
// --json is set. Keeps `{code, error}` stable so scripts can parse.
func printJSONError(code, msg string) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}{Code: code, Error: msg})
}

// humanBytes formats a byte count as e.g. "1.4 GB" for CLI display.
// Deliberately decimal (matches everyday user expectation — 1 GB = 1e9)
// even though S3 quota math is base-1024. The discrepancy is < 7% and
// the friendlier read wins for a CLI error.
func humanBytes(b int64) string {
	const (
		kb = 1000
		mb = kb * 1000
		gb = mb * 1000
		tb = gb * 1000
	)
	switch {
	case b >= tb:
		return fmt.Sprintf("%.1f TB", float64(b)/float64(tb))
	case b >= gb:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(kb))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// printOpsBanner renders the backend's ops message as a yellow band on
// stderr — one blank line, "! <msg>" in yellow, one blank line. Honors
// NO_COLOR (https://no-color.org) and skips ANSI when stderr isn't a TTY
// so piping stays clean.
func printOpsBanner(msg string) {
	const (
		reset  = "\033[0m"
		yellow = "\033[33m"
	)
	if noColor() {
		fmt.Fprintf(os.Stderr, "\n! %s\n\n", msg)
		return
	}
	fmt.Fprintf(os.Stderr, "\n%s! %s%s\n\n", yellow, msg, reset)
}

func noColor() bool {
	if os.Getenv("NO_COLOR") != "" {
		return true
	}
	fi, err := os.Stderr.Stat()
	if err != nil {
		return true
	}
	return (fi.Mode() & os.ModeCharDevice) == 0
}
