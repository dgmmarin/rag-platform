import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";

const useAuthMock = vi.fn();
vi.mock("@/lib/auth", () => ({ useAuth: () => useAuthMock() }));

import { TenantProvider, useTenant } from "./tenant";

function Probe() {
  const { current, tenants } = useTenant();
  return (
    <ul>
      {tenants.map((t) => (
        <li key={t.id}>
          {t.id}:{t.name}
        </li>
      ))}
      <li data-testid="current">{current?.id ?? ""}</li>
    </ul>
  );
}

beforeEach(() => {
  useAuthMock.mockReset();
  localStorage.clear();
  vi.restoreAllMocks();
});

describe("TenantProvider", () => {
  it("merges platform-admin /admin/tenants into the membership list, de-duped by id", async () => {
    useAuthMock.mockReturnValue({
      me: {
        user: { id: "u1", email: "a@b.com" },
        is_platform_admin: true,
        memberships: [{ tenant_id: "t1", slug: "acme", name: "Acme", role: "admin" }],
        csrf_token: "tok",
      },
    });
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response(
        JSON.stringify({
          items: [
            { id: "t1", slug: "acme", name: "Acme (stale admin copy)" },
            { id: "t3", slug: "initech", name: "Initech" },
          ],
        }),
        { status: 200 },
      ),
    );

    render(
      <TenantProvider>
        <Probe />
      </TenantProvider>,
    );

    // t1 comes from the membership (kept, not overwritten by the admin list);
    // t3 is merged in from /admin/tenants; nothing is duplicated.
    await waitFor(() => expect(screen.getByText("t3:Initech")).toBeInTheDocument());
    expect(screen.getByText("t1:Acme")).toBeInTheDocument();
    expect(screen.queryByText(/stale admin copy/)).not.toBeInTheDocument();
  });

  it("initialises current from localStorage when the stored id is still in the list", () => {
    localStorage.setItem("adminui.tenant", "t2");
    useAuthMock.mockReturnValue({
      me: {
        user: { id: "u1", email: "a@b.com" },
        is_platform_admin: false,
        memberships: [
          { tenant_id: "t1", slug: "acme", name: "Acme", role: "admin" },
          { tenant_id: "t2", slug: "globex", name: "Globex", role: "member" },
        ],
        csrf_token: "tok",
      },
    });

    render(
      <TenantProvider>
        <Probe />
      </TenantProvider>,
    );

    expect(screen.getByTestId("current").textContent).toBe("t2");
  });
});
