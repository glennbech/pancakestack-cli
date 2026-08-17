package main

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/glennbech/pancakestack-cli/internal/api"
	"github.com/glennbech/pancakestack-cli/internal/config"
	"github.com/spf13/cobra"
)

// `pancakestack metrics <jobId>` — fetch the CloudWatch time series for a
// job's spot instance and print a compact summary. Same data the JobDetail
// Advanced panel renders as sparklines; this shows peaks / averages in a
// terminal-friendly table.
//
// Doubles as the smoke-test surface for the /jobs/{id}/metrics endpoint.
// Any Lambda deploy that touches auth, IAM, or the CloudWatch integration
// should be verified with `pancakestack metrics <recent-jobId>` before
// declaring "done".
func newMetricsCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "metrics <job-id>",
		Short: "Show CloudWatch runtime metrics for a job's spot instance",
		Long: "Fetches the same time-series backing the JobDetail 'Advanced' " +
			"panel. Useful for post-deploy smoke-testing the /jobs/{id}/metrics " +
			"endpoint and for headless comparison across bench runs.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, err := api.New(config.BackendURL(backendURL))
			if err != nil {
				return err
			}
			m, err := client.GetJobMetrics(ctx, args[0])
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(m)
			}
			printMetrics(m)
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "Emit raw JSON instead of the summary table")
	return c
}

func printMetrics(m *api.JobMetrics) {
	fmt.Printf("instance:    %s\n", stringOrDash(m.InstanceID))
	fmt.Printf("window:      %s → %s\n", m.StartTime, m.EndTime)
	fmt.Printf("period:      %ds\n", m.PeriodSeconds)
	if m.Note != "" {
		fmt.Printf("note:        %s\n", m.Note)
	}
	if len(m.Series) == 0 {
		return
	}

	fmt.Println()
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SERIES\tCURRENT\tPEAK\tAVG\tPOINTS")
	// Stable order so re-runs are comparable.
	keys := make([]string, 0, len(m.Series))
	for k := range m.Series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		pts := m.Series[k]
		if len(pts) == 0 {
			fmt.Fprintf(w, "%s\t-\t-\t-\t0\n", k)
			continue
		}
		var peak, sum float64
		for _, p := range pts {
			if p.V > peak {
				peak = p.V
			}
			sum += p.V
		}
		avg := sum / float64(len(pts))
		last := pts[len(pts)-1].V
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\n",
			k, fmtMetric(k, last), fmtMetric(k, peak), fmtMetric(k, avg), len(pts))
	}
	w.Flush()
}

// fmtMetric picks a compact display format based on the series key. Percent
// series stay as XX%; Bps series get human byte-rate suffixes. Anything
// else falls back to %.2f so we don't hide unknown series types behind an
// aggressive formatter.
func fmtMetric(key string, v float64) string {
	switch {
	case len(key) >= 3 && key[len(key)-3:] == "Pct":
		return fmt.Sprintf("%.1f%%", v)
	case len(key) >= 3 && key[len(key)-3:] == "Bps":
		return humanBps(v)
	default:
		return fmt.Sprintf("%.2f", v)
	}
}

func humanBps(v float64) string {
	if v <= 0 {
		return "0"
	}
	units := []string{"B/s", "KB/s", "MB/s", "GB/s"}
	u := 0
	for v >= 1024 && u < len(units)-1 {
		v /= 1024
		u++
	}
	if v < 10 {
		return fmt.Sprintf("%.1f %s", v, units[u])
	}
	return fmt.Sprintf("%d %s", int(v), units[u])
}

func stringOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
