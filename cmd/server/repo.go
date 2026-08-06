package server

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/jmelahman/local-preview/internal/client"
)

func repoCmd() *cobra.Command {
	var serverURL string
	parent := &cobra.Command{
		Use:   "repo",
		Short: "Manage registered repositories",
	}
	addServerFlag(parent, &serverURL)

	var source string
	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Register a repository for previews",
		Long: "Register a repository: the server keeps a mirror clone of --source\n" +
			"(a local path or clone URL) and serves previews at <sha>.<name>.<domain>.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRepoCreate(cmd.Context(), resolveURL(cmd, serverURL), cmd.OutOrStdout(), args[0], source)
		},
	}
	create.Flags().StringVar(&source, "source", "", "Local path or clone URL of the repository (required)")
	_ = create.MarkFlagRequired("source")

	list := &cobra.Command{
		Use:   "list",
		Short: "List registered repositories",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRepoList(cmd.Context(), resolveURL(cmd, serverURL), cmd.OutOrStdout())
		},
	}

	del := &cobra.Command{
		Use:   "delete <name>",
		Short: "Unregister a repository and delete its previews",
		Long: "Unregister a repository: stops its preview backends and deletes its\n" +
			"deploys, artifacts, state directories, build logs, and mirror clone.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRepoDelete(cmd.Context(), resolveURL(cmd, serverURL), cmd.OutOrStdout(), args[0])
		},
	}

	parent.AddCommand(create, list, del)
	return parent
}

func runRepoDelete(ctx context.Context, url string, out io.Writer, name string) error {
	if err := client.New(url, nil).DeleteRepo(ctx, name); err != nil {
		return err
	}
	fmt.Fprintf(out, "deleted %s\n", name)
	return nil
}

func runRepoCreate(ctx context.Context, url string, out io.Writer, name, source string) error {
	repo, err := client.New(url, nil).CreateRepo(ctx, name, source)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "registered %s (source %s)\n", repo.Name, repo.Source)
	return nil
}

func runRepoList(ctx context.Context, url string, out io.Writer) error {
	repos, err := client.New(url, nil).ListRepos(ctx)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSOURCE\tCREATED")
	for _, r := range repos {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Name, r.Source, r.CreatedAt)
	}
	return tw.Flush()
}
