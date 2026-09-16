import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { apiFetch } from "./api";
import { useTenant } from "./tenant";

// Role mirrors the control-plane role enum (internal/cp/auth ParseRole): the
// four tenant roles the admin UI can grant.
export type Role = "owner" | "admin" | "editor" | "viewer";

export const ROLES: Role[] = ["owner", "admin", "editor", "viewer"];

// Member mirrors the memberView JSON returned by the members handlers
// (internal/cp/auth/members_handlers.go). `userId` is the control-plane user id;
// `email` identifies the person; `role` is their role in the current tenant.
export type Member = {
  userId: string;
  email: string;
  role: Role;
};

// AddMemberInput is the POST body: an existing user's email plus the role to
// grant. There is no invite flow in this story (ISSUE-0064) — the user must
// already have signed up.
export type AddMemberInput = {
  email: string;
  role: Role;
};

// ensureOk turns any non-2xx into an Error carrying the envelope's
// error.message (SPEC-07 {error:{code,message}}), falling back to the status.
// apiFetch already throws Unauthorized on 401, so that never reaches here.
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
  return `/admin/tenants/${tenantId}/members`;
}

export async function listMembers(tenantId: string): Promise<Member[]> {
  const res = await ensureOk(await apiFetch(base(tenantId)));
  return (await res.json()) as Member[];
}

export async function addMember(
  tenantId: string,
  csrfToken: string | undefined,
  input: AddMemberInput,
): Promise<Member> {
  const res = await ensureOk(
    await apiFetch(base(tenantId), {
      method: "POST",
      csrfToken,
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(input),
    }),
  );
  return (await res.json()) as Member;
}

export async function setMemberRole(
  tenantId: string,
  userId: string,
  csrfToken: string | undefined,
  role: Role,
): Promise<Member> {
  const res = await ensureOk(
    await apiFetch(`${base(tenantId)}/${userId}`, {
      method: "PATCH",
      csrfToken,
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ role }),
    }),
  );
  return (await res.json()) as Member;
}

export async function removeMember(
  tenantId: string,
  userId: string,
  csrfToken: string | undefined,
): Promise<void> {
  await ensureOk(
    await apiFetch(`${base(tenantId)}/${userId}`, { method: "DELETE", csrfToken }),
  );
}

// useMembers loads the current tenant's roster. The query is keyed by tenant so
// switching tenants refetches, and stays disabled until a tenant is known.
export function useMembers(): UseQueryResult<Member[], Error> {
  const tenantId = useTenant().current?.id;
  return useQuery({
    queryKey: ["members", tenantId],
    queryFn: () => listMembers(tenantId as string),
    enabled: !!tenantId,
  });
}
