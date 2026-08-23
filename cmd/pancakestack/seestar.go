package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/glennbech/pancakestack-cli/internal/api"
	"github.com/glennbech/pancakestack-cli/internal/config"
	"github.com/glennbech/pancakestack-cli/internal/seestar"
	"github.com/spf13/cobra"
)

func newSeestarCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "seestar",
		Short: "Talk to a ZWO Seestar smart telescope on your local network",
		Long: "Local-network integration with a ZWO Seestar smart telescope.\n\n" +
			"The scope must be in station mode (joined your home Wi-Fi, not\n" +
			"hotspot mode) so this laptop can reach both it AND the internet.\n\n" +
			"Subcommands:\n" +
			"  discover   Find scopes on the LAN\n" +
			"  ls         List observation folders (or the files in one)\n" +
			"  sync       Continuously pull new FITS from the scope into a collection\n\n" +
			"Firmware ≥7.18 requires an RSA challenge-response; supply the PEM\n" +
			"via SEESTAR_KEY_PATH or ~/.seestarpy/seestar.pem (extract from the\n" +
			"ZWO Android app — see astronomyk/seestarpy for the tooling).",
	}
	c.AddCommand(newSeestarDiscoverCmd())
	c.AddCommand(newSeestarLsCmd())
	c.AddCommand(newSeestarSyncCmd())
	return c
}

func newSeestarDiscoverCmd() *cobra.Command {
	var timeout time.Duration
	c := &cobra.Command{
		Use:   "discover",
		Short: "UDP-broadcast for Seestar scopes on the local subnet",
		Long: "Broadcasts the firmware's own scan_iscope probe on UDP 4720 and\n" +
			"lists every scope that answers. Reads no credentials — safe to\n" +
			"run without login.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			devs, err := seestar.Discover(cmd.Context(), timeout)
			if err != nil {
				return err
			}
			if len(devs) == 0 {
				return fmt.Errorf("no Seestars found on the local subnet — is yours in station mode?")
			}
			fmt.Printf("%-20s %-16s %s\n", "MODEL", "IP", "SERIAL")
			for _, d := range devs {
				fmt.Printf("%-20s %-16s %s\n", d.Model, d.IP, d.Serial)
			}
			return nil
		},
	}
	c.Flags().DurationVar(&timeout, "timeout", 3*time.Second, "How long to listen for replies.")
	return c
}

func newSeestarLsCmd() *cobra.Command {
	var ip, serial string
	c := &cobra.Command{
		Use:   "ls [folder]",
		Short: "List folders on the scope, or files inside one",
		Long: "With no argument: list every observation folder under MyWorks/.\n" +
			"With one argument: list every .fit file inside that folder.\n\n" +
			"Auto-discovers on the LAN by default. With >1 scope on the same\n" +
			"subnet, disambiguate with --serial <SN> (stable across DHCP) or\n" +
			"--ip <address>. Requires an RSA key on ≥7.18 firmware — see\n" +
			"`seestar --help`.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			target, err := resolveScopeIP(ctx, ip, serial)
			if err != nil {
				return err
			}
			rpc := seestar.NewClient(target, seestar.DefaultKeyPath())
			defer rpc.Close()
			if len(args) == 0 {
				folders, err := rpc.ListFolders(ctx)
				if err != nil {
					return err
				}
				if len(folders) == 0 {
					fmt.Fprintln(os.Stderr, "(no folders — has the scope taken any subs yet?)")
					return nil
				}
				fmt.Printf("%-40s %s\n", "FOLDER", "ALBUMS")
				for _, f := range folders {
					fmt.Printf("%-40s %d\n", f.Name, f.Count)
				}
				return nil
			}
			files, err := rpc.ListFiles(ctx, args[0])
			if err != nil {
				return err
			}
			if len(files) == 0 {
				fmt.Fprintln(os.Stderr, "(no FITS in that folder)")
				return nil
			}
			var total int64
			for _, f := range files {
				total += f.Size
				fmt.Printf("%10.1f MiB  %s\n", float64(f.Size)/1024/1024, f.Name)
			}
			fmt.Fprintf(os.Stderr, "\n%d file(s), %.1f MiB total\n",
				len(files), float64(total)/1024/1024)
			return nil
		},
	}
	c.Flags().StringVar(&ip, "ip", "", "Scope IP (skips discovery).")
	c.Flags().StringVar(&serial, "serial", "", "Scope serial number — stable across DHCP, preferred over --ip.")
	return c
}

