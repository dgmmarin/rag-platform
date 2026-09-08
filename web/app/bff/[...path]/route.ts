import { RAGCTL_API_URL } from "@/lib/config";

type Ctx = { params: Promise<{ path: string[] }> };

// ALLOWED_PREFIXES scopes the BFF to the two surfaces ragctl exposes (SPEC-11
// §3): the versioned API (`/v1/*`) and the platform-admin API (`/admin/*`).
// Anything else (e.g. a probe for `/bff/healthz`) is rejected instead of
// blindly forwarded upstream.
const ALLOWED_PREFIXES = new Set(["v1", "admin"]);

async function proxy(req: Request, ctx: Ctx): Promise<Response> {
  const { path } = await ctx.params;
  if (!ALLOWED_PREFIXES.has(path[0])) {
    return new Response(null, { status: 404 });
  }
  const url = new URL(req.url);
  const target = `${RAGCTL_API_URL}/${path.join("/")}${url.search}`;
  const headers = new Headers(req.headers);
  headers.delete("host");
  const upstream = await fetch(target, {
    method: req.method,
    headers,
    body: ["GET", "HEAD"].includes(req.method) ? undefined : await req.arrayBuffer(),
    redirect: "manual",
  });
  const outHeaders = new Headers(upstream.headers);
  // `new Headers(upstream.headers)` already comma-joined multiple Set-Cookie values into one
  // header, which would corrupt them; getSetCookie() reads each one separately from `upstream`
  // (not from the already-merged outHeaders), so re-emit them individually below.
  const setCookies = upstream.headers.getSetCookie();
  if (setCookies.length > 0) {
    outHeaders.delete("set-cookie");
    for (const cookie of setCookies) {
      outHeaders.append("set-cookie", cookie.replace(/;\s*Domain=[^;]*/i, "")); // host-only on the Next origin
    }
  }
  return new Response(upstream.body, { status: upstream.status, headers: outHeaders });
}

export const GET = proxy;
export const POST = proxy;
export const PUT = proxy;
export const PATCH = proxy;
export const DELETE = proxy;
