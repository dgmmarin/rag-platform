"""Parsing sidecar HTTP service (ADR-0006, SPEC-05 §2, STORY-05.3).

A stateless service that turns PDF/DOCX/PPTX/XLSX bytes into the normalised
``{title, blocks:[...]}`` representation the Go ingestion worker consumes. The Go
worker parses HTML/Markdown/text/CSV/JSON itself and calls this only for the heavy
formats (``internal/ingest/sidecar`` is the client).

Endpoints:
  GET  /healthz  -> 200 {"status":"ok"}          (liveness; used by compose)
  POST /parse    -> 200 {"title","blocks":[...]} (multipart: file=<bytes>, mime=<type>)

Errors are shaped so the worker can react per SPEC-05 §2/§8:
  415 unsupported mime  -> not a sidecar format (worker should not have called us)
  422 parse failure     -> the worker records metadata.parse_error, skips the doc
  413 payload too large -> body exceeded MAX_PARSE_BYTES

Flask handles multipart parsing and body-size limits (input validation at the
trust boundary); gunicorn serves it (see Dockerfile). Kept sync + per-worker
because extraction is CPU-bound — scale by process/replica, not async.
"""

from __future__ import annotations

import os

from flask import Flask, jsonify, request

import parsers

# Body ceiling; defaults to the platform's 50 MB upload limit (FR-SRC-02). Flask
# rejects an oversize multipart body with 413 before it reaches a parser.
MAX_PARSE_BYTES = int(os.environ.get("MAX_PARSE_BYTES", str(50 * 1024 * 1024)))

app = Flask(__name__)
app.config["MAX_CONTENT_LENGTH"] = MAX_PARSE_BYTES


@app.get("/healthz")
def healthz():
    return jsonify(status="ok")


@app.post("/parse")
def parse():
    mime = (request.form.get("mime") or "").strip().lower()
    upload = request.files.get("file")
    if upload is None:
        return jsonify(error="missing 'file' part"), 400
    if not mime:
        return jsonify(error="missing 'mime' field"), 400

    extractor = parsers.DISPATCH.get(mime)
    if extractor is None:
        return jsonify(error=f"unsupported mime {mime!r}"), 415

    data = upload.read()
    try:
        result = extractor(data)
    except Exception as exc:  # noqa: BLE001 - any parse failure is a 422, not a 500.
        return jsonify(error=f"parse failed: {exc}"), 422
    return jsonify(result)


@app.errorhandler(413)
def too_large(_err):
    return jsonify(error="payload too large"), 413


if __name__ == "__main__":
    # Dev fallback only; production runs under gunicorn (see Dockerfile).
    app.run(host="0.0.0.0", port=int(os.environ.get("PARSER_PORT", "8081")))
