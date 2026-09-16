import type { ApiKey } from "@/lib/apiKeys";

type ApiKeysTableProps = {
  keys: ApiKey[];
  onRevoke: (key: ApiKey) => void;
  // busyId disables the row's revoke action while a mutation for that key runs.
  busyId?: string | null;
};

// fmt renders an RFC3339 timestamp as a short local string, or an em dash when
// the value is absent. Mirrors SourcesTable.fmt.
function fmt(ts?: string): string {
  if (!ts) return "—";
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return ts;
  return d.toLocaleString();
}

function StatusBadge({ revoked }: { revoked: boolean }) {
  const tone = revoked ? "bg-bg-subtle text-fg-muted" : "bg-accent-tint text-accent-text";
  return (
    <span className={`inline-flex rounded-full px-2 py-0.5 text-xs font-medium ${tone}`}>
      {revoked ? "Revoked" : "Active"}
    </span>
  );
}

export function ApiKeysTable({ keys, onRevoke, busyId }: ApiKeysTableProps) {
  if (keys.length === 0) {
    return (
      <div className="mt-6 flex flex-col items-center gap-3 rounded-2xl border border-dashed border-border px-6 py-16 text-center">
        <h2 className="text-lg font-semibold tracking-tight text-fg">No API keys yet</h2>
        <p className="max-w-sm text-sm text-fg-muted">
          Create a key to let a service query or ingest for this tenant.
        </p>
      </div>
    );
  }

  function revoke(key: ApiKey) {
    if (window.confirm(`Revoke the key "${key.name}"? This cannot be undone.`)) {
      onRevoke(key);
    }
  }

  return (
    <div className="mt-6 overflow-x-auto rounded-2xl border border-border">
      <table className="w-full border-collapse text-left text-sm">
        <thead>
          <tr className="border-b border-border text-xs uppercase tracking-wide text-fg-muted">
            <th className="px-4 py-3 font-medium">Name</th>
            <th className="px-4 py-3 font-medium">Prefix</th>
            <th className="px-4 py-3 font-medium">Scopes</th>
            <th className="px-4 py-3 font-medium">Created</th>
            <th className="px-4 py-3 font-medium">Expires</th>
            <th className="px-4 py-3 font-medium">Last used</th>
            <th className="px-4 py-3 font-medium">Status</th>
            <th className="px-4 py-3 font-medium">
              <span className="sr-only">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {keys.map((k) => {
            const revoked = !!k.revokedAt;
            const busy = busyId === k.id;
            return (
              <tr key={k.id} className="border-b border-border last:border-b-0 align-top">
                <td className="px-4 py-3 font-medium text-fg">{k.name}</td>
                <td className="px-4 py-3 font-mono text-xs text-fg-muted">{k.prefix}</td>
                <td className="px-4 py-3">
                  <div className="flex flex-wrap gap-1">
                    {k.scopes.map((s) => (
                      <span
                        key={s}
                        className="inline-flex rounded-full bg-bg-subtle px-2 py-0.5 font-mono text-xs text-fg-muted"
                      >
                        {s}
                      </span>
                    ))}
                  </div>
                </td>
                <td className="px-4 py-3 text-xs text-fg-muted">{fmt(k.createdAt)}</td>
                <td className="px-4 py-3 text-xs text-fg-muted">{fmt(k.expiresAt)}</td>
                <td className="px-4 py-3 text-xs text-fg-muted">{fmt(k.lastUsedAt)}</td>
                <td className="px-4 py-3">
                  <StatusBadge revoked={revoked} />
                </td>
                <td className="px-4 py-3">
                  <div className="flex items-center justify-end">
                    {revoked ? null : (
                      <button
                        type="button"
                        onClick={() => revoke(k)}
                        disabled={busy}
                        className="h-8 rounded-md border border-border px-2.5 text-xs font-medium text-danger-text transition-colors hover:border-danger-text hover:bg-danger-tint focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg disabled:cursor-not-allowed disabled:opacity-50"
                      >
                        Revoke
                      </button>
                    )}
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
