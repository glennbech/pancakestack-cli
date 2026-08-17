package main

import (
	"fmt"

	"github.com/glennbech/pancakestack-cli/internal/api"
	"github.com/glennbech/pancakestack-cli/internal/config"
	"github.com/spf13/cobra"
)

// `pancakestack cancel <job-id>` — hit POST /jobs/{id}/cancel to stop a
// running or created job and terminate its EC2 instance. Server accepts
// terminal-state jobs as a no-op, so double-cancels are safe. Kept as a
// separate command (rather than a flag on `jobs`) because the semantics
// are destructive — a dedicated verb is easier to read in shell history
// and matches how kubectl / gh / other CLIs handle stop-verbs.
func newCancelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <job-id> [<job-id>...]",
		Short: "Cancel a running (or created) job — terminates its EC2 instance",
		Long: "Sends POST /jobs/{id}/cancel for each argument. Idempotent: " +
			"cancelling an already-terminal job returns success with a no-op message. " +
			"Terminates the EC2 instance (if any) so you stop paying for spot compute.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, err := api.New(config.BackendURL(backendURL))
			if err != nil {
				return err
			}
			var failures int
			for _, id := range args {
				if err := client.CancelJob(ctx, id); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "✗ %s — %v\n", id, err)
					failures++
					continue
				}
				fmt.Fprintf(cmd.OutOrStdout(), "✓ %s cancelled\n", id)
			}
			if failures > 0 {
				return fmt.Errorf("%d of %d cancels failed", failures, len(args))
			}
			return nil
		},
	}
}
