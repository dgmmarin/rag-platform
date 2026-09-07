# ADR-0056: Query endpoint with JSON and SSE streaming — the `internal/query` composition of retrieve+answer behind `POST /v1/query`, candidate-citations-first SSE, retrieval-only degradation, and the Queries counter

**Status:** Accepted · **Date:** 2026-09-07 · **Requirements:** FR-RET-06, SPEC-06 §6, SPEC-07 §2f, NFR-REL-04, FR-ACC-03, C-3, C-4 · **Decisions:** ADR-0007, ADR-0024, ADR-0051, ADR-0052, ADR-0053, ADR-0055

## Context
SPEC-06 §6 is the answering query endpoint (FR-RET-06): `POST /v1/query` wraps the
retrieval pipeline (STORY-08.1/08.3, `internal/retrieve`) and the answering stage
(STORY-08.5, `internal/answer`, ADR-0055) behind a single tenant-scoped route in
**two** response modes — a non-streaming JSON body and a streaming SSE event stream.
The AC: JSON and SSE modes; **citations emitted before text**; usage in the `done`
event. STORY-08.5 already builds the grounding gate, prompt assembly, `[n]` citation
mapping, usage folding and the `QueryLogger` seam behind `Service.Answer`; STORY-08.4
(`internal/llm`, ADR-0053) already offers a uniform streaming `Provider.Stream` pull
iterator (text deltas then a terminal `Done` with usage). This story is the wiring
and the two transports — it adds no ranking, prompt, or citation logic.

Constraints: the tenant is the authenticated principal, never a parameter
(FR-ACC-03); tenant content is reached only through the resolver + `*tenant.DB`
(ADR-0003, C-1, C-3); the tenant display name for the refusal message is
control-plane registry data (C-3); secrets/prompt content never logged (C-4);
loss of the LLM provider must degrade gracefully to retrieval-only (NFR-REL-04).

## Options / decisions
- **A new `internal/query` package — the composition root of retrieve + answer.**
  It is the only place that imports both `internal/retrieve` and `internal/answer`
  (neither imports it — no cycle). It orchestrates one query, renders the SPEC-06 §6
  JSON/SSE shapes, folds the `Queries` usage counter, and — for SSE — drives
  `llm.Provider.Stream` into the ordered events. Retrieval is injected as a small
  `Retriever` interface (`Search(ctx, tid, retrieve.Request)`) that `*retrieve.Service`
  satisfies structurally, so the query orchestration is unit-tested with a fake and
  no database, while the answering half runs the **real** `answer.Service` (hermetic,
  no DB) against a stub `llm.Provider`.
- **`Prepare` extracted from `answer.Answer` as the shared front half (ADR-0055
  additive refactor).** SSE cannot call `Answer` (which runs `Complete` internally),
  yet both modes MUST assemble the identical prompt and citation numbering. So
  `answer.Service.Prepare(ctx, Request) (Prepared, error)` now runs the grounding gate
  + budget + prompt assembly + provider build **without** generating, and `Answer`
  calls it then `Complete`. Behaviour is unchanged (the full 08.5 suite stays green);
  `Prepared` exposes the built `llm.Request`, the candidate citations, the grounding
  decision, and the provider. This keeps §4–5 semantics single-owned in `answer`.
- **Streaming citations resolved by approach (a): candidate citations up front
  (SPEC-06 §6 "retrieval (citations first)").** In streaming, the full answer text —
  and thus which `[n]` markers it uses — is not known when the first event must be
  sent. Rather than buffer the whole answer (approach (b)), the `retrieval` event
  emits the **candidate** citations: one per numbered context chunk (`N = 1..len`), in
  context order, with full `{n, document_id, title, uri, heading_path, snippet}`
  metadata. The client maps the streamed `[n]` markers into them; the numbering is
  stable because it is exactly the context order. This is `answer.candidateCitations`,
  reused for the degradation path so both stay consistent.
- **JSON keeps the post-hoc unreferenced-drop; the two modes are numbering-consistent.**
  JSON mode still returns only the referenced subset (`answer.mapCitations` drops the
  chunks no marker used). SSE returns all candidates. The **numbering is identical** —
  `n` = context position in both — so a `[3]` in a streamed answer and a `[3]` in a
  JSON answer point at the same chunk; SSE simply hands the client the full table up
  front and lets it drop the unused rows itself, whereas JSON drops them server-side
  after seeing the finished text. Documented on `Prepared.Citations`.
- **SSE event contract: `retrieval` → `delta`* → `done`.** `retrieval` carries
  `{citations}`; each `delta` carries `{text}` (one per provider text-delta); `done`
  carries `{id, grounded, model, usage{retrieval_ms, generation_ms, in_tokens,
  out_tokens}}` (AC: usage in `done`). Framing is standard `event:`/`data:` with a
  compact-JSON data line, flushed per event; headers set `text/event-stream`,
  `no-cache`, and `X-Accel-Buffering: no` so proxies do not hold tokens back.
- **Grounding refusal in both modes, no generation (SPEC-06 §4, FR-RET-05).** Below
  the floor, `Prepare` returns `Grounded=false` with the fixed message and **no
  provider built**. JSON returns the §6 refusal body; SSE emits `retrieval` (empty
  citations) + one `delta` (the fixed message) + `done` (zero LLM usage). No
  `Provider.Complete`/`Stream` call is made — proven by tests asserting the stub is
  untouched.
