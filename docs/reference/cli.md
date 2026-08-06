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
| `preview repo create <name> --source <path-or-url>` | Register a repository (mirror clone); `--watch` and `--branches` enable [watching](/guide/triggers#watched-repos) from the start |
| `preview repo list` | List registered repositories |
| `preview repo watch <name>` | Poll the repo and deploy new branch tips automatically (`--branches` narrows which, as comma-separated globs) |
| `preview repo unwatch <name>` | Stop watching |
| `preview repo delete <name>` | Unregister a repository and delete its previews (deploys, artifacts, state, logs, mirror clone) |

## `preview deploy`

| Command | Description |
| --- | --- |
| `preview deploy [ref]` | Deploy a commit (default: HEAD of the current repo) and print its preview URL, plus a download URL per [artifact](/reference/preview-toml#artifacts-name) file |
| `preview deploy list` | List deploys (`--repo`, `--branch`, `--author` to filter) |
| `preview deploy show <id>` | Print one deploy as JSON |
| `preview deploy logs <id>` | Print a deploy's build logs |

Flags on `preview deploy`:

| Flag | Description |
| --- | --- |
| `--repo` | Registered repo name; by default the current directory is matched against registered sources |
| `--rebuild` | Rebuild artifacts even if cached (a live backend keeps its old binary until restarted) |
| `--no-wait` | Return immediately instead of waiting for the build |
| `--json` | Print the deploy as JSON |

The command exits non-zero if the deploy fails, so it can gate scripts.

## `preview install-hook`

Run from inside a target repository. Installs a git post-commit hook that
requests a preview deploy of every new commit (`--dry-run` to preview). If
the repo uses the pre-commit framework, the equivalent
`.pre-commit-config.yaml` stanza is printed instead, for use with
`pre-commit install --hook-type post-commit`.

Existing hand-written post-commit hooks are never overwritten.

## `preview version`

`preview --version` prints the build version (populated from `git describe`
at release time).
