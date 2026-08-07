package server

import (
	"os"

	"github.com/spf13/cobra"
)

// addClientCommands attaches the user-facing CLI subcommand groups to root.
// They're thin wrappers over the HTTP API, useful for scripting and remote
// control of a running `preview serve`.
func addClientCommands(root *cobra.Command) {
	root.AddCommand(repoCmd(), deployCmd(), openCmd(), installHookCmd())
}

// resolveURL returns the effective server URL for a leaf command. PREVIEW_URL
// wins only when the user didn't explicitly pass --server.
func resolveURL(cmd *cobra.Command, serverURL string) string {
	if env := os.Getenv("PREVIEW_URL"); env != "" && !cmd.Flags().Changed("server") {
		return env
	}
	return serverURL
}

// addServerFlag registers --server as a persistent flag on the parent group
// so every leaf inherits it. The same string variable is reused across the
// group's subcommands.
func addServerFlag(parent *cobra.Command, dst *string) {
	parent.PersistentFlags().StringVar(dst, "server", "http://localhost:8080",
		"Base URL of the HTTP server")
}
