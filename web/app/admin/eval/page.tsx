"use client";

import { EvalRunsTable } from "@/components/EvalRunsTable";
import { useEvalRuns } from "@/lib/eval";

function LoadingSkeleton() {
  return (
    <div className="mt-6 overflow-hidden rounded-2xl border border-border">
      {[0, 1, 2].map((i) => (
        <div key={i} className="flex items-center gap-4 border-b border-border px-4 py-4 last:border-b-0">
          <div className="h-4 w-40 animate-pulse rounded bg-bg-subtle" />
          <div className="h-4 w-12 animate-pulse rounded bg-bg-subtle" />
          <div className="h-4 w-16 animate-pulse rounded bg-bg-subtle" />
          <div className="ml-auto h-8 w-16 animate-pulse rounded bg-bg-subtle" />
        </div>
      ))}
    </div>
  );
}

export default function EvalPage() {
  const { data, isLoading, isError, error } = useEvalRuns();

  return (
    <div className="flex flex-col gap-1">
      <p className="font-mono text-xs uppercase tracking-wide text-fg-muted">/admin/eval</p>
      <h1 className="text-lg font-semibold tracking-tight text-fg">Eval runs</h1>

      {isLoading ? (
        <LoadingSkeleton />
      ) : isError ? (
        <div className="mt-6 rounded-2xl border border-border bg-danger-tint px-4 py-4 text-sm text-danger-text">
          Could not load eval runs: {error instanceof Error ? error.message : "unknown error"}
        </div>
      ) : (
        <EvalRunsTable runs={data?.items ?? []} />
      )}
    </div>
  );
}
