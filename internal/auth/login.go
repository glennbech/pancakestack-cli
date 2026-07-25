package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/pkg/browser"
)

// Discovery is what the backend's /login endpoint returns.
type Discovery struct {
	CognitoDomain string   `json:"cognitoDomain"`
	CLIClientID   string   `json:"cliClientId"`
	WebClientID   string   `json:"webClientId,omitempty"`
	RedirectURI   string   `json:"redirectUri"`
	Scopes        []string `json:"scopes"`
	AuthEndpoint  string   `json:"authEndpoint"`
	TokenEndpoint string   `json:"tokenEndpoint"`
}

// FetchDiscovery hits the backend /login endpoint for Cognito config. This
// endpoint is public — no auth needed.
func FetchDiscovery(ctx context.Context, backendURL string) (*Discovery, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(backendURL, "/")+"/login", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call /login: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("/login returned %d", resp.StatusCode)
	}
	var d Discovery
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, fmt.Errorf("decode /login response: %w", err)
	}
	return &d, nil
}

// Login runs the full OAuth 2.0 Authorization Code + PKCE flow against
// Cognito's hosted UI, forcing the Google IdP. Returns the resulting Tokens
// bundle; caller decides where to persist.
//
// Flow:
//  1. Generate PKCE verifier + challenge + opaque state.
//  2. Start a local HTTP listener on `port` for the OAuth callback.
//  3. Open the user's browser to Cognito's /oauth2/authorize with
//     identity_provider=Google so they skip the Cognito picker.
//  4. User signs in with Google → Cognito → redirected to
//     http://localhost:<port>/callback?code=...&state=...
//  5. Local server validates state, exchanges code for tokens via
//     Cognito's /oauth2/token endpoint (PKCE proves it's the same client).
//  6. Return tokens.
func Login(ctx context.Context, d *Discovery, port int) (*Tokens, error) {
	pkce, err := newPKCEPair()
	if err != nil {
		return nil, fmt.Errorf("gen pkce: %w", err)
	}
	state, err := randomState()
	if err != nil {
		return nil, fmt.Errorf("gen state: %w", err)
	}

	redirect := fmt.Sprintf("http://localhost:%d/callback", port)

	// Start local listener FIRST — if the port's taken we fail before opening
	// the browser and confusing the user.
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("listen on port %d: %w — is something else using it?", port, err)
	}
	defer l.Close()

	// Build the authorize URL. `identity_provider=Google` skips Cognito's
	// picker page and goes straight to Google sign-in.
	authURL := d.AuthEndpoint + "?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {d.CLIClientID},
		"redirect_uri":          {redirect},
		"scope":                 {strings.Join(d.Scopes, " ")},
		"code_challenge":        {pkce.challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
		"identity_provider":     {"Google"},
	}.Encode()

	fmt.Fprintf(browserOut, "Opening browser to sign in with Google...\n")
	fmt.Fprintf(browserOut, "If it doesn't open automatically, visit:\n  %s\n\n", authURL)
	if err := browser.OpenURL(authURL); err != nil {
		// Not fatal — user can copy-paste the URL.
		fmt.Fprintf(browserOut, "(couldn't launch browser: %v — copy the URL above)\n", err)
	}

	// Wait for the callback. The HTTP handler pushes results onto these chans.
	type result struct {
		code string
		err  error
	}
	resultCh := make(chan result, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			http.Error(w, "sign-in failed: "+e+" — "+q.Get("error_description"), http.StatusBadRequest)
			resultCh <- result{err: fmt.Errorf("oauth error: %s (%s)", e, q.Get("error_description"))}
			return
		}
		if q.Get("state") != state {
			http.Error(w, "state mismatch — possible CSRF", http.StatusBadRequest)
			resultCh <- result{err: errors.New("state mismatch")}
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			resultCh <- result{err: errors.New("missing code")}
			return
		}
		fmt.Fprintln(w, successHTML)
		resultCh <- result{code: code}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(l) }()

	// 5-minute timeout — plenty for a human to log in.
	timeoutCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	var code string
	select {
	case r := <-resultCh:
		if r.err != nil {
			return nil, r.err
		}
		code = r.code
	case <-timeoutCtx.Done():
		return nil, errors.New("sign-in timed out after 5 minutes")
	}
	// Give the browser response a beat to flush before we shut the server.
	_ = server.Shutdown(context.Background())

	// Exchange code for tokens.
	return exchangeCode(ctx, d, pkce.verifier, code, redirect)
}

// exchangeCode POSTs to Cognito's /oauth2/token to swap the auth code for
// id_token / access_token / refresh_token. PKCE verifier proves it's the same
// client that started the flow.
func exchangeCode(ctx context.Context, d *Discovery, verifier, code, redirect string) (*Tokens, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {d.CLIClientID},
		"code":          {code},
		"redirect_uri":  {redirect},
		"code_verifier": {verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		var body [512]byte
		n, _ := resp.Body.Read(body[:])
		return nil, fmt.Errorf("token exchange returned %d: %s", resp.StatusCode, string(body[:n]))
	}
	var raw struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
		TokenType    string `json:"token_type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}

	return &Tokens{
		IDToken:      raw.IDToken,
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(raw.ExpiresIn) * time.Second),
		TokenType:    raw.TokenType,
		ClientID:     d.CLIClientID,
		TokenURL:     d.TokenEndpoint,
	}, nil
}

// Refresh swaps a refresh token for a fresh id_token + access_token. Cognito
// doesn't rotate refresh tokens by default; the existing one stays valid.
func Refresh(ctx context.Context, t *Tokens) (*Tokens, error) {
	if t.RefreshToken == "" {
		return nil, errors.New("no refresh token — run `pancakestack login`")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {t.ClientID},
		"refresh_token": {t.RefreshToken},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("refresh: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("refresh returned %d — you may need to `pancakestack login` again", resp.StatusCode)
	}
	var raw struct {
		IDToken     string `json:"id_token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		TokenType   string `json:"token_type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode refresh response: %w", err)
	}
	t.IDToken = raw.IDToken
	t.AccessToken = raw.AccessToken
	t.ExpiresAt = time.Now().Add(time.Duration(raw.ExpiresIn) * time.Second)
	t.TokenType = raw.TokenType
	return t, nil
}

// successHTML is what the user sees in their browser after a successful login.
const successHTML = `<!DOCTYPE html>
<html><head><title>pancakestack — signed in</title>
<style>body{font-family:system-ui,sans-serif;max-width:480px;margin:80px auto;text-align:center;color:#222}h1{font-weight:400}code{background:#eee;padding:2px 6px;border-radius:3px}</style>
</head><body>
<h1>Signed in ✓</h1>
<p>You can close this tab and return to your terminal.</p>
<p><code>pancakestack whoami</code> to verify.</p>
</body></html>`

// browserOut is where the login flow prints its guidance. Defaults to stderr
// so it doesn't get mixed into stdout piped output. Overridable for tests.
var browserOut io.Writer = os.Stderr
