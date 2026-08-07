# Deployment triggers

Deploying is one API call — `POST /api/deploys` — wrapped by `preview
deploy`. Everything else is a thin adapter over it, and every adapter
produces identical artifacts for the same commit. Pick whichever fits how
commits reach you; they compose freely.

## Manual

```bash
preview deploy            # HEAD of the current repo
preview deploy my-branch  # any branch, tag, or sha
```

The repo is auto-detected by matching the working directory (or its `origin`
URL) against registered sources.

## Post-commit hook

For the repo you're actively hacking on: deploy every commit the moment you
make it.

```bash
cd ~/code/myapp
preview install-hook
```

Installs a git post-commit hook that runs `preview deploy --no-wait` on each
new commit. Repos using the pre-commit framework get the equivalent
`.pre-commit-config.yaml` stanza printed instead. See
[`preview install-hook`](/reference/cli#preview-install-hook).

Alternatively, pre-commit-framework repos can subscribe to the hook this
repository publishes (`.pre-commit-hooks.yaml`). The framework builds the
CLI itself at the pinned rev — nothing needs to be preinstalled:

```yaml
  - repo: https://github.com/jmelahman/local-preview
    rev: v0.1.5  # pin to your server's release; any commit sha also works
    hooks:
      - id: local-preview-deploy
```

Install the stage with `pre-commit install --hook-type post-commit` (prek:
`prek install --hook-type post-commit`), and bump `rev` with `pre-commit
autoupdate` (prek: `prek auto-update`, `--bleeding-edge --freeze` to pin an
untagged sha).

## Watched repos

For repos that receive commits without you — a teammate's pushes, a bot, a
repo you registered by clone URL: the server polls the mirror and deploys
new branch tips on its own.

```bash
preview repo watch myapp                            # all branches
preview repo watch myapp --branches "main,release/*"
preview repo create myapp --source <url> --watch    # watched from the start
```

Watched repos are fetched every `--poll-interval` (default `1m`). Any
matched branch tip without a deploy is deployed; a branch that advances gets
a fresh preview while the old commits keep theirs. `--branches` takes
comma-separated globs matched against the branch name (globs don't cross
`/`, so `release/*` matches `release/1.0` but not `release/1.0/hotfix`).

Note that enabling watch deploys the *current* tip of every matched branch
that has no deploy yet — on a repo with many long-lived branches, narrow
with `--branches` first. Content addressing keeps the cost down (unchanged
sides of each commit are never rebuilt), but each distinct tip still builds
once.

The same poll cleans up in the other direction. Each fetch prunes branches
deleted upstream, and any preview whose commit is no longer reachable from a
surviving branch tip is then **evicted**: its backend processes stop and its
build artifacts are reclaimed, freeing disk, while the deploy record stays as
a tombstone. Reachability is judged against *every* branch, not just the
matched ones, so a commit that still lives on another branch keeps its
preview — as does one whose branch was merged into `main`'s history with a
plain merge commit (a squash or rebase merge rewrites the commit, so its
preview is evicted like any other deleted branch). Eviction is reversible: an
evicted host serves a *cleaned up* notice, and `preview deploy <sha>` — or the
**redeploy** button on the deployment's dashboard row — rebuilds it on demand.

`preview repo unwatch myapp` stops the polling; existing previews stay, and
eviction stops with it.

## GitHub webhooks

For pushes to GitHub, skip the polling delay: GitHub tells the server
directly.

1. Start the server with a shared secret and register the repo by its clone
   URL:

   ```bash
   PREVIEW_GITHUB_WEBHOOK_SECRET=<secret> preview serve
   preview repo create myapp --source https://github.com/acme/myapp.git
   ```

2. In the GitHub repository, add a webhook (**Settings → Webhooks → Add
   webhook**):
   - **Payload URL**: `http://<your-server>/api/webhooks/github` — the
     server must be reachable from GitHub (a tunnel like `ngrok` or
     `cloudflared` works for a laptop).
   - **Content type**: `application/json`
   - **Secret**: the same `<secret>`
   - **Events**: just the push event.

Each branch push deploys the pushed head commit. Deliveries are matched to
registered repos by repository URL (https, ssh, and git forms of the same
repo all match), so the source can be any clone-URL form. Tag pushes and
branch deletions are acknowledged but ignored; GitHub's webhook delivery
log shows the reason for anything skipped. (A branch deletion isn't a
teardown signal here — if the repo is also watched, the poller evicts the
orphaned previews on its next fetch, as [above](#watched-repos).)

The endpoint refuses unsigned or mis-signed deliveries, and doesn't exist
at all until a secret is configured. Details in the
[API reference](/reference/api#post-api-webhooks-github).

Webhooks and watching compose: a webhook gives instant deploys while
watching backstops missed deliveries on its next poll.