// resolveScopeIP turns the (--ip, --serial, no-flag) matrix into a
// concrete scope IP. Precedence: explicit --ip > --serial lookup >
// auto-pick when the network holds exactly one scope.
//
// On ambiguity (multiple scopes, no selector) prints the discovery
// table on stderr so the user can copy the serial they want, and
// returns an error explaining `--serial` and `seestar discover`.
// This is the one place we want to be loud — a silent auto-pick with
// two scopes on the LAN would sync to the wrong one.
func resolveScopeIP(ctx context.Context, ip, serial string) (string, error) {
	if ip != "" {
		return ip, nil
	}
	devs, err := seestar.Discover(ctx, 3*time.Second)
	if err != nil {
		return "", err
	}
	if len(devs) == 0 {
		return "", fmt.Errorf("no Seestar found on the LAN — is it in station mode? Try `pancakestack seestar discover`")
	}
	if serial != "" {
		for _, d := range devs {
			if d.Serial == serial {
				return d.IP, nil
			}
		}
		fmt.Fprintf(os.Stderr, "serial %q not found. Scopes on this LAN:\n\n", serial)
		printScopeTable(devs)
		return "", fmt.Errorf("unknown serial")
	}
	if len(devs) == 1 {
		return devs[0].IP, nil
	}
	fmt.Fprintln(os.Stderr, "multiple scopes on this LAN — pick one with --serial <SN>:")
	printScopeTable(devs)
	return "", fmt.Errorf("scope selector required")
}

// printScopeTable renders the discovery result in the same shape as
// `seestar discover` so users can copy-paste the serial number they want.
func printScopeTable(devs []seestar.Device) {
	fmt.Fprintf(os.Stderr, "  %-20s %-16s %s\n", "MODEL", "IP", "SERIAL")
	for _, d := range devs {
		fmt.Fprintf(os.Stderr, "  %-20s %-16s %s\n", d.Model, d.IP, d.Serial)
	}
	fmt.Fprintln(os.Stderr)
}

