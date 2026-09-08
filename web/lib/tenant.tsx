"use client";

import { createContext, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import { apiFetch } from "./api";
import { useAuth } from "./auth";

export type Tenant = { id: string; slug: string; name: string; role: string };

type TenantState = {
  current: Tenant | null;
  setCurrent: (tenant: Tenant) => void;
  tenants: Tenant[];
};

const TenantContext = createContext<TenantState | null>(null);

const STORAGE_KEY = "adminui.tenant";

// ponytail: localStorage can throw (private browsing, quota, or no `window` at
// all during SSR) — best-effort persistence only, a failure just means the
// pick doesn't survive a reload.
function readStoredTenantId(): string | null {
  try {
    return localStorage.getItem(STORAGE_KEY);
  } catch {
    return null;
  }
}

function writeStoredTenantId(id: string): void {
  try {
    localStorage.setItem(STORAGE_KEY, id);
  } catch {
    // see comment above
  }
}

export function TenantProvider({ children }: { children: ReactNode }) {
  const { me } = useAuth();
  const [adminTenants, setAdminTenants] = useState<Tenant[]>([]);
  const [explicitId, setExplicitId] = useState<string | null>(() => readStoredTenantId());

  const memberships: Tenant[] = useMemo(
    () => (me?.memberships ?? []).map((m) => ({ id: m.tenant_id, slug: m.slug, name: m.name, role: m.role })),
    [me],
  );

  const isPlatformAdmin = me?.is_platform_admin ?? false;

  useEffect(() => {
    if (!isPlatformAdmin) return;
    let cancelled = false;
    (async () => {
      const res = await apiFetch("/admin/tenants");
      // ponytail: first page only, no pagination — fine for the switcher's
      // tenant count today; upgrade to follow next_cursor if it grows large.
      const page = (await res.json()) as { items: { id: string; slug: string; name: string }[] };
      if (!cancelled) {
        setAdminTenants(page.items.map((t) => ({ id: t.id, slug: t.slug, name: t.name, role: "platform_admin" })));
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [isPlatformAdmin]);

  const tenants = useMemo(() => {
    const byId = new Map<string, Tenant>();
    for (const t of memberships) byId.set(t.id, t);
    for (const t of adminTenants) if (!byId.has(t.id)) byId.set(t.id, t);
    return [...byId.values()];
  }, [memberships, adminTenants]);

  // current = the explicit/stored pick if it's still a valid tenant, else the
  // first tenant in the list. Derived at render time (no effect needed) so it
  // stays correct as `tenants` loads in.
  const current = useMemo(() => {
    const picked = explicitId ? tenants.find((t) => t.id === explicitId) : undefined;
    return picked ?? tenants[0] ?? null;
  }, [tenants, explicitId]);

  function setCurrent(tenant: Tenant): void {
    setExplicitId(tenant.id);
    writeStoredTenantId(tenant.id);
  }

  return <TenantContext.Provider value={{ current, setCurrent, tenants }}>{children}</TenantContext.Provider>;
}

export function useTenant(): TenantState {
  const ctx = useContext(TenantContext);
  if (!ctx) throw new Error("useTenant must be used within TenantProvider");
  return ctx;
}
