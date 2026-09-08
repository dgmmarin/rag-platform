import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { Unauthorized } from "@/lib/api";

const push = vi.fn();
vi.mock("next/navigation", () => ({ useRouter: () => ({ push }) }));

const login = vi.fn();
vi.mock("@/lib/auth", () => ({ useAuth: () => ({ login }) }));

import LoginPage from "./page";

beforeEach(() => {
  push.mockClear();
  login.mockReset();
});

describe("LoginPage", () => {
  it("submits the typed email/password to login and redirects to /admin", async () => {
    login.mockResolvedValue(undefined);
    render(<LoginPage />);

    fireEvent.change(screen.getByLabelText(/email/i), { target: { value: "a@b.com" } });
    fireEvent.change(screen.getByLabelText(/password/i), { target: { value: "hunter2" } });
    fireEvent.click(screen.getByRole("button", { name: /sign in/i }));

    await waitFor(() => expect(login).toHaveBeenCalledWith("a@b.com", "hunter2"));
    await waitFor(() => expect(push).toHaveBeenCalledWith("/admin"));
  });

  it("shows an error message when login rejects with Unauthorized", async () => {
    login.mockRejectedValue(new Unauthorized());
    render(<LoginPage />);

    fireEvent.change(screen.getByLabelText(/email/i), { target: { value: "a@b.com" } });
    fireEvent.change(screen.getByLabelText(/password/i), { target: { value: "wrong" } });
    fireEvent.click(screen.getByRole("button", { name: /sign in/i }));

    expect(await screen.findByText(/invalid (email or password|credentials)/i)).toBeInTheDocument();
    expect(push).not.toHaveBeenCalled();
  });

  it("links the OIDC control to the BFF-proxied start endpoint", () => {
    render(<LoginPage />);
    expect(screen.getByRole("link", { name: /sign in with oidc/i })).toHaveAttribute(
      "href",
      "/bff/v1/auth/oidc/start",
    );
  });
});
