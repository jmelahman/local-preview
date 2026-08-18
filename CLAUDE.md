# local-preview — Claude Notes

A local-first preview-deployment orchestrator: a Go backend (repo root) plus
a React/Vite dashboard (`web/`). Registered git repos get a preview per
commit at `<sha>-<repo>.preview.localhost:8080` — content-addressed builds,
on-demand backend processes, lineage-forked state. Architecture notes live in
`docs/guide/concepts.md`; the target-repo contract in
`docs/reference/preview-toml.md`.
Source of truth for run commands is `.vscode/tasks.json`; this file translates
them for direct shell use.

## Running the app

Both processes are long-running. Start them with `run_in_background: true`,
then poll until their ports answer before driving the UI.

**Backend** (`:8080`):

```bash
wgo run . serve
```

**Frontend** (`:5173`, proxies `/api` to the backend):

```bash
cd web && npm install && npm run dev -- --host 0.0.0.0
```

**Frontend against a non-default backend** (`:5174`):

```bash
cd web && PREVIEW_BACKEND=localhost:8080 npm install && npm run dev -- --host 0.0.0.0 --port 5174
```

Wait for both to be reachable before navigating:

```bash
until curl -sf -m 1 http://localhost:8080/api/health >/dev/null \
   && curl -sf -m 1 http://localhost:5173/ >/dev/null; do sleep 2; done
```

## Reproducing fresh-install issues

Boot a clean instance with no on-disk DB:

```bash
wgo run . serve --in-memory
```

The server logs `WARNING: --in-memory set` at startup and uses an ephemeral
SQLite database for the lifetime of the process. Each launch starts from
zero, and shutting the process down discards everything.

## Tests / typecheck / lint

- Go: `go test ./...`
- Frontend types: `cd web && npm run typecheck`
- Frontend lint/format (Biome): `cd web && npm run check` (`check:fix` to
  auto-apply safe fixes). The `prek` `biome` hook runs the same check on
  staged `web/**/*.{ts,tsx,js,jsx,json}` files.
- Playwright E2E: `cd web && npm run test:e2e` (boots the real backend with
  `--in-memory` plus the Vite dev server).
- Pre-commit hooks: `prek run --all-files` (run before committing).

## Driving the UI with Playwright MCP

`.mcp.json` registers `@playwright/mcp --headless --isolated`. Use the
`mcp__playwright__browser_*` tools (e.g. `browser_navigate
http://localhost:5173/`, then `browser_snapshot`) — never spawn `npx
playwright` ad-hoc.

## Layout

- `main.go`, `cmd/`, `internal/` — Go server and CLI subcommands.
- `web/` — Vite + React 19 + Tailwind 4 frontend, embedded into the binary
  with `-tags embed`.
- `docs/` — VitePress site (`guide/`, `reference/api.md`, `reference/cli.md`).
- `.kanban.toml` — maps the task labels above to container ports for
  agentic-kanban sessions.
- `.devcontainer/` — dev sandbox image with an opt-in network firewall.

## Recurring regression notes

Tripwires for code that's bitten us before. Full write-ups live in
`REGRESSIONS.md` (kept out of the published `docs/` site — it's internal
lore, not user docs). When you fix something new and likely to recur, add the
full entry to `REGRESSIONS.md` and a one-line title here.

- Container tests in CI see the runner host's daemon, not their own
  filesystem — bind mounts silently resolve on the host; probe before
  asserting on bind-mounted output.
- Side publishes rename their subtree out of the shared scratch dir —
  anything reading the extracted tree (checksums, post-publish steps) must
  run before `PublishFrontend`/`PublishBackend`, or take its own extraction
  (as the post-ready artifact phase does).
- React's `autoFocus` prop loses the focus race against `<dialog>.showModal()`
  — opt a control into initial focus with `data-autofocus` (which `Modal`
  focuses after opening) instead.
- On-disk leftovers must never gate DB-owned decisions — deletes clean disk
  best-effort, so creates replace orphaned dirs; only DB rows may conflict.
- A startup backlog must not be enqueued from the goroutine that serves HTTP —
  a bounded-channel send before `ListenAndServe` wedges the whole server.
- State derived from a live-process table can't report a process that died —
  a failure record has to outlive the process, or "crashed" reads as "idle".
- Uploads must hash only the side being uploaded — computing every side's hash
  (via `resolveHashes`) makes a one-side upload fail on an unrelated partition.
- GitHub OIDC upload auth needs a custom `aud` (the default GH audience is
  org-wide) and must verify the token before the repo lookup (else uploads leak
  which repos exist).
- A GitHub OIDC token authenticates but authorizes nothing on its own — accept
  one only on a route that names a repo, and bind it to that repo's registered
  source, or any repo's CI can act on any other's.
- SSO's preview-access cookie must stay a separate scope from the apex session
  and be stripped in the proxy `Rewrite` — else the untrusted previewed backend
  receives (and can replay) the control-plane credential.

## Documentation upkeep

User-facing changes need a docs update in the same PR. Match the change to
the page:

- New/changed CLI flags or subcommands → `docs/reference/cli.md`.
- New/changed HTTP endpoints or request/response shapes → `docs/reference/api.md`.
- Config keys (env vars, `--in-memory`, etc.) → `docs/guide/configuration.md`.
- Install/setup steps → `docs/guide/install.md` or `docs/guide/quickstart.md`.

If a feature doesn't fit an existing page, add one under `docs/guide/` and
link it from `docs/.vitepress/config.ts`. Skip docs only for purely internal
refactors with no observable behavior change.
