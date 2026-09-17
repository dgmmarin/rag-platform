"use client";

import { useState } from "react";
import { DocumentsTable } from "@/components/DocumentsTable";
import { useDocuments, type DocumentFilter, type DocumentStatus } from "@/lib/documents";

// The status filter offers "all" plus each document status (schemas/tenant.sql
// document_status). "q" is a free-text search over the tenant's documents.
const STATUS_OPTIONS: { value: "all" | DocumentStatus; label: string }[] = [
  { value: "all", label: "All statuses" },
  { value: "active", label: "Active" },
  { value: "deleted", label: "Deleted" },
];

function LoadingSkeleton() {
  return (
    <div className="mt-6 overflow-hidden rounded-2xl border border-border">
      {[0, 1, 2].map((i) => (
        <div key={i} className="flex items-center gap-4 border-b border-border px-4 py-4 last:border-b-0">
          <div className="h-4 w-48 animate-pulse rounded bg-bg-subtle" />
          <div className="h-4 w-16 animate-pulse rounded bg-bg-subtle" />
          <div className="h-4 w-24 animate-pulse rounded bg-bg-subtle" />
          <div className="ml-auto h-8 w-16 animate-pulse rounded bg-bg-subtle" />
        </div>
      ))}
    </div>
  );
}

export default function DocumentsPage() {
  const [status, setStatus] = useState<"all" | DocumentStatus>("all");
  // `draft` is the search box value; `q` is the applied search (submitted on Enter)
  // so we do not refetch on every keystroke.
  const [draft, setDraft] = useState("");
  const [q, setQ] = useState("");

  const filter: DocumentFilter = {
    ...(status === "all" ? {} : { status }),
    ...(q ? { q } : {}),
  };
  const { data, isLoading, isError, error } = useDocuments(filter);

  return (
    <div className="flex flex-col gap-1">
      <p className="font-mono text-xs uppercase tracking-wide text-fg-muted">/admin/documents</p>

      <div className="flex flex-wrap items-center justify-between gap-4">
        <h1 className="text-lg font-semibold tracking-tight text-fg">Documents</h1>
        <div className="flex flex-wrap items-center gap-3">
          <form
            onSubmit={(e) => {
              e.preventDefault();
              setQ(draft.trim());
            }}
          >
            <input
              type="search"
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              placeholder="Search documents"
              aria-label="Search documents"
              className="h-9 w-56 rounded-md border border-border bg-bg px-3 text-sm text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg"
            />
          </form>
          <label className="flex items-center gap-2 text-sm text-fg-muted">
            <span>Status</span>
            <select
              value={status}
              onChange={(e) => setStatus(e.target.value as "all" | DocumentStatus)}
              className="h-9 rounded-md border border-border bg-bg px-2 text-sm text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg"
            >
              {STATUS_OPTIONS.map((o) => (
                <option key={o.value} value={o.value}>
                  {o.label}
                </option>
              ))}
            </select>
          </label>
        </div>
      </div>

      {isLoading ? (
        <LoadingSkeleton />
      ) : isError ? (
        <div className="mt-6 rounded-2xl border border-border bg-danger-tint px-4 py-4 text-sm text-danger-text">
          Could not load documents: {error instanceof Error ? error.message : "unknown error"}
        </div>
      ) : (
        <DocumentsTable documents={data?.items ?? []} />
      )}
    </div>
  );
}
