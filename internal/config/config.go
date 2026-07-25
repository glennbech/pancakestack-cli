// Package config resolves the backend URL and token store location.
package config

import (
	"os"
	"path/filepath"
)

// DefaultBackendURL is baked at build time via -ldflags.
// Override at runtime with --url or PANCAKESTACK_URL.
var DefaultBackendURL = "https://kjvlw4gb6ksxsypgr6x6jsqkl40ntoke.lambda-url.us-east-1.on.aws"

// BackendURL returns the resolved backend URL — flag > env > compiled default.
// flagValue is the value of --url; pass "" if unset.
func BackendURL(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if v := os.Getenv("PANCAKESTACK_URL"); v != "" {
		return v
	}
	return DefaultBackendURL
}

// CredentialsPath is where we store OAuth tokens. Respects XDG_CONFIG_HOME,
// falls back to ~/.config/pancakestack.
func CredentialsPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "credentials.json"), nil
}

func configDir() (string, error) {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "pancakestack"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "pancakestack"), nil
}
