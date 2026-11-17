# Worker-tier architecture

How a split control/worker deployment fits together: one small always-warm
**control node** that owns all state and decisions, and an elastic tier of
disposable **workers** that serve preview processes on its behalf. Flag-level
reference lives in [Configuration →
Split control / worker plane](/guide/configuration#split-control-worker-plane);
this page is the architecture.

```
                        ┌──────────────────────────────┐
 users ── ALB (TLS) ──▶ │ control node                  │
                        │  dashboard · /api/ · proxy    │
                        │  git watch · builds · SQLite  │
                        │  shared deps stack (compose)  │──┐ deps ports,
                        └──────┬───────────┬────────────┘  │ private IP only
                               │           │               │
                    worker API │           │ S3 artifact   │
              (private, shared │           │ tier          │
                       secret) ▼           ▼               │
                        ┌──────────────┐  ┌─────────────┐  │
                        │ worker(s)    │  │ S3 bucket   │  │
                        │ preview      │◀─│ artifacts   │  │
                        │ processes    │  │ (hydrate)   │  │
                        └──────┬───────┘  └─────────────┘  │
                               └───────────────────────────┘
```

## One orchestrator, two transports

There is a single orchestrator implementation (`supervise.Manager`); what
varies is the transport the proxy reaches it through. The proxy routes to a
`Backends` interface returning `host:port`: on a single node
(`--role=all`) that is the local manager over loopback, and on a control node
it is the **fleet registry**, which places each key on a worker and calls that
worker's API. A worker is the same binary with `--role=worker` — its own
manager, driven remotely.

## What travels to a worker, and how

A worker is deliberately stateless — that is what makes it disposable enough
to autoscale. Everything a process needs arrives through two channels:

- **Artifact files** hydrate from the S3 artifact tier on demand. Local disk
  on every node is only a cache of the tier; the extractor verifies the
  content-byte count recorded at upload time before publishing. A worker
  sweeps its coldest resident artifacts once they exceed half its data
  filesystem (override with `--cache-max-artifact-bytes`), so its root disk
  only needs to hold the run images plus the *working* set, not every preview
  it has ever served.
- **The run contract** (argv, run image, env, health path, timeouts, init
  steps) travels *on the wire*: the control node resolves it from its own
  database and ships it inside every ensure request (`supervise.WireSpec`).
  Workers never replicate the control database and never call back to it.

Two details of the wire spec are load-bearing:

- **State dirs travel as identity, not paths.** Each node recomputes the path
  against its own store root — and a worker's state dir starts fresh (see
  [limitations](#limitations)).
- **`init_done` flows back by inference, not by wire.** A successful backend
  ensure proves the init steps succeeded (the worker fails the ensure
  otherwise), so the control node records it in its own database and ships
  `true` from then on — a fresh worker skips init instead of re-running it.
  The worker's spec cache also keeps `init_done` sticky per node, as the
  backstop for offers that race that write. The reverse flows too: when an
  ensure that shipped `init_done: true` comes back as a worker-reported start
  failure (a 502 from the ensure route — not a transport error, which proves
  nothing), the control node clears its `init_done_at` and the worker marks
  the artifact revoked, so the next ensure re-runs init. A revocation
  outranks both the sticky cache and the next offer until an init succeeds
  again.
- **The `min_warm` floor is fleet-wide; `max_warm` is per worker.** The
  heartbeat loop ranks the fleet's processes by recency (`last_touch` rides
  each worker's report) and pushes every worker its share of the floor, so
  "min 12" protects the 12 most-recent processes total — not 12 per worker,
  which would multiply with fleet size and, since floor-protected processes
  never idle out, silently pin every worker against ASG scale-in. An
  optional `--min-warm-window` ("Mon-Fri 08:00-18:00 America/Chicago")
  zeroes the floor outside working hours so the fleet drains to zero;
  demand-driven scale-out is unaffected. Because the floor keeps fleet
  utilization above any sane load threshold, the control node also publishes
  `IdleWorkers` — fresh, non-draining workers serving nothing — so an alarm
  can reclaim an empty node mid-day without waiting for load to collapse
  (the example terraform wires this to the shared scale-in policy).

## Placement

The registry places keys by rendezvous (highest-random-weight) hashing on
(repo, artifact hash): the same artifact consistently lands on the same
worker, reusing its warm process and local cache, and workers joining or
leaving move a minimal set of keys. A process-mode frontend hashes on its
*backend's* hash — the pair shares a per-deploy docker network that exists on
exactly one node. Draining or full workers are skipped, falling back to the
least-loaded one.

## Reaching a worker's processes

Preview processes on a worker publish their ports on the worker's routable
address (derived from `--worker-listen`) in addition to loopback, and the
control node's proxy dials `worker_ip:port` directly. The worker API itself —
an intentional remote-code-execution surface, since it starts arbitrary
preview processes — listens on the worker's private IP behind a shared
secret, admitted only from the control node's security group, and must never
sit behind the public load balancer.

## The dashboard's view

Live process state (status, crash detail, resource samples, run logs) is
node-local truth on whichever worker runs the process. The control node's API
renders from a **fleet report**: each worker exposes a bulk dump of every
non-idle key, and the registry merges them into a briefly-cached view — one
round trip per worker per cache window, however many deploys the dashboard
lists. Run logs are fetched from the worker that reported the key (or by
asking the fleet, since logs outlive processes). Stops fan out to every
worker.

## Shared dependencies

Per-commit processes move to workers; the services every preview shares (a
repo's database, search, cache) stay on the control host as a compose stack,
publishing their ports **only on the control host's static private IP** — the
address a manifest reaches them at from any node. Credentials live in SSM,
land in each node's boot-time env file, and reach preview processes through
the manifest's [`{secret:NAME}`
placeholder](/reference/preview-toml#env-placeholders) — never in the
manifest, the database, or Terraform state. Per-commit isolation inside those
shared services keys on `{hash}` (e.g. `POSTGRES_DB = "preview_{hash}"`).

## Builds and uploads

Without ingestion workers, builds and CI uploads run on the control node.
The serving workers above retain read-only artifact access. The optional
[ingestion pool](#isolated-ingestion-pool) runs uploads and direct deploy
builds on separate nodes. Only the control node can publish canonical artifacts.

## Limitations

- **`{state_dir}` does not follow previews across nodes.** Worker state dirs
  start fresh — lineage forking is a build-time, control-node mechanism. A
  repo served by workers should keep per-commit state in shared services
  keyed by `{hash}` instead.
- **The fleet is static**: workers come from `--worker-endpoints`; discovery,
  scale-in draining, and autoscaling signals are infrastructure concerns (the
  load ratio is logged as `fleet: load=…` for a policy to target-track).
- **Retention runs on the control node.** Worker disks are caches — idle
  processes reap themselves, cold artifacts are swept and re-hydrate, and
  each preview's docker network goes away with its last container — but a
  worker's state dirs are only reclaimed by instance replacement.

## Isolated ingestion pool

Ingestion workers are separate from the serving workers above. They pull jobs
from a private control API. They do not serve preview traffic.

```text
CI -> control: reserve job -> S3: upload archive
CI -> control: submit job
control queue <- ingestion worker: claim and renew lease
S3 staging -> ingestion worker -> S3 attempt outputs
control: validate lease and outputs -> canonical S3 artifacts -> ready deploy
```

The control node stores job metadata in its own SQLite database. Each worker
uses a private scratch directory and an ephemeral job database. Nodes never
share a SQLite file. The worker resolves the commit, reads the manifest, and
computes content hashes. Clients cannot choose canonical artifact hashes.

A worker writes only attempt-scoped objects. Its IAM role cannot write
canonical artifacts. The control node checks the active lease and object
ETags before promotion. A stale worker cannot publish a newer attempt's result.
Publication survives control restarts. The deploy becomes ready in one database
transaction. Downloadable artifacts run as a later job, so they do not delay
preview readiness.

Admission accepts 100 unfinished jobs by default. One extra slot permits a
parent job to schedule its artifact continuation before it finishes. Each
worker runs one job at a time. Leases last one minute and renew every 15 seconds.
An expired lease can retry, up to three attempts within the 25-minute job
deadline. Scale-in protection covers the active job. An idle autoscaled worker
can terminate after five minutes, while respecting the group's minimum size.

The Terraform example defaults to zero minimum workers and four maximum
workers. Queued demand drives scale-out, including from zero. Each node has
80 GiB of disk. The ingestion daemon has an 8 GiB memory limit and two CPUs.
Each Docker build has a separate 2 GiB memory limit, no swap, and two CPUs.
A worker needs 10 GiB of free scratch space before it claims a job. Builds
that require Docker fail if Docker is unavailable. Plain host steps run inside
the bounded ingestion daemon.

The coordinator uses private port 9102 and a separate ingestion secret. Keep
this listener outside the public load balancer. Allow only ingestion nodes
to reach it. See [configuration](/guide/configuration#isolated-ingestion) and
[the worker command](/reference/cli#preview-ingest-worker).

Staged uploads use an exact-size presigned S3 POST. The default archive limit
is 2 GiB; worker extraction also has a 2 GiB limit. Signatures last ten minutes.
The example S3 lifecycle removes staging objects after one day. The server
retains terminal job history for seven days.

Remote job logs contain tails only: up to 256 KiB per preview side and
512 KiB total across downloadable artifacts. These limits do not describe
legacy local build logs or preview process logs.

This path offloads staged CI uploads, direct API deploy builds, and their
artifact continuations. Repository onboarding and watched-repository Git work
still use the control node's existing paths. The serving architecture above
and its shared dependency placement remain the same.
