export class Unauthorized extends Error {}

type Opts = RequestInit & { csrfToken?: string };

export async function apiFetch(path: string, opts: Opts = {}): Promise<Response> {
  const { csrfToken, headers, ...rest } = opts;
  const h = new Headers(headers);
  const mutating = !!rest.method && !["GET", "HEAD", "OPTIONS"].includes(rest.method.toUpperCase());
  if (mutating && csrfToken) h.set("X-CSRF-Token", csrfToken);
  const res = await fetch(`/bff${path}`, { ...rest, headers: h, credentials: "same-origin" });
  if (res.status === 401) throw new Unauthorized();
  return res;
}
