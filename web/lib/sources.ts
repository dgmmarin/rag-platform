import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { apiFetch } from "./api";
import { useTenant } from "./tenant";

// Source mirrors the control-plane JSON (internal/cp/sources/sources.go Source).
// `config` is an opaque per-kind object; the schema-driven form (Task 4) knows
// the shape for a given kind.
export type Source = {
  id: string;
  tenant_id: string;
  kind: string;
  name: string;
  status: string;
  config: Record<string, unknown>;
  schedule_cron?: string;
  next_run_at?: string;
  last_run_at?: string;
  last_success_at?: string;
  last_error?: string;
  created_at: string;
  updated_at: string;
};

export type SourceListPage = { items: Source[]; next_cursor?: string };

// ConnectorField / ConnectorKind describe the per-kind form schema served by
// GET /admin/connector-kinds. Task 4 renders these into inputs; the type is
// defined here so both slices share one source of truth.
export type ConnectorFieldType = "text" | "url" | "number" | "secret" | "bool";
export type ConnectorField = {
  name: string;
  label: string;
  type: ConnectorFieldType;
  required: boolean;
};
export type ConnectorKind = { kind: string; label: string; fields: ConnectorField[] };

// Input for create/edit. `schedule_cron` is optional; `config` is per-kind.
export type SourceInput = {
  kind: string;
  name: string;
  config: Record<string, unknown>;
  schedule_cron?: string;
};

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
  return `/admin/tenants/${tenantId}/sources`;
}

export async function listSources(
  tenantId: string,
  opts: { limit?: number; cursor?: string } = {},
): Promise<SourceListPage> {
  const q = new URLSearchParams();
  if (opts.limit != null) q.set("limit", String(opts.limit));
  if (opts.cursor) q.set("cursor", opts.cursor);
  const qs = q.toString();
  const res = await ensureOk(await apiFetch(`${base(tenantId)}${qs ? `?${qs}` : ""}`));
  return (await res.json()) as SourceListPage;
}

export async function getSource(tenantId: string, id: string): Promise<Source> {
  const res = await ensureOk(await apiFetch(`${base(tenantId)}/${id}`));
  return (await res.json()) as Source;
}

export async function createSource(
  tenantId: string,
  csrfToken: string | undefined,
  input: SourceInput,
): Promise<Source> {
  const res = await ensureOk(
    await apiFetch(base(tenantId), {
      method: "POST",
      csrfToken,
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(input),
    }),
  );
  return (await res.json()) as Source;
}

export async function updateSource(
  tenantId: string,
  id: string,
  csrfToken: string | undefined,
  input: Partial<SourceInput>,
): Promise<Source> {
  const res = await ensureOk(
    await apiFetch(`${base(tenantId)}/${id}`, {
      method: "PATCH",
      csrfToken,
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(input),
    }),
  );
  return (await res.json()) as Source;
}

export type DeleteResult = { status: string; job?: unknown };

export async function deleteSource(
  tenantId: string,
  id: string,
  csrfToken: string | undefined,
): Promise<DeleteResult> {
  const res = await ensureOk(
    await apiFetch(`${base(tenantId)}/${id}`, { method: "DELETE", csrfToken }),
  );
  return (await res.json()) as DeleteResult;
}

export async function syncSource(
  tenantId: string,
  id: string,
  csrfToken: string | undefined,
  full: boolean,
): Promise<unknown> {
  const res = await ensureOk(
    await apiFetch(`${base(tenantId)}/${id}/sync`, {
      method: "POST",
      csrfToken,
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ full }),
    }),
  );
  return (await res.json()) as unknown;
}

export async function testSource(
  tenantId: string,
  id: string,
  csrfToken: string | undefined,
): Promise<{ ok: boolean }> {
  const res = await ensureOk(
    await apiFetch(`${base(tenantId)}/${id}/test`, { method: "POST", csrfToken }),
  );
  return (await res.json()) as { ok: boolean };
}

export async function listConnectorKinds(): Promise<ConnectorKind[]> {
  const res = await ensureOk(await apiFetch("/admin/connector-kinds"));
  const body = (await res.json()) as { kinds: ConnectorKind[] };
  return body.kinds;
}

// useSources loads the current tenant's sources. The query is keyed by tenant
// so switching tenants refetches, and stays disabled until a tenant is known.
export function useSources(): UseQueryResult<SourceListPage, Error> {
  const tenantId = useTenant().current?.id;
  return useQuery({
    queryKey: ["sources", tenantId],
    queryFn: () => listSources(tenantId as string),
    enabled: !!tenantId,
  });
}
