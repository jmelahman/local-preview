# CLI

The binary is both the server and a client for it. Client subcommands talk to
a running `preview serve` over HTTP; point them at a non-default server with
`--server` or `$PREVIEW_URL`.

## `preview serve`

Start the orchestrator.

| Flag | Default | Description |
| --- | --- | --- |
| `--addr` | `:8080` | HTTP listen address |
| `--data-dir` | (XDG) | Override the data directory |
| `--in-memory` | `false` | Ephemeral in-memory SQLite |
| `--preview-domain` | `preview.localhost` | Base domain previews are served under |
| `--build-concurrency` | `2` | Number of deploys built in parallel |
| `--poll-interval` | `1m` | How often watched repos are fetched for new commits (`0` disables watching) |
| `--github-webhook-secret` | `$PREVIEW_GITHUB_WEBHOOK_SECRET` | Shared secret validating GitHub webhook deliveries (empty disables the endpoint) |

## `preview repo`

| Command | Description |
| --- | --- |
| `preview repo create <name> --source <path-or-url>` | Register a repository (mirror clone); `--watch` and `--branches` enable [watching](/guide/triggers#watched-repos) from the start. The server clones in the background; the command reports clone progress and waits until the repo is ready (non-zero exit if the clone fails) |
| `preview repo list` | List registered repositories with their clone status |
| `preview repo watch <name>` | Poll the repo and deploy new branch tips automatically (`--branches` narrows which, as comma-separated globs) |
| `preview repo unwatch <name>` | Stop watching |
| `preview repo delete <name>` | Unregister a repository and delete its previews (deploys, artifacts, state, logs, mirror clone) |

## `preview deploy`

| Command | Description |
| --- | --- |
| `preview deploy [ref]` | Deploy a commit (default: HEAD of the current repo) and print its preview URL, plus a download URL per [artifact](/reference/preview-toml#artifacts-name) file |
| `preview deploy list [query]` | List deploys. The optional query is a free-text search — a commit-sha prefix, or a substring of the repo, branch, ref, or author (case-insensitive); `--repo`, `--branch`, `--author`, `--status` each narrow by one field |
| `preview deploy show <id>` | Print one deploy as JSON |
| `preview deploy logs <id>` | Print a deploy's build logs |
| `preview deploy logs <id> --run` | Print the process run log — the preview server's stdout+stderr, init output included. `--side fe` selects a process-mode frontend; `-f`/`--follow` keeps polling for new output until interrupted |
| `preview deploy stats <id>` | Show live CPU/memory of the deploy's processes (docker-stats-like; samples twice a second apart to compute the CPU percentage) |
| `preview deploy stop <id>` | Stop the deploy's processes; they cold-start again on the next request. Processes are shared per artifact hash, so any sibling deploy on the same hash stops too |
| `preview deploy delete <id>` | Delete a deploy and reclaim any build artifacts, backend state, and process history no surviving deploy still references; its short-sha subdomain is freed |

Flags on `preview deploy`:

| Flag | Description |
| --- | --- |
| `--repo` | Registered repo name; by default the current directory is matched against registered sources |
| `--rebuild` | Rebuild artifacts even if cached (a live backend keeps its old binary until restarted) |
| `--no-wait` | Return immediately instead of waiting for the build |
| `--json` | Print the deploy as JSON |

The command exits non-zero if the deploy fails, so it can gate scripts.

## `preview open`

Jump from a checkout to its preview: `preview open` finds the deploy of the
current repo's HEAD (falling back to the checked-out branch's newest deploy
when that exact commit was never deployed) and opens its preview URL in the
browser. `preview open <ref>` accepts a branch — its newest deploy wins — a
tag, or a full or abbreviated commit sha; anything else is tried as a
free-text search of the repo's deploys. The URL is always printed, so the
command composes with scripts even when no browser is around.

| Flag | Description |
| --- | --- |
| `--repo` | Registered repo name; by default the current directory is matched against registered sources |
| `--print` | Print the preview URL only, without opening a browser |

Exits non-zero when the matched deploy isn't ready — still building, failed,
or evicted — with a hint at the follow-up command (`preview deploy`,
`preview deploy logs`).

## `preview install-hook`

Run from inside a target repository. Installs a git post-commit hook that
requests a preview deploy of every new commit (`--dry-run` to preview). If
the repo uses the pre-commit framework, the equivalent
`.pre-commit-config.yaml` stanza is printed instead, for use with
`pre-commit install --hook-type post-commit`:

```yaml
  - repo: local
    hooks:
      - id: local-preview
        name: local-preview deploy
        entry: sh -c 'preview deploy "$(git rev-parse HEAD)" --no-wait'
        language: system
        stages: [post-commit]
        always_run: true
        pass_filenames: false
```

Existing hand-written post-commit hooks are never overwritten.

## `preview version`

`preview --version` prints the build version (populated from `git describe`
at release time).
