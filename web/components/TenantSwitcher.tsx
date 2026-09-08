"use client";

import type { ChangeEvent } from "react";
import { useTenant } from "@/lib/tenant";

export function TenantSwitcher() {
  const { current, setCurrent, tenants } = useTenant();

  function onChange(e: ChangeEvent<HTMLSelectElement>): void {
    const tenant = tenants.find((t) => t.id === e.target.value);
    if (tenant) setCurrent(tenant);
  }

  return (
    <select aria-label="Tenant" value={current?.id ?? ""} onChange={onChange} disabled={tenants.length === 0}>
      {tenants.map((t) => (
        <option key={t.id} value={t.id}>
          {t.name}
        </option>
      ))}
    </select>
  );
}
