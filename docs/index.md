---
layout: home

hero:
  name: local-preview
  text: A preview deployment per commit.
  tagline: A local-first preview orchestrator — every commit of your repos becomes a servable deployment at its own subdomain, built once, deduplicated by content, served from one binary.
  actions:
    - theme: brand
      text: Get Started
      link: /guide/install
    - theme: alt
      text: Quickstart
      link: /guide/quickstart
    - theme: alt
      text: View on GitHub
      link: https://github.com/jmelahman/local-preview

features:
  - title: A URL for every commit
    details: Previews are served at <sha>.<repo>.preview.localhost — wildcard *.localhost resolves in every modern browser with zero DNS setup, and the same routing maps onto real wildcard domains later.
  - title: Content-addressed builds
    details: Each commit resolves to a (frontend, backend) artifact pair hashed from its git tree. Commits that don't touch a side reuse the existing artifact — a docs-only commit deploys in milliseconds.
  - title: Shared backends, on demand
    details: One supervised process per distinct backend version, started on the first request and reused by every commit that shares it. Hundreds of previews, a handful of processes.
  - title: State follows git lineage
    details: A new backend version forks its database from the nearest deployed ancestor. Straight-line commits feel like one persistent database; divergent branches can never corrupt each other.
  - title: Triggers are adapters
    details: One deploy API behind a CLI, a git post-commit hook, and (soon) pollers and webhooks. preview install-hook makes every commit deploy itself.
  - title: One small binary
    details: Reverse proxy, build queue, artifact store, process supervisor, REST API, CLI, and dashboard in a single self-contained Go binary with SQLite.
---
