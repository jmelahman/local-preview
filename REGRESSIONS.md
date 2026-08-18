# Regressions

Long-form write-ups of bugs that have bitten this project and are likely to
recur. Each entry gets a one-line tripwire in `CLAUDE.md`'s "Recurring
regression notes" section pointing here, so anyone touching the affected area
reads the full story first.

An entry should cover: the symptom, the root cause, why the fix works, and
what kind of change would reintroduce it.

## Container tests in CI see the runner host's daemon, not their own filesystem

**Symptom.** `TestAutoRunnerContainer` passed locally but failed on CI with
`container output on host: "", open .../out.txt: no such file or directory` —
the build container ran fine (its workdir output appeared), yet the file it
wrote never showed up in the test's scratch dir.

**Root cause.** The `golang` CI job runs inside the devcontainer image, and
GitHub Actions mounts the runner host's Docker socket into job containers.
Any container the test launches is therefore a *sibling* on the host daemon,
and bind-mount paths in `HostConfig.Binds` are resolved on the **daemon's
host**, not inside the CI job container. The scratch dir under the job
container's `/tmp` doesn't exist on the host, so the daemon auto-creates an
empty host-side directory, the build container writes its output there, and
the test process never sees it.

**Fix.** The test probes the precondition it actually needs: it writes a
marker file into the scratch dir and runs a throwaway container that checks
the marker is visible through a bind mount. If the probe fails, the daemon
doesn't share the test's filesystem and the test skips. Real coverage is
preserved anywhere docker is local (dev machines, docker-in-docker).

**What would reintroduce it.** Any new test (or product path) that launches
a container and expects bind-mounted output to round-trip will break the
same way whenever the code runs inside a container that talks to an outside
daemon — CI job containers, devcontainers with the socket mounted, remote
`DOCKER_HOST`. Gate such tests on the same marker-file probe. For product
code, note that `autoRunner` has the same blind spot: builds "succeed" but
outputs land on the daemon host.

## Side publishes rename their subtree out of the shared scratch dir

**Symptom.** Downloadable-artifact builds failed with `chdir
<scratch>/backend: no such file or directory` when they ran after the
backend build of the same deploy — even though the directory was extracted
and the backend build had just used it successfully.

**Root cause.** All of a deploy's builds share one extracted commit tree,
and `store.publish` lands artifacts with an atomic `os.Rename` of the built
subtree — `PublishBackend` moves `<scratch>/<backend.path>` itself into the
artifact store, and static/process frontends do the same with their dist or
path tree. Publishing a side therefore *consumes* part of the scratch tree;
any later step that reads it (a build cwd, declared output files) finds it
gone.

**Fix.** Originally, downloadable artifacts built first: their publish only
*copies* the declared files out, leaving the tree intact for the frontend
and backend publishes that follow. Since artifacts moved to a post-ready
build phase (they must not gate the preview), they take the other escape
hatch instead: `buildArtifacts` extracts its own tree and never touches
`buildDeploy`'s scratch dir.

**What would reintroduce it.** Adding a new output kind, reordering the
build sequence, or any post-publish step that touches the scratch tree
(checksums, manifests, uploads) after `PublishFrontend`/`PublishBackend`
ran. Anything that must read the extracted tree has to run before the
rename-based publishes — or take its own extraction, as the artifact phase
does.

## React `autoFocus` doesn't survive `<dialog>.showModal()`

**Symptom.** The `ConfirmDialog` confirm button carried React's `autoFocus`
prop so that pressing Enter right after the modal opened would confirm the
destructive action. It didn't: Enter cancelled instead, because focus sat on
the header close button.

**Root cause.** React implements the `autoFocus` prop as an imperative
`.focus()` call during commit — it does *not* render a real `autofocus`
attribute. `Modal` opens the dialog with `dialog.showModal()` from a *parent*
effect, which runs after the child button mounts. `showModal()` then runs the
native dialog-focusing steps, and with no `autofocus` attribute in the DOM to
delegate to, it focuses the first focusable descendant (the close button),
clobbering React's earlier `.focus()`.

**Fix.** Focus after the dialog is modal, not before. `Modal` calls
`dialog.querySelector("[data-autofocus]")?.focus()` immediately after
`showModal()`, and the control that wants initial focus opts in with a plain
`data-autofocus` attribute. Running last, this focus sticks. Using a data
attribute (not `autoFocus`) also sidesteps Biome's `a11y/noAutofocus` rule.

