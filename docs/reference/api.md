# REST API

All endpoints are JSON over HTTP, rooted at `/api/` on the apex host (any
Host that isn't a preview subdomain). Errors return a JSON body of the form
`{"error": "message"}`.

## Health

### `GET /api/health`

`preview_domain` is the base domain previews are served under, as resolved at
startup from [`--preview-domain`](/reference/cli#preview-serve) /
`$PREVIEW_DOMAIN`.

```json
{ "status": "ok", "version": "v0.1.0", "preview_domain": "preview.localhost" }
```

## Repos

### `POST /api/repos`

Registers a repository: the server mirror-clones `source` (a local path or
clone URL) **in the background** and responds immediately. `name` must be a
lowercase DNS label — it becomes the subdomain segment. `watch` and
`watch_branches` are optional and enable
[watching](/guide/triggers#watched-repos) from the start.

Request:

```json
{ "name": "myapp", "source": "/home/me/code/myapp" }
```

Response: `202 Accepted` with the repo in status `cloning`. `400` for an
invalid name/source, `409` if the name is taken by a registered repo
(including one whose clone failed — delete it to retry). Only current
registrations conflict: a mirror clone left on disk by a deleted registration
is replaced, so deleting a repo always frees its name.

```json
{
  "id": 1,
  "name": "myapp",
  "source": "/home/me/code/myapp",
  "watch": false,
  "watch_branches": "",
  "status": "cloning",
  "created_at": "2026-01-01T00:00:00Z"
}
```

`status` progresses `cloning → ready` (or `failed`, with `error` carrying
the clone failure). Poll `GET /api/repos/{name}` to follow it; while
cloning, the response may also include `progress` — the transport's latest
human-readable progress line (e.g. `"Receiving objects: 42%"`), when the
source's server reports one. A repo accepts deploys only once `ready`
(`POST /api/deploys` answers `409` before that), and a watched repo starts
polling on its own as soon as the clone lands. Clones interrupted by a
server restart resume at the next startup.

### `GET /api/repos`

Returns all repos. `GET /api/repos/{name}` returns one (`404` if missing).

### `PATCH /api/repos/{name}`

Updates a repo's watch settings; fields absent from the body keep their
current value. `watch: true` polls the repo every `--poll-interval` and
deploys new branch tips; `watch_branches` narrows which branches as
comma-separated globs (`""` = all; globs don't cross `/`).

Request:

```json
{ "watch": true, "watch_branches": "main,release/*" }
```

Response: `200 OK` with the updated repo. `400` for an invalid branch
pattern or an empty body, `404` if the name isn't registered.

### `DELETE /api/repos/{name}`

Unregisters a repository: stops its preview backends, then deletes its
deploys, artifacts, state directories, build logs, and mirror clone. The
name is immediately reusable.

Response: `204 No Content`. `404` if the name isn't registered.

## Deploys

### `POST /api/deploys`

Requests a deploy of `ref` (branch, tag, or full/abbreviated sha) in `repo`.
Idempotent per commit: re-posting a sha whose deploy is queued, building, or
ready returns the existing deploy; failed and evicted deploys are re-queued.
`rebuild: true` rebuilds artifacts even when cached.

Request:

```json
{ "repo": "myapp", "ref": "main", "rebuild": false }
```

Response: `202 Accepted` with the deploy. `404` for an unknown repo, `409`
for a repo that isn't `ready` (still cloning, or its clone failed), `400`
for an unresolvable ref.

```json
{
  "id": 7,
  "repo": "myapp",
  "sha": "a1b2c3d4…",
  "short_sha": "a1b2c3d",
  "ref": "main",
  "branch": "main",
  "author_name": "Ada Lovelace",
  "author_email": "ada@example.com",
  "fe_hash": "…",
  "be_hash": "…",
  "status": "queued",
  "attempt_count": 0,
  "created_at": "2026-01-01T00:00:00Z",
  "updated_at": "2026-01-01T00:00:00Z"
}
```

`status` is one of `queued`, `building`, `ready`, `failed`, `evicted`. Ready
deploys additionally carry `preview_url` and `process` (the live backend
state: `running` means warm, `starting` means a start is in flight, and
`idle` means the process will start on the first request — processes start
on demand), and `fe_process` (same states) when the frontend is a
[process](/reference/preview-toml#process-mode-frontends) rather than a
static bundle. Deploys with no backend never carry `process`.

Ready deploys whose manifest declares
[downloadable artifacts](/reference/preview-toml#artifacts-name) also carry
`artifacts`, one entry per name with a download URL per file:

```json
"artifacts": [
  {
    "name": "cli",
    "hash": "…",
    "files": [
      { "name": "mycli", "size": 5242880, "url": "/api/deploys/7/artifacts/cli/mycli" }
    ]
  }
]
```

`ref` is what the deploy request asked for (empty when it was a sha);
`branch`, `author_name`, and `author_email` are captured from git when the
deploy is created. `branch` is best-effort: the requested ref when it is a
branch, otherwise the first branch whose tip is the commit — deploys of
commits that are no longer any branch's tip leave it empty. Deploys made
before these fields existed omit them.

### `GET /api/deploys`

Returns deploys, newest first. Narrow the list with any combination of:

| Query param | Match |
| --- | --- |
| `?repo=<name>` | exact repo name |
| `?branch=<name>` | exact branch name |
| `?author=<text>` | case-insensitive substring of the commit author's name or email |

`?author=ada@example.com` is the "only my deployments" filter.

### `GET /api/deploys/{id}`

Returns one deploy (`404` if missing).

### `POST /api/deploys/{id}/stop`

Stops the deploy's supervised processes (backend and, for process-mode
frontends, frontend) without removing it. Because processes are shared per
artifact hash, any sibling deploy on the same hash stops too; each cold-starts
again on its next request. Build artifacts and the deploy row are untouched.

Response: `200 OK` with the deploy (its `process`/`fe_process` now read
`idle`). `404` if the deploy doesn't exist. A no-op — succeeds — when nothing
was running.

### `DELETE /api/deploys/{id}`

Hard-deletes a deploy: it removes the row, then stops and garbage-collects any
build artifacts, backend state, and process bookkeeping that no surviving
deploy still references. Artifacts and state are content-addressed and shared,
so a hash another deploy still uses is left intact. The deploy's `short_sha`
subdomain is freed and re-deploying the commit builds fresh.

Response: `204 No Content`. `404` if the deploy doesn't exist. On-disk cleanup
is best-effort once the row is gone — leftovers are unreachable and only cost
disk, so failures are logged rather than surfaced.

### `GET /api/deploys/{id}/logs`

Returns a plain-text snapshot of the frontend, backend, and artifact build
logs. Deploys that share an artifact share that artifact's build log.

### `GET /api/deploys/{id}/logs/run`

Returns an incremental slice of a **run log** — the supervised process's
combined stdout+stderr (init output included), the docker-logs view of a
preview. Run logs outlive their process, so crash output stays readable
after an exit.

| Query param | Meaning |
| --- | --- |
| `side` | `be` (default) or `fe` (process-mode frontends only; `404` for static frontends) |
| `attempt` | The attempt whose bytes the client already has |
| `offset` | How many bytes of it the client already has |

```json
{
  "side": "be",
  "attempt": 3,
  "offset": 4096,
  "content": "listening on 127.0.0.1:41234\n…",
  "truncated": false,
  "process": "running"
}
```

Each response covers the **latest start attempt** of the deploy's artifact
(processes are shared per artifact, so deploys with the same hash share a
run log; `attempt` counts that artifact's starts, `0` meaning it never
started). Echo `attempt` and `offset` back to receive only bytes appended
since — the polling loop behind the dashboard's live tail. When the
requested attempt is stale (the process restarted) the response resets to a
tail of the new attempt's log, with `truncated: true` if history beyond the
last 256 KiB was skipped. `process` is the side's live state
(`idle`/`starting`/`running`).

### `GET /api/deploys/{id}/stats`

Live resource usage of the deploy's supervised processes — the docker-stats
view. A side the deploy doesn't have is `null`.

```json
{
  "backend": {
    "state": "running",
    "runtime": "host",
    "cpu_percent": 1.2,
    "memory_bytes": 5709824,
    "memory_limit_bytes": 32887226368,
    "started_at": "2026-08-06T22:09:49Z"
  },
  "frontend": null
}
```

`state` is always present; the sampled fields appear only while the process
runs. `cpu_percent` is percent of one core (docker-stats convention — it can
exceed 100 on multi-core use) and needs two samples, so it appears from the
second request onward; poll at a steady interval for stable readings.
`memory_limit_bytes` is the container's cgroup limit or, for host
processes, total system memory. `runtime` is `host` or `container`. Host
processes are sampled from `/proc` (process-group-wide, so forked children
count); on non-Linux hosts only container-mode processes report samples.

### `GET /api/deploys/{id}/artifacts/{name}/{file}`

Downloads one file of a ready deploy's named
[downloadable artifact](/reference/preview-toml#artifacts-name), as
`application/octet-stream` with an attachment disposition. The URL is what
the deploy's `artifacts` field lists. `404` for an unknown deploy,
artifact, or file; `409` while the deploy isn't ready.

## Storage & retention

### `GET /api/storage`

Reports the instance's disk usage, by category and by repo. Sizes are
measured by walking the data directory on each call.

```json
{
  "total_bytes": 123456789,
  "artifacts_bytes": 100000000,
  "state_bytes": 2000000,
  "logs_bytes": 400000,
  "mirror_bytes": 20000000,
  "tmp_bytes": 0,
  "db_bytes": 1056789,
  "repos": [
    {
      "repo": "myapp",
      "artifacts_bytes": 100000000,
      "state_bytes": 2000000,
      "logs_bytes": 400000,
      "mirror_bytes": 20000000,
      "total_bytes": 122400000,
      "deploys": 12,
      "evicted_deploys": 3
    }
  ]
}
```

`deploys` counts a repo's non-evicted deploys; `evicted_deploys` the rows
kept as history after their artifacts were reclaimed. `db_bytes` is `0` for
`--in-memory` instances.

### `GET /api/retention`

Returns the [retention policy](/guide/configuration#retention-garbage-collection).
Both limits default to `0` (unlimited — automatic eviction disabled).

```json
{ "max_deploys_per_repo": 10, "max_age_days": 30 }
```

### `PUT /api/retention`

Replaces the retention policy. `max_deploys_per_repo` keeps at most N
non-evicted deploys per repo, newest first; `max_age_days` evicts deploys
created more than N days ago; `0` disables a limit. The policy takes effect
on the next sweep — hourly, or immediately via `POST /api/gc` — so saving
never evicts by itself.

Response: `200 OK` with the saved policy. `400` for a negative limit or
invalid JSON.

### `POST /api/gc`

Runs one retention sweep immediately and reports what it evicted. Evicting
marks the deploy `evicted` (the row survives as history; its subdomain
answers with a "redeploy to rebuild" page) and reclaims the artifacts,
backend state, and logs that no surviving deploy shares. Stale `tmp/`
staging leftovers are collected on every run, so the endpoint is useful even
with retention disabled.

Never evicted: queued/building deploys, each repo's newest ready deploy, and
deploys a branch alias routes to.

```json
{
  "policy": { "max_deploys_per_repo": 10, "max_age_days": 30 },
  "evicted": [
    { "id": 7, "repo": "myapp", "short_sha": "a1b2c3d", "branch": "main" }
  ],
  "freed_bytes": 52428800
}
```

## Webhooks

### `POST /api/webhooks/github`

Receives GitHub webhook deliveries and deploys pushed commits. Enabled only
when the server is started with a webhook secret; see
[GitHub webhooks](/guide/triggers#github-webhooks) for setup.

Every delivery must carry a valid `X-Hub-Signature-256` (HMAC-SHA256 of the
raw body with the shared secret); anything else is `403`. The payload's
repository URLs are matched against registered repo sources — https, ssh,
and git forms of the same repository compare equal.

Responses:

- `202 Accepted` with the deploy — a branch push that matched a registered
  repo. The pushed head sha is deployed (exactly what the event named, even
  if the branch has moved since).
- `200 OK` `{"status": "ignored", "reason": "…"}` — deliveries that are
  deliberately skipped: tag pushes, branch deletions, and non-push events.
  `ping` answers `{"status": "pong"}`.
- `404` — no registered repo matches the payload's repository.
- `503` — the server has no webhook secret configured.
