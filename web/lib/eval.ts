import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { apiFetch } from "./api";
import { useTenant } from "./tenant";

// The types mirror the control-plane eval report contract (internal/eval/report.go
// + run.go Summary). The admin screen renders the `ragctl eval report` data: a runs
// list and a per-run drill-down. Read-only — the mutating eval surface (cases, run)
// stays on the CLI.
export type Summary = {
  cases: number;
  k: number;
  recall_at_k: number;
  cases_scored_for_recall: number;
  grounded_rate: number;
  mean_latency_ms: number;
  errors: number;
  cases_judged?: number;
  correctness_rate?: number;
};

export type RunView = {
  id: string;
  config: unknown;
  started_at: string;
  finished_at?: string;
  summary?: Summary;
};

export type ResultView = {
  case_id: string;
  question?: string;
  expected_answer?: string;
  retrieved_doc_ids: string[];
  recall_hit: boolean | null;
  judged_correct: boolean | null;
  answer?: string;
  latency_ms: number;
};

export type Report = { run: RunView; results: ResultView[] };
export type RunListPage = { items: RunView[] };

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
  return `/admin/tenants/${tenantId}/eval/runs`;
}

export async function listEvalRuns(tenantId: string): Promise<RunListPage> {
  const res = await ensureOk(await apiFetch(base(tenantId)));
  return (await res.json()) as RunListPage;
}

export async function getEvalReport(tenantId: string, id: string): Promise<Report> {
  const res = await ensureOk(await apiFetch(`${base(tenantId)}/${id}`));
  return (await res.json()) as Report;
}

// useEvalRuns loads the current tenant's runs, keyed by tenant; disabled until a
// tenant is known.
export function useEvalRuns(): UseQueryResult<RunListPage, Error> {
  const tenantId = useTenant().current?.id;
  return useQuery({
    queryKey: ["eval-runs", tenantId],
    queryFn: () => listEvalRuns(tenantId as string),
    enabled: !!tenantId,
  });
}

// useEvalReport loads one run's full report, keyed by tenant and id.
export function useEvalReport(id: string): UseQueryResult<Report, Error> {
  const tenantId = useTenant().current?.id;
  return useQuery({
    queryKey: ["eval-report", tenantId, id],
    queryFn: () => getEvalReport(tenantId as string, id),
    enabled: !!tenantId && !!id,
  });
}

// pct renders a 0..1 rate as a whole-number percentage, or an em dash when absent.
export function pct(rate?: number): string {
  if (rate == null) return "—";
  return `${Math.round(rate * 100)}%`;
}
