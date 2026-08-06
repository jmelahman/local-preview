-- Schema for the local-preview orchestrator. Applied idempotently on every
-- Open; add new tables as CREATE TABLE IF NOT EXISTS statements.
--
-- Log/artifact CONTENT never lives here (single-connection SQLite) — only
-- paths. The supervisor's live process state is in-memory; process_records
-- exists purely for crash recovery and process_events for observability.

CREATE TABLE IF NOT EXISTS repos (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    -- A lowercase DNS label: it is the <repo> segment of preview hosts.
    name TEXT NOT NULL UNIQUE,
    source TEXT NOT NULL,
    bare_path TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE TABLE IF NOT EXISTS deploys (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL REFERENCES repos(id),
    sha TEXT NOT NULL,
    -- Shortest prefix (>=7 chars) unique among this repo's deploys at insert
    -- time; preview hosts route on sha prefixes.
    short_sha TEXT NOT NULL,
    ref TEXT NOT NULL DEFAULT '',
    fe_hash TEXT NOT NULL DEFAULT '',
    be_hash TEXT NOT NULL DEFAULT '',
    -- Build outcome only. Process runtime status is the supervisor's,
    -- merged into API responses at read time.
    status TEXT NOT NULL DEFAULT 'queued'
        CHECK (status IN ('queued', 'building', 'ready', 'failed', 'evicted')),
    error TEXT NOT NULL DEFAULT '',
    attempt_count INTEGER NOT NULL DEFAULT 0,
    fe_build_log_path TEXT NOT NULL DEFAULT '',
    be_build_log_path TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    UNIQUE (repo_id, sha),
    UNIQUE (repo_id, short_sha)
);

-- Used from M2 (branch alias routing); created now so the schema stays a
-- single idempotent file.
CREATE TABLE IF NOT EXISTS branch_aliases (
    repo_id INTEGER NOT NULL REFERENCES repos(id),
    branch TEXT NOT NULL,
    deploy_id INTEGER NOT NULL REFERENCES deploys(id),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (repo_id, branch)
);

-- Row existence means the state dir is fully provisioned (forked or fresh).
-- run_config is the backend section of preview.toml as JSON: be_hash
-- uniquely determines the run contract, so the supervisor never re-parses
-- the manifest.
CREATE TABLE IF NOT EXISTS backend_artifacts (
    repo_id INTEGER NOT NULL REFERENCES repos(id),
    be_hash TEXT NOT NULL,
    forked_from TEXT NOT NULL DEFAULT '',
    state_dir TEXT NOT NULL,
    run_config TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (repo_id, be_hash)
);

-- Crash-recovery bookkeeping only: upserted on process start, deleted on
-- clean stop. Leftover rows at startup mean an unclean exit.
CREATE TABLE IF NOT EXISTS process_records (
    repo_id INTEGER NOT NULL REFERENCES repos(id),
    be_hash TEXT NOT NULL,
    pid INTEGER NOT NULL,
    pgid INTEGER NOT NULL,
    port INTEGER NOT NULL,
    started_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    PRIMARY KEY (repo_id, be_hash)
);

CREATE TABLE IF NOT EXISTS process_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id INTEGER NOT NULL REFERENCES repos(id),
    be_hash TEXT NOT NULL,
    event TEXT NOT NULL,
    detail TEXT NOT NULL DEFAULT '',
    occurred_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