**What would reintroduce it.** Reaching for the `autoFocus` prop on any
control inside a `<dialog>` that opens via `showModal()` — it will lose the
focus race every time. Mark the target with `data-autofocus` instead. Same
trap applies to focusing in a child effect: child effects run before the
parent's `showModal()`, so that focus is clobbered too.

## On-disk leftovers must never gate DB-owned decisions

**Symptom.** A repo deleted from the "Registered repositories" modal could
not be re-registered: `POST /api/repos` failed with "already exists" even
though the repo was gone from the DB and the UI. With the registration gone,
`DELETE /api/repos/{name}` 404s — a permanent dead end with no UI escape.

**Root cause.** `gitrepo.Add` refused to clone when `repos/<name>.git`
already existed on disk. Deletion removes the mirror clone best-effort
*after* the DB rows, and the mirror can survive it: an in-flight fetch
(watcher poll, deploy ref-resolution) re-creates `refs/`/`objects/` subdirs
while `RemoveAll` walks the tree, and `--in-memory` sessions register
mirrors into the persistent data dir but forget the rows on shutdown. Either
way the name was bricked by state the DB no longer knew about.

**Fix.** The repos table is the sole authority on name ownership. `Add` now
clones into a temp dir inside the repos dir and swaps it over `<name>.git`,
replacing any orphaned leftover (temp-then-rename so a failed clone never
destroys a healthy mirror). The `409` conflict comes only from the DB check.

**What would reintroduce it.** Any new code path that treats presence on
disk (mirror clones, artifact/state/log dirs) as proof of registration, or
that errors instead of overwriting when creating a resource whose uniqueness
the DB already enforces. Disk contents are a cache; rows are the truth.

## A startup backlog must not be enqueued from the goroutine that serves HTTP

**Symptom.** After an instance replacement the orchestrator container ran with
zero CPU, printed nothing, never listened on its port, and the load balancer
returned 502 indefinitely. `SIGQUIT` showed goroutine 1 parked in `[chan send]`
at `build.(*Queue).enqueue`, called from `Queue.Start`.

**Root cause.** `Start` re-enqueued every unfinished deploy *before* launching
its workers. `work` is buffered at 256, so a data volume carrying more than 256
`queued`/`building` rows — trivially reached by a repo-wide backfill that was
interrupted — filled the buffer and blocked the caller. That caller is `run()`,
which had not yet reached `ListenAndServe`. Nothing could drain the channel and
nothing could accept the request that would have cancelled the work.

**Fix.** `Start` launches its workers first, then resumes the backlog from a
goroutine whose send selects on `ctx.Done()`. Workers drain behind it, and a
backlog of any size delays only its own resume.

**What would reintroduce it.** Any bounded-channel send on the startup path
before the server listens, or a `Start`-time loop that assumes the DB holds
fewer rows than a buffer. Startup work that can be proportional to accumulated
state belongs in a goroutine, and its sends need a cancellation arm.

## Uploads must hash only the side being uploaded

**Symptom.** `preview upload frontend …` (or backend, or one artifact) failed
with an unrelated side's error — e.g. `artifacts.cli: artifact partition
matches no files at this commit` — even though the frontend it was publishing
was perfectly valid.

**Root cause.** `Queue.Upload` computed the target slot by calling
`resolveHashes`, which hashes *every* side of the commit (that's what
`buildDeploy` needs — it decides what to build across all sides). Any side
whose partition is empty or otherwise unhashable at that commit made the whole
call fail, so uploading side A depended on sides B and C being valid.

**Fix.** Split the per-commit `hashInputs` (devcontainer + `LsTree`) from the
per-side computations (`feHashOf`/`beHashOf`/`artHashOf`). `resolveHashes`
composes all three for the build; `Upload` calls only the one for the side it
is publishing. Both paths share the same per-side funcs, so an upload still
lands byte-for-byte in the slot a build would target.

**What would reintroduce it.** Having `Upload` (or any single-side path) reach
for `resolveHashes` again for convenience. Hash exactly the side you're
touching — never the whole commit — unless you genuinely need every hash.
