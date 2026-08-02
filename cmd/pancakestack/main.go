// Command pancakestack — CLI for the pancakestack astro-stacking service.
package main

import (
	"fmt"
	"os"

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
	}
	root.PersistentFlags().StringVar(&backendURL, "url", "", "Backend URL (overrides PANCAKESTACK_URL and compiled default)")

	root.AddCommand(newLoginCmd())
	root.AddCommand(newLogoutCmd())
	root.AddCommand(newWhoamiCmd())
	root.AddCommand(newUploadCmd())
	root.AddCommand(newStackCmd())
	root.AddCommand(newJobsCmd())
	root.AddCommand(newLogsCmd())

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
