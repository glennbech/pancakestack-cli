// Command pancakestack — CLI for the pancakestack astro-stacking service.
package main

import (
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
		if errors.Is(err, api.ErrNotActivated) {
			fmt.Fprintln(os.Stderr, err.Error())
		} else {
			fmt.Fprintln(os.Stderr, "error:", err)
		}
		os.Exit(1)
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
