import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";

import { CreateKeyDialog } from "./CreateKeyDialog";
import type { CreateKeyResult } from "@/lib/apiKeys";

function result(over: Partial<CreateKeyResult> = {}): CreateKeyResult {
  return {
    key: "rp_live_secret_plaintext_value",
    record: {
      id: "k9",
      name: "New key",
      prefix: "rp_live",
      scopes: ["query"],
      createdAt: "2026-09-16T00:00:00Z",
    },
    ...over,
  };
}

beforeEach(() => {
  vi.restoreAllMocks();
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("CreateKeyDialog", () => {
  it("blocks submit and flags the error when the name is empty", async () => {
    const onCreate = vi.fn();
    render(<CreateKeyDialog onCreate={onCreate} />);

    fireEvent.click(screen.getByLabelText(/query/i));
    fireEvent.click(screen.getByRole("button", { name: /create key/i }));

    expect(await screen.findByText(/name is required/i)).toBeInTheDocument();
    expect(onCreate).not.toHaveBeenCalled();
  });

  it("blocks submit when no scope is selected", async () => {
    const onCreate = vi.fn();
    render(<CreateKeyDialog onCreate={onCreate} />);

    fireEvent.change(screen.getByLabelText(/^name$/i), { target: { value: "CI" } });
    fireEvent.click(screen.getByRole("button", { name: /create key/i }));

    expect(await screen.findByText(/at least one scope/i)).toBeInTheDocument();
    expect(onCreate).not.toHaveBeenCalled();
  });

  it("submits name, checked scopes and an RFC3339 expiry, then reveals the secret once", async () => {
    const onCreate = vi.fn().mockResolvedValue(result());
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", {
      value: { writeText },
      configurable: true,
    });

    render(<CreateKeyDialog onCreate={onCreate} />);

    fireEvent.change(screen.getByLabelText(/^name$/i), { target: { value: "CI pipeline" } });
    fireEvent.click(screen.getByLabelText(/query/i));
    fireEvent.click(screen.getByLabelText(/ingest/i));
    fireEvent.change(screen.getByLabelText(/exp/i), { target: { value: "2026-12-31" } });

    fireEvent.click(screen.getByRole("button", { name: /create key/i }));

    await waitFor(() => expect(onCreate).toHaveBeenCalledTimes(1));
    const arg = onCreate.mock.calls[0][0];
    expect(arg.name).toBe("CI pipeline");
    expect(arg.scopes).toEqual(["query", "ingest"]);
    // the date input value is converted to an RFC3339 timestamp
    expect(arg.expires_at).toBe(new Date("2026-12-31").toISOString());

    // one-time reveal: the plaintext secret and the "won't see again" warning
    expect(await screen.findByText("rp_live_secret_plaintext_value")).toBeInTheDocument();
    expect(screen.getByText(/again/i)).toBeInTheDocument();
    // the create form is gone (the secret is not refetchable, so no re-submit)
    expect(screen.queryByRole("button", { name: /create key/i })).not.toBeInTheDocument();

    // copy-to-clipboard writes the plaintext secret
    fireEvent.click(screen.getByRole("button", { name: /copy/i }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith("rp_live_secret_plaintext_value"));
  });

  it("omits expires_at when no expiry is set", async () => {
    const onCreate = vi.fn().mockResolvedValue(result());
    render(<CreateKeyDialog onCreate={onCreate} />);

    fireEvent.change(screen.getByLabelText(/^name$/i), { target: { value: "No expiry" } });
    fireEvent.click(screen.getByLabelText(/admin/i));
    fireEvent.click(screen.getByRole("button", { name: /create key/i }));

    await waitFor(() => expect(onCreate).toHaveBeenCalledTimes(1));
    expect(onCreate.mock.calls[0][0].expires_at).toBeUndefined();
  });

  it("surfaces a create error and keeps the form (no reveal)", async () => {
    const onCreate = vi.fn().mockRejectedValue(new Error("invalid scopes"));
    render(<CreateKeyDialog onCreate={onCreate} />);

    fireEvent.change(screen.getByLabelText(/^name$/i), { target: { value: "Bad" } });
    fireEvent.click(screen.getByLabelText(/query/i));
    fireEvent.click(screen.getByRole("button", { name: /create key/i }));

    expect(await screen.findByText(/invalid scopes/i)).toBeInTheDocument();
    // still on the form
    expect(screen.getByRole("button", { name: /create key/i })).toBeInTheDocument();
  });

  it("does not crash when the clipboard API is unavailable", async () => {
    const onCreate = vi.fn().mockResolvedValue(result());
    Object.defineProperty(navigator, "clipboard", { value: undefined, configurable: true });

    render(<CreateKeyDialog onCreate={onCreate} />);
    fireEvent.change(screen.getByLabelText(/^name$/i), { target: { value: "CI" } });
    fireEvent.click(screen.getByLabelText(/query/i));
    fireEvent.click(screen.getByRole("button", { name: /create key/i }));

    expect(await screen.findByText("rp_live_secret_plaintext_value")).toBeInTheDocument();
    // clicking copy is a no-op, not a throw
    fireEvent.click(screen.getByRole("button", { name: /copy/i }));
  });
});
