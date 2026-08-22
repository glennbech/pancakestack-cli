package main

import (
	"context"
	"fmt"
	"time"

	"github.com/glennbech/pancakestack-cli/internal/api"
	"github.com/glennbech/pancakestack-cli/internal/config"
	"github.com/spf13/cobra"
)

// `pancakestack archive <collection-id>` — POST /collections/{id}/archive
// then poll until the archive-op worker Lambda writes the terminal state.
// The backend tars every FITS under the collection prefix, uploads to
// Backblaze B2, and deletes the S3 originals. Collection counts against
// storage quota at 50% of raw size once archived; stacking is paused
// until unarchive.
//
// Type-to-confirm: the CLI passes the collectionId as the `confirm`
// field automatically. Server-side that string must match either the
// collectionId or the displayName (case-insensitive). We use the
// collectionId — the arg the user just typed — so no separate prompt.
func newArchiveCmd() *cobra.Command {
	var (
		noWait   bool
		waitFor  time.Duration
		pollFreq time.Duration
	)
	cmd := &cobra.Command{
		Use:   "archive <collection-id>",
		Short: "Archive a collection to Backblaze B2 cold storage",
		Long: "Files are tarred and moved to Backblaze B2. Collection counts " +
			"against storage quota at 50% of raw size while archived, and " +
			"cannot be stacked until unarchived (see `pancakestack unarchive`).\n\n" +
			"Waits (up to --timeout) for the async worker to write the " +
			"terminal state. Use --no-wait to fire and forget.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			collectionID := args[0]
			client, err := api.New(config.BackendURL(backendURL))
			if err != nil {
				return err
			}

			resp, err := client.ArchiveCollection(ctx, collectionID, collectionID)
			if err != nil {
				return fmt.Errorf("archive %s: %w", collectionID, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "→ %s archiving (archivedAt=%d)\n",
				resp.CollectionID, resp.ArchivedAt)

			if noWait {
				fmt.Fprintln(cmd.OutOrStdout(), "  (--no-wait: poll `GET /collections` for archiveState=archived)")
				return nil
			}
			return pollArchiveTerminal(ctx, cmd, client, collectionID, waitFor, pollFreq,
				"archived", "archive_failed")
		},
	}
	cmd.Flags().BoolVar(&noWait, "no-wait", false,
		"Return immediately after the 202. Don't poll for terminal state.")
	cmd.Flags().DurationVar(&waitFor, "timeout", 15*time.Minute,
		"How long to poll before giving up. Lambda's own ceiling is 15 min.")
	cmd.Flags().DurationVar(&pollFreq, "poll-interval", 3*time.Second,
		"Seconds between polls of GET /collections.")
	return cmd
}

// `pancakestack unarchive <collection-id>` — the inverse. Pulls the tar
// back from B2, extracts it to the original S3 keys, deletes the B2
// object. Gated on quota: the full working size must fit in the
// account's free storage or the server returns 402 INSUFFICIENT_STORAGE.
func newUnarchiveCmd() *cobra.Command {
	var (
		noWait   bool
		waitFor  time.Duration
		pollFreq time.Duration
	)
	cmd := &cobra.Command{
		Use:   "unarchive <collection-id>",
		Short: "Restore an archived collection from Backblaze B2 back to S3",
		Long: "Pulls the tar from Backblaze B2 and extracts it back into S3. " +
			"The collection's full working size must fit in your free storage " +
			"quota — otherwise the server returns INSUFFICIENT_STORAGE.\n\n" +
			"Waits (up to --timeout) for the async worker to write the " +
			"terminal state. Use --no-wait to fire and forget.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			collectionID := args[0]
			client, err := api.New(config.BackendURL(backendURL))
			if err != nil {
				return err
			}

			resp, err := client.UnarchiveCollection(ctx, collectionID)
			if err != nil {
				return fmt.Errorf("unarchive %s: %w", collectionID, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "→ %s unarchiving\n", resp.CollectionID)

			if noWait {
				fmt.Fprintln(cmd.OutOrStdout(), "  (--no-wait: poll `GET /collections` for archived attribute cleared)")
				return nil
			}
			// The terminal-restore path REMOVEs the `archived` attribute
			// entirely, so success is signalled by row.Archived==false
			// (Go zero-value on the missing DDB attribute). archiveState
			// is also cleared, so we watch for either "restored" or the
			// zero-value case.
			return pollUnarchiveTerminal(ctx, cmd, client, collectionID, waitFor, pollFreq)
		},
	}
	cmd.Flags().BoolVar(&noWait, "no-wait", false,
		"Return immediately after the 202. Don't poll for terminal state.")
	cmd.Flags().DurationVar(&waitFor, "timeout", 15*time.Minute,
		"How long to poll before giving up. Lambda's own ceiling is 15 min.")
	cmd.Flags().DurationVar(&pollFreq, "poll-interval", 3*time.Second,
		"Seconds between polls of GET /collections.")
	return cmd
}

// pollArchiveTerminal loops GET /collections every pollFreq until either
// archiveState matches successState / failState, or waitFor elapses.
// Emits a single-line spinner-ish progress marker on each poll.
func pollArchiveTerminal(ctx context.Context, cmd *cobra.Command, client *api.Client,
	collectionID string, waitFor, pollFreq time.Duration,
	successState, failState string,
) error {
	deadline := time.Now().Add(waitFor)
	for {
		row, err := client.GetCollection(ctx, collectionID)
		if err != nil {
			return fmt.Errorf("poll %s: %w", collectionID, err)
		}
		if row == nil {
			return fmt.Errorf("poll %s: collection disappeared", collectionID)
		}
		switch row.ArchiveState {
		case successState:
			fmt.Fprintf(cmd.OutOrStdout(), "✓ %s → %s\n", collectionID, successState)
			return nil
		case failState:
			msg := row.ArchiveError
			if msg == "" {
				msg = "(no error message)"
			}
			return fmt.Errorf("✗ %s → %s: %s", collectionID, failState, msg)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout after %s waiting for %s (last state=%q)",
				waitFor, successState, row.ArchiveState)
		}
		// Single-char progress marker on stderr so stdout stays parseable.
		fmt.Fprint(cmd.ErrOrStderr(), ".")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollFreq):
		}
	}
}

// pollUnarchiveTerminal is the unarchive-side equivalent. The success
// signal is different — the server removes the `archived` attribute
// entirely (row.Archived==false) rather than writing a state string —
// so we watch for that condition. Failure signal remains
// archiveState="unarchive_failed".
func pollUnarchiveTerminal(ctx context.Context, cmd *cobra.Command, client *api.Client,
	collectionID string, waitFor, pollFreq time.Duration,
) error {
	deadline := time.Now().Add(waitFor)
	for {
		row, err := client.GetCollection(ctx, collectionID)
		if err != nil {
			return fmt.Errorf("poll %s: %w", collectionID, err)
		}
		if row == nil {
			return fmt.Errorf("poll %s: collection disappeared", collectionID)
		}
		if !row.Archived {
			fmt.Fprintf(cmd.OutOrStdout(), "✓ %s → restored\n", collectionID)
			return nil
		}
		if row.ArchiveState == "unarchive_failed" {
			msg := row.ArchiveError
			if msg == "" {
				msg = "(no error message)"
			}
			return fmt.Errorf("✗ %s → unarchive_failed: %s", collectionID, msg)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout after %s waiting for unarchive (last state=%q)",
				waitFor, row.ArchiveState)
		}
		fmt.Fprint(cmd.ErrOrStderr(), ".")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollFreq):
		}
	}
}
