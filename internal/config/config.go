// Package config resolves where the application keeps its on-disk state.
package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// appName is the directory name used under the XDG data home. Keep it in
// sync with the binary name in .goreleaser.yaml and pyproject.toml.
const appName = "preview"

// DefaultPreviewDomain is the base domain previews are served under when
// neither the flag nor $PREVIEW_DOMAIN overrides it. *.localhost resolves to
// loopback in browsers with no DNS setup.
const DefaultPreviewDomain = "preview.localhost"

// Config holds the resolved runtime configuration.
type Config struct {
	DataDir       string
	PreviewDomain string
}

// Load resolves the data directory (flag override > $PREVIEW_DATA_DIR >
// $XDG_DATA_HOME/preview > ~/.local/share/preview) and ensures it exists,
// plus the preview base domain (flag override > $PREVIEW_DOMAIN > default).
func Load(dataDirOverride, previewDomainOverride string) (Config, error) {
	dir := dataDirOverride
	if dir == "" {
		dir = os.Getenv("PREVIEW_DATA_DIR")
	}
	if dir == "" {
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			dir = filepath.Join(xdg, appName)
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return Config{}, fmt.Errorf("resolve home dir: %w", err)
			}
			dir = filepath.Join(home, ".local", "share", appName)
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Config{}, fmt.Errorf("resolve data dir: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return Config{}, fmt.Errorf("create data dir: %w", err)
	}
	domain := previewDomainOverride
	if domain == "" {
		domain = os.Getenv("PREVIEW_DOMAIN")
	}
	if domain == "" {
		domain = DefaultPreviewDomain
	}
	return Config{DataDir: abs, PreviewDomain: domain}, nil
}

// DBPath returns the SQLite database path inside the data directory.
func (c Config) DBPath() string {
	return filepath.Join(c.DataDir, appName+".db")
}

// ReposDir holds one bare (mirror) clone per registered repo.
func (c Config) ReposDir() string { return filepath.Join(c.DataDir, "repos") }

// TmpDir is scratch space for builds and state-dir forks. Contents are never
// authoritative; anything stale here is swept at startup.
func (c Config) TmpDir() string { return filepath.Join(c.DataDir, "tmp") }

// ArtifactsDir holds published content-addressed artifacts
// (artifacts/<repo>/{fe,be}/<hash>/).
func (c Config) ArtifactsDir() string { return filepath.Join(c.DataDir, "artifacts") }

// StateDir holds mutable backend state directories (state/<repo>/<be_hash>/).
func (c Config) StateDir() string { return filepath.Join(c.DataDir, "state") }

// LogsDir holds build logs (logs/<repo>/{fe,be}/<hash>.log) and process run
// logs (logs/<repo>/run/<be_hash>/<n>.log).
func (c Config) LogsDir() string { return filepath.Join(c.DataDir, "logs") }
