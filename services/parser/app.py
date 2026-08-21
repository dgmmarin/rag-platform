"""Parsing sidecar stub (STORY-01.2).

A minimal, dependency-free HTTP service that stands in for the real Python
parser (ADR-0006, SPEC-05 §3). For this story it only needs a health endpoint
so the local stack can come up and be probed. STORY-05.3 replaces this with the
real `POST /parse` (Docling/Unstructured) implementation.

Kept on the standard library on purpose: no framework, no ML deps, tiny image.
"""

import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = 8081


class Handler(BaseHTTPRequestHandler):
    def _send(self, code: int, body: dict) -> None:
        payload = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self) -> None:  # noqa: N802 (http.server API)
        if self.path == "/healthz":
            self._send(200, {"status": "ok"})
            return
        self._send(404, {"error": "not found"})

    def do_POST(self) -> None:  # noqa: N802 (http.server API)
        if self.path == "/parse":
            # STORY-05.3 implements the real contract:
            #   multipart file + mime -> {title, blocks:[...]}.
            self._send(501, {"error": "parse not implemented (stub)"})
            return
        self._send(404, {"error": "not found"})

    def log_message(self, *_args) -> None:  # silence per-request stderr noise
        pass


def main() -> None:
    server = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    print(f"parser sidecar stub listening on :{PORT}", flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
