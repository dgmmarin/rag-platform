import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";

import { ApiKeysTable } from "./ApiKeysTable";
import type { ApiKey } from "@/lib/apiKeys";

function makeKey(over: Partial<ApiKey> = {}): ApiKey {
  return {
    id: "k1",
    name: "CI pipeline",
    prefix: "rp_ci123",
    scopes: ["query", "ingest"],
    createdAt: "2026-09-01T00:00:00Z",
    expiresAt: "2026-12-31T00:00:00Z",
    lastUsedAt: "2026-09-15T10:05:00Z",
    ...over,
  };
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe("ApiKeysTable", () => {
  it("renders a row per key with name, prefix, scopes, timestamps and status", () => {
    const keys = [
      makeKey({ id: "k1", name: "CI pipeline", prefix: "rp_ci123" }),
      makeKey({
        id: "k2",
        name: "Old key",
        prefix: "rp_old999",
        expiresAt: undefined,
        lastUsedAt: undefined,
        revokedAt: "2026-09-10T00:00:00Z",
      }),
    ];

    render(<ApiKeysTable keys={keys} onRevoke={vi.fn()} />);

    expect(screen.getByText("CI pipeline")).toBeInTheDocument();
    expect(screen.getByText("Old key")).toBeInTheDocument();
    expect(screen.getByText("rp_ci123")).toBeInTheDocument();
    expect(screen.getByText("rp_old999")).toBeInTheDocument();
    // scopes rendered (both keys carry the same two scopes)
    expect(screen.getAllByText("query").length).toBeGreaterThanOrEqual(1);
    expect(screen.getAllByText("ingest").length).toBeGreaterThanOrEqual(1);
    // status badges
    expect(screen.getByText("Active")).toBeInTheDocument();
    expect(screen.getByText("Revoked")).toBeInTheDocument();
    // absent expiry / last-used shown as an em dash
    expect(screen.getAllByText("—").length).toBeGreaterThanOrEqual(2);
  });

  it("shows an empty state and no rows when there are no keys", () => {
    render(<ApiKeysTable keys={[]} onRevoke={vi.fn()} />);

    expect(screen.getByText(/no api keys yet/i)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /revoke/i })).not.toBeInTheDocument();
  });

  it("offers a revoke action only for non-revoked keys and confirms before calling", () => {
    const onRevoke = vi.fn();
    vi.spyOn(window, "confirm").mockReturnValue(true);
    const active = makeKey({ id: "k1", name: "Active key" });
    const revoked = makeKey({ id: "k2", name: "Dead key", revokedAt: "2026-09-10T00:00:00Z" });

    render(<ApiKeysTable keys={[active, revoked]} onRevoke={onRevoke} />);

    // exactly one revoke button (the revoked row has none)
    const buttons = screen.getAllByRole("button", { name: /revoke/i });
    expect(buttons).toHaveLength(1);

    fireEvent.click(buttons[0]);
    expect(window.confirm).toHaveBeenCalled();
    expect(onRevoke).toHaveBeenCalledWith(active);
  });

  it("does not revoke when the confirmation is declined", () => {
    const onRevoke = vi.fn();
    vi.spyOn(window, "confirm").mockReturnValue(false);

    render(<ApiKeysTable keys={[makeKey()]} onRevoke={onRevoke} />);

    fireEvent.click(screen.getByRole("button", { name: /revoke/i }));
    expect(onRevoke).not.toHaveBeenCalled();
  });

  it("disables the revoke action for the busy key", () => {
    render(<ApiKeysTable keys={[makeKey({ id: "k1" })]} onRevoke={vi.fn()} busyId="k1" />);
    expect(screen.getByRole("button", { name: /revoke/i })).toBeDisabled();
  });
});
