# Configuration

Configuration is intentionally small: a few flags on `preview serve`, each
with an environment-variable fallback.

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

## Flags

| Flag | Default | Description |
| --- | --- | --- |
| `--addr` | `:8080` | HTTP listen address |
| `--data-dir` | (XDG) | Override the data directory |
| `--in-memory` | `false` | Ephemeral in-memory SQLite; deploy history is discarded on shutdown (artifacts still use the data directory) |
| `--preview-domain` | `preview.localhost` | Base domain previews are served under |
| `--build-concurrency` | `2` | Number of deploys built in parallel |

## Environment variables

| Variable | Used by | Description |
| --- | --- | --- |
| `PREVIEW_DATA_DIR` | `preview serve` | Data directory override |
| `PREVIEW_DOMAIN` | `preview serve` | Preview base domain (an explicit `--preview-domain` flag wins) |
| `PREVIEW_URL` | CLI subcommands | Server base URL (an explicit `--server` flag wins) |
| `PREVIEW_BACKEND` | `web/` dev server | Backend `host:port` the Vite proxy targets |

## The preview domain

Previews are addressed as `http://<sha-prefix>.<repo>.<domain>[:port]/`. The
default `preview.localhost` needs no DNS setup: browsers resolve any
`*.localhost` name to loopback. Requests whose Host doesn't match
`*.<domain>` (plain `localhost`, raw IPs) are served the dashboard. To host
previews under a real domain, point a wildcard DNS record at the server and
set `--preview-domain` accordingly.
