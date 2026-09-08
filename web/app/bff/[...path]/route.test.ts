import { describe, it, expect, vi, beforeEach } from "vitest";
import { GET } from "./route";

beforeEach(() => vi.restoreAllMocks());

describe("BFF proxy", () => {
  it("forwards cookie + X-CSRF-Token upstream and rewrites Set-Cookie domain", async () => {
    const upstream = vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response("{\"ok\":true}", {
        status: 200,
        headers: { "set-cookie": "rag_session=abc; Domain=api.internal; Path=/; HttpOnly" },
      }),
    );
    const req = new Request("http://ui.example/bff/v1/auth/me", {
      headers: { cookie: "rag_session=abc", "x-csrf-token": "tok" },
    });
    const res = await GET(req, { params: Promise.resolve({ path: ["v1", "auth", "me"] }) });
    const sentInit = upstream.mock.calls[0][1] as RequestInit;
    const sentHeaders = new Headers(sentInit.headers);
    expect(upstream.mock.calls[0][0]).toContain("/v1/auth/me");
    expect(sentHeaders.get("cookie")).toContain("rag_session=abc");
    expect(sentHeaders.get("x-csrf-token")).toBe("tok");
    expect(res.headers.get("set-cookie") ?? "").not.toContain("Domain=api.internal"); // domain stripped/rewritten
    expect(res.status).toBe(200);
  });

  it("passes through 401", async () => {
    vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response("", { status: 401 }));
    const res = await GET(new Request("http://ui.example/bff/v1/auth/me"), {
      params: Promise.resolve({ path: ["v1", "auth", "me"] }),
    });
    expect(res.status).toBe(401);
  });
});
