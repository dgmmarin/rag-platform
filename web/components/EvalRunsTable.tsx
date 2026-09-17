import Link from "next/link";
import { pct, type RunView } from "@/lib/eval";

type EvalRunsTableProps = {
  runs: RunView[];
};

function fmt(ts?: string): string {
  if (!ts) return "—";
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return ts;
  return d.toLocaleString();
}

export function EvalRunsTable({ runs }: EvalRunsTableProps) {
  if (runs.length === 0) {
    return (
      <div className="mt-6 flex flex-col items-center gap-3 rounded-2xl border border-dashed border-border px-6 py-16 text-center">
        <h2 className="text-lg font-semibold tracking-tight text-fg">No eval runs yet</h2>
        <p className="max-w-sm text-sm text-fg-muted">
          Run <code className="font-mono text-xs">ragctl eval run</code> to record a run here.
        </p>
      </div>
    );
  }

  return (
    <div className="mt-6 overflow-x-auto rounded-2xl border border-border">
      <table className="w-full border-collapse text-left text-sm">
        <thead>
          <tr className="border-b border-border text-xs uppercase tracking-wide text-fg-muted">
            <th className="px-4 py-3 font-medium">Started</th>
            <th className="px-4 py-3 font-medium">Cases</th>
            <th className="px-4 py-3 font-medium">Recall@k</th>
            <th className="px-4 py-3 font-medium">Grounded</th>
            <th className="px-4 py-3 font-medium">Correct</th>
            <th className="px-4 py-3 font-medium">Mean latency</th>
            <th className="px-4 py-3 font-medium">
              <span className="sr-only">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {runs.map((run) => {
            const s = run.summary;
            return (
              <tr key={run.id} className="border-b border-border last:border-b-0 align-top">
                <td className="px-4 py-3">
                  <Link
                    href={`/admin/eval/${run.id}`}
                    className="text-fg transition-colors hover:text-accent-text focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg"
                  >
                    {fmt(run.started_at)}
                  </Link>
                </td>
                <td className="px-4 py-3 text-xs text-fg-muted">{s?.cases ?? "—"}</td>
                <td className="px-4 py-3 text-xs text-fg-muted">{pct(s?.recall_at_k)}</td>
                <td className="px-4 py-3 text-xs text-fg-muted">{pct(s?.grounded_rate)}</td>
                <td className="px-4 py-3 text-xs text-fg-muted">
                  {s?.correctness_rate != null ? pct(s.correctness_rate) : "—"}
                </td>
                <td className="px-4 py-3 text-xs text-fg-muted">
                  {s?.mean_latency_ms != null ? `${s.mean_latency_ms}ms` : "—"}
                </td>
                <td className="px-4 py-3">
                  <div className="flex items-center justify-end">
                    <Link
                      href={`/admin/eval/${run.id}`}
                      className="flex h-8 items-center rounded-md border border-border px-2.5 text-xs font-medium text-fg transition-colors hover:border-border-strong hover:bg-bg-subtle focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg"
                    >
                      View
                    </Link>
                  </div>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
