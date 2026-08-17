# Concepts

What makes deployments cheap enough to run one per commit.

## Content addressing

A deploy doesn't build "a commit". It resolves the commit into two
artifacts:

- the **frontend hash** covers the git tree entries under `frontend.path`
- the **backend hash** covers entries under `backend.path`, minus the
  frontend subtree, minus `backend.exclude` patterns

Each hash also includes its own `preview.toml` section, so changing a build
command busts the cache even when no source changed. Hashes are computed from
git's own blob IDs (mode, object ID, path), not from file contents, so
hashing costs close to nothing.

Artifacts are stored content-addressed on disk. If a commit's frontend hash
already exists, that side isn't rebuilt, and a commit that touches neither
side (docs-only) deploys in milliseconds. Builds land atomically: an
artifact directory either exists completely or not at all.

Builds always run against the committed tree, extracted from the server's
mirror clone, never against a working directory. Every trigger therefore
produces identical artifacts for the same commit.

## Deploy-agnostic frontends

One frontend bundle is served under every subdomain that references it, so
bundles must not bake in per-deploy configuration: build with base path `/`
and call the backend with relative `/api/...` URLs. The proxy picks the right
backend from the Host header.

## Backend sharing and on-demand processes

The unit of a running backend is the *backend artifact*, not the commit. All
deploys whose backend hash matches share one supervised process, so
iterating on frontend code reuses the same backend, process and all.

A new deploy's processes start automatically as soon as its build turns
ready, so the first visit usually hits a warm process. Anything not
running — stopped by the idle timeout or warm cap, or after a server
restart — starts on demand instead: the first `/api/*` request boots the
backend (the proxy waits briefly, then shows a self-refreshing "starting"
page) and later requests hit the warm process. Backends bind loopback-only;
the only exposed listener is the orchestrator's own address.

## Downloadable artifacts

A manifest can also declare
[named artifacts](/reference/preview-toml#artifacts-name) — prebuilt
binaries like a monorepo's CLI — built from the same commit as the two
sides. They're hashed and cached exactly the same way, but nothing is ever
run: the declared build outputs are published once per hash and served as
downloads from the dashboard and API. Artifacts build only after the
frontend and backend have made the deploy ready, so a slow artifact matrix
never delays the preview itself.

## State follows git lineage

Each backend artifact owns a **state directory**, passed to the process via
`{state_dir}`. When a new backend hash first appears, its state dir is
*forked* — copied — from the nearest ancestor commit (first-parent walk) that
was deployed and still has state. With no such ancestor it starts empty.

Consequences:

- On a straight line of commits, previews feel like one persistent database:
  each backend change inherits the data you created before it.
- Two branches with divergent schema migrations can never corrupt each
  other. A state dir only ever receives migrations from its own ancestry.
- The orchestrator never parses or understands migrations. Your app migrates
  whatever state dir it's handed at boot, exactly like production.

Before copying, the ancestor's process is briefly stopped so the copy is
consistent; it cold-starts again on its next request.

Apps that migrate at boot can declare
[`init` commands](/reference/preview-toml#init-commands) instead of wedging
migrations into `run`: they execute once per backend artifact, right when its
state dir was forked from an older schema, and every later cold start skips
them. Exclusive state-dir ownership is what makes the skip safe rather than
an optimistic guess.

## Subdomain routing

Every preview host is `<sha-prefix>-<repo>.<domain>`. Labels resolve by sha
*prefix match*, and the router never guesses: an ambiguous prefix is refused
with the candidate list, and stored short-shas grow until unique.

Both parts share **one** DNS label on purpose: a wildcard record — and a
wildcard certificate — matches exactly one label, so a dotted
`<sha>.<repo>.<domain>` would need a record and a cert per repo, while
`*.<domain>` covers every repo at once. Repo names are themselves single DNS
labels and may contain hyphens, so the router resolves the split against the
repo registry rather than cutting at the first hyphen.

## Trigger adapters

Deploying is one API call (`POST /api/deploys`) wrapped by the CLI. Everything
else is a thin adapter — the git post-commit hook, the branch poller behind
watched repos, and the GitHub webhook receiver. The core doesn't care what
triggered a deploy; every trigger produces identical artifacts for the same
commit. See [deployment triggers](/guide/triggers).
