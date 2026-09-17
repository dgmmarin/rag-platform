import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";

import { DocumentsTable } from "./DocumentsTable";
import type { Document } from "@/lib/documents";

function makeDoc(over: Partial<Document> = {}): Document {
  return {
    id: "d1",
    source_id: "src-1",
    external_id: "ext-1",
    title: "Getting started",
    mime_type: "text/html",
    status: "active",
    first_seen_at: "2026-09-15T10:00:00Z",
    last_seen_at: "2026-09-16T10:00:00Z",
    ...over,
  };
}

describe("DocumentsTable", () => {
  it("renders a row per document with title, status, source and type", () => {
    const docs = [
      makeDoc({ id: "d1", title: "Getting started", status: "active", source_id: "src-1" }),
      makeDoc({ id: "d2", title: "Old page", status: "deleted", source_id: "src-2", mime_type: undefined }),
    ];

    render(<DocumentsTable documents={docs} />);

    expect(screen.getByText("Getting started")).toBeInTheDocument();
    expect(screen.getByText("Old page")).toBeInTheDocument();
    expect(screen.getByText("active")).toBeInTheDocument();
    expect(screen.getByText("deleted")).toBeInTheDocument();
    expect(screen.getByText("src-1")).toBeInTheDocument();
    expect(screen.getByText("text/html")).toBeInTheDocument();
  });

  it("falls back to uri then external id when there is no title", () => {
    const docs = [
      makeDoc({ id: "d1", title: undefined, uri: "https://x/y", external_id: "ext-1" }),
      makeDoc({ id: "d2", title: undefined, uri: undefined, external_id: "ext-2" }),
    ];

    render(<DocumentsTable documents={docs} />);

    expect(screen.getByText("https://x/y")).toBeInTheDocument();
    expect(screen.getByText("ext-2")).toBeInTheDocument();
  });

  it("shows an empty state when there are no documents", () => {
    render(<DocumentsTable documents={[]} />);
    expect(screen.getByText(/no documents yet/i)).toBeInTheDocument();
  });

  it("links each row to the document detail", () => {
    render(<DocumentsTable documents={[makeDoc({ id: "d9", title: "Doc nine" })]} />);
    expect(screen.getByRole("link", { name: /view/i })).toHaveAttribute("href", "/admin/documents/d9");
    expect(screen.getByRole("link", { name: "Doc nine" })).toHaveAttribute("href", "/admin/documents/d9");
  });
});
