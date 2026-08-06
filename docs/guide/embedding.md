# Embedding as a library

Everything the standalone server does is available to other Go applications
through the `orchestrator` package: the same pipeline behind a small
facade, wired into your own HTTP server. The first consumer is
[agentic-kanban](https://github.com/jmelahman/agentic-kanban), which serves
a preview per ticket branch.

```go
import "github.com/jmelahman/local-preview/orchestrator"

orch, err := orchestrator.New(orchestrator.Options{
    DataDir: filepath.Join(dataDir, "previews"),
    Addr:    ":8080", // your server's listen address (for preview URLs)
})
if err != nil { ... }
defer orch.Close()

// Requests to <sha>.<repo>.preview.localhost are served as previews;
// everything else falls through to your application.
srv := &http.Server{Addr: ":8080", Handler: orch.WrapHost(appHandler)}
```

Register repos and trigger deploys from your application's own lifecycle:

```go
// Idempotent: safe to call on every startup.
orch.RegisterRepo(ctx, "myapp", "/path/to/repo")

// Branch, tag, or sha; idempotent per commit.
d, err := orch.RequestDeploy(ctx, "myapp", "feature-branch", false)
// Poll orch.Deploy(d.ID) until StatusReady, then use d.PreviewURL.
```

Notes:

- The orchestrator keeps everything under `Options.DataDir` — its own
  SQLite file, mirror clones, artifacts, state, and logs. Give it a
  directory inside your application's data dir; nothing is shared with your
  schema.
- Worktree branches of a registered repo are deployable with no extra
  plumbing: worktrees share the repo's object store, so the orchestrator's
  mirror fetches them like any branch.
- `Close()` stops build workers and every supervised backend; previews
  never outlive the embedding process.

## Custom manifest locations

By default target repos declare themselves in `preview.toml`. Embedders can
accept the same schema from inside their own config file so repos need only
one file:

```go
orchestrator.Options{
    // preview.toml wins when both exist; sources are tried in order.
    ManifestSources: []orchestrator.ManifestSource{
        {Path: "preview.toml"},
        {Path: ".kanban.toml", Table: "previews"},
    },
}
```

Hashes cover the parsed manifest rather than its location, so relocating
unchanged config busts no caches.

## Custom build execution

By default build steps exec on the host. Inject `Options.Runner` to run
them elsewhere — for example inside the target repo's devcontainer for
reproducible builds:

```go
type dockerRunner struct{ /* your container client */ }

func (r dockerRunner) Run(ctx context.Context, spec orchestrator.RunSpec, out io.Writer) error {
    // Mount spec.ScratchDir into a container, run spec.Argv with the
    // working directory spec.Dir inside the mount, stream output to out.
}
```

`RunSpec` carries the repo name and sha, so a runner can pick (and cache)
the right environment image per repository.
