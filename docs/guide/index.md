# Introduction

local-preview is a self-hostable preview-deployment orchestrator: it turns
every commit of your registered git repositories into a servable preview at
its own subdomain, like `a1b2c3d.myapp.preview.localhost:8080`.

The server is one Go binary that embeds:

- a reverse proxy routing previews by Host header
- a build queue producing content-addressed artifacts
- a process supervisor running backend processes on demand
- a JSON REST API under `/api/`, a CLI, and a dashboard SPA
- a SQLite database (pure Go, no CGO)

Target applications declare how to build and run themselves in a small
[`preview.toml`](/reference/preview-toml) at their repo root. The
[Concepts](/guide/concepts) page explains the ideas that make previews cheap:
content addressing, backend sharing, and lineage-forked state.

## Layout

| Path | Purpose |
| --- | --- |
| `main.go`, `cmd/`, `internal/` | Go server and CLI subcommands |
| `web/` | Vite + React dashboard, embedded into the binary at release |
| `docs/` | This VitePress site |
| `.github/`, `.goreleaser.yaml`, `pyproject.toml` | CI and release automation |

Continue with [Install](/guide/install) or the [Quickstart](/guide/quickstart).
