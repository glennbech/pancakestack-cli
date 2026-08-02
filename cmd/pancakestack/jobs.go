package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/glennbech/pancakestack-cli/internal/api"
	"github.com/glennbech/pancakestack-cli/internal/config"
	"github.com/spf13/cobra"
)

func newJobsCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "jobs [job-id]",
		Short: "List your jobs, or show details for one",
		Long: "With no arguments, prints a table of your recent jobs (newest first). " +
			"With a job id, prints the full record — including a presigned download URL " +
			"when the job has succeeded.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			client, err := api.New(config.BackendURL(backendURL))
			if err != nil {
				return err
			}

			if len(args) == 1 {
				job, err := client.GetJob(ctx, args[0])
				if err != nil {
					return err
				}
				if asJSON {
					return printJSON(job)
				}
				printJobDetail(job)
				return nil
			}

			resp, err := client.ListJobs(ctx)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(resp)
			}
			printJobsTable(resp.Jobs)
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "Emit raw JSON instead of a table")
	return c
}

func printJobsTable(jobs []api.JobSummary) {
	if len(jobs) == 0 {
		fmt.Fprintln(os.Stderr, "No jobs yet — try `pancakestack upload` then `pancakestack stack`.")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "JOB ID\tCOLLECTION\tSCRIPT\tSTATE\tSTAGE\tCREATED\tAGE")
	for _, j := range jobs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			j.JobID,
			j.CollectionID,
			dashIfEmpty(j.ScriptID),
			j.State,
			dashIfEmpty(j.Stage),
			shortTime(j.CreatedAt),
			ageOf(j.CreatedAt),
		)
	}
	w.Flush()
}

func printJobDetail(j *api.JobDetail) {
	fmt.Printf("jobId:         %s\n", j.JobID)
	fmt.Printf("state:         %s\n", j.State)
	if j.Stage != "" {
		fmt.Printf("stage:         %s\n", j.Stage)
	}
	fmt.Printf("collection:    %s\n", j.CollectionID)
	if j.ScriptID != "" {
		fmt.Printf("script:        %s\n", j.ScriptID)
	}
	if j.InstanceType != "" {
		fmt.Printf("instanceType:  %s\n", j.InstanceType)
	}
	if j.InstanceID != "" {
		fmt.Printf("instanceId:    %s\n", j.InstanceID)
	}
	fmt.Printf("created:       %s\n", j.CreatedAt)
	fmt.Printf("updated:       %s\n", j.UpdatedAt)
	if j.InputPrefix != "" {
		fmt.Printf("input:         %s\n", j.InputPrefix)
	}
	if j.OutputPrefix != "" {
		fmt.Printf("output:        %s\n", j.OutputPrefix)
	}
	if j.ResultKey != "" {
		fmt.Printf("result:        %s\n", j.ResultKey)
	}
	if j.DownloadURL != "" {
		fmt.Printf("download:      %s\n", j.DownloadURL)
		if j.ResultFilename != "" {
			fmt.Printf("filename:      %s\n", j.ResultFilename)
		}
	}
	if j.Error != "" {
		fmt.Printf("error:         %s\n", j.Error)
	}
	if len(j.Params) > 0 {
		fmt.Printf("params:\n")
		for k, v := range j.Params {
			fmt.Printf("  %s = %v\n", k, v)
		}
	}
}

func printJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// shortTime renders an RFC3339-ish timestamp as "2006-01-02 15:04". Returns
// the input untouched if parsing fails so we still show *something*.
func shortTime(ts string) string {
	if ts == "" {
		return "-"
	}
	t, err := time.Parse("2006-01-02T15:04:05Z", ts)
	if err != nil {
		return ts
	}
	return t.Local().Format("2006-01-02 15:04")
}

func ageOf(ts string) string {
	t, err := time.Parse("2006-01-02T15:04:05Z", ts)
	if err != nil {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
