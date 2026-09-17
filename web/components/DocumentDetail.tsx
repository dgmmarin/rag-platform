import type { Chunk, DocumentDetail as Doc } from "@/lib/documents";

type DocumentDetailProps = {
  doc: Doc;
  chunks: Chunk[];
};

function fmt(ts?: string): string {
  if (!ts) return "—";
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return ts;
  return d.toLocaleString();
}

function StatusBadge({ status }: { status: string }) {
  const tone =
    status === "deleted" ? "bg-danger-tint text-danger-text" : "bg-accent-tint text-accent-text";
  return (
    <span className={`inline-flex rounded-full px-2 py-0.5 text-xs font-medium ${tone}`}>{status}</span>
  );
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex flex-col gap-1">
      <dt className="text-xs uppercase tracking-wide text-fg-muted">{label}</dt>
      <dd className="text-sm text-fg">{children}</dd>
    </div>
  );
}

// Chunks lists the document's chunks in position order: heading path, token count,
// embedding model and the chunk text. The embedding vector is never returned.
function Chunks({ chunks }: { chunks: Chunk[] }) {
  return (
    <div className="mt-6 rounded-2xl border border-border px-4 py-4">
      <h2 className="text-sm font-semibold tracking-tight text-fg">
        Chunks{chunks.length > 0 ? ` (${chunks.length})` : ""}
      </h2>
      {chunks.length === 0 ? (
        <p className="mt-4 text-sm text-fg-muted">This document has no chunks.</p>
      ) : (
        <ul className="mt-4 flex flex-col gap-4">
          {chunks.map((c) => (
            <li key={c.id} className="rounded-md border border-border px-3 py-3">
              <div className="flex flex-wrap items-center gap-2 text-xs text-fg-muted">
                <span className="font-mono">#{c.position}</span>
                {c.heading_path.length > 0 ? (
                  <span className="truncate">{c.heading_path.join(" › ")}</span>
                ) : null}
                <span className="ml-auto font-mono">{c.token_count} tok</span>
                <span className="font-mono">{c.embedding_model}</span>
              </div>
              <p className="mt-2 whitespace-pre-wrap text-sm text-fg">{c.content}</p>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

export function DocumentDetail({ doc, chunks }: DocumentDetailProps) {
  const version = doc.current_version_meta;
  return (
    <div className="mt-6 flex flex-col gap-6">
      <div className="rounded-2xl border border-border px-4 py-4">
        <div className="flex items-start justify-between gap-4">
          <h2 className="text-sm font-medium text-fg">{doc.title || doc.uri || doc.external_id}</h2>
          <StatusBadge status={doc.status} />
        </div>

        <dl className="mt-6 grid grid-cols-2 gap-4 sm:grid-cols-3">
          <Field label="Source">
            <span className="font-mono text-xs">{doc.source_id}</span>
          </Field>
          <Field label="External ID">
            <span className="font-mono text-xs">{doc.external_id}</span>
          </Field>
          <Field label="Type">{doc.mime_type ?? "—"}</Field>
          {doc.uri ? (
            <Field label="URI">
              <span className="break-all text-xs">{doc.uri}</span>
            </Field>
          ) : null}
          <Field label="First seen">{fmt(doc.first_seen_at)}</Field>
          <Field label="Last seen">{fmt(doc.last_seen_at)}</Field>
          {doc.deleted_at ? <Field label="Deleted">{fmt(doc.deleted_at)}</Field> : null}
        </dl>

        {version ? (
          <dl className="mt-6 grid grid-cols-2 gap-4 border-t border-border pt-6 sm:grid-cols-3">
            <Field label="Content hash">
              <span className="break-all font-mono text-xs">{version.content_hash}</span>
            </Field>
            <Field label="Characters">{version.char_count}</Field>
            {version.parser ? <Field label="Parser">{version.parser}</Field> : null}
            <Field label="Version built">{fmt(version.created_at)}</Field>
          </dl>
        ) : null}
      </div>

      <Chunks chunks={chunks} />
    </div>
  );
}
