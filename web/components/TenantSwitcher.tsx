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
    <select
      aria-label="Tenant"
      value={current?.id ?? ""}
      onChange={onChange}
      disabled={tenants.length === 0}
      className="h-9 rounded-md border border-border bg-bg-elevated px-2.5 text-sm text-fg transition-colors hover:border-border-strong focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg disabled:cursor-not-allowed disabled:opacity-60"
    >
      {tenants.map((t) => (
        <option key={t.id} value={t.id}>
          {t.name}
        </option>
      ))}
    </select>
  );
}
