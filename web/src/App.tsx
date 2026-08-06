import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { api, type DeployStatus } from "@/api/client";

const statusStyles: Record<DeployStatus, string> = {
  queued: "bg-border text-fg-muted",
  building: "bg-accent-600/20 text-accent-500",
  ready: "bg-success/15 text-success",
  failed: "bg-danger/15 text-danger",
  evicted: "bg-border text-fg-muted line-through",
};

function StatusBadge({ status }: { status: DeployStatus }) {
  return (
    <span className={`rounded px-1.5 py-0.5 text-xs font-medium ${statusStyles[status]}`}>
      {status}
    </span>
  );
}

const inputClass =
  "rounded-md border border-border bg-surface px-3 py-2 text-sm outline-none focus:ring-2 focus:ring-accent-500";
const buttonClass =
  "rounded-md bg-accent-600 px-4 py-2 text-sm font-medium text-white hover:bg-accent-500 disabled:opacity-50";

export default function App() {
  const queryClient = useQueryClient();
  const [repoName, setRepoName] = useState("");
  const [repoSource, setRepoSource] = useState("");
  const [deployRepo, setDeployRepo] = useState("");
  const [deployRef, setDeployRef] = useState("");

  const health = useQuery({ queryKey: ["health"], queryFn: api.health });
  const repos = useQuery({ queryKey: ["repos"], queryFn: api.listRepos });
  const deploys = useQuery({
    queryKey: ["deploys"],
    queryFn: api.listDeploys,
    // Poll while anything is in flight so status flips surface promptly.
    refetchInterval: (query) =>
      query.state.data?.some((d) => d.status === "queued" || d.status === "building") ? 1000 : 5000,
  });

  const createRepo = useMutation({
    mutationFn: () => api.createRepo(repoName.trim(), repoSource.trim()),
    onSuccess: () => {
      setRepoName("");
      setRepoSource("");
      queryClient.invalidateQueries({ queryKey: ["repos"] });
    },
  });
  const createDeploy = useMutation({
    mutationFn: () => api.createDeploy(deployRepo || repos.data?.[0]?.name || "", deployRef.trim()),
    onSuccess: () => {
      setDeployRef("");
      queryClient.invalidateQueries({ queryKey: ["deploys"] });
    },
  });

  return (
    <div className="flex min-h-screen flex-col items-center bg-bg px-4 py-12 text-fg">
      <main className="w-full max-w-3xl space-y-8">
        <header>
          <h1 className="text-2xl font-semibold">local-preview</h1>
          <p className="text-sm text-fg-muted">
            Per-commit preview deployments, served from one binary.
          </p>
        </header>

        <section className="space-y-2">
          <h2 className="text-sm font-medium text-fg-muted">Register a repository</h2>
          <form
            className="flex flex-wrap gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              if (repoName.trim() && repoSource.trim()) createRepo.mutate();
            }}
          >
            <input
              value={repoName}
              onChange={(e) => setRepoName(e.target.value)}
              placeholder="name (dns label)"
              className={`w-40 ${inputClass}`}
            />
            <input
              value={repoSource}
              onChange={(e) => setRepoSource(e.target.value)}
              placeholder="/path/to/repo or clone URL"
              className={`flex-1 min-w-64 ${inputClass}`}
            />
            <button
              type="submit"
              disabled={!repoName.trim() || !repoSource.trim() || createRepo.isPending}
              className={buttonClass}
            >
              Register
            </button>
          </form>
          {createRepo.error && <p className="text-sm text-danger">{String(createRepo.error)}</p>}
        </section>

        <section className="space-y-2">
          <h2 className="text-sm font-medium text-fg-muted">Deploy a commit</h2>
          <form
            className="flex flex-wrap gap-2"
            onSubmit={(e) => {
              e.preventDefault();
              if (deployRef.trim()) createDeploy.mutate();
            }}
          >
            <select
              value={deployRepo}
              onChange={(e) => setDeployRepo(e.target.value)}
              className={`w-40 ${inputClass}`}
              aria-label="Repository"
            >
              {repos.data?.map((r) => (
                <option key={r.id} value={r.name}>
                  {r.name}
                </option>
              ))}
            </select>
            <input
              value={deployRef}
              onChange={(e) => setDeployRef(e.target.value)}
              placeholder="sha or branch"
              className={`flex-1 min-w-48 ${inputClass}`}
            />
            <button
              type="submit"
              disabled={!deployRef.trim() || !repos.data?.length || createDeploy.isPending}
              className={buttonClass}
            >
              Deploy
            </button>
          </form>
          {createDeploy.error && (
            <p className="text-sm text-danger">{String(createDeploy.error)}</p>
          )}
        </section>

        <section className="space-y-2">
          <h2 className="text-sm font-medium text-fg-muted">Deploys</h2>
          <div className="overflow-x-auto rounded-md border border-border bg-surface">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-b border-border text-left text-xs text-fg-muted">
                  <th className="px-3 py-2 font-medium">Repo</th>
                  <th className="px-3 py-2 font-medium">Commit</th>
                  <th className="px-3 py-2 font-medium">Status</th>
                  <th className="px-3 py-2 font-medium">Artifacts</th>
                  <th className="px-3 py-2 font-medium">Preview</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-border">
                {deploys.data?.length === 0 && (
                  <tr>
                    <td colSpan={5} className="px-3 py-6 text-center text-fg-muted">
                      No deploys yet. Register a repo and deploy a commit.
                    </td>
                  </tr>
                )}
                {deploys.data?.map((d) => (
                  <tr key={d.id}>
                    <td className="px-3 py-2">{d.repo}</td>
                    <td className="px-3 py-2 font-mono text-xs">
                      {d.short_sha}
                      {d.ref ? <span className="ml-1 text-fg-muted">({d.ref})</span> : null}
                    </td>
                    <td className="px-3 py-2">
                      <StatusBadge status={d.status} />
                      {d.status === "failed" && d.error ? (
                        <span className="ml-2 text-xs text-danger">{d.error}</span>
                      ) : null}
                    </td>
                    <td className="px-3 py-2 font-mono text-xs text-fg-muted">
                      {d.fe_hash ? `fe:${d.fe_hash.slice(0, 8)}` : ""}
                      {d.be_hash ? ` be:${d.be_hash.slice(0, 8)}` : ""}
                      {d.process ? ` (${d.process})` : ""}
                    </td>
                    <td className="px-3 py-2">
                      {d.preview_url ? (
                        <a
                          href={d.preview_url}
                          target="_blank"
                          rel="noreferrer"
                          className="text-accent-500 hover:underline"
                        >
                          open ↗
                        </a>
                      ) : (
                        <span className="text-fg-muted">—</span>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>

        <footer className="text-xs text-fg-muted">
          {health.data ? `server ${health.data.version}` : "server unreachable"}
        </footer>
      </main>
    </div>
  );
}
