import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { type ReactNode, useEffect, useRef, useState } from "react";
import {
  api,
  type Deploy,
  type DeployArtifact,
  type DeployStatus,
  type LogSide,
  type ProcessState,
  type Repo,
  type SideStats,
} from "@/api/client";

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

// A deploy's displayed state: the build status until it's ready, then the
// merged runtime state of its supervised processes (backend and, for
// process-mode frontends, the frontend) — starting while any side warms
// up, running only once every side is warm, idle otherwise.
type DeployState = DeployStatus | ProcessState;

function deployState(d: Deploy): DeployState {
  if (d.status !== "ready") return d.status;
  const procs = [d.process, d.fe_process].filter((p): p is ProcessState => p != null);
  if (procs.length === 0) return d.status;
  if (procs.includes("starting")) return "starting";
  return procs.every((p) => p === "running") ? "running" : "idle";
}

const stateStyles: Record<DeployState, { dot: string; pill: string; hint: string }> = {
  queued: {
    dot: "bg-fg-muted",
    pill: "border-border text-fg-muted",
    hint: "Waiting for a build slot",
  },
  building: {
    dot: "animate-pulse bg-warning",
    pill: "border-warning/30 text-warning",
    hint: "Build in progress",
  },
  ready: {
    dot: "bg-success",
    pill: "border-success/30 text-success",
    hint: "Static preview — served instantly",
  },
  idle: {
    dot: "bg-success/40",
    pill: "border-success/20 text-success/70",
    hint: "Built — starts on the first request",
  },
  starting: {
    dot: "animate-pulse bg-success",
    pill: "border-success/30 text-success",
    hint: "Starting up",
  },
  running: {
    dot: "bg-success",
    pill: "border-success/30 text-success",
    hint: "Warm — served instantly",
  },
  failed: {
    dot: "bg-danger",
    pill: "border-danger/30 text-danger",
    hint: "Build failed",
  },
  evicted: {
    dot: "bg-fg-muted/50",
    pill: "border-border text-fg-muted/70 line-through",
    hint: "Artifacts were cleaned up — redeploy to rebuild",
  },
};

