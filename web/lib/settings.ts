import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { apiFetch } from "./api";
import { useTenant } from "./tenant";

// Settings mirrors the SPEC-02 §5 tenant settings document served by
// GET /admin/tenants/{tenantId}/settings. GET/PATCH always operate on a
// complete, schema-valid document; PATCH accepts a partial (only the changed
// sections). embedding.dim is fixed at provisioning and can change only through
// a reindex (ADR-0022, SPEC-03 §5), so the form renders it read-only and never
// submits it.
export type Settings = {
  embedding: { provider: string; model: string; dim: number };
  llm: { provider: string; model: string; max_tokens: number; models_allowed: string[] };
  reranker: { enabled: boolean; provider: string; model: string; top_n: number };
  rewrite: { enabled: boolean };
  expansion: { mode: string; model?: string };
  chunking: { target_tokens: number; overlap_tokens: number };
  retrieval: { k_vector: number; k_text: number; final_k: number; min_score: number };
  answering: { token_budget: number; history_n: number };
  limits: { qps: number; max_upload_mb: number; max_pages_per_crawl: number };
  providers_allowed: string[];
};

// SettingsPatch is a partial settings document: only the changed sections are
// sent. embedding.dim is omitted from the embedding section because it is
// immutable. The API merges the patch into the current document and validates
// the merged result.
export type SettingsPatch = {
  embedding?: Partial<Pick<Settings["embedding"], "provider" | "model">>;
  llm?: Partial<Settings["llm"]>;
  reranker?: Partial<Settings["reranker"]>;
  rewrite?: Partial<Settings["rewrite"]>;
  expansion?: Partial<Settings["expansion"]>;
  chunking?: Partial<Settings["chunking"]>;
  retrieval?: Partial<Settings["retrieval"]>;
  answering?: Partial<Settings["answering"]>;
  limits?: Partial<Settings["limits"]>;
  providers_allowed?: string[];
};

// FieldError is one per-field validation failure. The settings endpoint uses a
// different error envelope from the sources API: a top-level `error` string and
// an optional `fields` list of dotted paths (e.g. "retrieval.min_score"). The
// wire keys are lowercase (`field`/`message`), matching the backend's json tags
// (internal/cp/tenants/settings_validate.go FieldError).
export type FieldError = { field: string; message: string };

// SettingsError carries the settings envelope: the top-level message and, when a
// validation (400) or immutable-field (409) failure occurred, the per-field
// list so the form can place each message next to its input.
export class SettingsError extends Error {
  fields?: FieldError[];
  constructor(message: string, fields?: FieldError[]) {
    super(message);
    this.name = "SettingsError";
    this.fields = fields;
  }
}

// ensureOk turns any non-2xx into a SettingsError. Unlike the sources client's
// ensureOk (which reads `error.message` from the `{error:{code,message}}`
// envelope), the settings endpoint returns `{ error: "<message>", fields: [...] }`,
// so this reads `error` as a string and lifts `fields`. apiFetch already throws
// Unauthorized on 401, so that never reaches here.
async function ensureOk(res: Response): Promise<Response> {
  if (res.ok) return res;
  let message = `request failed with status ${res.status}`;
  let fields: FieldError[] | undefined;
  try {
    const body = (await res.json()) as { error?: string; fields?: FieldError[] };
    if (body?.error) message = body.error;
    if (body?.fields && body.fields.length > 0) fields = body.fields;
  } catch {
    // non-JSON body — keep the status-based default
  }
  throw new SettingsError(message, fields);
}

function endpoint(tenantId: string): string {
  return `/admin/tenants/${tenantId}/settings`;
}

export async function getSettings(tenantId: string): Promise<Settings> {
  const res = await ensureOk(await apiFetch(endpoint(tenantId)));
  return (await res.json()) as Settings;
}

export async function updateSettings(
  tenantId: string,
  csrfToken: string | undefined,
  patch: SettingsPatch,
): Promise<Settings> {
  const res = await ensureOk(
    await apiFetch(endpoint(tenantId), {
      method: "PATCH",
      csrfToken,
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(patch),
    }),
  );
  return (await res.json()) as Settings;
}

// useSettings loads the current tenant's settings. The query is keyed by tenant
// so switching tenants refetches, and stays disabled until a tenant is known.
export function useSettings(): UseQueryResult<Settings, Error> {
  const tenantId = useTenant().current?.id;
  return useQuery({
    queryKey: ["settings", tenantId],
    queryFn: () => getSettings(tenantId as string),
    enabled: !!tenantId,
  });
}
