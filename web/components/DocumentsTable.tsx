import Link from "next/link";
import type { Document } from "@/lib/documents";

type DocumentsTableProps = {
  documents: Document[];
};

// fmt renders an RFC3339 timestamp as a short local string, or an em dash when the
// value is absent.
function fmt(ts?: string): string {
  if (!ts) return "—";
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return ts;
  return d.toLocaleString();
}

// StatusBadge maps a document status to a tone: deleted → danger, active → accent.
function StatusBadge({ status }: { status: string }) {
  const tone =
    status === "deleted" ? "bg-danger-tint text-danger-text" : "bg-accent-tint text-accent-text";
  return (
    <span className={`inline-flex rounded-full px-2 py-0.5 text-xs font-medium ${tone}`}>{status}</span>
  );
}

// label picks the human-facing name for a document row: its title, else its URI,
// else its external id.
function label(doc: Document): string {
  return doc.title || doc.uri || doc.external_id;
}

export function DocumentsTable({ documents }: DocumentsTableProps) {
  if (documents.length === 0) {
    return (
      <div className="mt-6 flex flex-col items-center gap-3 rounded-2xl border border-dashed border-border px-6 py-16 text-center">
        <h2 className="text-lg font-semibold tracking-tight text-fg">No documents yet</h2>
        <p className="max-w-sm text-sm text-fg-muted">
          Documents appear here once a source syncs or a file is uploaded.
        </p>
      </div>
    );
  }

  return (
    <div className="mt-6 overflow-x-auto rounded-2xl border border-border">
      <table className="w-full border-collapse text-left text-sm">
        <thead>
          <tr className="border-b border-border text-xs uppercase tracking-wide text-fg-muted">
            <th className="px-4 py-3 font-medium">Title</th>
            <th className="px-4 py-3 font-medium">Status</th>
            <th className="px-4 py-3 font-medium">Source</th>
            <th className="px-4 py-3 font-medium">Type</th>
            <th className="px-4 py-3 font-medium">Last seen</th>
            <th className="px-4 py-3 font-medium">
              <span className="sr-only">Actions</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {documents.map((doc) => (
            <tr key={doc.id} className="border-b border-border last:border-b-0 align-top">
              <td className="px-4 py-3">
                <Link
                  href={`/admin/documents/${doc.id}`}
                  className="text-fg transition-colors hover:text-accent-text focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg"
                >
                  {label(doc)}
                </Link>
              </td>
              <td className="px-4 py-3">
                <StatusBadge status={doc.status} />
              </td>
              <td className="px-4 py-3 font-mono text-xs text-fg-muted">{doc.source_id}</td>
              <td className="px-4 py-3 text-xs text-fg-muted">{doc.mime_type ?? "—"}</td>
              <td className="px-4 py-3 text-xs text-fg-muted">{fmt(doc.last_seen_at)}</td>
              <td className="px-4 py-3">
                <div className="flex items-center justify-end">
                  <Link
                    href={`/admin/documents/${doc.id}`}
                    className="flex h-8 items-center rounded-md border border-border px-2.5 text-xs font-medium text-fg transition-colors hover:border-border-strong hover:bg-bg-subtle focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg"
                  >
                    View
                  </Link>
                </div>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
