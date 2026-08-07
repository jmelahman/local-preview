# Configuration

There isn't much to configure. `preview serve` takes a few flags, each with
an environment-variable fallback.

## Data directory

Everything the orchestrator owns lives under one data directory, resolved in
order:

1. `--data-dir` flag
2. `$PREVIEW_DATA_DIR`
3. `$XDG_DATA_HOME/preview`
4. `~/.local/share/preview`

Inside it:

| Path | Contents |
| --- | --- |
| `preview.db` | SQLite database (repos, deploys, artifacts, process records) |
| `repos/<repo>.git/` | Mirror clones of registered repos |
| `artifacts/<repo>/{fe,be}/<hash>/` | Content-addressed build artifacts |
| `state/<repo>/<be_hash>/` | Mutable backend state directories |
| `logs/<repo>/` | Build logs (per artifact hash) and process run logs |
| `tmp/` | Build scratch space; swept at startup |

## Local manifests

Repos that can't carry a `preview.toml` upstream can be onboarded with an
out-of-repo manifest at `<config>/manifests/<repo>.toml`, where `<config>`
resolves in order:

1. `$PREVIEW_CONFIG_DIR`
2. `$XDG_CONFIG_HOME/preview`
3. `~/.config/preview`

See [local manifests](/reference/preview-toml#local-manifests-repos-you-can-t-change)
for lookup order and caching semantics.

## Flags

| Flag | Default | Description |
| --- | --- | --- |
| `--addr` | `:8080` | HTTP listen address |
| `--data-dir` | (XDG) | Override the data directory |
| `--in-memory` | `false` | Ephemeral in-memory SQLite; deploy history is discarded on shutdown (artifacts still use the data directory) |
| `--preview-domain` | `preview.localhost` | Base domain previews are served under |
| `--build-concurrency` | `2` | Number of deploys built in parallel |
| `--max-warm` | `8` | Maximum concurrently running preview processes; the least-recently-used are stopped beyond it (`0` = unlimited) |
| `--poll-interval` | `1m` | How often [watched repos](/guide/triggers#watched-repos) are fetched for new commits (`0` disables watching) |
| `--github-webhook-secret` | (unset) | Shared secret validating [GitHub webhook](/guide/triggers#github-webhooks) deliveries; empty disables the endpoint. Prefer the environment variable — flags are visible in `ps` |

## Environment variables

| Variable | Used by | Description |
| --- | --- | --- |
| `PREVIEW_DATA_DIR` | `preview serve` | Data directory override |
| `PREVIEW_CONFIG_DIR` | `preview serve` | Config directory override (local manifests live in `manifests/` under it) |
| `PREVIEW_DOMAIN` | `preview serve` | Preview base domain (an explicit `--preview-domain` flag wins) |
| `PREVIEW_GITHUB_WEBHOOK_SECRET` | `preview serve` | GitHub webhook shared secret (an explicit `--github-webhook-secret` flag wins) |
| `PREVIEW_URL` | CLI subcommands | Server base URL (an explicit `--server` flag wins) |
| `PREVIEW_BACKEND` | `web/` dev server | Backend `host:port` the Vite proxy targets |

## Docker requirements

Two manifest features need a reachable docker daemon (`$DOCKER_HOST`,
`/var/run/docker.sock`, or the rootless per-user socket):

- Build `image` steps fall back to host execution with a warning when no
  daemon is reachable.
- `run_image` processes have **no fallback** — the runtime isn't on the
  host by construction, so the start fails with a clear error instead.

Published container ports bind to the docker host's loopback. A composed
server (docker compose deployment) must therefore run with
`network_mode: host` to reach `run_image` processes — see the note in
`compose.yaml`. A `preview serve` running directly on the docker host
needs nothing extra.

## Warm previews

A freshly built deploy starts its processes immediately; after that they
start on demand and stay warm either way. Two mechanisms bound the
footprint:

- **Idle timeout** — each side's `idle_timeout` (default `30m`) stops its
  process after that long without a request through the proxy.
- **LRU cap** — `--max-warm` bounds how many processes run at once; beyond
  it the least-recently-used are stopped. The hottest previews stay warm,
  cold ones restart on their next request (a cold start, not a rebuild).

A process-mode frontend holds its backend's address for its lifetime, so a
paired backend inherits the frontend's recency and is never stopped while
that frontend runs — the frontend goes first, the backend on a later sweep.

## Retention & garbage collection

Build artifacts accumulate: every deployed commit publishes
content-addressed artifacts, state, and logs that outlive the commit's
usefulness. A retention policy bounds that growth with two limits, each `0`
(unlimited) by default:

- **Deploys kept per repo** — keep at most N non-evicted deploys per repo,
  newest first.
- **Max age** — evict deploys created more than N days ago.

The policy is stored in the database and edited at runtime — from the
dashboard's **Storage & retention** dialog (the database icon in the
header) or via [`PUT /api/retention`](/reference/api#put-api-retention) —
no server restart involved. A background sweep enforces it hourly (and once
at startup); **Run GC now** / [`POST /api/gc`](/reference/api#post-api-gc)
applies it immediately.

Eviction is not deletion: the deploy row survives as history with status
`evicted`, its preview subdomain answers with a "redeploy to rebuild" page,
and redeploying the commit — the **redeploy** button on the deployment's
dashboard row, or `POST /api/deploys` with its sha — revives it with a
fresh build. The sweep reclaims
the artifacts, backend state directories, and build/run logs that no
surviving deploy shares — content-addressed artifacts shared with a
surviving deploy are kept.

Some deploys are never evicted automatically:

- queued and building deploys (in flight);
- each repo's newest ready deploy — a repo always keeps one working preview;
- deploys a branch alias routes to, so branch URLs keep working.

Use `DELETE /api/deploys/{id}` (or the dashboard's trash button) to remove a
deploy entirely, history included.

The dashboard's Storage & retention dialog also reports disk usage by
category (artifacts, state, logs, mirror clones, tmp, database) and per
repo, backed by [`GET /api/storage`](/reference/api#get-api-storage).

## The preview domain

Previews are addressed as `http://<sha-prefix>.<repo>.<domain>[:port]/`. The
default `preview.localhost` needs no DNS setup: browsers resolve any
`*.localhost` name to loopback. Requests whose Host doesn't match
`*.<domain>` (plain `localhost`, raw IPs) are served the dashboard. To host
previews under a real domain, point a wildcard DNS record at the server and
set `--preview-domain` accordingly.
