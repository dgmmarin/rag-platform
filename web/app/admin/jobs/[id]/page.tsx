"use client";

import Link from "next/link";
import { useParams } from "next/navigation";
import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useAuth } from "@/lib/auth";
import { useTenant } from "@/lib/tenant";
import { JobDetail } from "@/components/JobDetail";
import { cancelJob, useJob, type Job } from "@/lib/jobs";

export default function JobDetailPage() {
  const params = useParams<{ id: string }>();
  const id = params.id;
  const { me } = useAuth();
  const tenantId = useTenant().current?.id;
  const csrf = me?.csrf_token;
  const qc = useQueryClient();

  const [notice, setNotice] = useState<{ tone: "ok" | "error"; text: string } | null>(null);
  const { data, isLoading, isError, error } = useJob(id);

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
      void qc.invalidateQueries({ queryKey: ["job", tenantId, id] });
    },
    onError: (e: Error) => setNotice({ tone: "error", text: e.message }),
  });

  return (
    <div className="flex flex-col gap-1">
      <p className="font-mono text-xs uppercase tracking-wide text-fg-muted">/admin/jobs/{id}</p>

      <div className="flex items-center justify-between gap-4">
        <h1 className="text-lg font-semibold tracking-tight text-fg">Job</h1>
        <Link
          href="/admin/jobs"
          className="text-sm font-medium text-fg-muted transition-colors hover:text-fg"
        >
          Back to jobs
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
        <div className="mt-6 h-40 animate-pulse rounded-2xl border border-border bg-bg-subtle" />
      ) : isError ? (
        <div className="mt-6 rounded-2xl border border-border bg-danger-tint px-4 py-4 text-sm text-danger-text">
          Could not load the job: {error instanceof Error ? error.message : "unknown error"}
        </div>
      ) : data ? (
        <JobDetail job={data} onCancel={(j) => cancel.mutate(j)} busy={cancel.isPending} />
      ) : null}
    </div>
  );
}
