import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";

import { SourcesTable } from "./SourcesTable";
import type { Source } from "@/lib/sources";

function makeSource(over: Partial<Source> = {}): Source {
  return {
    id: "s1",
    tenant_id: "t1",
    kind: "webcrawl",
    name: "Docs site",
    status: "active",
    config: {},
    next_run_at: "2026-09-20T10:00:00Z",
    last_run_at: "2026-09-15T10:00:00Z",
    last_success_at: "2026-09-15T10:05:00Z",
    created_at: "2026-09-01T00:00:00Z",
    updated_at: "2026-09-15T10:05:00Z",
    ...over,
  };
}

describe("SourcesTable", () => {
  it("renders a row per source with name, kind, status and error summary", () => {
    const sources = [
      makeSource({ id: "s1", name: "Docs site", kind: "webcrawl", status: "active" }),
      makeSource({
        id: "s2",
        name: "Wiki export",
        kind: "sitemap",
        status: "error",
        last_error: "connection refused",
      }),
    ];

    render(<SourcesTable sources={sources} onSync={vi.fn()} onFullSync={vi.fn()} onTest={vi.fn()} onDelete={vi.fn()} />);

    expect(screen.getByText("Docs site")).toBeInTheDocument();
    expect(screen.getByText("Wiki export")).toBeInTheDocument();
    expect(screen.getByText("webcrawl")).toBeInTheDocument();
    expect(screen.getByText("sitemap")).toBeInTheDocument();
    expect(screen.getByText("active")).toBeInTheDocument();
    expect(screen.getByText("error")).toBeInTheDocument();
    expect(screen.getByText("connection refused")).toBeInTheDocument();
  });

  it("shows an empty state and no rows when there are no sources", () => {
    render(<SourcesTable sources={[]} onSync={vi.fn()} onFullSync={vi.fn()} onTest={vi.fn()} onDelete={vi.fn()} />);

    expect(screen.getByText(/no sources yet/i)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /sync/i })).not.toBeInTheDocument();
  });

  it("exposes sync, test, edit and delete actions per row and wires the callbacks", () => {
    const onSync = vi.fn();
    const onTest = vi.fn();
    const onDelete = vi.fn();
    const source = makeSource({ id: "s1", name: "Docs site" });

    render(
      <SourcesTable
        sources={[source]}
        onSync={onSync}
        onFullSync={vi.fn()}
        onTest={onTest}
        onDelete={onDelete}
      />,
    );

    // edit is a link to the (Task 4) edit route
    const edit = screen.getByRole("link", { name: /edit/i });
    expect(edit).toHaveAttribute("href", "/admin/sources/s1/edit");

    fireEvent.click(screen.getByRole("button", { name: /^sync$/i }));
    fireEvent.click(screen.getByRole("button", { name: /test/i }));
    fireEvent.click(screen.getByRole("button", { name: /delete/i }));

    expect(onSync).toHaveBeenCalledWith(source);
    expect(onTest).toHaveBeenCalledWith(source);
    expect(onDelete).toHaveBeenCalledWith(source);
  });

  it("offers Full re-crawl for a crawl source and wires it, but not for upload", () => {
    const onFullSync = vi.fn();
    const crawl = makeSource({ id: "s1", name: "Docs site", kind: "web_crawl" });

    const { rerender } = render(
      <SourcesTable
        sources={[crawl]}
        onSync={vi.fn()}
        onFullSync={onFullSync}
        onTest={vi.fn()}
        onDelete={vi.fn()}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: /full re-crawl/i }));
    expect(onFullSync).toHaveBeenCalledWith(crawl);

    // upload sources have nothing to re-crawl, so the action is hidden.
    rerender(
      <SourcesTable
        sources={[makeSource({ id: "s2", name: "Upload", kind: "upload" })]}
        onSync={vi.fn()}
        onFullSync={onFullSync}
        onTest={vi.fn()}
        onDelete={vi.fn()}
      />,
    );
    expect(screen.queryByRole("button", { name: /full re-crawl/i })).not.toBeInTheDocument();
  });
});
