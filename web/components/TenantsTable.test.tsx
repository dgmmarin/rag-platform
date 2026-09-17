import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";

import { TenantsTable } from "./TenantsTable";
import type { PlatformTenant } from "@/lib/tenants";

function makeTenant(over: Partial<PlatformTenant> = {}): PlatformTenant {
  return {
    id: "t1",
    slug: "acme",
    name: "Acme Inc.",
    status: "active",
    region: "eu-west-1",
    created_at: "2026-09-15T10:00:00Z",
    updated_at: "2026-09-15T10:00:00Z",
    ...over,
  };
}

const noop = { onSuspend: vi.fn(), onActivate: vi.fn(), onDelete: vi.fn() };

describe("TenantsTable", () => {
  it("renders a row per tenant with name, slug, status and region", () => {
    const tenants = [
      makeTenant({ id: "t1", name: "Acme Inc.", slug: "acme", status: "active" }),
      makeTenant({ id: "t2", name: "Globex", slug: "globex", status: "suspended" }),
    ];

    render(<TenantsTable tenants={tenants} {...noop} />);

    expect(screen.getByText("Acme Inc.")).toBeInTheDocument();
    expect(screen.getByText("globex")).toBeInTheDocument();
    expect(screen.getByText("active")).toBeInTheDocument();
    expect(screen.getByText("suspended")).toBeInTheDocument();
    expect(screen.getAllByText("eu-west-1").length).toBe(2);
  });

  it("shows an empty state when there are no tenants", () => {
    render(<TenantsTable tenants={[]} {...noop} />);
    expect(screen.getByText(/no tenants yet/i)).toBeInTheDocument();
  });

  it("offers Suspend for an active tenant and Activate for a suspended one", () => {
    render(
      <TenantsTable
        tenants={[
          makeTenant({ id: "a", status: "active" }),
          makeTenant({ id: "s", status: "suspended" }),
        ]}
        {...noop}
      />,
    );
    expect(screen.getByRole("button", { name: /suspend/i })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /activate/i })).toBeInTheDocument();
  });

  it("hides suspend/activate/delete for a deleting tenant", () => {
    render(<TenantsTable tenants={[makeTenant({ status: "deleting" })]} {...noop} />);
    expect(screen.queryByRole("button", { name: /suspend|activate|delete/i })).not.toBeInTheDocument();
  });

  it("wires the suspend and delete callbacks", () => {
    const onSuspend = vi.fn();
    const onDelete = vi.fn();
    const t = makeTenant({ id: "t9", status: "active" });

    render(<TenantsTable tenants={[t]} onSuspend={onSuspend} onActivate={vi.fn()} onDelete={onDelete} />);

    fireEvent.click(screen.getByRole("button", { name: /suspend/i }));
    expect(onSuspend).toHaveBeenCalledWith(t);

    fireEvent.click(screen.getByRole("button", { name: /delete/i }));
    expect(onDelete).toHaveBeenCalledWith(t);
  });

  it("disables the busy row's controls", () => {
    render(<TenantsTable tenants={[makeTenant({ id: "t1", status: "active" })]} {...noop} busyId="t1" />);
    expect(screen.getByRole("button", { name: /suspend/i })).toBeDisabled();
    expect(screen.getByRole("button", { name: /delete/i })).toBeDisabled();
  });
});
