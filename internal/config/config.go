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
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "credentials.json"), nil
}

// SeestarKeyPath is the primary lookup for the Seestar RSA PEM. Kept
// alongside credentials.json so one config dir owns every secret the
// CLI needs. Seestar auth code also falls back to the seestarpy layout
// (env var + ~/.seestarpy/seestar.pem) so users already set up with
// that tooling work out of the box.
func SeestarKeyPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "seestar.pem"), nil
}

// SeestarStatePath is the sync-loop's per-file bookkeeping — same
// config dir as everything else, keeps a wipe-and-restart to one
// obvious location.
func SeestarStatePath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "seestar-sync.json"), nil
}

// ConfigDir returns the resolved config directory (~/.config/pancakestack
// by default). Exported so the seestar package can drop files next to
// credentials.json.
func ConfigDir() (string, error) {
	if v := os.Getenv("XDG_CONFIG_HOME"); v != "" {
		return filepath.Join(v, "pancakestack"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "pancakestack"), nil
}
