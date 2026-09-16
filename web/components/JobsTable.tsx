import Link from "next/link";
import { isCancellable, type Job } from "@/lib/jobs";

type JobsTableProps = {
  jobs: Job[];
  onCancel: (job: Job) => void;
  // busyId disables the row's cancel action while a mutation for that job runs.
  busyId?: string | null;
};

// fmt renders an RFC3339 timestamp as a short local string, or an em dash when
// the value is absent.
function fmt(ts?: string): string {
  if (!ts) return "—";
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return ts;
  return d.toLocaleString();
}

// humanDuration renders a millisecond span as ms or s, or an em dash when the
// value is absent.
export function humanDuration(ms?: number): string {
  if (ms == null) return "—";
  if (ms < 1000) return `${ms}ms`;
  return `${(ms / 1000).toFixed(1)}s`;
}

// StatusBadge maps a job status to a tone: failed → danger, succeeded/running →
// accent, everything else (queued/cancelled) → subtle.
function StatusBadge({ status }: { status: string }) {
  const tone =
    status === "failed"
      ? "bg-danger-tint text-danger-text"
      : status === "succeeded" || status === "running"
        ? "bg-accent-tint text-accent-text"
        : "bg-bg-subtle text-fg-muted";
  return (
    <span className={`inline-flex rounded-full px-2 py-0.5 text-xs font-medium ${tone}`}>{status}</span>
  );
}

function RowAction({
  label,
  onClick,
  disabled,
  danger,
}: {
  label: string;
  onClick: () => void;
  disabled?: boolean;
  danger?: boolean;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={disabled}
      className={`h-8 rounded-md border px-2.5 text-xs font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg disabled:cursor-not-allowed disabled:opacity-50 ${
        danger
          ? "border-border text-danger-text hover:border-danger-text hover:bg-danger-tint"
          : "border-border text-fg hover:border-border-strong hover:bg-bg-subtle"
      }`}
    >
      {label}
    </button>
  );
}

export function JobsTable({ jobs, onCancel, busyId }: JobsTableProps) {
  if (jobs.length === 0) {
    return (
      <div className="mt-6 flex flex-col items-center gap-3 rounded-2xl border border-dashed border-border px-6 py-16 text-center">
        <h2 className="text-lg font-semibold tracking-tight text-fg">No jobs yet</h2>
        <p className="max-w-sm text-sm text-fg-muted">
          Jobs appear here when a source syncs or a reindex runs.
        </p>
      </div>
    );
  }

  return (
    <div className="mt-6 overflow-x-auto rounded-2xl border border-border">
      <table className="w-full border-collapse text-left text-sm">
        <thead>
          <tr className="border-b border-border text-xs uppercase tracking-wide text-fg-muted">
            <th className="px-4 py-3 font-medium">Kind</th>
            <th className="px-4 py-3 font-medium">Status</th>
            <th className="px-4 py-3 font-medium">Source</th>
            <th className="px-4 py-3 font-medium">Attempt</th>
            <th className="px-4 py-3 font-medium">Duration</th>
            <th className="px-4 py-3 font-medium">Queued</th>
            <th className="px-4 py-3 font-medium">Finished</th>
            <th className="px-4 py-3 font-medium">
              <span className="sr-only">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {jobs.map((j) => {
            const busy = busyId === j.id;
            return (
              <tr key={j.id} className="border-b border-border last:border-b-0 align-top">
                <td className="px-4 py-3">
                  <Link
                    href={`/admin/jobs/${j.id}`}
                    className="font-mono text-xs text-fg transition-colors hover:text-accent-text focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg"
                  >
                    {j.kind}
                  </Link>
                </td>
                <td className="px-4 py-3">
                  <StatusBadge status={j.status} />
                </td>
                <td className="px-4 py-3 font-mono text-xs text-fg-muted">{j.source_id ?? "—"}</td>
                <td className="px-4 py-3 text-xs text-fg-muted">
                  {j.attempt}/{j.max_attempts}
                </td>
                <td className="px-4 py-3 text-xs text-fg-muted">{humanDuration(j.duration_ms)}</td>
                <td className="px-4 py-3 text-xs text-fg-muted">{fmt(j.queued_at)}</td>
                <td className="px-4 py-3 text-xs text-fg-muted">{fmt(j.finished_at)}</td>
                <td className="px-4 py-3">
                  <div className="flex items-center justify-end gap-1.5">
                    {isCancellable(j.status) ? (
                      <RowAction label="Cancel" onClick={() => onCancel(j)} disabled={busy} danger />
                    ) : null}
                    <Link
                      href={`/admin/jobs/${j.id}`}
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
