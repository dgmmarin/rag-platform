"use client";

import Link from "next/link";
import { useParams } from "next/navigation";
import { EvalReport } from "@/components/EvalReport";
import { useEvalReport } from "@/lib/eval";

export default function EvalReportPage() {
  const params = useParams<{ id: string }>();
  const id = params.id;
  const { data, isLoading, isError, error } = useEvalReport(id);

  return (
    <div className="flex flex-col gap-1">
      <p className="font-mono text-xs uppercase tracking-wide text-fg-muted">/admin/eval/{id}</p>

      <div className="flex items-center justify-between gap-4">
        <h1 className="text-lg font-semibold tracking-tight text-fg">Eval run</h1>
        <Link
          href="/admin/eval"
          className="text-sm font-medium text-fg-muted transition-colors hover:text-fg"
        >
          Back to runs
        </Link>
      </div>

      {isLoading ? (
        <div className="mt-6 h-40 animate-pulse rounded-2xl border border-border bg-bg-subtle" />
      ) : isError ? (
        <div className="mt-6 rounded-2xl border border-border bg-danger-tint px-4 py-4 text-sm text-danger-text">
          Could not load the eval run: {error instanceof Error ? error.message : "unknown error"}
        </div>
      ) : data ? (
        <EvalReport report={data} />
      ) : null}
    </div>
  );
}
