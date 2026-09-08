import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";

const replace = vi.fn();
const usePathnameMock = vi.fn();
vi.mock("next/navigation", () => ({
  useRouter: () => ({ replace }),
  usePathname: () => usePathnameMock(),
}));

const logout = vi.fn();
const useAuthMock = vi.fn();
vi.mock("@/lib/auth", () => ({ useAuth: () => useAuthMock() }));

import AdminLayout from "./layout";

const NAV_LINKS: Record<string, string> = {
  Sources: "/admin/sources",
  Jobs: "/admin/jobs",
  Documents: "/admin/documents",
  Members: "/admin/members",
  Settings: "/admin/settings",
  Query: "/admin/query",
  Eval: "/admin/eval",
};

beforeEach(() => {
  replace.mockClear();
  logout.mockReset();
  useAuthMock.mockReset();
  usePathnameMock.mockReset();
  localStorage.clear();
  usePathnameMock.mockReturnValue("/admin/sources");
  useAuthMock.mockReturnValue({
    me: {
      user: { id: "u1", email: "admin@example.com" },
      is_platform_admin: false,
      memberships: [{ tenant_id: "t1", slug: "acme", name: "Acme", role: "admin" }],
      csrf_token: "tok",
    },
    loading: false,
    logout,
  });
});

describe("AdminLayout", () => {
  it("renders the shell for an authed user: email, logout, tenant switcher, nav links, children", () => {
    render(
      <AdminLayout params={Promise.resolve({})}>
        <div>page content</div>
      </AdminLayout>,
    );

    expect(screen.getByText("admin@example.com")).toBeInTheDocument();
    expect(screen.getByRole("combobox")).toBeInTheDocument();
    expect(screen.getByText("Acme")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /log out/i }));
    expect(logout).toHaveBeenCalled();

    for (const [label, href] of Object.entries(NAV_LINKS)) {
      expect(screen.getByRole("link", { name: label })).toHaveAttribute("href", href);
    }

    expect(screen.getByText("page content")).toBeInTheDocument();
  });

  it("renders children bare (no shell/guard) at /admin/login", () => {
    usePathnameMock.mockReturnValue("/admin/login");
    useAuthMock.mockReturnValue({ me: null, loading: false, logout });

    render(
      <AdminLayout params={Promise.resolve({})}>
        <div>login form</div>
      </AdminLayout>,
    );

    expect(screen.getByText("login form")).toBeInTheDocument();
    expect(replace).not.toHaveBeenCalled();
    expect(screen.queryByRole("combobox")).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /log out/i })).not.toBeInTheDocument();
  });
});
