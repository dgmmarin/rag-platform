import { describe, it, expect, vi } from "vitest";
import { apiFetch, Unauthorized } from "./api";

describe("apiFetch", () => {
  it("prefixes /bff and attaches X-CSRF-Token on mutations", async () => {
    const spy = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response("{}", { status: 200 }));
    await apiFetch("/v1/x", { method: "POST", csrfToken: "tok" });
    expect(spy.mock.calls[0][0]).toBe("/bff/v1/x");
    expect(new Headers((spy.mock.calls[0][1] as RequestInit).headers).get("X-CSRF-Token")).toBe("tok");
  });

  it("throws Unauthorized on 401", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response("", { status: 401 }));
    await expect(apiFetch("/v1/auth/me")).rejects.toBeInstanceOf(Unauthorized);
  });
});
