package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newUploadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "upload <collection-id> <path>",
		Short: "Tar a directory of light frames and upload as a named collection",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			// TODO: implement (needs internal/api + internal/archive)
			return fmt.Errorf("not implemented yet — CLI scaffolding is up, upload command coming next")
		},
	}
}

func newStackCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "stack <collection-id>",
		Short: "Kick off a stack of a previously-uploaded collection",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// TODO: implement
			return fmt.Errorf("not implemented yet — CLI scaffolding is up, stack command coming next")
		},
	}
	c.Flags().String("script", "", "Catalog script id (seestar | seestar-drizzle | raw-basic)")
	c.Flags().StringSlice("param", nil, "Repeatable. key=value")
	c.Flags().String("instance", "", "EC2 instance type")
	return c
}
