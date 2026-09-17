import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";

import { EvalRunsTable } from "./EvalRunsTable";
import type { RunView } from "@/lib/eval";

function makeRun(over: Partial<RunView> = {}): RunView {
  return {
    id: "r1",
    config: {},
    started_at: "2026-09-15T10:00:00Z",
    finished_at: "2026-09-15T10:02:00Z",
    summary: {
      cases: 12,
      k: 5,
      recall_at_k: 0.75,
      cases_scored_for_recall: 12,
      grounded_rate: 0.9,
      mean_latency_ms: 320,
      errors: 0,
      cases_judged: 12,
      correctness_rate: 0.83,
    },
    ...over,
  };
}

describe("EvalRunsTable", () => {
  it("renders a row per run with summary metrics as percentages", () => {
    render(<EvalRunsTable runs={[makeRun()]} />);

    expect(screen.getByText("12")).toBeInTheDocument();
    expect(screen.getByText("75%")).toBeInTheDocument(); // recall
    expect(screen.getByText("90%")).toBeInTheDocument(); // grounded
    expect(screen.getByText("83%")).toBeInTheDocument(); // correctness
    expect(screen.getByText("320ms")).toBeInTheDocument();
  });

  it("shows em dashes when a run has no summary", () => {
    render(<EvalRunsTable runs={[makeRun({ summary: undefined })]} />);
    // several cells fall back to em dash; at least the metric cells do
    expect(screen.getAllByText("—").length).toBeGreaterThan(0);
  });

  it("links each run to its report", () => {
    render(<EvalRunsTable runs={[makeRun({ id: "r9" })]} />);
    expect(screen.getByRole("link", { name: /view/i })).toHaveAttribute("href", "/admin/eval/r9");
  });

  it("shows an empty state when there are no runs", () => {
    render(<EvalRunsTable runs={[]} />);
    expect(screen.getByText(/no eval runs yet/i)).toBeInTheDocument();
  });
});
