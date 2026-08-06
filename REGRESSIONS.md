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
