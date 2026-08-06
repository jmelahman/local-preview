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
};

export type Repo = {
  id: number;
  name: string;
  source: string;
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

export const api = {
  health: () => request<Health>("/api/health"),
  listRepos: () => request<Repo[]>("/api/repos"),
  createRepo: (name: string, source: string) =>
    request<Repo>("/api/repos", { method: "POST", body: JSON.stringify({ name, source }) }),
  listDeploys: () => request<Deploy[]>("/api/deploys"),
  createDeploy: (repo: string, ref: string) =>
    request<Deploy>("/api/deploys", { method: "POST", body: JSON.stringify({ repo, ref }) }),
};
