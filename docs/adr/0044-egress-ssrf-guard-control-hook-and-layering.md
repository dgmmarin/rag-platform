# ADR-0044: Egress SSRF guard — a `net.Dialer.Control` hook, the `internal/egress` package, and how it layers with the crawler allowlist

**Status:** Accepted · **Date:** 2026-09-04 · **Requirements:** NFR-SEC-04, SPEC-09 §4, C-3 · **Decisions:** ADR-0003, ADR-0040, ADR-0043

## Context
STORY-07.1 (ADR-0043) shipped the web crawler with the egress `Doer` seam left
open: the default client used `http.DefaultTransport` and *permitted* loopback so
httptest-based tests could reach 127.0.0.1. STORY-07.2 must close it: resolve DNS
and refuse to connect to RFC1918, loopback, link-local and cloud-metadata
(169.254.169.254) addresses, re-validated on every redirect hop, with a 20 MB
response-size cap and a 30 s timeout (SPEC-09 §4, NFR-SEC-04). The AC additionally
calls out **DNS rebinding** and **a test per class**.

This is security-critical: a tenant-supplied crawl config is attacker-controlled
input, and the classic SSRF payload is a public-looking hostname that resolves (or
rebinds) to `169.254.169.254` to steal cloud credentials.

## Decisions

### 1. Enforce at `net.Dialer.Control`, not with a pre-resolve check (DNS-rebinding defence)
The check is a `Control func(network, address string, c syscall.RawConn) error` hook
on the dialer. `Control` runs **after** name resolution, on the concrete `IP:port`
the socket is about to connect to, and *before* the connection is established.

- A pre-resolve check ("resolve the host, inspect the IPs, then `http.Get` the
  hostname") is TOCTOU-vulnerable: the second resolution can return a different,
  private IP (DNS rebinding), and the guard validated an address the socket never
  used. Rejected.
- `Control` validates the exact address the socket will use, so whatever DNS
  returned — first time or after a rebind — is what gets classified. The rebinding
  window is structurally closed. Chosen.

Redirect re-validation falls out for free: each redirect hop opens a new connection
and therefore re-dials through the same `Control` hook, so a `302 → http://169.254.169.254/`
is blocked at connect on the redirect hop with no per-hop bookkeeping (the crawler's
redirect cap stays as the loop bound).

### 2. IP classification over stdlib `net.IP`, no dependency
`IsBlocked(ip)` uses `net.IP` predicates — `IsUnspecified`, `IsLoopback`,
`IsPrivate` (RFC1918 **and** IPv6 ULA `fc00::/7`), `IsLinkLocalUnicast` (169.254/16
including the metadata IP, and `fe80::/10`), the multicast predicates, and an
explicit `net.IPv4bcast` check — then requires `IsGlobalUnicast` as the final gate
so anything the named classes miss (documentation, benchmarking, reserved ranges)
fails closed. IPv4-mapped IPv6 (`::ffff:a.b.c.d`) is unwrapped with `To4()` first so
a private address cannot be smuggled through the mapped encoding; an explicit
`fc00::/7` bit-check backs up `IsPrivate` independent of the stdlib version. No new
dependency — `net` covers every class (reuse-first).

### 3. Its own package `internal/egress`, not a crawler-private func
The guard is placed in `internal/egress` (exported `IsBlocked`, `Control`,
`GuardedDialer`, `GuardedTransport`, `GuardedClient`, and the `ErrBlocked` sentinel /
`BlockedError` type), not buried unexported in `webcrawl`. Justification: STORY-07.5
(sitemap) and STORY-07.6 (HTTP API connector) fetch operator/tenant-supplied URLs
too and must apply the identical IP policy. A small shared package is the same size
as an unexported crawler helper but reusable; the webcrawl `defaultDoer` is now a
three-line call to `egress.GuardedClient(fetchTimeout, maxRedirects)`.

### 4. Two layers, deliberately separate: frontier allowlist vs. egress IP guard
SPEC-09 §4 says "allowlist enforced on scheme+host+path prefix". STORY-07.1 already
enforces that at the **frontier**: `allowed()` gates every discovered URL by
scheme+host+path-prefix (or the seed host when no allowlist is configured) and
`denied()` blocks deny patterns, so out-of-scope URLs are never queued. STORY-07.2
does **not** duplicate that logic. The egress guard is the layer *below* it: it
governs which resolved **IP** addresses may be dialed, so a host that is in-scope by
name but resolves/rebinds to an internal IP is still refused. The two compose as
defence-in-depth — the allowlist decides *which URLs*, the egress guard decides
*which IPs* — and only the scheme is (implicitly) re-touched by egress: the guarded
transport speaks HTTP(S) only, and the connector already rejects non-http(s) seeds
in `validateSemantics`.

### 5. Fail-closed default; tests inject a permissive Doer
`webcrawl.defaultDoer()` is now the guarded client, and the connector's `Sync` uses
it via the package-level `syncDoer`, so **any real binary blocks private ranges with
no composition-root wiring** — fail-closed is the correct security posture (a
cli-side opt-in that could be forgotten would be fail-open). Unit crawl tests bypass
the seam by constructing the crawler with `newCrawler(cfg, srv.Client())` directly;
the httptest e2e, which drives the real `Sync`, installs the server's permissive
client through the narrowly-named `SetEgressDoerForTest`. This realises ADR-0043's
"guarded transport … with tests overriding it with a permissive one" — the guard is
the connector's own default composition, so `internal/cli` needs no change (the
EPIC-09 worker, the real production caller of `Sync`, inherits the guard for free).

### 6. Size cap enforced in the read path (rejection, not truncation)
The 20 MB cap is enforced where the body is read (`crawl.go`): the reader takes
`io.LimitReader(body, cap+1)` and **rejects** a response whose length exceeds the
cap (recorded as a page error, not emitted), rather than silently truncating into a
half-parsed document. Capping the read also bounds memory against a hostile or
runaway response; the 30 s client timeout bounds slow-drip (slowloris) bodies. The
cap is a crawler field (`maxBytes`, default `maxResponseBytes`) so tests exercise it
without streaming 20 MB.

## Consequences
- No schema change (drift guard green); no route/response change (`api/openapi.yaml`
  unchanged). No new dependency.
- Production crawls are SSRF-guarded by default; the metadata IP, all RFC1918/ULA,
  loopback, link-local, IPv4-mapped-private and broadcast/multicast targets are
  refused at connect, on the initial request and on every redirect hop.
- `internal/egress` is the reuse point for STORY-07.5/07.6; adding a connector that
  fetches URLs means calling `egress.GuardedClient` / `egress.GuardedTransport`, no
  re-implementation (NFR-MNT-01).
- Trade-off: the guard blocks *all* private ranges unconditionally. A future
  deployment that legitimately needs to crawl an internal host would need an explicit
  allowance (the SPEC-09 §4 "egress proxy with the same deny rules" is the intended
  path); not needed for v1.
