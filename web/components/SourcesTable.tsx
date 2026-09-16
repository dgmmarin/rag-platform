import Link from "next/link";
import type { Source } from "@/lib/sources";

type SourcesTableProps = {
  sources: Source[];
  onSync: (source: Source) => void;
  onTest: (source: Source) => void;
  onDelete: (source: Source) => void;
  // busyId disables the row's actions while a mutation for that source runs.
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

function StatusBadge({ status }: { status: string }) {
  const tone =
    status === "error"
      ? "bg-danger-tint text-danger-text"
      : status === "active"
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

export function SourcesTable({ sources, onSync, onTest, onDelete, busyId }: SourcesTableProps) {
  if (sources.length === 0) {
    return (
      <div className="mt-6 flex flex-col items-center gap-3 rounded-2xl border border-dashed border-border px-6 py-16 text-center">
        <h2 className="text-lg font-semibold tracking-tight text-fg">No sources yet</h2>
        <p className="max-w-sm text-sm text-fg-muted">
          Add a source to start ingesting content for this tenant.
        </p>
        <Link
          href="/admin/sources/new"
          className="mt-1 inline-flex h-9 items-center rounded-md bg-accent px-3 text-sm font-medium text-accent-fg transition-colors hover:bg-accent-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg"
        >
          New source
        </Link>
      </div>
    );
  }

  return (
    <div className="mt-6 overflow-x-auto rounded-2xl border border-border">
      <table className="w-full border-collapse text-left text-sm">
        <thead>
          <tr className="border-b border-border text-xs uppercase tracking-wide text-fg-muted">
            <th className="px-4 py-3 font-medium">Name</th>
            <th className="px-4 py-3 font-medium">Kind</th>
            <th className="px-4 py-3 font-medium">Status</th>
            <th className="px-4 py-3 font-medium">Last sync</th>
            <th className="px-4 py-3 font-medium">Next sync</th>
            <th className="px-4 py-3 font-medium">
              <span className="sr-only">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {sources.map((s) => {
            const busy = busyId === s.id;
            const lastSync = s.last_success_at ?? s.last_run_at;
            return (
              <tr key={s.id} className="border-b border-border last:border-b-0 align-top">
                <td className="px-4 py-3">
                  <span className="font-medium text-fg">{s.name}</span>
                  {s.last_error ? (
                    <p className="mt-1 max-w-xs text-xs text-danger-text">{s.last_error}</p>
                  ) : null}
                </td>
                <td className="px-4 py-3 font-mono text-xs text-fg-muted">{s.kind}</td>
                <td className="px-4 py-3">
                  <StatusBadge status={s.status} />
                </td>
                <td className="px-4 py-3 text-xs text-fg-muted">{fmt(lastSync)}</td>
                <td className="px-4 py-3 text-xs text-fg-muted">{fmt(s.next_run_at)}</td>
                <td className="px-4 py-3">
                  <div className="flex items-center justify-end gap-1.5">
                    <RowAction label="Sync" onClick={() => onSync(s)} disabled={busy} />
                    <RowAction label="Test" onClick={() => onTest(s)} disabled={busy} />
                    <Link
                      href={`/admin/sources/${s.id}/edit`}
                      className="flex h-8 items-center rounded-md border border-border px-2.5 text-xs font-medium text-fg transition-colors hover:border-border-strong hover:bg-bg-subtle focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg"
                    >
                      Edit
                    </Link>
                    <RowAction label="Delete" onClick={() => onDelete(s)} disabled={busy} danger />
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
