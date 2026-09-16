import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";

import { JobDetail } from "./JobDetail";
import type { Job } from "@/lib/jobs";

function makeJob(over: Partial<Job> = {}): Job {
  return {
    id: "j1",
    tenant_id: "t1",
    source_id: "src-1",
    kind: "sync_source",
    status: "queued",
    attempt: 1,
    max_attempts: 3,
    queued_at: "2026-09-15T10:00:00Z",
    ...over,
  };
}

describe("JobDetail", () => {
  it("shows the cancel action for a cancellable job and wires it", () => {
    const onCancel = vi.fn();
    render(<JobDetail job={makeJob({ status: "queued" })} onCancel={onCancel} />);

    const cancel = screen.getByRole("button", { name: /cancel/i });
    fireEvent.click(cancel);
    expect(onCancel).toHaveBeenCalledTimes(1);
  });

  it("hides the cancel action for a terminal job", () => {
    render(<JobDetail job={makeJob({ status: "succeeded" })} onCancel={vi.fn()} />);
    expect(screen.queryByRole("button", { name: /cancel/i })).not.toBeInTheDocument();
  });

  it("disables the cancel action while busy", () => {
    render(<JobDetail job={makeJob({ status: "running" })} onCancel={vi.fn()} busy />);
    expect(screen.getByRole("button", { name: /cancel/i })).toBeDisabled();
  });

  it("renders known numeric stats and any extra keys generically", () => {
    const job = makeJob({
      status: "succeeded",
      stats: {
        docs_seen: 12,
        docs_changed: 3,
        chunks_written: 40,
        embed_tokens: 999,
        // an unknown key must still be shown, not dropped
        pages_fetched: 7,
      },
    });
    render(<JobDetail job={job} onCancel={vi.fn()} />);

    expect(screen.getByText("12")).toBeInTheDocument();
    expect(screen.getByText("40")).toBeInTheDocument();
    // unknown stat key surfaced generically
    expect(screen.getByText(/pages_fetched/i)).toBeInTheDocument();
    expect(screen.getByText("7")).toBeInTheDocument();
  });

  it("lists the errors[] array from stats when present", () => {
    const job = makeJob({
      status: "failed",
      error: "sync aborted",
      stats: { docs_seen: 1, errors: ["url timed out", "bad gateway"] },
    });
    render(<JobDetail job={job} onCancel={vi.fn()} />);

    expect(screen.getByText("sync aborted")).toBeInTheDocument();
    expect(screen.getByText(/url timed out/i)).toBeInTheDocument();
    expect(screen.getByText(/bad gateway/i)).toBeInTheDocument();
  });
});
