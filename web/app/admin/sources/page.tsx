"use client";

import Link from "next/link";
import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useAuth } from "@/lib/auth";
import { useTenant } from "@/lib/tenant";
import { SourcesTable } from "@/components/SourcesTable";
import { deleteSource, syncSource, testSource, useSources, type Source } from "@/lib/sources";

function LoadingSkeleton() {
  return (
    <div className="mt-6 overflow-hidden rounded-2xl border border-border">
      {[0, 1, 2].map((i) => (
        <div key={i} className="flex items-center gap-4 border-b border-border px-4 py-4 last:border-b-0">
          <div className="h-4 w-40 animate-pulse rounded bg-bg-subtle" />
          <div className="h-4 w-20 animate-pulse rounded bg-bg-subtle" />
          <div className="h-4 w-16 animate-pulse rounded bg-bg-subtle" />
          <div className="ml-auto h-8 w-40 animate-pulse rounded bg-bg-subtle" />
        </div>
      ))}
    </div>
  );
}

export default function SourcesPage() {
  const { me } = useAuth();
  const { current } = useTenant();
  const tenantId = current?.id;
  const csrf = me?.csrf_token;
  const qc = useQueryClient();

  const [notice, setNotice] = useState<{ tone: "ok" | "error"; text: string } | null>(null);
  const { data, isLoading, isError, error } = useSources();

  function invalidate() {
    void qc.invalidateQueries({ queryKey: ["sources", tenantId] });
  }

  const sync = useMutation({
    mutationFn: (s: Source) => syncSource(tenantId as string, s.id, csrf, false),
    onSuccess: (_r, s) => {
      setNotice({ tone: "ok", text: `Sync started for "${s.name}".` });
      invalidate();
    },
    onError: (e: Error) => setNotice({ tone: "error", text: e.message }),
  });

  const test = useMutation({
    mutationFn: (s: Source) => testSource(tenantId as string, s.id, csrf),
    onSuccess: (_r, s) => setNotice({ tone: "ok", text: `Connection test passed for "${s.name}".` }),
    onError: (e: Error) => setNotice({ tone: "error", text: e.message }),
  });

  const del = useMutation({
    mutationFn: (s: Source) => deleteSource(tenantId as string, s.id, csrf),
    onSuccess: (_r, s) => {
      setNotice({ tone: "ok", text: `Removal started for "${s.name}".` });
      invalidate();
    },
    onError: (e: Error) => setNotice({ tone: "error", text: e.message }),
  });

  const busyId: string | null =
    (sync.isPending ? sync.variables?.id : undefined) ??
    (test.isPending ? test.variables?.id : undefined) ??
    (del.isPending ? del.variables?.id : undefined) ??
    null;

  return (
    <div className="flex flex-col gap-1">
      <p className="font-mono text-xs uppercase tracking-wide text-fg-muted">/admin/sources</p>

      <div className="flex items-center justify-between gap-4">
        <h1 className="text-lg font-semibold tracking-tight text-fg">Sources</h1>
        <Link
          href="/admin/sources/new"
          className="inline-flex h-9 items-center rounded-md bg-accent px-3 text-sm font-medium text-accent-fg transition-colors hover:bg-accent-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg"
        >
          New source
        </Link>
      </div>

      {notice ? (
        <p
          className={`mt-4 rounded-md border px-3 py-2 text-sm ${
            notice.tone === "error"
              ? "border-border bg-danger-tint text-danger-text"
              : "border-border bg-accent-tint text-accent-text"
          }`}
        >
          {notice.text}
        </p>
      ) : null}

      {isLoading ? (
        <LoadingSkeleton />
      ) : isError ? (
        <div className="mt-6 rounded-2xl border border-border bg-danger-tint px-4 py-4 text-sm text-danger-text">
          Could not load sources: {error instanceof Error ? error.message : "unknown error"}
        </div>
      ) : (
        <SourcesTable
          sources={data?.items ?? []}
          onSync={(s) => sync.mutate(s)}
          onTest={(s) => test.mutate(s)}
          onDelete={(s) => del.mutate(s)}
          busyId={busyId}
        />
      )}
    </div>
  );
}
