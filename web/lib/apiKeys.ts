import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { apiFetch } from "./api";
import { useTenant } from "./tenant";

// ApiKey mirrors the session-admin api-keys JSON view (internal/cp/auth/
// apikey_handlers.go apiKeyView). Timestamps are RFC3339 strings. There is NO
// secret field here: the plaintext exists only once, in CreateKeyResult.key
// (FR-ACC-04, C-4) — it is never stored and never refetchable.
export type ApiKey = {
  id: string;
  name: string;
  prefix: string;
  scopes: string[];
  createdAt: string;
  expiresAt?: string;
  revokedAt?: string;
  lastUsedAt?: string;
};

// The scopes a key may hold (SPEC-07 §2). The form offers exactly these.
export const KEY_SCOPES = ["query", "ingest", "admin"] as const;
export type KeyScope = (typeof KEY_SCOPES)[number];

// CreateKeyInput is the POST body. `expires_at` is optional RFC3339.
export type CreateKeyInput = {
  name: string;
  scopes: string[];
  expires_at?: string;
};

// CreateKeyResult carries the plaintext secret ONCE alongside the stored record.
// The `key` is shown once and is never refetchable.
export type CreateKeyResult = { key: string; record: ApiKey };

// ensureOk turns any non-2xx into an Error carrying the envelope's
// error.message (SPEC-07), falling back to the status. apiFetch already throws
// Unauthorized on 401, so that never reaches here. Mirrors sources.ts.
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
  return `/admin/tenants/${tenantId}/api-keys`;
}

export async function listApiKeys(tenantId: string): Promise<ApiKey[]> {
  const res = await ensureOk(await apiFetch(base(tenantId)));
  return (await res.json()) as ApiKey[];
}

export async function createApiKey(
  tenantId: string,
  csrfToken: string | undefined,
  input: CreateKeyInput,
): Promise<CreateKeyResult> {
  const res = await ensureOk(
    await apiFetch(base(tenantId), {
      method: "POST",
      csrfToken,
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(input),
    }),
  );
  return (await res.json()) as CreateKeyResult;
}

export async function revokeApiKey(
  tenantId: string,
  id: string,
  csrfToken: string | undefined,
): Promise<void> {
  // 204 No Content on success; ensureOk maps 404 (not found) to an Error.
  await ensureOk(await apiFetch(`${base(tenantId)}/${id}`, { method: "DELETE", csrfToken }));
}

// useApiKeys loads the current tenant's keys. Keyed by tenant so switching
// tenants refetches; disabled until a tenant is known. Mirrors useSources.
export function useApiKeys(): UseQueryResult<ApiKey[], Error> {
  const tenantId = useTenant().current?.id;
  return useQuery({
    queryKey: ["api-keys", tenantId],
    queryFn: () => listApiKeys(tenantId as string),
    enabled: !!tenantId,
  });
}
