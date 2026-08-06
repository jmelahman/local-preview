import { useQuery } from "@tanstack/react-query";
import { api } from "@/api/client";

export default function App() {
  const health = useQuery({ queryKey: ["health"], queryFn: api.health });

  return (
    <div className="flex min-h-screen flex-col items-center bg-bg px-4 py-16 text-fg">
      <main className="w-full max-w-md space-y-6">
        <header>
          <h1 className="text-2xl font-semibold">local-preview</h1>
          <p className="text-sm text-fg-muted">
            Per-commit preview deployments, served from one binary.
          </p>
        </header>

        <footer className="text-xs text-fg-muted">
          {health.data ? `server ${health.data.version}` : "server unreachable"}
        </footer>
      </main>
    </div>
  );
}
