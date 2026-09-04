# Upload connector (`upload`)

**Traces:** FR-SRC-02 · **Spec:** SPEC-04 §5/§5a, SPEC-07 §2b · **Credentials:** none · **Scheduled:** no

The `upload` connector is for content you push into the platform yourself, one file
at a time, rather than content the platform fetches. It is **not scheduled**: there
is no crawl or enumeration. Each uploaded file becomes a document version.

Every tenant has an implicit `upload` source, so you do not normally need to create
one. You may create additional `upload` sources to attribute uploads to a named
source.

## How it works

1. `POST /v1/documents` with a `multipart/form-data` body containing a `file` part
   (optionally a `source` field naming an `upload` source UUID, and an
   `Idempotency-Key` header).
2. The API validates the file's type and size (below), writes the raw bytes to
   object storage, and enqueues an `ingest_document` job. The response is `202` with
   the queued job as your handle.
3. The job parses → chunks → embeds → commits the file as a new immutable document
   version and flips the document's `current_version` in one transaction, so a query
   never sees a half-built document (SPEC-05 §5, ADR-0008).
4. **Re-uploading a file with the same filename creates a new version** of the same
   document; identity is `(upload source, filename)`. A single upload never
   soft-deletes your other documents (it is an incremental ingest, not a full
   enumeration).

## Allowed file types

The extension must be on the allowlist, and the file's **leading bytes are sniffed**
and must be consistent with the extension — the client `Content-Type` header is
never trusted (SPEC-04 §5a). A mislabelled or hostile file is rejected with a `400`.

| Extension | Canonical type |
|---|---|
| `.pdf` | `application/pdf` |
| `.docx` | `application/vnd.openxmlformats-officedocument.wordprocessingml.document` |
| `.md` | `text/markdown` |
| `.html`, `.htm` | `text/html` |
| `.txt` | `text/plain` |
| `.csv` | `text/csv` |

## Size limit

An upload must not exceed the per-tenant ceiling
`settings.limits.max_upload_mb` (SPEC-02 §5; **default 50 MB**). If the tenant
setting is unavailable, the request fails safe to the global `MAX_UPLOAD_BYTES`
environment ceiling (**default 50 MB**, SPEC-07 §2b). An oversize upload is a `400`.

## Configuration reference

An `upload` source carries **no meaningful configuration** — there is nothing to
crawl or authenticate. `config` may be omitted, or be any well-formed JSON object;
a non-object body is rejected.

## Test-connection

`POST /v1/sources/{id}/test` for an `upload` source is a trivial success: there is
no external system and no credentials to verify. Object-storage health is a
platform-wide readiness concern (`/readyz`), deliberately not a per-source test
(SPEC-04 §1b).

## Example

Create an optional named upload source (config is empty):

```jsonc
// POST /v1/sources
{
  "kind": "upload",
  "name": "Handbook uploads",
  "config": {}
}
```

Upload a file to it (multipart):

```bash
curl -X POST https://<host>/v1/documents \
  -H "Authorization: Bearer <api-key>" \
  -H "Idempotency-Key: 2f9c…" \
  -F "file=@handbook.pdf" \
  -F "source=<source-uuid>"
```
