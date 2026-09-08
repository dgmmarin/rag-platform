# Runbook: Provider outage

**Traces:** NFR-REL-04, SPEC-06 §7, SPEC-09 §2. **Alert:** `ProviderErrorRateHigh`
([observability](observability.md#provider-errors)).

An external provider — LLM (answering / rerank), embedding, or reranker — is failing or
slow. The platform degrades gracefully rather than hard-failing:

- Each provider client has bounded retries + a **circuit breaker**; when it opens, calls
  short-circuit with `ErrCircuitOpen`.
- On generation unavailability the query returns a **retrieval-only** result
  (grounded, cited, no generated prose) instead of erroring (NFR-REL-04).
- A sustained embedding outage makes an ingest job **snooze** (not fail), so it resumes when
  the provider recovers (SPEC-05 §8).

## Triage

1. **Which provider and op?** The `provider_errors_total` / `provider_request_duration_seconds`
   metrics label by `provider` + `op` (`llm.complete` / `llm.stream` / `embed` / `rerank`).
   The providers dashboard shows the error rate and which plane (serve vs worker) is hit.
2. **Provider-side or us?** Check the provider's status page and whether errors are auth
   (key/quota) vs 5xx/timeouts. Auth/quota errors are ours to fix; 5xx/timeouts are theirs.
3. **Blast radius:** answering degrades to retrieval-only (users still get cited context);
   ingestion snoozes (backlog grows — watch [Queue depth](observability.md#queue-depth)).

## Mitigate

- **Credentials/quota:** rotate or raise the provider key/quota; keys are per deployment and
  never logged (C-4).
- **Allowlist:** a tenant's `settings.providers_allowed` gates which providers its data may
  reach — do not "fix" an outage by pointing a tenant at a provider it has not allowed
  (SPEC-09 §2).
- **Wait out a transient:** the breaker half-opens and recovers automatically; snoozed
  ingest jobs resume. No manual requeue needed for the snooze case.
- **Persistent outage:** if a provider is down for long, communicate the degraded
  (retrieval-only) answering state per [Incident response](incident-response.md); ingestion
  catches up once the provider returns (watch the queue drain).

## Verify recovery

- `provider_errors_total` rate falls back under the 5 % threshold; the alert clears.
- Answering returns generated prose again (not just retrieval-only); the ingest backlog
  drains.
