package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/jmelahman/local-preview/internal/client"
)

func deployCmd() *cobra.Command {
	var serverURL string
	var repoFlag string
	var rebuild, noWait, asJSON bool

	parent := &cobra.Command{
		Use:   "deploy [ref]",
		Short: "Deploy a commit as a preview",
		Long: "Deploy a commit (default: HEAD of the current repo) and print its\n" +
			"preview URL. The repo is auto-detected from the working directory\n" +
			"unless --repo is given.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := ""
			if len(args) == 1 {
				ref = args[0]
			}
			return runDeploy(cmd.Context(), resolveURL(cmd, serverURL), cmd.OutOrStdout(),
				repoFlag, ref, rebuild, noWait, asJSON)
		},
	}
	addServerFlag(parent, &serverURL)
	parent.Flags().StringVar(&repoFlag, "repo", "", "Registered repo name (default: auto-detect from cwd)")
	parent.Flags().BoolVar(&rebuild, "rebuild", false, "Rebuild artifacts even if cached (needs a restart to affect a live backend)")
	parent.Flags().BoolVar(&noWait, "no-wait", false, "Return immediately instead of waiting for the build")
	parent.Flags().BoolVar(&asJSON, "json", false, "Print the deploy as JSON")

	var listFilter client.DeployFilter
	list := &cobra.Command{
		Use:   "list",
		Short: "List deploys",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDeployList(cmd.Context(), resolveURL(cmd, serverURL), cmd.OutOrStdout(), listFilter)
		},
	}
	list.Flags().StringVar(&listFilter.Repo, "repo", "", "Only deploys of this repo")
	list.Flags().StringVar(&listFilter.Branch, "branch", "", "Only deploys of this branch")
	list.Flags().StringVar(&listFilter.Author, "author", "", "Only deploys whose commit author name or email contains this (case-insensitive)")

	show := &cobra.Command{
		Use:   "show <id>",
		Short: "Show one deploy",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseInt64(args[0], "deploy id")
			if err != nil {
				return err
			}
			d, err := client.New(resolveURL(cmd, serverURL), nil).GetDeploy(cmd.Context(), id)
			if err != nil {
				return err
			}
			return printDeployJSON(cmd.OutOrStdout(), d)
		},
	}

	logs := &cobra.Command{
		Use:   "logs <id>",
		Short: "Print a deploy's build logs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := parseInt64(args[0], "deploy id")
			if err != nil {
				return err
			}
			text, err := client.New(resolveURL(cmd, serverURL), nil).GetDeployLogs(cmd.Context(), id)
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), text)
			return nil
		},
	}

	parent.AddCommand(list, show, logs)
	return parent
}

func runDeploy(ctx context.Context, url string, out io.Writer, repoName, ref string, rebuild, noWait, asJSON bool) error {
	c := client.New(url, nil)

	if ref == "" {
		sha, err := gitOutput(ctx, "rev-parse", "HEAD")
		if err != nil {
			return fmt.Errorf("no ref given and no commit found in the current directory: %w", err)
		}
		ref = sha
	}
	if repoName == "" {
		var err error
		repoName, err = detectRepo(ctx, c)
		if err != nil {
			return err
		}
	}

	d, err := c.CreateDeploy(ctx, repoName, ref, rebuild)
	if err != nil {
		return err
	}
	if noWait {
		if asJSON {
			return printDeployJSON(out, d)
		}
		fmt.Fprintf(out, "deploy %d (%s@%s) %s\n", d.ID, d.Repo, d.ShortSHA, d.Status)
		return nil
	}

	for d.Status == "queued" || d.Status == "building" {
		time.Sleep(500 * time.Millisecond)
		d, err = c.GetDeploy(ctx, d.ID)
		if err != nil {
			return err
		}
	}
	if asJSON {
		return printDeployJSON(out, d)
	}
	switch d.Status {
	case "ready":
		fmt.Fprintf(out, "ready: %s\n", d.PreviewURL)
		for _, a := range d.Artifacts {
			for _, f := range a.Files {
				fmt.Fprintf(out, "%s: %s%s\n", a.Name, strings.TrimRight(url, "/"), f.URL)
			}
		}
		if rebuild {
			fmt.Fprintf(out, "note: --rebuild replaced the artifacts on disk; a running backend keeps its old binary until restarted\n")
		}
		return nil
	default:
		fmt.Fprintf(out, "deploy %d %s: %s\n", d.ID, d.Status, d.Error)
		fmt.Fprintf(out, "full logs: preview deploy logs %d\n", d.ID)
		return fmt.Errorf("deploy did not become ready")
	}
}

// detectRepo matches the cwd's git toplevel or origin URL against the
// server's registered repos.
func detectRepo(ctx context.Context, c *client.Client) (string, error) {
	repos, err := c.ListRepos(ctx)
	if err != nil {
		return "", err
	}
	if top, err := gitOutput(ctx, "rev-parse", "--show-toplevel"); err == nil {
		absTop, _ := filepath.Abs(top)
		for _, r := range repos {
			if absSource, err := filepath.Abs(r.Source); err == nil && absSource == absTop {
				return r.Name, nil
			}
		}
	}
	if origin, err := gitOutput(ctx, "config", "--get", "remote.origin.url"); err == nil && origin != "" {
		for _, r := range repos {
			if r.Source == origin {
				return r.Name, nil
			}
		}
	}
	return "", fmt.Errorf("could not match the current directory to a registered repo; pass --repo (registered: %s)", repoNames(repos))
}

func repoNames(repos []client.Repo) string {
	if len(repos) == 0 {
		return "none"
	}
	names := make([]string, len(repos))
	for i, r := range repos {
		names[i] = r.Name
	}
	return strings.Join(names, ", ")
}

func gitOutput(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func runDeployList(ctx context.Context, url string, out io.Writer, f client.DeployFilter) error {
	deploys, err := client.New(url, nil).ListDeploys(ctx, f)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tREPO\tSHA\tBRANCH\tAUTHOR\tSTATUS\tURL")
	for _, d := range deploys {
		url := d.PreviewURL
		if url == "" {
			url = "-"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			d.ID, d.Repo, d.ShortSHA, orDash(d.Branch), orDash(d.AuthorName), displayState(d), url)
	}
	return tw.Flush()
}

// displayState is the state a human should see: the build status, or for
// ready deploys with supervised processes the merged runtime state —
// starting while any side warms up, running only once every side is warm,
// idle otherwise (a cold start awaits the first request).
func displayState(d client.Deploy) string {
	if d.Status != "ready" {
		return d.Status
	}
	warm := true
	seen := false
	for _, p := range []string{d.Process, d.FeProcess} {
		switch p {
		case "starting":
			return "starting"
		case "idle", "running":
			seen = true
			warm = warm && p == "running"
		}
	}
	if !seen {
		return d.Status
	}
	if warm {
		return "running"
	}
	return "idle"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func printDeployJSON(out io.Writer, d client.Deploy) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(d)
}

// parseInt64 wraps strconv.ParseInt with a descriptive error.
func parseInt64(s, label string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", label, s, err)
	}
	return id, nil
}
