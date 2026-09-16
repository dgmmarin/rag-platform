import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { apiFetch } from "./api";
import { useTenant } from "./tenant";

// Job mirrors the control-plane JSON (internal/cp/jobs/jobs.go Job). The `jobs`
// table is the mirrored history view (ADR-0005), not the River queue. `stats`
// is an opaque per-kind object; the detail view renders known numeric keys and
// falls back to generic rendering so new keys are never dropped.
export type JobStatus = "queued" | "running" | "succeeded" | "failed" | "cancelled";

export type Job = {
  id: string;
  tenant_id: string;
  source_id?: string;
  kind: string;
  status: JobStatus;
  attempt: number;
  max_attempts: number;
  stats?: Record<string, unknown>;
  error?: string;
  queued_at: string;
  started_at?: string;
  finished_at?: string;
  worker_id?: string;
  duration_ms?: number;
};

export type JobListPage = { items: Job[]; next_cursor?: string };

// JobFilter carries the optional list filters (SPEC-08). An empty filter lists
// the whole tenant, newest first.
export type JobFilter = {
  status?: JobStatus;
  kind?: string;
  source?: string;
};

// A queued or running job may be cancelled; a terminal job may not.
export function isCancellable(status: JobStatus): boolean {
  return status === "queued" || status === "running";
}

// ensureOk turns any non-2xx into an Error carrying the envelope's
// error.message (falling back to the status). apiFetch already throws
// Unauthorized on 401, so that never reaches here.
async function ensureOk(res: Response): Promise<Response> {
  if (res.ok) return res;
  let message = `request failed with status ${res.status}`;
  try {
    const body = (await res.json()) as { error?: { message?: string } };
    if (body?.error?.message) message = body.error.message;
  } catch {
    // non-JSON body — keep the status-based default
  }
  throw new Error(message);
}

function base(tenantId: string): string {
  return `/admin/tenants/${tenantId}/jobs`;
}

export async function listJobs(
  tenantId: string,
  filter: JobFilter = {},
  opts: { limit?: number; cursor?: string } = {},
): Promise<JobListPage> {
  const q = new URLSearchParams();
  if (filter.status) q.set("status", filter.status);
  if (filter.kind) q.set("kind", filter.kind);
  if (filter.source) q.set("source", filter.source);
  if (opts.limit != null) q.set("limit", String(opts.limit));
  if (opts.cursor) q.set("cursor", opts.cursor);
  const qs = q.toString();
  const res = await ensureOk(await apiFetch(`${base(tenantId)}${qs ? `?${qs}` : ""}`));
  return (await res.json()) as JobListPage;
}

export async function getJob(tenantId: string, id: string): Promise<Job> {
  const res = await ensureOk(await apiFetch(`${base(tenantId)}/${id}`));
  return (await res.json()) as Job;
}

// cancelJob requests cancellation. The API answers 200 (cancelled now) or 202
// (cancel requested, the worker finalises) for a cancellable job; both are 2xx
// and carry the Job. ensureOk surfaces 409 (not cancellable) / 404 as errors.
export async function cancelJob(
  tenantId: string,
  id: string,
  csrfToken: string | undefined,
): Promise<Job> {
  const res = await ensureOk(
    await apiFetch(`${base(tenantId)}/${id}/cancel`, { method: "POST", csrfToken }),
  );
  return (await res.json()) as Job;
}

// useJobs loads the current tenant's jobs for the given filter. The query is
// keyed by tenant and filter so switching tenant or filter refetches, and stays
// disabled until a tenant is known.
export function useJobs(filter: JobFilter = {}): UseQueryResult<JobListPage, Error> {
  const tenantId = useTenant().current?.id;
  return useQuery({
    queryKey: ["jobs", tenantId, filter],
    queryFn: () => listJobs(tenantId as string, filter),
    enabled: !!tenantId,
  });
}

// useJob loads one job by id, keyed by tenant and id.
export function useJob(id: string): UseQueryResult<Job, Error> {
  const tenantId = useTenant().current?.id;
  return useQuery({
    queryKey: ["job", tenantId, id],
    queryFn: () => getJob(tenantId as string, id),
    enabled: !!tenantId && !!id,
  });
}
