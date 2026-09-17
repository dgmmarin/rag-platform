import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";

import { DocumentDetail } from "./DocumentDetail";
import type { Chunk, DocumentDetail as Doc } from "@/lib/documents";

function makeDoc(over: Partial<Doc> = {}): Doc {
  return {
    id: "d1",
    source_id: "src-1",
    external_id: "ext-1",
    title: "Getting started",
    mime_type: "text/html",
    status: "active",
    first_seen_at: "2026-09-15T10:00:00Z",
    last_seen_at: "2026-09-16T10:00:00Z",
    current_version_meta: {
      id: "v1",
      content_hash: "abc123",
      char_count: 4200,
      parser: "html",
      created_at: "2026-09-16T10:00:00Z",
    },
    ...over,
  };
}

function makeChunk(over: Partial<Chunk> = {}): Chunk {
  return {
    id: "c1",
    position: 0,
    heading_path: ["Intro"],
    content: "hello world",
    token_count: 12,
    embedding_model: "text-embedding-3-small",
    created_at: "2026-09-16T10:00:00Z",
    ...over,
  };
}

describe("DocumentDetail", () => {
  it("renders the document summary and current-version metadata", () => {
    render(<DocumentDetail doc={makeDoc()} chunks={[]} />);

    expect(screen.getByText("src-1")).toBeInTheDocument();
    expect(screen.getByText("ext-1")).toBeInTheDocument();
    expect(screen.getByText("text/html")).toBeInTheDocument();
    expect(screen.getByText("abc123")).toBeInTheDocument();
    expect(screen.getByText("4200")).toBeInTheDocument();
    expect(screen.getByText("active")).toBeInTheDocument();
  });

  it("omits the version block when there is no current version", () => {
    render(<DocumentDetail doc={makeDoc({ current_version_meta: undefined })} chunks={[]} />);
    expect(screen.queryByText(/content hash/i)).not.toBeInTheDocument();
  });

  it("lists chunks with position, token count, model and content", () => {
    const chunks = [
      makeChunk({ id: "c1", position: 0, content: "first chunk", token_count: 12 }),
      makeChunk({ id: "c2", position: 1, content: "second chunk", token_count: 20 }),
    ];

    render(<DocumentDetail doc={makeDoc()} chunks={chunks} />);

    expect(screen.getByText(/chunks \(2\)/i)).toBeInTheDocument();
    expect(screen.getByText("first chunk")).toBeInTheDocument();
    expect(screen.getByText("second chunk")).toBeInTheDocument();
    expect(screen.getByText("#0")).toBeInTheDocument();
    expect(screen.getByText("12 tok")).toBeInTheDocument();
  });

  it("shows a no-chunks message when the document has none", () => {
    render(<DocumentDetail doc={makeDoc()} chunks={[]} />);
    expect(screen.getByText(/no chunks/i)).toBeInTheDocument();
  });
});
