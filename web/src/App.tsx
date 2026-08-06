import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useEffect, useRef, useState } from "react";
import { api, type DeployStatus, type Repo } from "@/api/client";

const THEME_KEY = "app.themeMode";
type ThemeMode = "system" | "light" | "dark";

function loadThemeMode(): ThemeMode {
  try {
    const raw = localStorage.getItem(THEME_KEY);
    return raw === "light" || raw === "dark" ? raw : "system";
  } catch {
    return "system";
  }
}

function applyThemeMode(mode: ThemeMode) {
  const dark =
    mode === "dark" || (mode === "system" && matchMedia("(prefers-color-scheme: dark)").matches);
  document.documentElement.dataset.theme = dark ? "dark" : "light";
}

function ThemeToggle() {
  const [mode, setMode] = useState<ThemeMode>(loadThemeMode);

  useEffect(() => {
    applyThemeMode(mode);
    try {
      localStorage.setItem(THEME_KEY, mode);
    } catch {
      // Private browsing; theme just won't persist.
    }
  }, [mode]);

  useEffect(() => {
    if (mode !== "system") return;
    const mq = matchMedia("(prefers-color-scheme: dark)");
    const onChange = () => applyThemeMode("system");
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, [mode]);

  const next: Record<ThemeMode, ThemeMode> = { system: "light", light: "dark", dark: "system" };
  const icon = mode === "light" ? <IconSun /> : mode === "dark" ? <IconMoon /> : <IconMonitor />;
  return (
    <button
      type="button"
      onClick={() => setMode(next[mode])}
      title={`Theme: ${mode}`}
      aria-label={`Theme: ${mode} (click to change)`}
      className="inline-flex h-8 w-8 items-center justify-center rounded-md text-fg-muted transition-colors hover:bg-surface-2 hover:text-fg"
    >
      {icon}
    </button>
  );
}

const statusStyles: Record<DeployStatus, { dot: string; pill: string }> = {
  queued: { dot: "bg-fg-muted", pill: "border-border text-fg-muted" },
  building: { dot: "animate-pulse bg-warning", pill: "border-warning/30 text-warning" },
  ready: { dot: "bg-success", pill: "border-success/30 text-success" },
  failed: { dot: "bg-danger", pill: "border-danger/30 text-danger" },
  evicted: { dot: "bg-fg-muted/50", pill: "border-border text-fg-muted/70 line-through" },
};

function StatusBadge({ status }: { status: DeployStatus }) {
  const s = statusStyles[status];
  return (
    <span
      className={`inline-flex items-center gap-1.5 rounded-full border px-2.5 py-0.5 text-xs font-medium ${s.pill}`}
    >
      <span className={`h-1.5 w-1.5 shrink-0 rounded-full ${s.dot}`} />
      {status}
    </span>
  );
}

function timeAgo(iso: string): string {
  const secs = Math.max(0, Math.floor((Date.now() - new Date(iso).getTime()) / 1000));
  if (secs < 45) return "just now";
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `${Math.max(1, mins)}m ago`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours}h ago`;
  return `${Math.floor(hours / 24)}d ago`;
}

const inputClass =
  "h-9 w-full rounded-md border border-border bg-bg px-3 text-sm text-fg outline-none transition-colors placeholder:text-fg-muted/60 focus:border-accent-500 focus:ring-2 focus:ring-accent-500/25";
const buttonClass =
  "inline-flex h-8 shrink-0 items-center rounded-md bg-accent-600 px-3.5 text-sm font-medium text-white transition-colors hover:bg-accent-500 disabled:cursor-not-allowed disabled:opacity-50";
const ghostButtonClass =
  "inline-flex h-8 shrink-0 items-center rounded-md border border-border px-3 text-sm font-medium text-fg-muted transition-colors hover:border-border-strong hover:text-fg";

function FieldLabel({ children }: { children: string }) {
  return <span className="mb-1.5 block text-xs font-medium text-fg-muted">{children}</span>;
}

function DialogFooter({
  error,
  hint,
  children,
}: {
  error?: unknown;
  hint: string;
  children: ReactNode;
}) {
  return (
    <div className="flex items-center justify-between gap-4 border-t border-border bg-surface-2/50 px-5 py-3">
      {error ? (
        <p className="min-w-0 truncate text-xs text-danger" title={String(error)}>
          {String(error)}
        </p>
      ) : (
        <p className="min-w-0 truncate text-xs text-fg-muted">{hint}</p>
      )}
      {children}
    </div>
  );
}

function Modal({
  title,
  onClose,
  children,
}: {
  title: string;
  onClose: () => void;
  children: ReactNode;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    // Guarded and without a close() cleanup: StrictMode re-runs the effect, and
    // closing here would fire the close event and unmount the dialog instantly.
    const dialog = ref.current;
    if (dialog && !dialog.open) dialog.showModal();
  }, []);
  return (
    // biome-ignore lint/a11y/useKeyWithClickEvents: backdrop click-to-close; Escape is handled natively by <dialog>.
    <dialog
      ref={ref}
      onClose={onClose}
      onClick={(e) => {
        if (e.target === ref.current) onClose();
      }}
      className="m-auto w-full max-w-md overflow-hidden rounded-xl border border-border bg-surface p-0 text-fg shadow-2xl backdrop:bg-black/50"
    >
      <div className="flex items-center justify-between px-5 pt-4">
        <h2 className="text-sm font-semibold">{title}</h2>
        <button
          type="button"
          onClick={onClose}
          aria-label="Close"
          className="-mr-1.5 inline-flex h-7 w-7 items-center justify-center rounded-md text-fg-muted transition-colors hover:bg-surface-2 hover:text-fg"
        >
          <IconX />
        </button>
      </div>
      {children}
    </dialog>
  );
}

function RegisterRepoDialog({ onClose }: { onClose: () => void }) {
  const queryClient = useQueryClient();
  const [name, setName] = useState("");
  const [source, setSource] = useState("");
  const createRepo = useMutation({
    mutationFn: () => api.createRepo(name.trim(), source.trim()),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["repos"] });
      onClose();
    },
  });
  return (
    <Modal title="Register a repository" onClose={onClose}>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (name.trim() && source.trim()) createRepo.mutate();
        }}
      >
        <div className="space-y-4 p-5 pt-3">
          <p className="text-[13px] text-fg-muted">
            Point at a local path or clone URL to start deploying from it.
          </p>
          <label className="block">
            <FieldLabel>Name</FieldLabel>
            <input
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="my-app"
              className={inputClass}
            />
          </label>
          <label className="block">
            <FieldLabel>Source</FieldLabel>
            <input
              value={source}
              onChange={(e) => setSource(e.target.value)}
              placeholder="/path/to/repo or clone URL"
              className={inputClass}
            />
          </label>
        </div>
        <DialogFooter
          error={createRepo.error}
          hint="The name becomes the preview subdomain (DNS label)."
        >
          <button
            type="submit"
            disabled={!name.trim() || !source.trim() || createRepo.isPending}
            className={buttonClass}
          >
            Register
          </button>
        </DialogFooter>
      </form>
    </Modal>
  );
}

function DeployDialog({ repos, onClose }: { repos: Repo[]; onClose: () => void }) {
  const queryClient = useQueryClient();
  const [repo, setRepo] = useState(repos[0]?.name ?? "");
  const [gitRef, setGitRef] = useState("");
  const createDeploy = useMutation({
    mutationFn: () => api.createDeploy(repo, gitRef.trim()),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["deploys"] });
      onClose();
    },
  });
  return (
    <Modal title="Deploy a commit" onClose={onClose}>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (gitRef.trim() && repo) createDeploy.mutate();
        }}
      >
        <div className="space-y-4 p-5 pt-3">
          <p className="text-[13px] text-fg-muted">
            Builds are content-addressed; unchanged halves are reused.
          </p>
          <label className="block">
            <FieldLabel>Repository</FieldLabel>
            <div className="relative">
              <select
                value={repo}
                onChange={(e) => setRepo(e.target.value)}
                className={`${inputClass} appearance-none pr-8`}
                aria-label="Repository"
              >
                {repos.length === 0 && <option value="">no repositories yet</option>}
                {repos.map((r) => (
                  <option key={r.id} value={r.name}>
                    {r.name}
                  </option>
                ))}
              </select>
              <IconChevronDown className="pointer-events-none absolute top-1/2 right-2.5 h-4 w-4 -translate-y-1/2 text-fg-muted" />
            </div>
          </label>
          <label className="block">
            <FieldLabel>Ref</FieldLabel>
            <input
              value={gitRef}
              onChange={(e) => setGitRef(e.target.value)}
              placeholder="sha or branch"
              className={inputClass}
            />
          </label>
        </div>
        <DialogFooter
          error={createDeploy.error}
          hint={
            repos.length
              ? "Served at <sha>.<repo>.preview.localhost."
              : "Register a repository first."
          }
        >
          <button
            type="submit"
            disabled={!gitRef.trim() || !repos.length || createDeploy.isPending}
            className={buttonClass}
          >
            Deploy
          </button>
        </DialogFooter>
      </form>
    </Modal>
  );
}

export default function App() {
  const [dialog, setDialog] = useState<"register" | "deploy" | null>(null);

  const health = useQuery({ queryKey: ["health"], queryFn: api.health });
  const repos = useQuery({ queryKey: ["repos"], queryFn: api.listRepos });
  const deploys = useQuery({
    queryKey: ["deploys"],
    queryFn: api.listDeploys,
    // Poll while anything is in flight so status flips surface promptly.
    refetchInterval: (query) =>
      query.state.data?.some((d) => d.status === "queued" || d.status === "building") ? 1000 : 5000,
  });

  const hasRepos = (repos.data?.length ?? 0) > 0;

  return (
    <div className="flex min-h-screen flex-col bg-bg text-fg">
      <header className="sticky top-0 z-10 border-b border-border bg-bg/80 backdrop-blur">
        <div className="mx-auto flex h-14 w-full max-w-5xl items-center justify-between px-4 sm:px-6">
          <div className="flex items-center gap-2.5">
            <Logo />
            <h1 className="text-sm font-semibold tracking-tight">Previews</h1>
          </div>
          <div className="flex items-center gap-2">
            <button
              type="button"
              onClick={() => setDialog("register")}
              className={ghostButtonClass}
            >
              Register repo
            </button>
            <div className="flex items-center gap-2 rounded-full border border-border px-3 py-1 text-xs text-fg-muted">
              <span
                className={`h-1.5 w-1.5 rounded-full ${
                  health.data ? "bg-success" : health.isPending ? "bg-fg-muted" : "bg-danger"
                }`}
              />
              {health.data
                ? `server ${health.data.version}`
                : health.isPending
                  ? "server …"
                  : "server unreachable"}
            </div>
            <ThemeToggle />
          </div>
        </div>
      </header>

      <main className="mx-auto w-full max-w-5xl flex-1 px-4 py-10 sm:px-6">
        <section>
          <div className="mb-4 flex items-center justify-between gap-4">
            <div className="flex items-baseline gap-2.5">
              <h2 className="text-lg font-semibold tracking-tight">Deployments</h2>
              {deploys.data && deploys.data.length > 0 && (
                <span className="text-xs text-fg-muted tabular-nums">{deploys.data.length}</span>
              )}
            </div>
            <button type="button" onClick={() => setDialog("deploy")} className={buttonClass}>
              Deploy
            </button>
          </div>
          <div className="overflow-hidden rounded-xl border border-border bg-surface">
            {deploys.isPending && (
              <div className="divide-y divide-border">
                {[0, 1, 2].map((i) => (
                  <div key={i} className="flex animate-pulse items-center gap-4 px-4 py-3.5">
                    <div className="h-5 w-16 rounded-full bg-surface-2" />
                    <div className="h-4 w-40 rounded bg-surface-2" />
                    <div className="ml-auto h-4 w-24 rounded bg-surface-2" />
                  </div>
                ))}
              </div>
            )}
            {deploys.data?.length === 0 && (
              <div className="flex flex-col items-center gap-1 px-6 py-16 text-center">
                <IconDeploy className="mb-2 h-8 w-8 text-fg-muted/50" />
                <p className="text-sm font-medium">No deployments yet</p>
                <p className="text-[13px] text-fg-muted">
                  {hasRepos
                    ? "Deploy a commit to get your first preview."
                    : "Register a repository to get started."}
                </p>
                <button
                  type="button"
                  onClick={() => setDialog(hasRepos ? "deploy" : "register")}
                  className={`${buttonClass} mt-3`}
                >
                  {hasRepos ? "Deploy a commit" : "Register a repository"}
                </button>
              </div>
            )}
            <ul className="divide-y divide-border">
              {deploys.data?.map((d) => {
                const artifacts = [
                  d.fe_hash ? `fe:${d.fe_hash.slice(0, 8)}` : null,
                  d.be_hash ? `be:${d.be_hash.slice(0, 8)}` : null,
                  d.process ? `(${d.process})` : null,
                ]
                  .filter(Boolean)
                  .join(" ");
                return (
                  <li
                    key={d.id}
                    className="flex flex-wrap items-center gap-x-4 gap-y-1.5 px-4 py-3.5 transition-colors hover:bg-surface-2/40"
                  >
                    <StatusBadge status={d.status} />
                    <div className="min-w-0 flex-1">
                      <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5">
                        <span className="text-sm font-medium">{d.repo}</span>
                        <code className="font-mono text-xs text-fg-muted">{d.short_sha}</code>
                        {d.ref && (
                          <span className="rounded-full border border-border bg-surface-2 px-2 py-px font-mono text-[11px] text-fg-muted">
                            {d.ref}
                          </span>
                        )}
                      </div>
                      {artifacts && (
                        <div className="mt-0.5 font-mono text-xs text-fg-muted/80">{artifacts}</div>
                      )}
                      {d.status === "failed" && d.error && (
                        <p
                          className="mt-0.5 max-w-full truncate text-xs text-danger"
                          title={d.error}
                        >
                          {d.error}
                        </p>
                      )}
                    </div>
                    <time
                      dateTime={d.created_at}
                      title={d.created_at}
                      className="text-xs text-fg-muted tabular-nums"
                    >
                      {timeAgo(d.created_at)}
                    </time>
                    {d.preview_url ? (
                      <a
                        href={d.preview_url}
                        target="_blank"
                        rel="noreferrer"
                        className="inline-flex h-7 items-center gap-1 rounded-md border border-border px-2.5 text-xs font-medium text-fg-muted transition-colors hover:border-border-strong hover:text-fg"
                      >
                        open
                        <IconArrowUpRight className="h-3 w-3" />
                      </a>
                    ) : (
                      <span className="w-14 text-center text-xs text-fg-muted/50">—</span>
                    )}
                  </li>
                );
              })}
            </ul>
          </div>
        </section>
      </main>

      {dialog === "register" && <RegisterRepoDialog onClose={() => setDialog(null)} />}
      {dialog === "deploy" && (
        <DeployDialog repos={repos.data ?? []} onClose={() => setDialog(null)} />
      )}
    </div>
  );
}

function Logo() {
  return (
    <svg viewBox="0 0 32 32" className="h-5 w-5" aria-hidden="true">
      <rect x="2" y="2" width="28" height="28" rx="6" className="fill-surface-3" />
      <rect x="7" y="7" width="18" height="5" rx="2" className="fill-accent-500" />
      <rect x="7" y="14" width="18" height="5" rx="2" className="fill-fg-muted" />
      <rect x="7" y="21" width="11" height="5" rx="2" className="fill-fg-muted" />
    </svg>
  );
}

function IconSun() {
  return (
    <svg
      viewBox="0 0 24 24"
      className="h-4 w-4"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      aria-hidden="true"
    >
      <circle cx="12" cy="12" r="4" />
      <path d="M12 2v2m0 16v2M4.9 4.9l1.4 1.4m11.4 11.4 1.4 1.4M2 12h2m16 0h2M4.9 19.1l1.4-1.4m11.4-11.4 1.4-1.4" />
    </svg>
  );
}

function IconMoon() {
  return (
    <svg
      viewBox="0 0 24 24"
      className="h-4 w-4"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M21 12.8A9 9 0 1 1 11.2 3 7 7 0 0 0 21 12.8z" />
    </svg>
  );
}

function IconMonitor() {
  return (
    <svg
      viewBox="0 0 24 24"
      className="h-4 w-4"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <rect x="2" y="3" width="20" height="14" rx="2" />
      <path d="M8 21h8m-4-4v4" />
    </svg>
  );
}

function IconX() {
  return (
    <svg
      viewBox="0 0 24 24"
      className="h-4 w-4"
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M18 6 6 18M6 6l12 12" />
    </svg>
  );
}

function IconChevronDown({ className = "" }: { className?: string }) {
  return (
    <svg
      viewBox="0 0 24 24"
      className={className}
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="m6 9 6 6 6-6" />
    </svg>
  );
}

function IconArrowUpRight({ className = "" }: { className?: string }) {
  return (
    <svg
      viewBox="0 0 24 24"
      className={className}
      fill="none"
      stroke="currentColor"
      strokeWidth="2"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M7 17 17 7M8 7h9v9" />
    </svg>
  );
}

function IconDeploy({ className = "" }: { className?: string }) {
  return (
    <svg
      viewBox="0 0 24 24"
      className={className}
      fill="none"
      stroke="currentColor"
      strokeWidth="1.5"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="m12 3 9 5-9 5-9-5 9-5z" />
      <path d="m3 13 9 5 9-5" opacity="0.5" />
    </svg>
  );
}
