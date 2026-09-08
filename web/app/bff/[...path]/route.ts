import { RAGCTL_API_URL } from "@/lib/config";

type Ctx = { params: Promise<{ path: string[] }> };

async function proxy(req: Request, ctx: Ctx): Promise<Response> {
  const { path } = await ctx.params;
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
  const setCookie = upstream.headers.get("set-cookie");
  if (setCookie) outHeaders.set("set-cookie", setCookie.replace(/;\s*Domain=[^;]*/i, "")); // host-only on the Next origin
  return new Response(upstream.body, { status: upstream.status, headers: outHeaders });
}

export const GET = proxy;
export const POST = proxy;
export const PUT = proxy;
export const PATCH = proxy;
export const DELETE = proxy;
