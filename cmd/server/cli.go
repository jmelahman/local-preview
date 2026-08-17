package server

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/jmelahman/local-preview/internal/config"
)

// defaultServerURL is where the CLI looks when nothing else says otherwise:
// a `preview serve` on this machine.
const defaultServerURL = "http://localhost:8080"

// addClientCommands attaches the user-facing CLI subcommand groups to root.
// They're thin wrappers over the HTTP API, useful for scripting and remote
// control of a running `preview serve`.
func addClientCommands(root *cobra.Command) {
	root.AddCommand(repoCmd(), deployCmd(), openCmd(), installHookCmd(), configureCmd())
}

// resolveURL returns the effective server URL for a leaf command: an
// explicit --server wins, otherwise the ambient configuration decides.
func resolveURL(cmd *cobra.Command, serverURL string) (string, error) {
	if cmd.Flags().Changed("server") {
		return serverURL, nil
	}
	url, _, err := effectiveServer()
	return url, err
}

// effectiveServer resolves the server URL used when no --server is passed,
// in precedence order: $PREVIEW_URL, the config file, then the built-in
// default. It also reports which of those won, for `preview configure`.
// A malformed config file is an error rather than a fall-through — silently
// talking to localhost when the file names a remote server would look like
// success.
func effectiveServer() (url, source string, err error) {
	if env := os.Getenv("PREVIEW_URL"); env != "" {
		return env, "$PREVIEW_URL", nil
	}
	cfg, err := config.LoadClientConfig()
	if err != nil {
		return "", "", err
	}
	if cfg.Server != "" {
		return cfg.Server, "config file", nil
	}
	return defaultServerURL, "default", nil
}

// addServerFlag registers --server as a persistent flag on the parent group
// so every leaf inherits it. The same string variable is reused across the
// group's subcommands.
func addServerFlag(parent *cobra.Command, dst *string) {
	// No backticks in the usage string: cobra reads a back-quoted word as
	// the flag's value placeholder.
	parent.PersistentFlags().StringVar(dst, "server", defaultServerURL,
		"Base URL of the HTTP server (default: $PREVIEW_URL, else the server set by 'preview configure')")
}
