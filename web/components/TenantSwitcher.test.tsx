import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";

const useAuthMock = vi.fn();
vi.mock("@/lib/auth", () => ({ useAuth: () => useAuthMock() }));

import { TenantProvider, useTenant } from "@/lib/tenant";
import { TenantSwitcher } from "./TenantSwitcher";

function CurrentTenantId() {
  const { current } = useTenant();
  return <span data-testid="current-id">{current?.id ?? ""}</span>;
}

beforeEach(() => {
  useAuthMock.mockReset();
  localStorage.clear();
});

describe("TenantSwitcher", () => {
  it("renders memberships; selecting one writes localStorage['adminui.tenant'] and updates useTenant().current", () => {
    useAuthMock.mockReturnValue({
      me: {
        user: { id: "u1", email: "a@b.com" },
        is_platform_admin: false,
        memberships: [
          { tenant_id: "t2", slug: "globex", name: "Globex", role: "member" },
          { tenant_id: "t1", slug: "acme", name: "Acme", role: "admin" },
        ],
        csrf_token: "tok",
      },
    });

    render(
      <TenantProvider>
        <TenantSwitcher />
        <CurrentTenantId />
      </TenantProvider>,
    );

    expect(screen.getByText("Acme")).toBeInTheDocument();
    // defaults to the first membership until a selection or a stored value says otherwise
    expect(screen.getByTestId("current-id").textContent).toBe("t2");

    fireEvent.change(screen.getByRole("combobox"), { target: { value: "t1" } });

    expect(localStorage.getItem("adminui.tenant")).toBe("t1");
    expect(screen.getByTestId("current-id").textContent).toBe("t1");
  });
});