- **Graceful degradation on `llm.ErrCircuitOpen` (NFR-REL-04).** When the provider's
  circuit is open, the query does **not** hard-fail. It degrades to retrieval-only: a
  `200` with `grounded=true`, the candidate citations, a fixed "generation is
  temporarily unavailable" message, and retrieval-only usage (JSON); or `retrieval` +
  a single `delta` (that message) + `done{generation_unavailable:true}` (SSE). Only
  `ErrCircuitOpen` degrades; other generation failures are a generic `500` (C-4,
  never leaking provider internals). In SSE, a failure that happens *after* the
  retrieval event (status already committed) is surfaced as an `error` event, not a
  changed status.
- **Pre-stream failures stay JSON.** Retrieval (empty query → 400, unavailable tenant
  → 503, embedding failure → 500) and provider *build* failures happen before any SSE
  frame, so the handler writes a normal JSON error envelope; the `httpSink` only
  writes SSE headers on the first event, and the handler checks `sink.started`.
- **`Queries` counter incremented here, exactly once (ADR-0024).** 08.5 deliberately
  left the `Queries` count to 08.6 to avoid a double count: `answer` folds only LLM
  tokens (`LLMInTokens/LLMOutTokens`); the query service adds `Delta{Queries:1}` on
  every answered query (grounded, refusal, or degraded). For SSE, `answer` cannot fold
  the LLM tokens inline (it never runs `Complete`), so `Service.RecordStreamed` folds
  the terminal-event usage into `usage_daily` and logs the query — the streaming
  counterpart of the accounting `Answer` does inline.
- **Tenant name via a control-plane `NameService` (C-3).** The refusal message needs
  the tenant display name (`tenants.name`) — registry data, not tenant content — so a
  new `tenants.NameService` reads it over the control-plane pool (reusing the
  `SettingsDB` seam). A lookup failure is **non-fatal**: it only affects the refusal
  wording, so the service logs and falls back to a generic name (ponytail).
- **Query log is a nil no-op seam (STORY-08.8).** `answer.Service.Logger` is left nil;
  the async query-log + feedback persistence is 08.8. `RecordStreamed` and `Answer`
  both call the seam on every path, so 08.8 fills it with no query-side change.

## Decision
Add `internal/query`: `Service` (`Query` JSON, `QueryStream` SSE via an `EventSink`),
`Handlers` (`POST /v1/query`, `stream` flag selects the mode, `httpSink` flushes SSE),
the `Retriever`/`SettingsSource`/`TenantNameSource`/`UsageRecorder` seams, settings
parsing and `retrieve.Result → answer.Chunk` mapping. Add `answer.Prepare`/`Prepared`/
`candidateCitations`/`RecordStreamed` (behaviour-preserving extract of `Answer`). Add
`tenants.NameService`. Mount the route behind `RequireScopeQuery` + rate limit
(`internal/api` router), document it in the code-derived OpenAPI (`internal/api`
`liveRoutes`), regenerate `api/openapi.yaml`. Wire it in `internal/cli` (`ragctl
serve`) with the shared `llm.Factory` and the `usage.Counter`. SPEC-06 §6 and SPEC-07
§2f document the endpoint.

## Consequences
- **The pipeline is now reachable end-to-end over HTTP** — `POST /v1/query` is the
  first place retrieve+answer run together against the real stack; the golden-path e2e
  deferred by ADR-0053/0054/0055 lands here.
- **No new dependency.** Reuses `internal/retrieve`, `internal/answer`, `internal/llm`,
  `internal/cp/usage`, `internal/cp/tenants`, `net/http` SSE. No schema migration
  (the query log is 08.8; `settings.answering` already landed in 08.5).
- **OpenAPI extends, drift/contract stay green.** One `liveRoutes` row + regenerated
  YAML; the summary notes both response modes (the minimal code-derived document does
  not model request bodies or the SSE media type beyond the summary, matching the
  house pattern for `/v1/retrieve`).
- **Two modes, one truth.** JSON and SSE share `Prepare`, so the prompt, the grounding
  decision, and the citation numbering cannot diverge; only the citation *presentation*
  differs (post-hoc drop vs candidates-up-front), documented on `Prepared`.
- **Ceilings (ponytail):** (1) a tenant-name lookup failure falls back to a generic
  name rather than failing the query — wording only. (2) SSE degradation/refusal fold
  no `generation_ms`; retrieval-only usage is reported. (3) The query log is a nil
  no-op until 08.8. (4) The OpenAPI document describes the SSE stream in prose, not a
  modelled `text/event-stream` schema — consistent with ADR-0028's minimal generator.
- **Tests.** Unit (hermetic, no keys/network/DB): JSON returns the §6 shape with the
  referenced citation + usage; SSE emits `retrieval`→`delta`→`done` in order with the
  candidate citations before any text and usage in `done`; refusal in both modes makes
  **zero** provider calls; degradation on `ErrCircuitOpen` returns retrieval-only
  (JSON 200 / SSE `generation_unavailable`) instead of failing; the `Queries` counter
  is incremented exactly once and LLM tokens folded once; scope + no-tenant → 401,
  empty query → 400, unavailable tenant → 503, unknown body field → 400.
  `internal/query` 81%, `internal/answer` 90%. A DB-backed e2e
  (`test/e2e/query_endpoint_e2e_test.go`) drives the REAL router + resolver + hybrid
  SQL with a stubbed embedder and a stubbed LLM: JSON grounded answer + `[1]` citation
  to the seeded doc + usage, the full SSE order with citations-before-text, and a
  below-floor refusal carrying the tenant name and no generation.
