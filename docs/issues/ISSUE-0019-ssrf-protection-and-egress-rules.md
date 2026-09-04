# ISSUE-0019: SSRF protection and egress rules

**Type:** Feature (security) · **Status:** Done · **Story:** STORY-07.2 · **Traces:** NFR-SEC-04, SPEC-09 §4, ADR-0044

> Note: this repository tracks the *what* primarily in the delivery backlog
> (`docs/backlog/BACKLOG_STATUS.md` / `BACKLOG_TASKS.md`) and the *why* in ADRs
> (`docs/adr/`). This issue records STORY-07.2 for traceability; the backlog story
> remains the authoritative work item. STORY-07.2 advances EPIC-07 to 11/39.

## Summary
Close the crawler's egress `Doer` seam (left open by STORY-07.1) with a DNS-rebinding-
safe SSRF guard: resolve DNS and refuse to connect to private, loopback, link-local
and cloud-metadata (`169.254.169.254`) addresses, re-validated on every redirect hop,
plus the SPEC-09 §4 20 MB response cap and 30 s timeout.

## Scope
- **New `internal/egress` package** — the shared outbound-network guard, reusable by
  the sitemap (07.5) and HTTP API (07.6) connectors:
  - `IsBlocked(net.IP) bool`: classifies every forbidden class over stdlib `net.IP`
    (loopback, RFC1918, IPv6 ULA `fc00::/7`, link-local incl. the metadata IP,
    multicast, IPv4 broadcast, unspecified, IPv4-mapped unwrapped) and requires
    `IsGlobalUnicast` as the fail-closed final gate.
  - `Control(network, address, RawConn) error`: the `net.Dialer.Control` hook — runs
    after resolution on the concrete dialed IP, the TOCTOU/rebinding-safe point.
  - `GuardedDialer` / `GuardedTransport` / `GuardedClient`: the wired dialer,
    `http.DefaultTransport.Clone()` with the guarded `DialContext`, and the client
    with timeout + redirect cap.
  - `ErrBlocked` sentinel + `BlockedError{IP}` for `errors.Is`/`errors.As`.
- **`internal/connector/webcrawl`**: `defaultDoer()` now returns `egress.GuardedClient`
  (fail-closed default); `Sync` uses it via the `syncDoer` seam; `SetEgressDoerForTest`
  lets the httptest e2e inject a permissive Doer. The read path enforces the size cap
  by **rejecting** an over-cap body (crawler `maxBytes` field, default 20 MB).
- Not in scope: the SPEC-09 §4 egress *proxy* deployment option (a future ops
  concern); the frontier scheme+host+path allowlist (already shipped in STORY-07.1 —
  see ADR-0044 §4 for how the two layers combine).

## Resolution
- **DNS-rebinding defence.** Enforcement is a `net.Dialer.Control` hook, not a
  pre-resolve check: `Control` validates the exact `IP:port` the socket will use, so a
  hostname that resolves clean and then rebinds to a private IP is still blocked.
  Redirect re-validation is automatic — each hop re-dials through the same hook.
- **Layering.** The egress guard is the IP-layer control *below* STORY-07.1's
  frontier allowlist (scheme+host+path prefix); they compose as defence-in-depth and
  the crawler logic is untouched (only `defaultDoer`/the read path changed).
- **Fail-closed.** The guard is the connector's default, so production is safe with no
  composition-root wiring; `internal/cli` needs no change (ADR-0044 §5).
- **No new dependency** (stdlib `net`/`syscall`); **no migration / no OpenAPI change**
  (drift guards green).

## Verification
- TDD (tests watched red before implementation):
  - `internal/egress/egress_test.go`: `TestIsBlockedPerClass` — a table row per class
    (IPv4 loopback, RFC1918 ×3, link-local, **169.254.169.254 metadata**, unspecified,
    broadcast, multicast; IPv6 loopback/unspecified/link-local/`fc00::/7`/multicast;
    IPv4-mapped private/loopback/metadata; and public IPv4/IPv6 allowed) — the AC's
    "test per class". Plus `Control` rejects resolved private IPs and unparseable/
    hostname addresses, allows public; `GuardedDialer`/`GuardedClient` block loopback
    at connect; `TestRedirectToPrivateBlockedAtConnect` proves a `302 → 169.254.169.254`
    is blocked on the redirect hop via the shipped guard.
  - `internal/connector/webcrawl/egress_test.go`: `TestDefaultDoerIsSSRFGuarded`
    (production default is fail-closed) and `TestSizeCapRejectsOverCapBody` (an
    over-cap page is rejected, not emitted; an under-cap page still is).
- `go test ./internal/egress/... ./internal/connector/webcrawl/...`: green; coverage
  82.1% (egress) / 79.9% (webcrawl), both ≥70% gate.
- `go vet ./...` clean; `gofmt` clean. Schema-drift (`internal/migrate`,
  `internal/tenant`) and OpenAPI (`internal/api`) guards green.
- `internal/cli` unit reds are the documented `.env`-injection caveat (pass with a
  clean env: `env -i … go test ./internal/cli/...` → ok); nothing else regressed.
- e2e (`test/e2e/webcrawl_e2e_test.go`, `-tags e2e`) against the real stack still
  passes with the injected permissive Doer (resumability + `crawl_pages` unchanged):
  PASS in ~12 s. Assertions via pgxpool / httptest, never `docker compose exec`
  (ISSUE-0014).
