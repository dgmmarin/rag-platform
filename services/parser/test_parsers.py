"""Sidecar tests (STORY-05.3): every fixture parses to headings/tables, and the
HTTP contract (/parse, /healthz, error codes) holds.

The six ``testdata/`` fixtures span the four sidecar formats (SPEC-05 §2). Run
``python gen_fixtures.py`` to (re)generate them.
"""

from __future__ import annotations

import os

import pytest

import app as sidecar_app
import parsers

TESTDATA = os.path.join(os.path.dirname(__file__), "testdata")

DOCX = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
PPTX = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
XLSX = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"

# fixture -> (mime, must_have_table). Every fixture must yield at least one
# heading; PDF tables are best-effort (ponytail) so only the office formats are
# required to yield a table.
MANIFEST = {
    "report.pdf": ("application/pdf", False),
    "article.pdf": ("application/pdf", False),
    "memo.docx": (DOCX, True),
    "letter.docx": (DOCX, True),
    "deck.pptx": (PPTX, True),
    "budget.xlsx": (XLSX, True),
}


def _read(name: str) -> bytes:
    with open(os.path.join(TESTDATA, name), "rb") as f:
        return f.read()


@pytest.mark.parametrize("name", sorted(MANIFEST))
def test_fixture_blocks(name: str) -> None:
    mime, must_have_table = MANIFEST[name]
    result = parsers.DISPATCH[mime](_read(name))

    assert set(result) == {"title", "blocks"}
    types = [b["type"] for b in result["blocks"]]
    assert "heading" in types, f"{name}: no heading block"

    for b in result["blocks"]:
        assert b["type"] in {"heading", "paragraph", "table", "list", "code"}
        if b["type"] == "table":
            # A table carries structured rows and matching GFM markdown.
            assert b["rows"] and b["text"].startswith("|")
            assert parsers.markdown_table(b["rows"]) == b["text"]

    if must_have_table:
        assert "table" in types, f"{name}: expected a table block"


def test_manifest_covers_six_types() -> None:
    assert len(MANIFEST) >= 6


def test_markdown_table_escapes_pipes() -> None:
    md = parsers.markdown_table([["a", "b|c"], ["1", "2"]])
    assert md.splitlines()[0] == "| a | b\\|c |"
    assert md.splitlines()[1] == "| --- | --- |"


@pytest.fixture()
def client():
    sidecar_app.app.config.update(TESTING=True)
    return sidecar_app.app.test_client()


def test_healthz(client) -> None:
    resp = client.get("/healthz")
    assert resp.status_code == 200 and resp.get_json() == {"status": "ok"}


def test_parse_endpoint_ok(client) -> None:
    resp = client.post(
        "/parse",
        data={"mime": DOCX, "file": (open(os.path.join(TESTDATA, "memo.docx"), "rb"), "memo.docx")},
        content_type="multipart/form-data",
    )
    assert resp.status_code == 200
    body = resp.get_json()
    assert any(b["type"] == "heading" for b in body["blocks"])
    assert any(b["type"] == "table" for b in body["blocks"])


def test_parse_unsupported_mime(client) -> None:
    resp = client.post(
        "/parse",
        data={"mime": "application/zip", "file": (open(os.path.join(TESTDATA, "memo.docx"), "rb"), "x.zip")},
        content_type="multipart/form-data",
    )
    assert resp.status_code == 415


def test_parse_corrupt_payload_is_422(client) -> None:
    import io

    resp = client.post(
        "/parse",
        data={"mime": DOCX, "file": (io.BytesIO(b"not a real docx"), "bad.docx")},
        content_type="multipart/form-data",
    )
    assert resp.status_code == 422


def test_parse_missing_parts(client) -> None:
    assert client.post("/parse", data={"mime": DOCX}, content_type="multipart/form-data").status_code == 400
