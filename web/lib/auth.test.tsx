import { it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { AuthProvider, useAuth } from "./auth";

beforeEach(() => vi.restoreAllMocks());

function Probe() {
  const { me, loading, logout } = useAuth();
  return (
    <div>
      <span data-testid="state">{loading ? "loading" : (me?.user.email ?? "none")}</span>
      <button onClick={() => void logout()}>logout</button>
    </div>
  );
}

// logout() must clear `me` even when the session already expired server-side
// (apiFetch throws Unauthorized on the logout call's 401) — it must not leave
// an unhandled rejection or skip resetting local auth state.
it("logout() clears me even when the logout request itself 401s (expired session)", async () => {
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input) => {
    const url = typeof input === "string" ? input : input.toString();
    if (url.includes("/auth/me")) {
      return new Response(
        JSON.stringify({
          user: { id: "1", email: "a@b.com" },
          is_platform_admin: false,
          memberships: [],
          csrf_token: "tok",
        }),
        { status: 200, headers: { "Content-Type": "application/json" } },
      );
    }
    if (url.includes("/auth/logout")) {
      return new Response("", { status: 401 });
    }
    return new Response("{}", { status: 200 });
  });

  render(
    <AuthProvider>
      <Probe />
    </AuthProvider>,
  );

  await waitFor(() => expect(screen.getByTestId("state")).toHaveTextContent("a@b.com"));

  fireEvent.click(screen.getByRole("button", { name: /logout/i }));

  await waitFor(() => expect(screen.getByTestId("state")).toHaveTextContent("none"));
});