function StatusBadge({ state }: { state: DeployState }) {
  const s = stateStyles[state];
  return (
    <span
      title={s.hint}
      className={`inline-flex w-22 items-center justify-center gap-1.5 rounded-full border py-0.5 text-xs font-medium ${s.pill}`}
    >
      <span className={`h-1.5 w-1.5 shrink-0 rounded-full ${s.dot}`} />
      {state}
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
const dangerButtonClass =
  "inline-flex shrink-0 items-center rounded bg-danger px-3 py-1 text-sm text-white transition-colors duration-150 hover:opacity-90 disabled:cursor-not-allowed disabled:opacity-50";

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
  wide = false,
  children,
}: {
  title: string;
  onClose: () => void;
  wide?: boolean;
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
      className={`m-auto ${wide ? "w-[780px]" : "w-[520px]"} max-w-[calc(100vw-2rem)] rounded border border-border bg-bg p-0 text-fg shadow-lg backdrop:bg-black/50`}
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

// ConfirmDialog is a reusable modal for a destructive action: a body
// explaining the consequence and a danger-styled confirm button. The confirm
// button drives the caller's mutation; disable interaction while it's in
// flight and surface its error inline.
function ConfirmDialog({
  title,
  confirmLabel,
  onConfirm,
  onClose,
  pending,
  error,
  children,
}: {
  title: string;
  confirmLabel: string;
  onConfirm: () => void;
  onClose: () => void;
  pending: boolean;
  error?: unknown;
  children: ReactNode;
}) {
  return (
    <Modal title={title} onClose={() => !pending && onClose()}>
      <div className="flex flex-col gap-2 p-4 text-sm">{children}</div>
      <DialogFooter error={error} hint="This can't be undone.">
        <button
          type="button"
          onClick={onClose}
          disabled={pending}
          className={`${neutralButtonClass} px-3 py-1 text-sm`}
        >
          Cancel
        </button>
        <button type="button" onClick={onConfirm} disabled={pending} className={dangerButtonClass}>
          {confirmLabel}
        </button>
      </DialogFooter>
    </Modal>
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

function DeployDialog({
  repos,
  previewDomain,
  onClose,
}: {
  repos: Repo[];
  // Absent until /api/health lands; the hint drops the domain until then
  // rather than guessing the default, which --preview-domain can override.
  previewDomain?: string;
  onClose: () => void;
}) {
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
            !repos.length
              ? "Register a repository first."
              : previewDomain
                ? `Served at <sha>.<repo>.${previewDomain}.`
                : "Served at <sha>.<repo> on the preview domain."
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

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ["KiB", "MiB", "GiB", "TiB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v >= 100 ? v.toFixed(0) : v.toFixed(1)} ${units[i]}`;
}

function uptime(iso: string): string {
  const secs = Math.max(0, Math.floor((Date.now() - new Date(iso).getTime()) / 1000));
  if (secs < 60) return `${secs}s`;
  const mins = Math.floor(secs / 60);
  if (mins < 60) return `${mins}m ${secs % 60}s`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours}h ${mins % 60}m`;
  return `${Math.floor(hours / 24)}d ${hours % 24}h`;
}

// usePinnedScroll keeps a log pane glued to its bottom edge as content
// streams in, unless the user scrolled up to read history.
function usePinnedScroll(dep: unknown) {
  const ref = useRef<HTMLPreElement>(null);
  const pinned = useRef(true);
  // biome-ignore lint/correctness/useExhaustiveDependencies: dep drives re-scroll on content change.
  useEffect(() => {
    const el = ref.current;
    if (el && pinned.current) el.scrollTop = el.scrollHeight;
  }, [dep]);
  const onScroll = () => {
    const el = ref.current;
    if (el) pinned.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
  };
  return { ref, onScroll };
}

const logPaneClass =
  "h-72 overflow-auto whitespace-pre-wrap break-words rounded border border-border bg-surface p-2 font-mono text-xs leading-relaxed";

function Metric({ label, value, title }: { label: string; value: string; title?: string }) {
  return (
    <span className="inline-flex items-baseline gap-1 tabular-nums" title={title}>
      <span className="text-[10px] uppercase tracking-wide text-fg-muted">{label}</span>
      {value}
    </span>
  );
}

// StatsRow is one side's docker-stats-like line: state, CPU, memory, uptime.
function StatsRow({ label, stats }: { label: string; stats: SideStats }) {
  const mem =
    stats.memory_bytes != null
      ? `${formatBytes(stats.memory_bytes)}${
          stats.memory_limit_bytes ? ` / ${formatBytes(stats.memory_limit_bytes)}` : ""
        }`
      : "—";
  return (
    <div className="flex flex-wrap items-center gap-x-4 gap-y-1 px-3 py-1.5 text-xs">
      <span className="w-16 font-medium">{label}</span>
      <StatusBadge state={stats.state} />
      <Metric
        label="cpu"
        value={stats.cpu_percent != null ? `${stats.cpu_percent.toFixed(1)}%` : "—"}
        title="Percent of one CPU core, like docker stats"
      />
      <Metric label="mem" value={mem} title="Resident memory / total available" />
      <Metric label="up" value={stats.started_at ? uptime(stats.started_at) : "—"} />
      {stats.runtime && <span className="ml-auto text-fg-muted">{stats.runtime}</span>}
    </div>
  );
}

// RunLogPane tails a process run log: an initial tail, then only appended
// bytes each poll. A restart (new attempt) resets the view.
function RunLogPane({ deployId, side }: { deployId: number; side: LogSide }) {
  const [text, setText] = useState("");
  const [attempt, setAttempt] = useState(0);
  const [truncated, setTruncated] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const cursor = useRef({ attempt: 0, offset: 0 });
  const { ref, onScroll } = usePinnedScroll(text);

  useEffect(() => {
    setText("");
    setAttempt(0);
    setTruncated(false);
    cursor.current = { attempt: 0, offset: 0 };
    let stopped = false;
    let inFlight = false;
    const tick = async () => {
      if (stopped || inFlight) return;
      inFlight = true;
      try {
        const c = await api.getRunLog(
          deployId,
          side,
          cursor.current.attempt,
          cursor.current.offset,
        );
        if (stopped) return;
        setError(null);
        if (c.attempt !== cursor.current.attempt) {
          // First fetch, or the process restarted into a fresh log file.
          setText(c.content);
          setTruncated(c.truncated ?? false);
          setAttempt(c.attempt);
        } else if (c.content) {
          setText((t) => t + c.content);
        }
        cursor.current = { attempt: c.attempt, offset: c.offset };
      } catch (e) {
        if (!stopped) setError(String(e));
      } finally {
        inFlight = false;
      }
    };
    tick();
    const interval = setInterval(tick, 1000);
    return () => {
      stopped = true;
      clearInterval(interval);
    };
  }, [deployId, side]);

  return (
    <div className="flex flex-col gap-1">
      <pre ref={ref} onScroll={onScroll} className={logPaneClass}>
        {truncated && <span className="text-fg-muted">{"… earlier output omitted\n"}</span>}
        {text ||
          (attempt === 0 ? (
            <span className="text-fg-muted">
              No output yet — the process starts on the preview's first request.
            </span>
          ) : (
            <span className="text-fg-muted">The process hasn't written any output.</span>
          ))}
      </pre>
      <div className="flex justify-between font-mono text-[11px] text-fg-muted">
        <span>{error ? `log fetch failed: ${error}` : "stdout+stderr, refreshed live"}</span>
        {attempt > 0 && <span>start attempt {attempt}</span>}
      </div>
    </div>
  );
}

// BuildLogPane shows the build-log snapshot, refreshing while a build runs.
function BuildLogPane({ deployId, building }: { deployId: number; building: boolean }) {
  const logs = useQuery({
    queryKey: ["deploy-build-log", deployId],
    queryFn: () => api.getBuildLogs(deployId),
    refetchInterval: building ? 1000 : false,
  });
  const { ref, onScroll } = usePinnedScroll(logs.data);
  return (
    <div className="flex flex-col gap-1">
      <pre ref={ref} onScroll={onScroll} className={logPaneClass}>
        {logs.error ? String(logs.error) : (logs.data ?? "loading…")}
      </pre>
      <div className="font-mono text-[11px] text-fg-muted">
        {building ? "build in progress — refreshing" : "frontend and backend build output"}
      </div>
    </div>
  );
}

// ArtifactDownload is one artifact's download affordance: a plain link when
// the build produced a single file, a dropdown listing every file (e.g. one
// binary per platform) when it produced several. The menu is fixed-positioned
// so the list container's overflow-hidden can't clip it.
function ArtifactDownload({ artifact }: { artifact: DeployArtifact }) {
  const [menuPos, setMenuPos] = useState<{ top: number; right: number } | null>(null);
  const buttonRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    if (!menuPos) return;
    const close = () => setMenuPos(null);
    const onPointerDown = (e: PointerEvent) => {
      if (!buttonRef.current?.parentElement?.contains(e.target as Node)) close();
    };
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") close();
    };
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    window.addEventListener("scroll", close, true);
    window.addEventListener("resize", close);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
      window.removeEventListener("scroll", close, true);
      window.removeEventListener("resize", close);
    };
  }, [menuPos]);

  if (artifact.files.length === 1) {
    const f = artifact.files[0];
    return (
      <a
        href={f.url}
        download={f.name}
        title={`${f.name} (${formatBytes(f.size)})`}
        className={`${neutralButtonClass} gap-1 font-mono`}
      >
        <IconDownload className="h-3 w-3" />
        {artifact.name}
      </a>
    );
  }

  return (
    <div className="relative">
      <button
        ref={buttonRef}
        type="button"
        aria-haspopup="menu"
        aria-expanded={menuPos != null}
        title={`Download ${artifact.name} for your platform`}
        onClick={() => {
          if (menuPos) {
            setMenuPos(null);
            return;
          }
          const r = buttonRef.current?.getBoundingClientRect();
          if (r) setMenuPos({ top: r.bottom + 4, right: window.innerWidth - r.right });
        }}
        className={`${neutralButtonClass} gap-1 font-mono`}
      >
        <IconDownload className="h-3 w-3" />
        {artifact.name}
        <IconChevronDown className="h-3 w-3" />
      </button>
      {menuPos && (
        <div
          role="menu"
          style={{ top: menuPos.top, right: menuPos.right }}
          className="fixed z-10 min-w-48 rounded border border-border bg-bg py-1 shadow-lg"
        >
          {artifact.files.map((f) => (
            <a
              key={f.name}
              role="menuitem"
              href={f.url}
              download={f.name}
              onClick={() => setMenuPos(null)}
              className="flex items-baseline justify-between gap-4 px-3 py-1.5 font-mono text-xs text-fg transition-colors duration-150 hover:bg-surface-2"
            >
              {f.name}
              <span className="text-[10px] text-fg-muted tabular-nums">{formatBytes(f.size)}</span>
            </a>
          ))}
        </div>
      )}
    </div>
  );
}

type LogTab = LogSide | "build";

// DeployDetailDialog is the observability view of one deployment: live
// resource stats plus docker-logs-like views of its process and build logs.
function DeployDetailDialog({ deploy, onClose }: { deploy: Deploy; onClose: () => void }) {
  const building = deploy.status === "queued" || deploy.status === "building";
  const hasBackend = !!deploy.be_hash;
  const hasFeProcess = deploy.fe_process != null;
  const [tab, setTab] = useState<LogTab>(hasBackend && !building ? "be" : "build");

  const stats = useQuery({
    queryKey: ["deploy-stats", deploy.id],
    queryFn: () => api.getDeployStats(deploy.id),
    // Two samples make a CPU percentage, so keep a steady cadence.
    refetchInterval: 2000,
  });

  const tabs: { id: LogTab; label: string }[] = [
    ...(hasBackend ? [{ id: "be" as const, label: "backend log" }] : []),
    ...(hasFeProcess ? [{ id: "fe" as const, label: "frontend log" }] : []),
    { id: "build", label: "build log" },
  ];

  return (
    <Modal title={`${deploy.repo} @ ${deploy.short_sha}`} onClose={onClose} wide>
      <div className="flex flex-col gap-3 p-4">
        {(stats.data?.backend || stats.data?.frontend) && (
          <div className="divide-y divide-border rounded border border-border">
            {stats.data.backend && <StatsRow label="backend" stats={stats.data.backend} />}
            {stats.data.frontend && <StatsRow label="frontend" stats={stats.data.frontend} />}
          </div>
        )}
        <div className="flex gap-1">
          {tabs.map((t) => (
            <button
              key={t.id}
              type="button"
              onClick={() => setTab(t.id)}
              className={`rounded px-2 py-1 text-xs transition-colors duration-150 ${
                tab === t.id
                  ? "bg-surface-3 font-medium text-fg"
                  : "bg-surface-2 text-fg-muted hover:bg-surface-3"
              }`}
            >
              {t.label}
            </button>
          ))}
        </div>
        {tab === "build" ? (
          <BuildLogPane deployId={deploy.id} building={building} />
        ) : (
          <RunLogPane deployId={deploy.id} side={tab} />
        )}
      </div>
    </Modal>
  );
}

// StopButton quiesces a deploy's running processes; they cold-start again on
// the next request, so it needs no confirmation. Rendered only when there's a
// live process to stop.
function StopButton({ deploy }: { deploy: Deploy }) {
  const queryClient = useQueryClient();
  const stop = useMutation({
    mutationFn: () => api.stopDeploy(deploy.id),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["deploys"] }),
  });
  return (
    <button
      type="button"
      onClick={() => stop.mutate()}
      disabled={stop.isPending}
      title="Stop the running process (restarts on the next request)"
      className={`${neutralButtonClass} gap-1 disabled:opacity-50`}
    >
      <IconStop className="h-3 w-3" />
      stop
    </button>
  );
}

// DeleteDeployButton hard-deletes a deploy behind a confirmation: the row and
// its unshared artifacts/state are removed and the subdomain freed.
function DeleteDeployButton({ deploy }: { deploy: Deploy }) {
  const queryClient = useQueryClient();
  const [confirming, setConfirming] = useState(false);
  const del = useMutation({
    mutationFn: () => api.deleteDeploy(deploy.id),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["deploys"] });
      setConfirming(false);
    },
  });
  return (
    <>
      <button
        type="button"
        onClick={() => setConfirming(true)}
        title="Delete this deployment"
        aria-label="Delete this deployment"
        className="inline-flex h-7 w-7 shrink-0 items-center justify-center rounded bg-surface-2 text-fg-muted transition-colors duration-150 hover:bg-danger/15 hover:text-danger"
      >
        <IconTrash className="h-3.5 w-3.5" />
      </button>
      {confirming && (
        <ConfirmDialog
          title="Delete deployment"
          confirmLabel={del.isPending ? "Deleting…" : "Delete"}
          onConfirm={() => del.mutate()}
          onClose={() => setConfirming(false)}
          pending={del.isPending}
          error={del.error}
        >
          <p>
            Delete the <span className="font-medium">{deploy.repo}</span> preview at{" "}
            <code className="font-mono text-fg-muted">{deploy.short_sha}</code>?
          </p>
          <p className="text-xs text-fg-muted">
            Its build artifacts and state are removed and the{" "}
            <code className="font-mono">{deploy.short_sha}</code> subdomain is freed. Artifacts
            still shared with another deployment are kept.
          </p>
        </ConfirmDialog>
      )}
    </>
  );
}

// RepoRow is one registered repository in the management modal: its name and
// source, an editable watch toggle + branch globs, and an unregister action.
function RepoRow({ repo }: { repo: Repo }) {
  const queryClient = useQueryClient();
  const [watch, setWatch] = useState(repo.watch);
  const [branches, setBranches] = useState(repo.watch_branches);
  const [confirming, setConfirming] = useState(false);

  // Track the server's value when it changes under us (e.g. after a save
  // refetch) so the dirty check and inputs stay accurate.
  useEffect(() => {
    setWatch(repo.watch);
    setBranches(repo.watch_branches);
  }, [repo.watch, repo.watch_branches]);

  const save = useMutation({
    mutationFn: () => api.updateRepo(repo.name, { watch, watch_branches: branches.trim() }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["repos"] }),
  });
  const del = useMutation({
    mutationFn: () => api.deleteRepo(repo.name),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["repos"] });
      queryClient.invalidateQueries({ queryKey: ["deploys"] });
      setConfirming(false);
    },
  });

  const dirty = watch !== repo.watch || branches.trim() !== repo.watch_branches;
  return (
    <li className="flex flex-col gap-2 px-3 py-2.5">
      <div className="flex items-center gap-2">
        <div className="min-w-0 flex-1">
          <div className="text-sm font-medium">{repo.name}</div>
          <div className="truncate font-mono text-[11px] text-fg-muted" title={repo.source}>
            {repo.source}
          </div>
        </div>
        <button
          type="button"
          onClick={() => setConfirming(true)}
          className="inline-flex shrink-0 items-center rounded bg-surface-2 px-2 py-1 text-xs text-fg-muted transition-colors duration-150 hover:bg-danger/15 hover:text-danger"
        >
          Unregister
        </button>
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <label className="flex shrink-0 cursor-pointer items-center gap-1.5 text-xs text-fg-muted">
          <input
            type="checkbox"
            checked={watch}
            onChange={(e) => setWatch(e.target.checked)}
            className="accent-accent-600"
          />
          Watch
        </label>
        <input
          value={branches}
          disabled={!watch}
          onChange={(e) => setBranches(e.target.value)}
          placeholder="all branches (e.g. main,release/*)"
          className={`${inputClass} min-w-40 flex-1 disabled:opacity-40`}
        />
        {dirty && (
          <button
            type="button"
            onClick={() => save.mutate()}
            disabled={save.isPending}
            className={`${neutralButtonClass} px-3 py-1 text-sm`}
          >
            Save
          </button>
        )}
      </div>
      {save.error && (
        <p className="text-xs text-danger" title={String(save.error)}>
          {String(save.error)}
        </p>
      )}
      {confirming && (
        <ConfirmDialog
          title="Unregister repository"
          confirmLabel={del.isPending ? "Removing…" : "Unregister"}
          onConfirm={() => del.mutate()}
          onClose={() => setConfirming(false)}
          pending={del.isPending}
          error={del.error}
        >
          <p>
            Unregister <span className="font-medium">{repo.name}</span>?
          </p>
          <p className="text-xs text-fg-muted">
            This stops its preview backends and deletes all of its deployments, artifacts, state,
            build logs, and mirror clone. The name becomes reusable immediately.
          </p>
        </ConfirmDialog>
      )}
    </li>
  );
}

// ManageReposDialog lists registered repositories and lets you edit their
// watch settings or unregister them — the repo-side counterpart to the deploy
// list.
function ManageReposDialog({ onClose }: { onClose: () => void }) {
  const repos = useQuery({ queryKey: ["repos"], queryFn: api.listRepos });
  return (
    <Modal title="Registered repositories" onClose={onClose} wide>
      <div className="flex flex-col gap-3 p-4 text-sm">
        {repos.data && repos.data.length > 0 ? (
          <ul className="divide-y divide-border rounded border border-border">
            {repos.data.map((r) => (
              <RepoRow key={r.id} repo={r} />
            ))}
          </ul>
        ) : (
          <p className="px-1 py-6 text-center text-xs text-fg-muted">
            No repositories registered yet.
          </p>
        )}
      </div>
      <DialogFooter
        error={repos.error}
        hint="Watch polls a repo and deploys new branch tips automatically."
      >
        <button
          type="button"
          onClick={onClose}
          className={`${neutralButtonClass} px-3 py-1 text-sm`}
        >
          Done
        </button>
      </DialogFooter>
    </Modal>
  );
}

export default function App() {
  const [dialog, setDialog] = useState<"register" | "deploy" | "repos" | null>(null);
  const [detailId, setDetailId] = useState<number | null>(null);

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
    // Poll while anything is in flight (builds or cold starts) so state
    // flips surface promptly.
    refetchInterval: (query) =>
      query.state.data?.some(
        (d) =>
          d.status === "queued" ||
          d.status === "building" ||
          d.process === "starting" ||
          d.fe_process === "starting",
      )
        ? 1000
        : 5000,
  });

  const hasRepos = (repos.data?.length ?? 0) > 0;
  // Resolve from the live list each render so the dialog tracks state
  // changes (build finishing, processes warming) instead of a snapshot.
  const detail = detailId != null ? deploys.data?.find((d) => d.id === detailId) : undefined;

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
          <a
            href="https://jamison.lahman.dev/local-preview/guide/"
            target="_blank"
            rel="noopener noreferrer"
            title="Help"
            aria-label="Help"
            className="inline-flex h-7 w-7 items-center justify-center rounded bg-surface-2 text-fg transition-colors duration-150 hover:bg-surface-3"
          >
            <IconHelp />
          </a>
          <button
            type="button"
            onClick={() => setDialog("repos")}
            title="Manage registered repositories"
            aria-label="Manage registered repositories"
            className="inline-flex h-7 w-7 items-center justify-center rounded bg-surface-2 text-fg transition-colors duration-150 hover:bg-surface-3"
          >
            <IconSettings />
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
                    <div className="h-5 w-22 rounded-full bg-surface-2" />
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
                // Per-service content hashes live in the commit sha's hover
                // instead of taking up a row of their own.
                const shaTitle = [
                  d.sha,
                  d.fe_hash ? `frontend ${d.fe_hash.slice(0, 12)}` : null,
                  d.be_hash ? `backend ${d.be_hash.slice(0, 12)}` : null,
                  ...(d.artifacts?.map((a) => `${a.name} ${a.hash.slice(0, 12)}`) ?? []),
                ]
                  .filter(Boolean)
                  .join("\n");
                const state = deployState(d);
                // A process is worth stopping only once it's warm or warming.
                const stoppable = state === "running" || state === "starting";
                return (
                  <li
                    key={d.id}
                    className="flex flex-wrap items-center gap-x-4 gap-y-1.5 px-3 py-3 transition-colors hover:bg-surface-2/40"
                  >
                    <StatusBadge state={state} />
                    <div className="min-w-0 flex-1">
                      <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5">
                        <span className="text-sm font-medium">{d.repo}</span>
                        <code className="font-mono text-xs text-fg-muted" title={shaTitle}>
                          {d.short_sha}
                        </code>
                        {d.author_name && (
                          <span className="text-xs text-fg-muted" title={d.author_email}>
                            by {d.author_name}
                          </span>
                        )}
                      </div>
                      {(d.branch || (d.ref && d.ref !== d.branch)) && (
                        <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-0.5">
                          {d.branch && (
                            <span className="inline-flex min-w-0 items-center gap-1 rounded-full border border-border bg-surface-2 px-2 py-px font-mono text-[11px] text-fg-muted">
                              <IconGitBranch className="h-3 w-3 shrink-0" />
                              <span className="truncate">{d.branch}</span>
                            </span>
                          )}
                          {d.ref && d.ref !== d.branch && (
                            <span className="min-w-0 truncate rounded-full border border-border bg-surface-2 px-2 py-px font-mono text-[11px] text-fg-muted">
                              {d.ref}
                            </span>
                          )}
                        </div>
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
                    {d.artifacts?.map((a) => (
                      <ArtifactDownload key={a.name} artifact={a} />
                    ))}
                    {stoppable && <StopButton deploy={d} />}
                    <button
                      type="button"
                      onClick={() => setDetailId(d.id)}
                      title="Logs and resource usage"
                      className={`${neutralButtonClass} gap-1`}
                    >
                      <IconPulse className="h-3 w-3" />
                      logs
                    </button>
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
                    <time
                      dateTime={d.created_at}
                      title={d.created_at}
                      className="w-14 shrink-0 text-right text-xs text-fg-muted tabular-nums"
                    >
                      {timeAgo(d.created_at)}
                    </time>
                    <DeleteDeployButton deploy={d} />
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
      {dialog === "repos" && <ManageReposDialog onClose={() => setDialog(null)} />}
      {dialog === "deploy" && (
        <DeployDialog
          repos={repos.data ?? []}
          previewDomain={health.data?.preview_domain}
          onClose={() => setDialog(null)}
        />
      )}
      {detail && <DeployDetailDialog deploy={detail} onClose={() => setDetailId(null)} />}
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

function IconDownload({ className = "" }: { className?: string }) {
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
      <path d="M12 3v12m0 0 4-4m-4 4-4-4" />
      <path d="M4 17v2a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-2" />
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

function IconPulse({ className = "" }: { className?: string }) {
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
      <path d="M22 12h-4l-3 9L9 3l-3 9H2" />
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

function IconStop({ className = "" }: { className?: string }) {
  return (
    <svg viewBox="0 0 24 24" className={className} fill="currentColor" aria-hidden="true">
      <rect x="6" y="6" width="12" height="12" rx="1.5" />
    </svg>
  );
}

function IconTrash({ className = "" }: { className?: string }) {
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
      <path d="M3 6h18M8 6V4a1 1 0 0 1 1-1h6a1 1 0 0 1 1 1v2m2 0v14a1 1 0 0 1-1 1H7a1 1 0 0 1-1-1V6" />
      <path d="M10 11v6M14 11v6" />
    </svg>
  );
}

function IconHelp() {
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
      <circle cx="12" cy="12" r="10" />
      <path d="M9.09 9a3 3 0 0 1 5.83 1c0 2-3 3-3 3" />
      <line x1="12" y1="17" x2="12.01" y2="17" />
    </svg>
  );
}

function IconSettings() {
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
      <circle cx="12" cy="12" r="3" />
      <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z" />
    </svg>
  );
}
