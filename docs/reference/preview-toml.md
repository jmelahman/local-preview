# preview.toml

The contract a target repository declares at its root so the orchestrator can
build and run it. It is always read from the committed tree of the deployed
sha (`git show <sha>:preview.toml`) — never from a checkout — so old commits
build with the manifest they shipped with.

Unknown keys are a hard error, so typos fail the deploy instead of silently
changing nothing.

## Example

The shape this repository itself uses (Go backend at the root, Vite frontend
in `web/`):

```toml
[frontend]
path  = "web"                    # hash root + build cwd (repo-relative)
build = [["npm", "ci"], ["npm", "run", "build"]]
dist  = "dist"                   # build output, relative to path

[backend]
path          = "."              # hash root + build cwd
exclude       = ["docs/", "*.md", ".github/"]   # hashing only
build         = [["go", "build", "-o", "bin/server", "."]]
run           = ["./bin/server", "--addr", ":{port}", "--data-dir", "{state_dir}"]
health_path   = "/api/health"
start_timeout = "20s"            # optional
```

## `[frontend]`

| Key | Required | Description |
| --- | --- | --- |
| `path` | yes | Subtree that defines the frontend hash; build commands run with this as their working directory |
| `build` | yes | Build steps as argv arrays (no shell; use `["sh", "-c", "..."]` explicitly if you need one) |
| `dist` | yes | Directory the build produces, relative to `path`; published as the static bundle |
| `image` | no | Container image the build steps run in (see [Build images](#build-images)) |

The bundle must be deploy-agnostic: base path `/`, relative `/api` calls, no
per-deploy values baked in at build time. One bundle is served under every
subdomain that references it.

## `[backend]`

| Key | Required | Description |
| --- | --- | --- |
| `path` | yes | Subtree that defines the backend hash (minus `frontend.path` and `exclude`); build cwd; the built subtree becomes the artifact |
| `exclude` | no | Patterns removed from the backend hash: `dir/` prefixes, or globs matched against the full path and basename (`*.md`) |
| `build` | yes | Build steps as argv arrays |
| `run` | yes | Command that starts the server, executed with the artifact directory as cwd |
| `health_path` | yes | Path polled until it returns 200 after start |
| `start_timeout` | no | How long the process gets to become healthy (default `20s`) |
| `idle_timeout` | no | Idle period before the process is stopped (default `30m`; enforcement lands in a future release) |
| `image` | no | Container image the build steps (not the server) run in (see [Build images](#build-images)) |

### Run templating

Two placeholders are substituted into every `run` argv element:

| Placeholder | Value |
| --- | --- |
| `{port}` | The loopback port assigned to this process — bind `127.0.0.1:{port}` |
| `{state_dir}` | The artifact's mutable state directory (see [state lineage](/guide/concepts#state-follows-git-lineage)) |

## Alternate locations (embedders)

Applications embedding the orchestrator can offer additional manifest
locations via `Options.ManifestSources` — typically a table inside their own
config file, so target repos need only one file. agentic-kanban, for
example, accepts the same schema under a `[previews]` table in
`.kanban.toml`:

```toml
[previews.frontend]
path  = "web"
build = [["npm", "ci"], ["npm", "run", "build"]]
dist  = "dist"

[previews.backend]
path        = "."
build       = [["go", "build", "-o", "bin/server", "."]]
run         = ["./bin/server", "serve", "--addr", "127.0.0.1:{port}", "--data-dir", "{state_dir}"]
health_path = "/api/health"
```

Sources are tried in order (`preview.toml` first, by convention) and the
manifest is still always read from the committed tree. Artifact hashes cover
the parsed manifest, not its location, so moving unchanged config between
sources doesn't invalidate caches.

## Build images

With `image` set on a side, its build steps run inside one-shot containers
(the extracted commit tree bind-mounted, output streamed into the build
log) instead of on the server's host — reproducible toolchains that don't
care what the host has installed, including when the server itself runs in
a toolchain-less container. The image is part of the manifest section, so
it feeds the artifact hash: bumping the image rebuilds.

Requires a reachable Docker daemon (`$DOCKER_HOST`, `/var/run/docker.sock`,
or the rootless per-user socket). If none is reachable, the build logs a
warning and falls back to host execution. Toolchain caches (npm, Go) are
kept warm on a named volume per image. Embedding applications with their
own runners (agentic-kanban's devcontainer builds) honor `image` when set —
an explicit image beats environment discovery.

## Hashing caveats

Each side's hash covers its declared partition **plus its own manifest
section** — editing a build command rebuilds that side. Files outside the
declared partition don't bust the cache; if your backend depends on files
across the whole tree, use `path = "."`.
