import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { Unauthorized } from "@/lib/api";

const push = vi.fn();
vi.mock("next/navigation", () => ({ useRouter: () => ({ push }) }));

const login = vi.fn();
const { LoginFailed } = vi.hoisted(() => {
  class LoginFailed extends Error {
    status: number;
    constructor(status: number) {
      super(`login failed with status ${status}`);
      this.status = status;
    }
  }
  return { LoginFailed };
});
vi.mock("@/lib/auth", () => ({ useAuth: () => ({ login }), LoginFailed }));

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

  it("shows a lockout message when login rejects with a 429 LoginFailed", async () => {
    login.mockRejectedValue(new LoginFailed(429));
    render(<LoginPage />);

    fireEvent.change(screen.getByLabelText(/email/i), { target: { value: "a@b.com" } });
    fireEvent.change(screen.getByLabelText(/password/i), { target: { value: "wrong" } });
    fireEvent.click(screen.getByRole("button", { name: /sign in/i }));

    expect(await screen.findByText(/account temporarily locked/i)).toBeInTheDocument();
    expect(push).not.toHaveBeenCalled();
  });

  it("shows a generic error message for an unexpected (non-401, non-429) failure", async () => {
    login.mockRejectedValue(new LoginFailed(500));
    render(<LoginPage />);

    fireEvent.change(screen.getByLabelText(/email/i), { target: { value: "a@b.com" } });
    fireEvent.change(screen.getByLabelText(/password/i), { target: { value: "wrong" } });
    fireEvent.click(screen.getByRole("button", { name: /sign in/i }));

    expect(await screen.findByText(/something went wrong/i)).toBeInTheDocument();
    expect(push).not.toHaveBeenCalled();
  });

  // OIDC login is deferred (ISSUE-0058): the button is removed until the callback
  // 303-redirects the browser instead of returning JSON.
  it("does not render an OIDC login control", () => {
    render(<LoginPage />);
    expect(screen.queryByRole("link", { name: /oidc/i })).not.toBeInTheDocument();
  });
});
