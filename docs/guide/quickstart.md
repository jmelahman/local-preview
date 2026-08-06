# Quickstart

## Start the server

```sh
preview serve
```

The server listens on `:8080`. Open <http://localhost:8080/> for the
dashboard.

## Onboard a repository

The target repo needs a [`preview.toml`](/reference/preview-toml) at its root
describing how to build and run it. Then register it — the server keeps its
own mirror clone, so previews always build from committed trees, never your
working directory:

```sh
preview repo create myapp --source ~/code/myapp
```

`--source` can be a local path or any clone URL. The name becomes the
subdomain segment, so it must be a lowercase DNS label.

## Deploy a commit

From inside the target repo:

```sh
preview deploy            # deploys HEAD
preview deploy main       # or any branch, tag, or sha
```

The command waits for the build and prints the preview URL:

```
ready: http://a1b2c3d.myapp.preview.localhost:8080/
```

Open it in a browser — `*.localhost` resolves without any DNS setup. The
frontend is served statically; `/api/*` is proxied to the preview's backend
process, which starts on the first request.

## Deploy every commit automatically

```sh
cd ~/code/myapp && preview install-hook
```

This installs a git post-commit hook that requests a deploy of each new
commit (or, if the repo uses the pre-commit framework, prints the stanza to
add to `.pre-commit-config.yaml`).

## Ephemeral runs

For demos and tests, keep the database in memory — deploy history is
discarded on shutdown:

```sh
preview serve --in-memory
```

## Development

Run the backend and dashboard separately for hot reload:

```sh
# Terminal 1 — backend on :8080 (wgo restarts on save; plain `go run` works too)
wgo run . serve

# Terminal 2 — dashboard on :5173, proxying /api to the backend
cd web && npm install && npm run dev
```
