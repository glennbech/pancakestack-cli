package main

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/glennbech/pancakestack-cli/internal/api"
	"github.com/glennbech/pancakestack-cli/internal/config"
	"github.com/spf13/cobra"
)

// defaultTail is how many events we fetch when the user gives no other
// selection knob. Matches `kubectl logs`'s implicit tail behaviour — small
// enough to be fast, big enough to see recent context.
const defaultTail = 100

func newLogsCmd() *cobra.Command {
	var source string
	var tail int
	var limit int
	var since time.Duration
	var all bool
	var follow bool
	var pollEvery time.Duration
	c := &cobra.Command{
		Use:   "logs <job-id>",
		Short: "Print CloudWatch logs for a job",
		Long: "Streams a job's log events from CloudWatch.\n\n" +
			"Sources:\n" +
			"  --source job         (default) the Siril run itself\n" +
			"  --source cloudinit   boot log — useful when a job never gets to run\n\n" +
			"Selection (mutually exclusive):\n" +
			"  --tail N / -n N      last N events (default 100)\n" +
			"  --since <duration>   only events newer than now-<duration> (e.g. 5m, 1h)\n" +
			"  --all                every event from stream start\n\n" +
			"Other:\n" +
			"  --limit N            server page size (1..10000); rarely needed\n" +
			"  --follow / -f        keep polling after the initial page\n" +
			"  --poll-every         follow poll interval (default 3s)",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			jobID := args[0]
			if source != "job" && source != "cloudinit" {
				return errors.New("--source must be 'job' or 'cloudinit'")
			}
			// Selection modes are exclusive — combining them is almost always a
			// user error and the resulting server call would be ambiguous.
			selectorsSet := 0
			tailExplicit := cmd.Flags().Changed("tail")
			if tailExplicit {
				selectorsSet++
			}
			if since > 0 {
				selectorsSet++
			}
			if all {
				selectorsSet++
			}
			if selectorsSet > 1 {
				return errors.New("--tail, --since and --all are mutually exclusive")
			}
			if limit < 0 || limit > 10000 {
				return errors.New("--limit must be in [0, 10000]")
			}

			ctx := cmd.Context()
			client, err := api.New(config.BackendURL(backendURL))
			if err != nil {
				return err
			}

			q := buildLogsQuery(source, tail, tailExplicit, limit, since, all)

			// One-shot: print whatever page came back and exit.
			if !follow {
				resp, err := client.GetLogs(ctx, jobID, q)
				if err != nil {
					return err
				}
				if !resp.HasStream {
					fmt.Fprintln(os.Stderr, "(log stream not yet created — CloudWatch agent takes ~30s to register after launch)")
					return nil
				}
				printLogEvents(resp.Events)
				return nil
			}

			// Follow mode: do the initial fetch honouring the user's selector,
			// then advance NextForwardToken from that point on. GetLogEvents
			// returns the same token when there are no new events, so a plain
			// poll loop is enough.
			token := ""
			warnedNoStream := false
			first := true
			for {
				if !first {
					q = api.LogsQuery{Source: source, NextToken: token}
					if limit > 0 {
						q.Limit = limit
					}
				}
				resp, err := client.GetLogs(ctx, jobID, q)
				if err != nil {
					return err
				}
				if !resp.HasStream {
					if !warnedNoStream {
						fmt.Fprintln(os.Stderr, "(waiting for log stream to appear...)")
						warnedNoStream = true
					}
				} else {
					printLogEvents(resp.Events)
					token = resp.NextForwardToken
					first = false
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(pollEvery):
				}
			}
		},
	}
	c.Flags().StringVar(&source, "source", "job", "Log source: 'job' or 'cloudinit'")
	c.Flags().IntVarP(&tail, "tail", "n", defaultTail, "Show only the last N events")
	c.Flags().DurationVar(&since, "since", 0, "Only events newer than now-<duration> (e.g. 5m, 1h)")
	c.Flags().BoolVar(&all, "all", false, "Show every event from stream start")
	c.Flags().IntVar(&limit, "limit", 0, "Server page size (1..10000; 0 = server default)")
	c.Flags().BoolVarP(&follow, "follow", "f", false, "Poll for new events until interrupted")
	c.Flags().DurationVar(&pollEvery, "poll-every", 3*time.Second, "Poll interval when --follow is set")
	return c
}

// buildLogsQuery translates the user-facing flags into a LogsQuery. Modes:
//
//	--all      → StartFromHead=true, no time window, server-default limit
//	--since D  → StartFromHead=true, StartTime=now-D, server-default limit
//	default    → StartFromHead=false, Limit=tail (last N events)
func buildLogsQuery(source string, tail int, tailExplicit bool, limit int, since time.Duration, all bool) api.LogsQuery {
	q := api.LogsQuery{Source: source}
	switch {
	case all:
		t := true
		q.StartFromHead = &t
	case since > 0:
		t := true
		q.StartFromHead = &t
		q.StartTime = time.Now().Add(-since).UnixMilli()
	default:
		// Tail mode (explicit -n or default).
		_ = tailExplicit
		f := false
		q.StartFromHead = &f
		q.Limit = tail
	}
	// --limit overrides the derived page size (only meaningful for --all and
	// --since, where we didn't already set one).
	if limit > 0 {
		q.Limit = limit
	}
	return q
}

func printLogEvents(events []api.LogEvent) {
	for _, e := range events {
		t := time.UnixMilli(e.Timestamp).Local().Format("15:04:05")
		fmt.Printf("%s  %s\n", t, e.Message)
	}
}
