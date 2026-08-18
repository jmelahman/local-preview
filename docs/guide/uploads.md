# Uploading prebuilt artifacts

Your CI already builds every frontend, backend, and downloadable artifact for
each commit. Rather than have the preview server rebuild them, you can **upload**
what CI built and let the server serve it canonically. An upload lands in the
exact [content-addressed](/guide/concepts#content-addressing) slot a build would
have targeted, so a deploy of that commit finds the side already present and
skips the build.

The server stays the authority on the hash: you hand it a ref and a tarball, it
resolves the commit, reads `preview.toml`, computes the side's hash the same way
a build does, and publishes your bytes there. You never compute a hash yourself.

## The flow

An upload primes the store; it does not create a deploy. Order is up to you —
upload then deploy, or deploy then upload a side to replace what was built. The
common CI shape is build → upload → deploy:

```bash
# In CI, after building each side:
preview upload frontend web/dist          --repo myapp --server $PREVIEW_URL
preview upload artifact  cli   dist/cli    --repo myapp --server $PREVIEW_URL
preview deploy $GITHUB_SHA                 --repo myapp --server $PREVIEW_URL
```

Each `upload` defaults the ref to HEAD; pass an explicit ref (a sha is ideal in
CI) as the last argument. `--repo` is matched from the working directory when
omitted, exactly like `preview deploy`.

Fold it into one step with `--deploy`, which deploys the commit after the upload
and waits for its preview URL:

```bash
preview upload frontend web/dist $SHA --repo myapp --deploy
```

## What each side expects

`<path>` may be a directory (its **contents** are tarred — no wrapping folder,
so the tar root becomes the published root) or an existing `.tar`/`.tar.gz`.

| Side | Upload |
| --- | --- |
| `frontend` | The `dist` tree for a static bundle, or the built `path` tree for a [process-mode frontend](/reference/preview-toml#process-mode-frontends) |
| `backend` | The built `backend.path` tree — run as-is by the deploy's `run` command |
| `artifact <name>` | A tree containing the artifact's declared `files` at their `path`-relative locations |

Uploading the backend doesn't change how a deploy wires it up: the deploy still
provisions the [state directory](/guide/concepts#state-follows-git-lineage) and
starts the process on demand — it just skips the build step.

## Sharing and caching

Uploads inherit content-addressing wholesale. Upload a side once and every
commit whose partition is unchanged reuses it — a docs-only or backend-only
commit serves the same uploaded frontend without another upload. A repeated
upload of an already-present hash is a no-op (`published: false`); pass
`--overwrite` to replace the bytes (a running backend keeps its old process
until restarted, same as `--rebuild`).

## Trust and cleanup

- **Trust.** The server can't verify uploaded bytes reproduce the commit —
  content addressing here is by git tree, not by built output. It trusts your
  upload exactly as it trusts its own build. There is no authentication on the
  endpoint, matching the rest of the [API](/reference/api#uploads); run the
  server where only your CI can reach it.
- **Cleanup.** An uploaded side is reclaimed by [garbage
  collection](/reference/api#post-api-gc) once the deploy referencing its hash
  is removed, like any built artifact. A side you upload but *never* deploy has
  no deploy referencing it and isn't swept — using `--deploy` (or deploying the
  commit) keeps uploads on the normal retention path.
