import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { apiFetch } from "./api";

// The platform-admin tenants surface (STORY-11.7) is platform-global: it operates
// across all tenants via /admin/tenants (no {tenantId} segment), gated server-side
// by RequirePlatformAdmin. The types mirror internal/cp/tenants/admin.go Tenant.
export type TenantStatus = "provisioning" | "active" | "suspended" | "deleting" | "deleted";

export type PlatformTenant = {
  id: string;
  slug: string;
  name: string;
  status: TenantStatus;
  region: string;
  created_at: string;
  updated_at: string;
  deleted_at?: string;
  delete_after?: string;
};

export type TenantPage = { items: PlatformTenant[]; next_cursor?: string };

// CreateTenantInput is the enrol body (internal/cp/tenants admin_handlers Create):
// slug + name + region, and the embedding dimension the tenant's vector store is
// provisioned with.
export type CreateTenantInput = {
  slug: string;
  name: string;
  region: string;
  embedding_dim: number;
};

// CreateResult is the 201 body: the new tenant plus the provisioning job's id.
export type CreateResult = { tenant: PlatformTenant; job_id: string };

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

const BASE = "/admin/tenants";

export async function listTenants(): Promise<TenantPage> {
  const res = await ensureOk(await apiFetch(BASE));
  return (await res.json()) as TenantPage;
}

export async function createTenant(
  csrfToken: string | undefined,
  input: CreateTenantInput,
): Promise<CreateResult> {
  const res = await ensureOk(
    await apiFetch(BASE, {
      method: "POST",
      csrfToken,
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(input),
    }),
  );
  return (await res.json()) as CreateResult;
}

// setTenantStatus suspends ("suspended") or resumes ("active") a tenant via PATCH.
export async function setTenantStatus(
  id: string,
  csrfToken: string | undefined,
  status: "active" | "suspended",
): Promise<PlatformTenant> {
  const res = await ensureOk(
    await apiFetch(`${BASE}/${id}`, {
      method: "PATCH",
      csrfToken,
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ status }),
    }),
  );
  return (await res.json()) as PlatformTenant;
}

// deleteTenant schedules deletion (202, tenant moves to "deleting" with a grace
// window). The server applies its default grace unless overridden.
export async function deleteTenant(id: string, csrfToken: string | undefined): Promise<void> {
  await ensureOk(await apiFetch(`${BASE}/${id}`, { method: "DELETE", csrfToken }));
}

// useTenants loads the platform tenant list. ponytail: first page only, matching
// the tenant switcher (web/lib/tenant.tsx) — follow next_cursor if the list grows.
export function useTenants(): UseQueryResult<TenantPage, Error> {
  return useQuery({ queryKey: ["platform-tenants"], queryFn: listTenants });
}