func newSeestarSyncCmd() *cobra.Command {
	var ip, serial string
	var interval time.Duration
	var concurrency int
	var stackWhen int
	var stackScript string
	var stackParams []string

	c := &cobra.Command{
		Use:   "sync <folder> <collection-id>",
		Short: "Continuously upload new FITS from a scope folder into a collection",
		Long: "Watches an observation folder on the scope and streams every\n" +
			"new .fit through to a pancakestack collection as it lands.\n" +
			"State is tracked in ~/.pancakestack/seestar-sync.json so a\n" +
			"restart never re-uploads.\n\n" +
			"Only .fit files are uploaded — the scope's own JPEG previews and\n" +
			"thumbnails are skipped (pancakestack renders its own siblings\n" +
			"server-side, so uploading them would be duplicate work).\n\n" +
			"Ctrl-C exits after the current cycle.\n\n" +
			"Example: keep pushing an active session to a fresh collection\n" +
			"  pancakestack seestar sync \"M 81_sub\" m81-aug23\n\n" +
			"Auto-stack once 50 frames land:\n" +
			"  pancakestack seestar sync \"M 81_sub\" m81-aug23 \\\n" +
			"    --stack-when 50 --stack-script seestar-advanced",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			folder := args[0]
			collectionID := args[1]
			ctx := cmd.Context()

			if !validCollectionID(collectionID) {
				return fmt.Errorf("collectionId %q not allowed — use [a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}", collectionID)
			}

			fmt.Fprintln(os.Stderr, "discovering scope on the LAN...")
			target, err := resolveScopeIP(ctx, ip, serial)
			if err != nil {
				return err
			}

			// Re-discover to attach Model + Serial to the state file's
			// per-scope key. resolveScopeIP already did a discovery, but
			// we don't get the Device back from it — a tiny second sweep
			// is cheaper than plumbing that through. If discovery misses
			// the scope (unusual — same subnet, same port), fall back to
			// an IP-derived key so state still keys uniquely.
			var device seestar.Device
			devs, _ := seestar.Discover(ctx, 2*time.Second)
			for _, d := range devs {
				if d.IP == target {
					device = d
					break
				}
			}
			if device.IP == "" {
				device = seestar.Device{IP: target, Serial: "ip-" + target, Model: "Seestar (undetected)"}
			}

			// Preflight the folder name against the scope. Blind sync
			// against a typo would appear to work — poll every 30s and
			// silently upload nothing — so match "seestar sync foo" against
			// the folder list up front and, on miss, print the folders so
			// the user can pick the exact name and retry. Case-sensitive
			// because filenames on the scope are.
			rpc := seestar.NewClient(device.IP, seestar.DefaultKeyPath())
			folders, err := rpc.ListFolders(ctx)
			_ = rpc.Close()
			if err != nil {
				return fmt.Errorf("preflight ListFolders: %w", err)
			}
			var match bool
			for _, f := range folders {
				if f.Name == folder {
					match = true
					break
				}
			}
			if !match {
				fmt.Fprintf(os.Stderr, "folder %q not found on the scope. Available folders:\n\n", folder)
				fmt.Fprintf(os.Stderr, "  %-40s %s\n", "FOLDER", "ALBUMS")
				for _, f := range folders {
					fmt.Fprintf(os.Stderr, "  %-40s %d\n", f.Name, f.Count)
				}
				fmt.Fprintln(os.Stderr, "\nRe-run with the exact folder name (quote if it contains spaces).")
				return fmt.Errorf("unknown folder")
			}

			client, err := api.New(config.BackendURL(backendURL))
			if err != nil {
				return err
			}
			state, err := seestar.LoadState("")
			if err != nil {
				return err
			}

			params, err := parseParams(stackParams)
			if err != nil {
				return err
			}

			// Graceful shutdown on Ctrl-C: cancel the sync context so the
			// current cycle finishes and Run returns nil.
			runCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
			go func() {
				<-sig
				fmt.Fprintln(os.Stderr, "\ninterrupted — finishing current cycle, then exiting")
				cancel()
			}()

			return seestar.Run(runCtx, seestar.SyncOptions{
				Device:        device,
				KeyPath:       seestar.DefaultKeyPath(),
				Folder:        folder,
				CollectionID:  collectionID,
				Interval:      interval,
				Concurrency:   concurrency,
				StackWhen:     stackWhen,
				StackScriptID: stackScript,
				StackParams:   params,
				Client:        client,
				State:         state,
				Log:           func(f string, a ...any) { fmt.Fprintf(os.Stderr, f, a...) },
			})
		},
	}
	c.Flags().StringVar(&ip, "ip", "", "Scope IP (skips discovery).")
	c.Flags().StringVar(&serial, "serial", "", "Scope serial number — stable across DHCP, preferred over --ip.")
	c.Flags().DurationVar(&interval, "interval", 30*time.Second, "Polling interval between folder-listing checks.")
	c.Flags().IntVar(&concurrency, "concurrency", 4, "Concurrent S3 uploads.")
	c.Flags().IntVar(&stackWhen, "stack-when", 0, "Auto-launch a stack once this many NEW files have been uploaded (0 = never).")
	c.Flags().StringVar(&stackScript, "stack-script", "", "Catalog script id for the auto-stack (e.g. seestar, seestar-advanced).")
	c.Flags().StringSliceVar(&stackParams, "stack-param", nil, "Repeatable. key=value passed to the auto-stack.")
	return c
}

