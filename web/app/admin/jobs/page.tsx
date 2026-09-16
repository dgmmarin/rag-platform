"use client";

import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useAuth } from "@/lib/auth";
import { useTenant } from "@/lib/tenant";
import { JobsTable } from "@/components/JobsTable";
import { cancelJob, useJobs, type Job, type JobFilter, type JobStatus } from "@/lib/jobs";

// The status filter offers "all" plus each job status (SPEC-08).
const STATUS_OPTIONS: { value: "all" | JobStatus; label: string }[] = [
  { value: "all", label: "All statuses" },
  { value: "queued", label: "Queued" },
  { value: "running", label: "Running" },
  { value: "succeeded", label: "Succeeded" },
  { value: "failed", label: "Failed" },
  { value: "cancelled", label: "Cancelled" },
];

function LoadingSkeleton() {
  return (
    <div className="mt-6 overflow-hidden rounded-2xl border border-border">
      {[0, 1, 2].map((i) => (
        <div key={i} className="flex items-center gap-4 border-b border-border px-4 py-4 last:border-b-0">
          <div className="h-4 w-40 animate-pulse rounded bg-bg-subtle" />
          <div className="h-4 w-20 animate-pulse rounded bg-bg-subtle" />
          <div className="h-4 w-16 animate-pulse rounded bg-bg-subtle" />
          <div className="ml-auto h-8 w-24 animate-pulse rounded bg-bg-subtle" />
        </div>
      ))}
    </div>
  );
}

export default function JobsPage() {
  const { me } = useAuth();
  const { current } = useTenant();
  const tenantId = current?.id;
  const csrf = me?.csrf_token;
  const qc = useQueryClient();

  const [status, setStatus] = useState<"all" | JobStatus>("all");
  const filter: JobFilter = status === "all" ? {} : { status };

  const [notice, setNotice] = useState<{ tone: "ok" | "error"; text: string } | null>(null);
  const { data, isLoading, isError, error } = useJobs(filter);

  const cancel = useMutation({
    mutationFn: (j: Job) => cancelJob(tenantId as string, j.id, csrf),
    onSuccess: (updated) => {
      setNotice({
        tone: "ok",
        text:
          updated.status === "cancelled"
            ? "Job cancelled."
            : "Cancellation requested; the worker will stop the job.",
      });
      void qc.invalidateQueries({ queryKey: ["jobs", tenantId, filter] });
    },
    onError: (e: Error) => setNotice({ tone: "error", text: e.message }),
  });

  const busyId: string | null = (cancel.isPending ? cancel.variables?.id : undefined) ?? null;

  return (
    <div className="flex flex-col gap-1">
      <p className="font-mono text-xs uppercase tracking-wide text-fg-muted">/admin/jobs</p>

      <div className="flex items-center justify-between gap-4">
        <h1 className="text-lg font-semibold tracking-tight text-fg">Jobs</h1>
        <label className="flex items-center gap-2 text-sm text-fg-muted">
          <span>Status</span>
          <select
            value={status}
            onChange={(e) => setStatus(e.target.value as "all" | JobStatus)}
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
          Could not load jobs: {error instanceof Error ? error.message : "unknown error"}
        </div>
      ) : (
        <JobsTable jobs={data?.items ?? []} onCancel={(j) => cancel.mutate(j)} busyId={busyId} />
      )}
    </div>
  );
}
