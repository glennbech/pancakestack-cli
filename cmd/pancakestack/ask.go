package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/glennbech/pancakestack-cli/internal/api"
	"github.com/glennbech/pancakestack-cli/internal/config"
	"github.com/spf13/cobra"
)

// Baked at build time via -ldflags; falls back to the current prod URL.
// Not sourced from the primary backend to keep the two decoupled — RAG
// runs on its own Lambda URL, separate deploy pipeline.
var DefaultRagURL = "https://sf44ueri2cibnlcwk37ogmhgwi0gepak.lambda-url.us-east-1.on.aws/"

// `pancakestack ask "<query>"` — hit the RAG lambda for Siril / astrophoto
// processing questions. Auth reuses the same Cognito bearer token as the
// main backend. Answer + citations printed as plain text.
func newAskCmd() *cobra.Command {
	var k int
	var ragURL string
	var asJSON bool
	c := &cobra.Command{
		Use:   "ask <query>",
		Short: "Ask the pancakestack RAG (Siril manual + curated astrophoto docs)",
		Long: "Retrieves relevant chunks from the ingested Siril manual and any\n" +
			"curated processing docs, then asks Claude to answer grounded in\n" +
			"those chunks. Prints the answer + citation IDs so you can see\n" +
			"which sources it drew from.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := strings.Join(args, " ")
			ctx := cmd.Context()
			client, err := api.New(config.BackendURL(backendURL))
			if err != nil {
				return err
			}
			url := DefaultRagURL
			if ragURL != "" {
				url = ragURL
			}
			resp, err := client.Ask(ctx, url, api.AskRequest{Query: query, K: k})
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(resp)
			}
			fmt.Println(resp.Answer)
			if len(resp.Citations) > 0 {
				fmt.Fprintln(os.Stderr)
				fmt.Fprintf(os.Stderr, "Sources (%d, RRF-ranked):\n", len(resp.Citations))
				for _, c := range resp.Citations {
					parts := []string{c.Kind}
					if c.Chapter != "" {
						parts = append(parts, c.Chapter)
					}
					if c.Section != "" {
						parts = append(parts, c.Section)
					}
					fmt.Fprintf(os.Stderr, "  - %s  (rrf=%.3f)\n", strings.Join(parts, " · "), c.RRF)
				}
			}
			fmt.Fprintf(os.Stderr, "\n(%dms)\n", resp.ElapsedMs)
			return nil
		},
	}
	c.Flags().IntVar(&k, "k", 8, "Number of chunks to retrieve (1..15)")
	c.Flags().StringVar(&ragURL, "rag-url", "", "Override the RAG endpoint URL (defaults to prod)")
	c.Flags().BoolVar(&asJSON, "json", false, "Emit raw JSON instead of the formatted answer")
	return c
}
