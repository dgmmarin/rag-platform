import { describe, it, expect, vi, beforeEach } from "vitest";
import { GET, POST } from "./route";

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

  it("relays multiple Set-Cookie headers separately, each with Domain stripped", async () => {
    const upstreamHeaders = new Headers();
    upstreamHeaders.append("set-cookie", "rag_oidc_state=; Path=/; HttpOnly; Max-Age=0");
    upstreamHeaders.append("set-cookie", "rag_session=abc; Domain=api.internal; Path=/; HttpOnly");
    vi.spyOn(globalThis, "fetch").mockResolvedValue(
      new Response("{\"ok\":true}", { status: 200, headers: upstreamHeaders }),
    );
    const res = await GET(new Request("http://ui.example/bff/v1/auth/oidc/callback"), {
      params: Promise.resolve({ path: ["v1", "auth", "oidc", "callback"] }),
    });
    const cookies = res.headers.getSetCookie();
    expect(cookies).toHaveLength(2);
    expect(cookies.some((c) => c.startsWith("rag_oidc_state=") && c.includes("Max-Age=0"))).toBe(true);
    expect(cookies.some((c) => c.startsWith("rag_session=abc") && c.includes("Path=/") && c.includes("HttpOnly"))).toBe(true);
    for (const c of cookies) expect(c).not.toContain("Domain=api.internal");
  });

  it("rejects a disallowed path prefix with 404 instead of forwarding", async () => {
    const upstream = vi.spyOn(globalThis, "fetch");
    const res = await GET(new Request("http://ui.example/bff/healthz"), {
      params: Promise.resolve({ path: ["healthz"] }),
    });
    expect(res.status).toBe(404);
    expect(upstream).not.toHaveBeenCalled();
  });

  it("still proxies allowed v1/admin prefixes", async () => {
    const upstream = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response("{}", { status: 200 }));
    const v1 = await GET(new Request("http://ui.example/bff/v1/auth/me"), {
      params: Promise.resolve({ path: ["v1", "auth", "me"] }),
    });
    const admin = await GET(new Request("http://ui.example/bff/admin/tenants"), {
      params: Promise.resolve({ path: ["admin", "tenants"] }),
    });
    expect(v1.status).toBe(200);
    expect(admin.status).toBe(200);
    expect(upstream).toHaveBeenCalledTimes(2);
  });

  it("forwards the request body on a mutation", async () => {
    const upstream = vi.spyOn(globalThis, "fetch").mockResolvedValue(new Response("{}", { status: 200 }));
    const body = JSON.stringify({ hello: "world" });
    const req = new Request("http://ui.example/bff/v1/sources", { method: "POST", body });
    await POST(req, { params: Promise.resolve({ path: ["v1", "sources"] }) });
    const sentInit = upstream.mock.calls[0][1] as RequestInit;
    expect(new TextDecoder().decode(sentInit.body as ArrayBuffer)).toBe(body);
  });
});
