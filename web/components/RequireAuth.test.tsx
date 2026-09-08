import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";

const replace = vi.fn();
vi.mock("next/navigation", () => ({ useRouter: () => ({ replace }) }));

const useAuth = vi.fn();
vi.mock("@/lib/auth", () => ({ useAuth: () => useAuth() }));

import { RequireAuth } from "./RequireAuth";

beforeEach(() => {
  replace.mockClear();
  useAuth.mockReset();
});

describe("RequireAuth", () => {
  it("redirects to /admin/login when me is null", () => {
    useAuth.mockReturnValue({ me: null, loading: false });
    render(
      <RequireAuth>
        <div>secret</div>
      </RequireAuth>,
    );
    expect(replace).toHaveBeenCalledWith("/admin/login");
    expect(screen.queryByText("secret")).not.toBeInTheDocument();
  });

  it("renders children when me is set", () => {
    useAuth.mockReturnValue({ me: { user: { id: "1", email: "a@b.com" } }, loading: false });
    render(
      <RequireAuth>
        <div>secret</div>
      </RequireAuth>,
    );
    expect(replace).not.toHaveBeenCalled();
    expect(screen.getByText("secret")).toBeInTheDocument();
  });
});
