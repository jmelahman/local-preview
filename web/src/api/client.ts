export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

export type Health = {
  status: string;
  version: string;
  // Base domain previews are served under, e.g. "preview.localhost". Fixed
  // at server startup; the dashboard has no other way to learn it.
  preview_domain: string;
};

// Mirror-clone state of a registered repo. Registration returns while the
// clone runs in the background; a repo is deployable only once "ready".
export type RepoStatus = "cloning" | "ready" | "failed";

export type Repo = {
  id: number;
  name: string;
  source: string;
  // Watch polls the repo for new commits and deploys matching branch tips.
  // watch_branches narrows which branches (comma-separated globs, "" = all).
  watch: boolean;
  watch_branches: string;
  status: RepoStatus;
  // The clone failure message, while status is "failed".
  error?: string;
  // The clone's live progress line ("Receiving objects: 42% …"), while
  // status is "cloning" and the transport reports one.
  progress?: string;
  created_at: string;
};

export type DeployStatus = "queued" | "building" | "ready" | "failed" | "evicted";

// Live runtime state of a supervised process, present on ready deploys:
// `process` for the backend, `fe_process` for process-mode frontends.
export type ProcessState = "idle" | "starting" | "running";

export type ArtifactFile = {
  name: string;
  size: number;
  // Download path on this host.
  url: string;
};

// A named downloadable artifact ([artifacts.<name>] in preview.toml),
// present on ready deploys.
export type DeployArtifact = {
  name: string;
  hash: string;
  files: ArtifactFile[];
};

export type Deploy = {
  id: number;
  repo: string;
  sha: string;
  short_sha: string;
  ref?: string;
  branch?: string;
  author_name?: string;
  author_email?: string;
  fe_hash?: string;
  be_hash?: string;
  status: DeployStatus;
  error?: string;
  attempt_count: number;
  preview_url?: string;
  process?: ProcessState;
  fe_process?: ProcessState;
  artifacts?: DeployArtifact[];
  created_at: string;
  updated_at: string;
};

export type ProcessRuntime = "host" | "container";

// One side's live resource sample. Sampled fields are absent while the
// process isn't running; cpu_percent needs two samples, so it appears from
// the second poll onward.
export type SideStats = {
  state: ProcessState;
  runtime?: ProcessRuntime;
  cpu_percent?: number;
  memory_bytes?: number;
  memory_limit_bytes?: number;
  started_at?: string;
};

// Live resource usage of a deploy's processes; a side the deploy doesn't
// have is null.
export type DeployStats = {
  backend: SideStats | null;
  frontend: SideStats | null;
};

export type LogSide = "be" | "fe";

// An incremental slice of a process run log (the supervised server's
// stdout+stderr). Echo attempt/offset back to receive only new bytes; a
// changed attempt means the process restarted and the view reset.
export type RunLogChunk = {
  side: LogSide;
  attempt: number;
  offset: number;
  content: string;
  truncated?: boolean;
  process?: ProcessState;
};

// Instance-wide artifact retention policy. 0 disables a limit; with both at
// 0 the hourly sweep evicts nothing.
export type RetentionPolicy = {
  // Keep at most N non-evicted deploys per repo, newest first.
  max_deploys_per_repo: number;
  // Evict deploys created more than N days ago.
  max_age_days: number;
};

// One repo's slice of the storage report.
export type RepoUsage = {
  repo: string;
  artifacts_bytes: number;
  state_bytes: number;
  logs_bytes: number;
  mirror_bytes: number;
  total_bytes: number;
  deploys: number;
  evicted_deploys: number;
};

// GET /api/storage: how much disk the instance uses, by category and by repo.
export type StorageReport = {
  total_bytes: number;
  artifacts_bytes: number;
  state_bytes: number;
  logs_bytes: number;
  mirror_bytes: number;
  tmp_bytes: number;
  db_bytes: number;
  repos: RepoUsage[];
};

export type EvictedDeploy = {
  id: number;
  repo: string;
  short_sha: string;
  branch?: string;
};

// POST /api/gc: what one sweep evicted and how much disk it gave back.
export type GCResult = {
  policy: RetentionPolicy;
  evicted: EvictedDeploy[];
  freed_bytes: number;
};

// Narrows GET /api/deploys; empty fields don't filter. q is a free-text
// search matching a commit-sha prefix or a substring of the repo, branch,
// ref, or author (case-insensitive); status matches the build status exactly.
export type DeployFilter = {
  q?: string;
  status?: DeployStatus | "";
};

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    headers: { "Content-Type": "application/json" },
    ...init,
  });
  if (!res.ok) {
    let message = `${res.status} ${res.statusText}`;
    try {
      const body = await res.json();
      if (body && typeof body.error === "string") message = body.error;
    } catch {
      // Non-JSON error body; keep the status line.
    }
    throw new ApiError(res.status, message);
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

// Watch settings a PATCH /api/repos/{name} can update; omitted fields keep
// their stored value.
export type RepoUpdate = {
  watch?: boolean;
  watch_branches?: string;
};

export const api = {
  health: () => request<Health>("/api/health"),
  listRepos: () => request<Repo[]>("/api/repos"),
  getRepo: (name: string) => request<Repo>(`/api/repos/${encodeURIComponent(name)}`),
  createRepo: (name: string, source: string) =>
    request<Repo>("/api/repos", { method: "POST", body: JSON.stringify({ name, source }) }),
  updateRepo: (name: string, patch: RepoUpdate) =>
    request<Repo>(`/api/repos/${encodeURIComponent(name)}`, {
      method: "PATCH",
      body: JSON.stringify(patch),
    }),
  deleteRepo: (name: string) =>
    request<void>(`/api/repos/${encodeURIComponent(name)}`, { method: "DELETE" }),
  listDeploys: (filter?: DeployFilter) => {
    const params = new URLSearchParams();
    if (filter?.q) params.set("q", filter.q);
    if (filter?.status) params.set("status", filter.status);
    const qs = params.toString();
    return request<Deploy[]>(`/api/deploys${qs ? `?${qs}` : ""}`);
  },
  createDeploy: (repo: string, ref: string) =>
    request<Deploy>("/api/deploys", { method: "POST", body: JSON.stringify({ repo, ref }) }),
  stopDeploy: (id: number) => request<Deploy>(`/api/deploys/${id}/stop`, { method: "POST" }),
  deleteDeploy: (id: number) => request<void>(`/api/deploys/${id}`, { method: "DELETE" }),
  getDeployStats: (id: number) => request<DeployStats>(`/api/deploys/${id}/stats`),
  getRunLog: (id: number, side: LogSide, attempt: number, offset: number) =>
    request<RunLogChunk>(
      `/api/deploys/${id}/logs/run?side=${side}&attempt=${attempt}&offset=${offset}`,
    ),
  getStorage: () => request<StorageReport>("/api/storage"),
  getRetention: () => request<RetentionPolicy>("/api/retention"),
  putRetention: (policy: RetentionPolicy) =>
    request<RetentionPolicy>("/api/retention", { method: "PUT", body: JSON.stringify(policy) }),
  runGC: () => request<GCResult>("/api/gc", { method: "POST" }),
  getBuildLogs: async (id: number): Promise<string> => {
    const res = await fetch(`/api/deploys/${id}/logs`);
    if (!res.ok) throw new ApiError(res.status, `${res.status} ${res.statusText}`);
    return res.text();
  },
};
