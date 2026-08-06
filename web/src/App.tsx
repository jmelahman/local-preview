import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useEffect, useRef, useState } from "react";
import { api, type DeployStatus, type Repo } from "@/api/client";

const THEME_KEY = "app.themeMode";
type Theme = "light" | "dark";

function storedTheme(): Theme | null {
  try {
    const raw = localStorage.getItem(THEME_KEY);
    return raw === "light" || raw === "dark" ? raw : null;
  } catch {
    return null;
  }
}

function systemTheme(): Theme {
  return matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

function ThemeToggle() {
  const [theme, setTheme] = useState<Theme>(() => storedTheme() ?? systemTheme());
  // Follow the OS appearance until the user explicitly picks a side; after
  // that the choice is persisted and system flips are ignored.
  const chosen = useRef(storedTheme() != null);

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
  }, [theme]);

  useEffect(() => {
    const mq = matchMedia("(prefers-color-scheme: dark)");
    const onChange = () => {
      if (!chosen.current) setTheme(mq.matches ? "dark" : "light");
    };
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, []);

  const other: Theme = theme === "dark" ? "light" : "dark";
  return (
    <button
      type="button"
      onClick={() => {
        chosen.current = true;
        setTheme(other);
        try {
          localStorage.setItem(THEME_KEY, other);
        } catch {
          // Private browsing; theme just won't persist.
        }
      }}
      title={`Switch to ${other} theme`}
      aria-label={`Switch to ${other} theme`}
      className="inline-flex h-7 w-7 items-center justify-center rounded bg-surface-2 text-fg transition-colors duration-150 hover:bg-surface-3"
    >
      {theme === "dark" ? <IconMoon /> : <IconSun />}
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
  "w-full rounded bg-surface px-2 py-1 text-sm text-fg placeholder:text-fg-muted/60";
const buttonClass =
  "inline-flex shrink-0 items-center rounded bg-accent-700 px-3 py-1 text-sm text-white transition-colors duration-150 hover:bg-accent-600 disabled:cursor-not-allowed disabled:opacity-50";
const neutralButtonClass =
  "inline-flex shrink-0 items-center rounded bg-surface-2 px-2 py-1 text-xs text-fg transition-colors duration-150 hover:bg-surface-3";

function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    // biome-ignore lint/a11y/noLabelWithoutControl: children slot holds the control at runtime.
    <label className="flex flex-col gap-1">
      <span className="text-xs text-fg-muted">{label}</span>
      {children}
    </label>
  );
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
    <div className="flex items-center justify-between gap-4 border-t border-border px-4 py-2">
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
      className="m-auto w-[520px] max-w-[calc(100vw-2rem)] rounded border border-border bg-bg p-0 text-fg shadow-lg backdrop:bg-black/50"
    >
      <header className="flex items-center justify-between border-b border-border px-3 py-2">
        <h2 className="text-sm font-semibold">{title}</h2>
        <button
          type="button"
          onClick={onClose}
          aria-label="Close"
          className="inline-flex h-7 w-7 items-center justify-center rounded bg-surface-2 text-fg transition-colors duration-150 hover:bg-surface-3"
        >
          <IconX />
        </button>
      </header>
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
        <div className="flex flex-col gap-3 p-4 text-sm">
          <p className="text-xs text-fg-muted">
            Point at a local path or clone URL to start deploying from it.
          </p>
          <Field label="Name">
            <input
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="my-app"
              className={inputClass}
            />
          </Field>
          <Field label="Source">
            <input
              value={source}
              onChange={(e) => setSource(e.target.value)}
              placeholder="/path/to/repo or clone URL"
              className={inputClass}
            />
          </Field>
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
        <div className="flex flex-col gap-3 p-4 text-sm">
          <p className="text-xs text-fg-muted">
            Builds are content-addressed; unchanged halves are reused.
          </p>
          <Field label="Repository">
            <select
              value={repo}
              onChange={(e) => setRepo(e.target.value)}
              className="w-full cursor-pointer rounded bg-surface px-2 py-1 text-sm"
            >
              {repos.length === 0 && <option value="">no repositories yet</option>}
              {repos.map((r) => (
                <option key={r.id} value={r.name}>
                  {r.name}
                </option>
              ))}
            </select>
          </Field>
          <Field label="Ref">
            <input
              value={gitRef}
              onChange={(e) => setGitRef(e.target.value)}
              placeholder="sha or branch"
              className={inputClass}
            />
          </Field>
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

  const health = useQuery({
    queryKey: ["health"],
    queryFn: api.health,
    // Keep polling so the unreachable banner clears itself on recovery.
    refetchInterval: 5000,
  });
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
      <header className="flex items-center gap-3 border-b border-border px-3 py-2">
        <div className="flex items-center gap-2.5">
          <Logo />
          <h1 className="text-lg font-semibold">Previews</h1>
        </div>
        <div className="ml-auto flex items-center gap-2">
          <button
            type="button"
            onClick={() => setDialog("register")}
            className={neutralButtonClass}
          >
            Register repo
          </button>
          <ThemeToggle />
        </div>
      </header>
      {health.isError && (
        <div className="border-b border-amber-700 bg-amber-950/60 px-4 py-1 text-xs text-amber-200">
          Server unreachable — retrying…
        </div>
      )}

      <main className="mx-auto w-full max-w-5xl flex-1 px-3 py-4">
        <section>
          <div className="mb-2 flex items-center justify-between gap-2">
            <div className="flex items-baseline gap-2">
              <h2 className="text-sm font-semibold uppercase tracking-wide">Deployments</h2>
              {deploys.data && deploys.data.length > 0 && (
                <span className="text-xs text-fg-muted tabular-nums">{deploys.data.length}</span>
              )}
            </div>
            <button type="button" onClick={() => setDialog("deploy")} className={buttonClass}>
              Deploy
            </button>
          </div>
          <div className="overflow-hidden rounded border border-border bg-surface">
            {deploys.isPending && (
              <div className="divide-y divide-border">
                {[0, 1, 2].map((i) => (
                  <div key={i} className="flex animate-pulse items-center gap-4 px-3 py-3">
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
                <p className="text-xs text-fg-muted">
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
                  d.fe_hash
                    ? `fe:${d.fe_hash.slice(0, 8)}${d.fe_process ? ` (${d.fe_process})` : ""}`
                    : null,
                  d.be_hash ? `be:${d.be_hash.slice(0, 8)}` : null,
                  d.process ? `(${d.process})` : null,
                ]
                  .filter(Boolean)
                  .join(" ");
                return (
                  <li
                    key={d.id}
                    className="flex flex-wrap items-center gap-x-4 gap-y-1.5 px-3 py-3 transition-colors hover:bg-surface-2/40"
                  >
                    <StatusBadge status={d.status} />
                    <div className="min-w-0 flex-1">
                      <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5">
                        <span className="text-sm font-medium">{d.repo}</span>
                        <code className="font-mono text-xs text-fg-muted">{d.short_sha}</code>
                        {d.branch && (
                          <span className="inline-flex items-center gap-1 rounded-full border border-border bg-surface-2 px-2 py-px font-mono text-[11px] text-fg-muted">
                            <IconGitBranch className="h-3 w-3" />
                            {d.branch}
                          </span>
                        )}
                        {d.ref && d.ref !== d.branch && (
                          <span className="rounded-full border border-border bg-surface-2 px-2 py-px font-mono text-[11px] text-fg-muted">
                            {d.ref}
                          </span>
                        )}
                        {d.author_name && (
                          <span className="text-xs text-fg-muted" title={d.author_email}>
                            by {d.author_name}
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
                        className={`${neutralButtonClass} gap-1`}
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

      <footer className="border-t border-border px-3 py-2 font-mono text-[11px] text-fg-muted">
        {health.data ? `server ${health.data.version}` : "server …"}
      </footer>

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

function IconGitBranch({ className = "" }: { className?: string }) {
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
      <line x1="6" y1="3" x2="6" y2="15" />
      <circle cx="18" cy="6" r="3" />
      <circle cx="6" cy="18" r="3" />
      <path d="M18 9a9 9 0 0 1-9 9" />
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
