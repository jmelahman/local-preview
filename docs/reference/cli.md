# CLI

The binary is both the server and a client for it. Client subcommands talk to
a running `preview serve` over HTTP; point them at a non-default server with
`--server`, `$PREVIEW_URL`, or — persistently — [`preview
configure`](#preview-configure).

## `preview serve`

Start the orchestrator.

| Flag | Default | Description |
| --- | --- | --- |
| `--addr` | `:8080` | HTTP listen address |
| `--data-dir` | (XDG) | Override the data directory |
| `--in-memory` | `false` | Ephemeral in-memory SQLite |
| `--preview-domain` | `preview.localhost` | Base domain previews are served under |
| `--preview-base-url` | (derived) | Public base URL of previews, e.g. `https://preview.example.com` — sets the scheme, domain, and port of generated preview URLs when the server sits behind a proxy |
| `--build-concurrency` | `2` | Number of deploys built in parallel |
| `--poll-interval` | `1m` | How often watched repos are fetched for new commits (`0` disables watching) |
| `--github-webhook-secret` | `$PREVIEW_GITHUB_WEBHOOK_SECRET` | Shared secret validating GitHub webhook deliveries (empty disables the endpoint) |
| `--sso-github-client-id` | `$PREVIEW_SSO_GITHUB_CLIENT_ID` | GitHub OAuth App client ID; setting it turns on [SSO login](/guide/sso) for the dashboard, API, and previews |
| `--sso-github-client-secret` | `$PREVIEW_SSO_GITHUB_CLIENT_SECRET` | GitHub OAuth App client secret |
| `--sso-callback-url` | `$PREVIEW_SSO_CALLBACK_URL` | Public OAuth callback URL; must match the OAuth App exactly |
| `--sso-allowed-org` / `--sso-allowed-team` | `$PREVIEW_SSO_ALLOWED_ORG` / `_TEAM` | Allow a GitHub org (optionally one team within it) |
| `--sso-allowed-logins` / `--sso-allowed-emails` | `$PREVIEW_SSO_ALLOWED_LOGINS` / `_EMAILS` | Comma-separated usernames / verified emails |

## `preview repo`

| Command | Description |
| --- | --- |
| `preview repo create <name> --source <path-or-url>` | Register a repository (mirror clone); `--watch` and `--branches` enable [watching](/guide/triggers#watched-repos) from the start. The server clones in the background; the command reports clone progress and waits until the repo is ready (non-zero exit if the clone fails) |
| `preview repo list` | List registered repositories with their clone status |
| `preview repo watch <name>` | Poll the repo and deploy branch tips as they move (`--branches` narrows which, as comma-separated globs; `**` spans `/`, and a `!` prefix excludes, e.g. `!main` or `!gh-readonly-queue/**`). Starts from the repo's current state; `--backfill` also deploys the tips it already has |
| `preview repo unwatch <name>` | Stop watching |
| `preview repo delete <name>` | Unregister a repository and delete its previews (deploys, artifacts, state, logs, mirror clone) |

## `preview deploy`

| Command | Description |
| --- | --- |
| `preview deploy [ref]` | Deploy a commit (default: HEAD of the current repo) and print its preview URL, plus a download URL per [artifact](/reference/preview-toml#artifacts-name) file |
| `preview deploy list [query]` | List deploys. The optional query is a free-text search — a commit-sha prefix, or a substring of the repo, branch, ref, or author (case-insensitive); `--repo`, `--branch`, `--author`, `--status` each narrow by one field (`--status crashed` lists the ready deploys whose process died, which `--status ready` leaves out) |
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

## `preview upload`

Publish a CI-built side into the server's content-addressed store so a deploy
of the commit serves it without rebuilding — see
[uploading prebuilt artifacts](/guide/uploads). The server resolves the ref
and computes the same hash a build would target, then lands the upload in that
slot. Each `<path>` may be a directory (its **contents** are tarred, so the tar
root maps to the published root) or an existing `.tar`/`.tar.gz` file. The ref
defaults to HEAD of the current repo.

| Command | Description |
| --- | --- |
| `preview upload frontend <path> [ref]` | Upload a prebuilt frontend — the `dist` tree for a static bundle, or the built `path` tree for a [process-mode frontend](/reference/preview-toml#process-mode-frontends) |
| `preview upload backend <path> [ref]` | Upload a prebuilt backend tree (the built `backend.path` tree, run as-is) |
| `preview upload artifact <name> <path> [ref]` | Upload a named [downloadable artifact](/reference/preview-toml#artifacts-name); the tree must contain the artifact's declared `files` at their `path`-relative locations |

Flags (shared by every subcommand):

| Flag | Description |
| --- | --- |
| `--repo` | Registered repo name; by default the current directory is matched against registered sources |
| `--overwrite` | Replace the artifact if it is already present (an upload of an already-present hash is otherwise a no-op) |
| `--deploy` | Deploy the commit after uploading and wait for its preview URL — the one-command CI flow |
| `--oidc` | Authenticate with a [GitHub Actions OIDC](/guide/uploads#authenticating-with-github-actions-oidc) token minted from the runner (needs `permissions: id-token: write`) |
| `--oidc-audience` | Audience to request for the OIDC token (default: `$PREVIEW_GITHUB_OIDC_AUDIENCE`, else the server URL) — must match the server's `--github-oidc-audience` |

Setting `$PREVIEW_UPLOAD_TOKEN` sends that token as-is (a bearer `Authorization`
header), taking precedence over `--oidc` and requiring no runner — an escape
hatch for non-Actions OIDC or local testing.

An upload primes the store independently of any deploy: order doesn't matter,
and an uploaded side is shared by every commit with the same hash. Exits
non-zero on an unresolvable ref, a manifest error, or a tar missing a declared
artifact file.

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

## `preview configure`

Store which server the client subcommands talk to, so `preview open`,
`preview deploy`, and friends reach a remote instance without `--server` on
every invocation:

```bash
preview configure https://preview.example.com
```

The setting is written to the [CLI config
file](/guide/configuration#cli-configuration) and applies to every shell —
including the git hooks installed by `preview install-hook`, which run
`preview deploy` with no flags of their own. With no URL argument the
command prompts for one, offering the current value as the default. After
saving, it pings the server and reports its version and preview domain; an
unreachable server is a warning, not a failure, so you can configure the CLI
before the server is up.

| Flag | Description |
| --- | --- |
| `--show` | Print the config file's path, the stored server, whether a token is set, and which source the effective server comes from |
| `--unset` | Remove the stored server, falling back to `http://localhost:8080` |
| `--token <pat>` | Store a bearer token — a GitHub personal-access token — sent to a server that requires [SSO](/guide/sso); an empty value clears it. Distinct from `preview upload`'s `--oidc`/`$PREVIEW_UPLOAD_TOKEN`, which authenticate only uploads |

Resolution order for the server URL, first match winning:

1. `--server`
2. `$PREVIEW_URL`
3. the config file's `server`
4. `http://localhost:8080`

The bearer token is resolved similarly: `$PREVIEW_TOKEN`, then the config
file's `token`. It is sent only when set, so it's harmless against a server
without SSO.

## `preview install-hook`

Run from inside a target repository. Installs a git post-commit hook that
requests a preview deploy of every new commit (`--dry-run` to preview). If
the repo uses the pre-commit framework, no hook file is written; instead the
command prints a `.pre-commit-config.yaml` stanza subscribing to the hook
this repository publishes in its `.pre-commit-hooks.yaml`, for use with
`pre-commit install --hook-type post-commit`:

```yaml
  - repo: https://github.com/jmelahman/local-preview
    rev: v0.1.8  # pre-filled with the CLI's own release when it is one
    hooks:
      - id: local-preview-deploy
```

See [deployment triggers](/guide/triggers#post-commit-hook) for how the
published hook works and how to keep `rev` current.

Existing hand-written post-commit hooks are never overwritten.

## `preview version`

`preview --version` prints the build version (populated from `git describe`
at release time).
