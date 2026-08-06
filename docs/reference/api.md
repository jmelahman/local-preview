# REST API

All endpoints are JSON over HTTP, rooted at `/api/` on the apex host (any
Host that isn't a preview subdomain). Errors return a JSON body of the form
`{"error": "message"}`.

## Health

### `GET /api/health`

```json
{ "status": "ok", "version": "v0.1.0" }
```

## Repos

### `POST /api/repos`

Registers a repository: the server mirror-clones `source` (a local path or
clone URL). `name` must be a lowercase DNS label — it becomes the subdomain
segment.

Request:

```json
{ "name": "myapp", "source": "/home/me/code/myapp" }
```

Response: `201 Created` with the repo. `400` for an invalid name/source or a
failed clone, `409` if the name is taken.

```json
{ "id": 1, "name": "myapp", "source": "/home/me/code/myapp", "created_at": "2026-01-01T00:00:00Z" }
```

### `GET /api/repos`

Returns all repos. `GET /api/repos/{name}` returns one (`404` if missing).

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

Response: `202 Accepted` with the deploy. `404` for an unknown repo, `400`
for an unresolvable ref.

```json
{
  "id": 7,
  "repo": "myapp",
  "sha": "a1b2c3d4…",
  "short_sha": "a1b2c3d",
  "ref": "main",
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
state: `running`, `starting`, or `stopped` — backends start on demand), and
`fe_process` (same states) when the frontend is a
[process](/reference/preview-toml#process-mode-frontends) rather than a
static bundle.

### `GET /api/deploys`

Returns deploys, newest first. Filter with `?repo=<name>`.

### `GET /api/deploys/{id}`

Returns one deploy (`404` if missing).

### `GET /api/deploys/{id}/logs`

Returns a plain-text snapshot of the frontend and backend build logs.
Deploys that share an artifact share that artifact's build log.
