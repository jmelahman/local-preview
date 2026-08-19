# Configuration

There isn't much to configure. `preview serve` takes a few flags, each with
an environment-variable fallback; the client subcommands need to know which
server to talk to.

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

## CLI configuration

The client subcommands (`preview open`, `preview deploy`, `preview repo`, …)
default to a server at `http://localhost:8080`. Point them somewhere else
persistently with:

```bash
preview configure https://preview.example.com
```

That writes `<config>/config.toml` (same `<config>` as [local
manifests](#local-manifests) below):

```toml
server = "https://preview.example.com"
```

`preview configure --show` prints the file's path, the stored server, and
which source actually wins; `preview configure --unset` clears it. The file
holds client settings only — the server's own configuration is the flags and
environment variables above, not this file.

Each subcommand resolves its server in this order:

1. `--server`
2. `$PREVIEW_URL`
3. `server` in `<config>/config.toml`
4. `http://localhost:8080`

The config file is what makes a remote instance work from a git hook:
`preview install-hook` writes a hook that runs a bare `preview deploy`, which
picks up the configured server without depending on the environment of
whatever shell or GUI client made the commit.

Unknown keys and a `server` without an `http://`/`https://` scheme are
startup errors rather than ignored settings — a config file that quietly
falls back to localhost would let commands "succeed" against the wrong
instance.

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
| `--preview-base-url` | (derived from `--preview-domain` and `--addr`) | Public base URL of previews, e.g. `https://preview.example.com`. Sets the scheme, domain, and port of the URLs the server hands out — see [hosting behind a proxy](#hosting-behind-a-proxy) |
| `--build-concurrency` | `2` | Number of deploys built in parallel |
| `--max-warm` | `8` | Maximum concurrently running preview processes; the least-recently-used are stopped beyond it (`0` = unlimited) |
| `--poll-interval` | `1m` | How often [watched repos](/guide/triggers#watched-repos) are fetched for new commits (`0` disables watching) |
| `--github-webhook-secret` | (unset) | Shared secret validating [GitHub webhook](/guide/triggers#github-webhooks) deliveries; empty disables the endpoint. Prefer the environment variable — flags are visible in `ps` |
| `--github-oidc-audience` | (unset) | Expected `aud` of [GitHub Actions OIDC](/guide/uploads#authenticating-with-github-actions-oidc) tokens. Setting it **requires uploads to authenticate**; use a value unique to this server (its URL is a good choice) |
| `--max-upload-bytes` | `2147483648` (2 GiB) | Maximum bytes a CI [upload](/guide/uploads) may stream: the compressed request body is rejected with `413` above it, and extraction aborts if the decompressed tar exceeds it — a bound on an untrusted client's disk use and the guard against a gzip bomb. Raise it for larger artifacts; `0` disables both caps |
| `--github-oidc-issuer` | `https://token.actions.githubusercontent.com` | OIDC issuer; override only for GitHub Enterprise Server |
| `--sso-github-client-id` | (unset) | GitHub OAuth App client ID. Setting it turns on [SSO login](/guide/sso) for the dashboard, API, and previews |
| `--sso-github-client-secret` | (unset) | GitHub OAuth App client secret. Prefer the environment variable — flags are visible in `ps` |
| `--sso-callback-url` | (unset) | Public OAuth callback URL, e.g. `https://preview.example.com/api/auth/callback`; must match the OAuth App exactly |
| `--sso-allowed-org` | (unset) | Allow members of this GitHub org to sign in |
| `--sso-allowed-team` | (unset) | Narrow `--sso-allowed-org` to one team slug |
| `--sso-allowed-logins` | (unset) | Comma-separated GitHub usernames allowed to sign in |
| `--sso-allowed-emails` | (unset) | Comma-separated verified emails allowed to sign in |
| `--s3-endpoint` | (unset) | S3 (or MinIO) endpoint `host:port`. Setting it **and** `--s3-bucket` enables the [artifact tier](#artifact-tier-s3); empty disables it |
| `--s3-bucket` | (unset) | Bucket for the artifact tier (required to enable it) |
| `--s3-prefix` | (unset) | Optional key prefix within the bucket |
| `--s3-region` | (unset) | Region for the bucket |
| `--s3-access-key` | (unset) | Static access key, for an endpoint with no ambient identity (MinIO). Leave unset to use the AWS environment or instance role. Prefer the environment variable — flags are visible in `ps` |
| `--s3-secret-key` | (unset) | Matching secret key; must be set together with `--s3-access-key`. Prefer the environment variable — flags are visible in `ps` |
| `--s3-use-ssl` | `true` | Use TLS for the endpoint; set `false` for a local MinIO over http |

## Environment variables

| Variable | Used by | Description |
| --- | --- | --- |
| `PREVIEW_DATA_DIR` | `preview serve` | Data directory override |
| `PREVIEW_CONFIG_DIR` | `preview serve`, CLI subcommands | Config directory override (`config.toml` sits directly under it, local manifests in `manifests/`) |
| `PREVIEW_DOMAIN` | `preview serve` | Preview base domain (an explicit `--preview-domain` flag wins) |
| `PREVIEW_BASE_URL` | `preview serve` | Public base URL of previews (an explicit `--preview-base-url` flag wins). Not to be confused with `PREVIEW_URL`, which is a client setting |
| `PREVIEW_GITHUB_WEBHOOK_SECRET` | `preview serve` | GitHub webhook shared secret (an explicit `--github-webhook-secret` flag wins) |
| `PREVIEW_MAX_UPLOAD_BYTES` | `preview serve` | Maximum bytes a CI upload may stream, both compressed on the wire and decompressed on disk (an explicit `--max-upload-bytes` flag wins). Defaults to 2 GiB; `0` disables both caps |
| `PREVIEW_GITHUB_OIDC_AUDIENCE` | `preview serve`, `preview upload` | The OIDC audience: on the server it's the expected `aud` (an explicit `--github-oidc-audience` flag wins); on the client it's the audience requested for the token when `--oidc-audience` is unset |
| `PREVIEW_GITHUB_OIDC_ISSUER` | `preview serve` | OIDC issuer override for GitHub Enterprise Server (an explicit `--github-oidc-issuer` flag wins) |
| `PREVIEW_UPLOAD_TOKEN` | `preview upload` | A pre-fetched bearer token sent as-is; wins over `--oidc` and needs no runner |
| `PREVIEW_SSO_GITHUB_CLIENT_ID` | `preview serve` | GitHub OAuth App client ID (an explicit `--sso-github-client-id` flag wins) |
| `PREVIEW_SSO_GITHUB_CLIENT_SECRET` | `preview serve` | GitHub OAuth App client secret (an explicit `--sso-github-client-secret` flag wins) |
| `PREVIEW_SSO_CALLBACK_URL` | `preview serve` | Public OAuth callback URL (an explicit `--sso-callback-url` flag wins) |
| `PREVIEW_SSO_ALLOWED_ORG` / `_TEAM` / `_LOGINS` / `_EMAILS` | `preview serve` | Allowlist rules (the matching `--sso-allowed-*` flag wins) |
| `PREVIEW_S3_ENDPOINT` | `preview serve` | Artifact-tier endpoint `host:port` (an explicit `--s3-endpoint` flag wins) |
| `PREVIEW_S3_BUCKET` | `preview serve` | Artifact-tier bucket (an explicit `--s3-bucket` flag wins) |
| `PREVIEW_S3_PREFIX` / `PREVIEW_S3_REGION` | `preview serve` | Key prefix and region (the matching flag wins) |
| `PREVIEW_S3_ACCESS_KEY` / `PREVIEW_S3_SECRET_KEY` | `preview serve` | Static artifact-tier keypair (the matching flag wins). Unset both to resolve credentials from the AWS environment or instance role instead |
| `PREVIEW_CACHE_MAX_ARTIFACT_BYTES` | `preview serve` | Soft cap on resident (local-disk) artifact bytes; the coldest are swept to the durable tier above it (an explicit `--cache-max-artifact-bytes` flag wins). Requires the artifact tier; `0` (default) keeps every artifact resident |
| `PREVIEW_WORKER_SECRET` | `preview serve` | Shared secret for the internal worker API (an explicit `--worker-secret` flag wins) |
| `PREVIEW_WORKER_ENDPOINT` | `preview serve --role=control` | A worker's private worker-API base URL (an explicit `--worker-endpoint` flag wins) |
| `PREVIEW_WORKER_ENDPOINTS` | `preview serve --role=control` | Comma-separated worker-API base URLs forming the fleet (an explicit `--worker-endpoints` flag wins) |
| `PREVIEW_URL` | CLI subcommands | Server base URL (an explicit `--server` flag wins; this in turn beats the config file) |
| `PREVIEW_TOKEN` | CLI subcommands | Bearer token (a GitHub PAT) sent to an [SSO-protected](/guide/sso) server (wins over the config file's `token`) |
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

## Artifact tier (S3)

By default an evicted deploy's artifacts are deleted from local disk, and
redeploying its commit does a full rebuild from the git tree. Point the server
at an S3 bucket (or any S3-compatible store, such as MinIO) and it keeps a
**durable copy** of every build artifact instead: eviction still frees local
disk, but a redeploy *hydrates* the artifact from the bucket and skips the
build.

Enable it by setting an endpoint and bucket — everything else is optional:

```bash
preview serve \
  --s3-endpoint s3.amazonaws.com \
  --s3-bucket my-preview-artifacts \
  --s3-region us-east-1
```

No credentials are configured above, which is the intended shape on AWS: with
the keypair left unset the tier resolves credentials from the environment —
`AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` / `AWS_SESSION_TOKEN`, then the
EC2 instance role. Grant the role the bucket and there is no long-lived secret
to store or rotate.

Set `PREVIEW_S3_ACCESS_KEY` / `PREVIEW_S3_SECRET_KEY` only for an endpoint with
no ambient identity, such as MinIO. Set both or neither — half a keypair is
rejected at startup rather than silently falling back.

For a local MinIO over plain http:

```bash
PREVIEW_S3_ACCESS_KEY=… PREVIEW_S3_SECRET_KEY=… \
preview serve --s3-endpoint localhost:9000 --s3-bucket preview-artifacts --s3-use-ssl=false
```

The bucket must already exist — the server verifies it at startup and **refuses
to start** if it's missing or unreachable, rather than silently dropping every
upload. How it works:

- **What's stored.** The immutable, content-addressed build artifacts —
  frontend bundles, backend build trees, and downloadable artifacts — as
  `<prefix>/<repo>/{fe,be,dl}/<hash>.tar.zst`. Objects are keyed by content
  hash, so identical builds across commits or repos deduplicate, and an object
  is never rewritten. Uploads happen in the background right after a build
  publishes locally, so they never slow a build; downloadable artifacts upload
  off the readiness path.
- **What's *not* stored.** Mutable backend **state directories** aren't
  archived. A hydrated backend starts from a fresh state directory and re-runs
  its `init` step — exactly what an evict-then-rebuild does today, so nothing
  changes for a repo now. (A repo whose manifest templates `{state_dir}` can't
  reconstruct that mutable state from the tier; keep this in mind if you later
  run more than one node.)
- **Bucket growth.** Because every build is kept, set a bucket **lifecycle
  rule** to expire old objects if storage is a concern — a rebuild transparently
  re-creates anything that's been expired.
- **Reconcile pass.** On startup and hourly thereafter, the server reconciles
  the bucket against its live (ready) deploys: any artifact missing or failing
  its recorded integrity metadata is re-uploaded from the resident local copy.
  This closes gaps from a hard crash that dropped an in-flight upload, a
  cache-hit build that skipped the persist, and artifacts built before the tier
  was enabled. An artifact that is *both* absent from the bucket and no longer
  resident locally is logged as a gap — only a redeploy rebuilds it. Uploads
  still in flight at a normal shutdown are drained before the process exits.
- **Local disk as a cache.** With a tier configured, local disk becomes a cache
  of it rather than the authoritative copy. Set `--cache-max-artifact-bytes`
  (default `0`, disabled) to cap the resident footprint: above the cap the
  coldest artifacts are swept off local disk while their deploys stay live, and
  the **next request transparently re-hydrates** the artifact before serving —
  the only difference the user sees is a one-time hydration latency. This lets
  the data volume be sized to the *working* set rather than the whole retained
  set, with retention depth becoming a bucket-lifecycle question. A freshly
  built artifact is never swept before its background upload lands, and an
  artifact with a running preview is never swept out from under it.

## Split control / worker plane (experimental)

By default (`--role=all`) one process does everything: API, dashboard, proxy,
and local process supervision. For elastic scaling the plane can be split into a
small always-on **control** node and a **worker** tier that supervises preview
processes on its behalf. The proxy is address-based and transport-agnostic — it
drives a local process over loopback or a remote worker over the internal worker
API through the *same* interface, so there is only one orchestrator
implementation, not two.

- A **worker** (`--role=worker --worker-listen :9100 --worker-secret …`) exposes
  the internal worker API and supervises processes for the control node.
- The **control** node (`--role=control --worker-endpoints
  http://<w1>:9100,http://<w2>:9100 --worker-secret …`) routes previews to the
  worker **fleet** instead of running them locally.

The control node tracks each worker by heartbeat (capacity and a draining flag)
and **places** a preview on a worker by rendezvous hashing on `(repo, hash)`:
the same artifact consistently lands on the same worker, so its local cache and
any warm process are reused, and workers joining or leaving reshuffle a minimal
set of keys. A process-mode frontend is **co-placed with its backend** (they
share a per-deploy docker network that exists on only one node). A worker that
is draining or at its warm cap is skipped for new placements, falling back to
the least-loaded worker. The fleet-wide load ratio (committed warm slots ÷
capacity) is logged as `fleet: load=…` — the signal an autoscaling policy
target-tracks. A worker can be told to drain (stop taking new work while
finishing what is warm) ahead of instance termination.

Note that only *process-mode* previews (backends, and frontends that run as a
process) are routed to workers. A **static frontend** has no process, so the
control node serves it directly from its own disk — meaning a control node also
needs the artifact tier configured, so those frontend artifacts remain
hydratable if the control node's local cache evicts them.

The worker API starts arbitrary preview processes on request, so it is a
remote-code-execution surface by design: it authenticates with a shared secret
and **must live on a private listener that is never reachable from the ALB or
the internet** — a private subnet with a security-group rule that admits only
the control node.

This is early scaffolding: the worker registry is static (endpoints from flags,
not an ASG-driven registry), scale-out/in and warm pools are left to the
infrastructure layer, and a worker still needs read access to the deploy
database to resolve run configs.

## The preview domain

Previews are addressed as `http://<sha-prefix>-<repo>.<domain>[:port]/`. The
default `preview.localhost` needs no DNS setup: browsers resolve any
`*.localhost` name to loopback. Requests whose Host doesn't match
`*.<domain>` (plain `localhost`, raw IPs) are served the dashboard. To host
previews under a real domain, point a wildcard DNS record at the server and
set `--preview-domain` accordingly.

## Hosting behind a proxy

The server hands out preview URLs — in `preview open`, `preview deploy`, the
dashboard, and the API's `preview_url` field. By default it builds them from
its own listen address: scheme `http`, port from `--addr`. That's right only
when browsers reach the server directly. Behind a TLS-terminating reverse
proxy the guess is wrong twice over: the scheme is really `https`, and the
public port is the proxy's, not `:8080`.

`--preview-base-url` states the public answer directly:

```bash
preview serve --addr :8080 --preview-base-url https://preview.example.com
```

Previews are then `https://<sha-prefix>-<repo>.preview.example.com/`, whatever
the server listens on. The URL supplies all three parts — scheme, domain, and
port (omitted when it's the scheme's default) — so it supersedes
`--preview-domain`; setting both to different hosts is a startup error rather
than a silent winner.

The full checklist for a hosted instance:

- a wildcard DNS record for `*.preview.example.com` pointing at the proxy,
  plus the apex `preview.example.com` for the dashboard;
- a wildcard TLS certificate covering `*.preview.example.com` (previews are
  one label deep, so a single wildcard is enough);
- the proxy forwarding the original `Host` header — preview routing is
  entirely Host-based;
- `--preview-base-url` on the server, so generated URLs match what the proxy
  actually serves;
- `preview configure https://preview.example.com` on each client, so the CLI
  talks to it.

## Authentication

By default the API has **no authentication**: anything that can reach it can
register repositories and run their build and run commands on the host.

Configure [GitHub SSO login](/guide/sso) to require a sign-in for the
dashboard, the API, and the previews. Setting `--sso-github-client-id` turns it
on; the server then refuses to start without a client secret, a callback URL,
and a non-empty allowlist (it fails closed rather than admitting everyone).
Browsers sign in with GitHub; the CLI presents a GitHub personal-access token
(see [`preview configure --token`](/reference/cli#preview-configure)). The
GitHub webhook keeps its HMAC signature and the upload endpoints keep their
[GitHub Actions OIDC](/guide/uploads) gate — both stay reachable without a
session.

::: warning
Until SSO (or another control at the proxy — mTLS, an IP allowlist) is
configured, do not expose a `preview serve` instance to an untrusted network.
:::
