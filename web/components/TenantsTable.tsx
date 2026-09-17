import type { PlatformTenant } from "@/lib/tenants";

type TenantsTableProps = {
  tenants: PlatformTenant[];
  onSuspend: (t: PlatformTenant) => void;
  onActivate: (t: PlatformTenant) => void;
  onDelete: (t: PlatformTenant) => void;
  // busyId disables the row's controls while a mutation for that tenant runs.
  busyId?: string | null;
};

function fmt(ts?: string): string {
  if (!ts) return "—";
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return ts;
  return d.toLocaleString();
}

// StatusBadge tones: active → accent, suspended → subtle, deleting/deleted → danger.
function StatusBadge({ status }: { status: string }) {
  const tone =
    status === "active"
      ? "bg-accent-tint text-accent-text"
      : status === "deleting" || status === "deleted"
        ? "bg-danger-tint text-danger-text"
        : "bg-bg-subtle text-fg-muted";
  return (
    <span className={`inline-flex rounded-full px-2 py-0.5 text-xs font-medium ${tone}`}>{status}</span>
  );
}

function RowButton({
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

export function TenantsTable({ tenants, onSuspend, onActivate, onDelete, busyId }: TenantsTableProps) {
  if (tenants.length === 0) {
    return (
      <div className="mt-6 flex flex-col items-center gap-3 rounded-2xl border border-dashed border-border px-6 py-16 text-center">
        <h2 className="text-lg font-semibold tracking-tight text-fg">No tenants yet</h2>
        <p className="max-w-sm text-sm text-fg-muted">Enrol a tenant to provision its store and schema.</p>
      </div>
    );
  }

  return (
    <div className="mt-6 overflow-x-auto rounded-2xl border border-border">
      <table className="w-full border-collapse text-left text-sm">
        <thead>
          <tr className="border-b border-border text-xs uppercase tracking-wide text-fg-muted">
            <th className="px-4 py-3 font-medium">Name</th>
            <th className="px-4 py-3 font-medium">Slug</th>
            <th className="px-4 py-3 font-medium">Status</th>
            <th className="px-4 py-3 font-medium">Region</th>
            <th className="px-4 py-3 font-medium">Created</th>
            <th className="px-4 py-3 font-medium">
              <span className="sr-only">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {tenants.map((t) => {
            const busy = busyId === t.id;
            const terminal = t.status === "deleting" || t.status === "deleted";
            return (
              <tr key={t.id} className="border-b border-border last:border-b-0 align-top">
                <td className="px-4 py-3 font-medium text-fg">{t.name}</td>
                <td className="px-4 py-3 font-mono text-xs text-fg-muted">{t.slug}</td>
                <td className="px-4 py-3">
                  <StatusBadge status={t.status} />
                </td>
                <td className="px-4 py-3 text-xs text-fg-muted">{t.region}</td>
                <td className="px-4 py-3 text-xs text-fg-muted">{fmt(t.created_at)}</td>
                <td className="px-4 py-3">
                  <div className="flex items-center justify-end gap-1.5">
                    {t.status === "active" ? (
                      <RowButton label="Suspend" onClick={() => onSuspend(t)} disabled={busy} />
                    ) : null}
                    {t.status === "suspended" ? (
                      <RowButton label="Activate" onClick={() => onActivate(t)} disabled={busy} />
                    ) : null}
                    {!terminal ? (
                      <RowButton label="Delete" onClick={() => onDelete(t)} disabled={busy} danger />
                    ) : null}
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
