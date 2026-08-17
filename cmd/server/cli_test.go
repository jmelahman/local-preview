package server

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

// isolateConfig points the config-dir lookup at a fresh temp dir, so tests
// never read (or write) the developer's real ~/.config/preview.
func isolateConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PREVIEW_CONFIG_DIR", dir)
	return dir
}

// writeClientConfig drops a config.toml into the isolated config dir.
func writeClientConfig(t *testing.T, dir, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runLeaf executes a parent command with an inherited --server flag and
// returns what resolveURL reports inside the leaf's RunE — the same shape as
// the real CLI subcommands.
func runLeaf(t *testing.T, args ...string) string {
	t.Helper()
	url, err := runLeafErr(t, args...)
	if err != nil {
		t.Fatal(err)
	}
	return url
}

func runLeafErr(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var serverURL string
	var got string
	var gotErr error
	parent := &cobra.Command{Use: "root"}
	addServerFlag(parent, &serverURL)
	leaf := &cobra.Command{
		Use:           "leaf",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			got, gotErr = resolveURL(cmd, serverURL)
			return gotErr
		},
	}
	parent.AddCommand(leaf)
	parent.SetArgs(append([]string{"leaf"}, args...))
	parent.SetOut(io.Discard)
	parent.SetErr(io.Discard)
	if err := parent.Execute(); err != nil {
		return "", err
	}
	return got, gotErr
}

func TestResolveURL(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		isolateConfig(t)
		if got := runLeaf(t); got != defaultServerURL {
			t.Errorf("resolveURL = %q, want default", got)
		}
	})

	t.Run("env wins over default", func(t *testing.T) {
		isolateConfig(t)
		t.Setenv("PREVIEW_URL", "http://env:1234")
		if got := runLeaf(t); got != "http://env:1234" {
			t.Errorf("resolveURL = %q, want env value", got)
		}
	})

	t.Run("explicit flag wins over env", func(t *testing.T) {
		isolateConfig(t)
		t.Setenv("PREVIEW_URL", "http://env:1234")
		if got := runLeaf(t, "--server", "http://flag:5678"); got != "http://flag:5678" {
			t.Errorf("resolveURL = %q, want flag value", got)
		}
	})

	t.Run("config file wins over default", func(t *testing.T) {
		dir := isolateConfig(t)
		writeClientConfig(t, dir, "server = \"https://preview.example.com\"\n")
		if got := runLeaf(t); got != "https://preview.example.com" {
			t.Errorf("resolveURL = %q, want the configured server", got)
		}
	})

	t.Run("env wins over config file", func(t *testing.T) {
		dir := isolateConfig(t)
		writeClientConfig(t, dir, "server = \"https://preview.example.com\"\n")
		t.Setenv("PREVIEW_URL", "http://env:1234")
		if got := runLeaf(t); got != "http://env:1234" {
			t.Errorf("resolveURL = %q, want env value", got)
		}
	})

	t.Run("explicit flag wins over config file", func(t *testing.T) {
		dir := isolateConfig(t)
		writeClientConfig(t, dir, "server = \"https://preview.example.com\"\n")
		if got := runLeaf(t, "--server", "http://flag:5678"); got != "http://flag:5678" {
			t.Errorf("resolveURL = %q, want flag value", got)
		}
	})

	// A config file naming the wrong server must not silently degrade to
	// localhost: the command would "succeed" against the wrong instance.
	t.Run("broken config file errors", func(t *testing.T) {
		for name, contents := range map[string]string{
			"malformed":   "server = \n",
			"unknown key": "srv = \"https://preview.example.com\"\n",
			"no scheme":   "server = \"preview.example.com\"\n",
		} {
			t.Run(name, func(t *testing.T) {
				dir := isolateConfig(t)
				writeClientConfig(t, dir, contents)
				if got, err := runLeafErr(t); err == nil {
					t.Errorf("resolveURL = %q, want an error", got)
				}
			})
		}
	})

	// An explicit --server is honored even when the config file is broken —
	// the flag never consults it.
	t.Run("explicit flag skips a broken config file", func(t *testing.T) {
		dir := isolateConfig(t)
		writeClientConfig(t, dir, "srv = \"https://preview.example.com\"\n")
		if got := runLeaf(t, "--server", "http://flag:5678"); got != "http://flag:5678" {
			t.Errorf("resolveURL = %q, want flag value", got)
		}
	})
}
