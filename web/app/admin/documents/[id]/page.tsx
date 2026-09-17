"use client";

import Link from "next/link";
import { useParams } from "next/navigation";
import { DocumentDetail } from "@/components/DocumentDetail";
import { useChunks, useDocument } from "@/lib/documents";

export default function DocumentDetailPage() {
  const params = useParams<{ id: string }>();
  const id = params.id;

  const doc = useDocument(id);
  const chunks = useChunks(id);

  return (
    <div className="flex flex-col gap-1">
      <p className="font-mono text-xs uppercase tracking-wide text-fg-muted">/admin/documents/{id}</p>

      <div className="flex items-center justify-between gap-4">
        <h1 className="text-lg font-semibold tracking-tight text-fg">Document</h1>
        <Link
          href="/admin/documents"
          className="text-sm font-medium text-fg-muted transition-colors hover:text-fg"
        >
          Back to documents
        </Link>
      </div>

      {doc.isLoading ? (
        <div className="mt-6 h-40 animate-pulse rounded-2xl border border-border bg-bg-subtle" />
      ) : doc.isError ? (
        <div className="mt-6 rounded-2xl border border-border bg-danger-tint px-4 py-4 text-sm text-danger-text">
          Could not load the document: {doc.error instanceof Error ? doc.error.message : "unknown error"}
        </div>
      ) : doc.data ? (
        <DocumentDetail doc={doc.data} chunks={chunks.data?.items ?? []} />
      ) : null}
    </div>
  );
}
