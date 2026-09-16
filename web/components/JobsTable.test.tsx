import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";

import { JobsTable } from "./JobsTable";
import type { Job } from "@/lib/jobs";

function makeJob(over: Partial<Job> = {}): Job {
  return {
    id: "j1",
    tenant_id: "t1",
    source_id: "src-1",
    kind: "sync_source",
    status: "succeeded",
    attempt: 1,
    max_attempts: 3,
    queued_at: "2026-09-15T10:00:00Z",
    finished_at: "2026-09-15T10:05:00Z",
    duration_ms: 1500,
    ...over,
  };
}

describe("JobsTable", () => {
  it("renders a row per job with kind, status, source, attempt and duration", () => {
    const jobs = [
      makeJob({ id: "j1", kind: "sync_source", status: "succeeded", source_id: "src-1" }),
      makeJob({ id: "j2", kind: "reindex", status: "failed", source_id: undefined, duration_ms: undefined }),
    ];

    render(<JobsTable jobs={jobs} onCancel={vi.fn()} />);

    expect(screen.getByText("sync_source")).toBeInTheDocument();
    expect(screen.getByText("reindex")).toBeInTheDocument();
    expect(screen.getByText("succeeded")).toBeInTheDocument();
    expect(screen.getByText("failed")).toBeInTheDocument();
    expect(screen.getByText("src-1")).toBeInTheDocument();
    // attempt / max_attempts
    expect(screen.getAllByText("1/3").length).toBeGreaterThan(0);
    // human duration
    expect(screen.getByText("1.5s")).toBeInTheDocument();
  });

  it("shows an empty state and no rows when there are no jobs", () => {
    render(<JobsTable jobs={[]} onCancel={vi.fn()} />);

    expect(screen.getByText(/no jobs yet/i)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /cancel/i })).not.toBeInTheDocument();
  });

  it("offers Cancel only for queued or running jobs, never terminal ones", () => {
    const jobs = [
      makeJob({ id: "queued", status: "queued" }),
      makeJob({ id: "running", status: "running" }),
      makeJob({ id: "succeeded", status: "succeeded" }),
      makeJob({ id: "failed", status: "failed" }),
      makeJob({ id: "cancelled", status: "cancelled" }),
    ];

    render(<JobsTable jobs={jobs} onCancel={vi.fn()} />);

    // two cancellable rows -> two Cancel buttons
    expect(screen.getAllByRole("button", { name: /cancel/i })).toHaveLength(2);
  });

  it("links each row to the job detail and wires the cancel callback", () => {
    const onCancel = vi.fn();
    const job = makeJob({ id: "j9", status: "queued" });

    render(<JobsTable jobs={[job]} onCancel={onCancel} />);

    expect(screen.getByRole("link", { name: /view|details|j9/i })).toHaveAttribute(
      "href",
      "/admin/jobs/j9",
    );

    fireEvent.click(screen.getByRole("button", { name: /cancel/i }));
    expect(onCancel).toHaveBeenCalledWith(job);
  });

  it("disables the busy row's cancel action", () => {
    const job = makeJob({ id: "j1", status: "running" });
    render(<JobsTable jobs={[job]} onCancel={vi.fn()} busyId="j1" />);
    expect(screen.getByRole("button", { name: /cancel/i })).toBeDisabled();
  });
});
