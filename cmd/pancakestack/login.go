package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/glennbech/pancakestack-cli/internal/auth"
	"github.com/glennbech/pancakestack-cli/internal/config"
	"github.com/spf13/cobra"
)

func newLoginCmd() *cobra.Command {
	var port int
	c := &cobra.Command{
		Use:   "login",
		Short: "Sign in with Google via Cognito",
		Long: "Runs the OAuth 2.0 Authorization Code + PKCE flow against Cognito's " +
			"hosted UI, opens your browser to Google, captures the callback locally, " +
			"and stores tokens in ~/.config/pancakestack/credentials.json (mode 0600).",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			d, err := auth.FetchDiscovery(ctx, config.BackendURL(backendURL))
			if err != nil {
				return fmt.Errorf("fetch discovery: %w", err)
			}

			tokens, err := auth.Login(ctx, d, port)
			if err != nil {
				return err
			}
			tokens.Issuer = d.CognitoDomain

			path, err := config.CredentialsPath()
			if err != nil {
				return err
			}
			if err := auth.Save(path, tokens); err != nil {
				return err
			}

			sub, email := claimsPeek(tokens.IDToken)
			fmt.Printf("✓ Signed in as %s (%s)\n", email, sub)
			fmt.Printf("  Tokens stored in %s\n", path)
			return nil
		},
	}
	c.Flags().IntVar(&port, "port", 8765, "Localhost port for the OAuth callback")
	return c
}

func newLogoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Delete stored tokens",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := config.CredentialsPath()
			if err != nil {
				return err
			}
			if err := auth.Delete(path); err != nil {
				return err
			}
			fmt.Printf("✓ Deleted %s\n", path)
			return nil
		},
	}
}

func newWhoamiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Print the signed-in user",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := config.CredentialsPath()
			if err != nil {
				return err
			}
			t, err := auth.Load(path)
			if err != nil {
				return err
			}
			if t == nil {
				return errors.New("not signed in — run `pancakestack login`")
			}
			sub, email := claimsPeek(t.IDToken)
			fmt.Printf("sub:   %s\n", sub)
			fmt.Printf("email: %s\n", email)
			fmt.Printf("expires: %s\n", t.ExpiresAt.Format("2006-01-02 15:04:05 MST"))
			if t.IsExpired() {
				fmt.Println("(access token expired — refresh will happen on next call)")
			}
			return nil
		},
	}
}

// claimsPeek decodes an id_token's payload without verifying the signature.
// Only for display in `whoami` — never for authz decisions. If the caller's
// token was tampered with, the backend will reject the next API call anyway.
func claimsPeek(idToken string) (sub, email string) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var c struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
	}
	_ = json.Unmarshal(payload, &c)
	return c.Sub, c.Email
}
