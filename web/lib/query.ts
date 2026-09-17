import { apiFetch } from "./api";

// The types mirror the control-plane JSON (internal/answer/answer.go Result and
// Citation). The playground calls the non-streaming variant (stream:false) and
// renders the answer, its citations and a grounded flag once complete. SSE is a
// later progressive enhancement — the JSON path yields the identical Result.
export type Citation = {
  n: number;
  document_id: string;
  title: string;
  uri: string;
  heading_path: string[];
  snippet: string;
};

export type Usage = {
  retrieval_ms: number;
  generation_ms: number;
  in_tokens: number;
  out_tokens: number;
};

export type QueryResult = {
  id: string;
  answer: string;
  grounded: boolean;
  citations: Citation[];
  usage: Usage;
  model: string;
};

// ensureOk turns any non-2xx into an Error carrying the envelope's error.message
// (falling back to the status). apiFetch already throws Unauthorized on 401.
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

// runQuery asks one grounded question against the current tenant. top_k is left to
// the server default unless given.
export async function runQuery(
  tenantId: string,
  question: string,
  csrfToken: string | undefined,
  opts: { topK?: number } = {},
): Promise<QueryResult> {
  const body: Record<string, unknown> = { question, stream: false };
  if (opts.topK != null) body.top_k = opts.topK;
  const res = await ensureOk(
    await apiFetch(`/admin/tenants/${tenantId}/query`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
      csrfToken,
    }),
  );
  return (await res.json()) as QueryResult;
}

// sendFeedback records a thumbs rating (1 up, -1 down) on an answered query.
export async function sendFeedback(
  tenantId: string,
  queryId: string,
  rating: 1 | -1,
  csrfToken: string | undefined,
  comment?: string,
): Promise<void> {
  const body: Record<string, unknown> = { query_id: queryId, rating };
  if (comment) body.comment = comment;
  await ensureOk(
    await apiFetch(`/admin/tenants/${tenantId}/feedback`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
      csrfToken,
    }),
  );
}
