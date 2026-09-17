import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";

import { EvalReport } from "./EvalReport";
import type { Report, ResultView } from "@/lib/eval";

function makeResult(over: Partial<ResultView> = {}): ResultView {
  return {
    case_id: "c1",
    question: "What colour is the sky?",
    expected_answer: "blue",
    retrieved_doc_ids: ["d1", "d2"],
    recall_hit: true,
    judged_correct: true,
    answer: "blue",
    latency_ms: 300,
    ...over,
  };
}

function makeReport(over: Partial<Report> = {}): Report {
  return {
    run: {
      id: "r1",
      config: {},
      started_at: "2026-09-15T10:00:00Z",
      finished_at: "2026-09-15T10:02:00Z",
      summary: {
        cases: 2,
        k: 5,
        recall_at_k: 0.5,
        cases_scored_for_recall: 2,
        grounded_rate: 1,
        mean_latency_ms: 250,
        errors: 0,
        cases_judged: 2,
        correctness_rate: 0.5,
      },
    },
    results: [makeResult()],
    ...over,
  };
}

describe("EvalReport", () => {
  it("renders the summary tiles from the run summary", () => {
    render(<EvalReport report={makeReport()} />);

    expect(screen.getByText(/recall@5/i)).toBeInTheDocument();
    expect(screen.getAllByText("50%").length).toBe(2); // recall + correctness both 50%
    expect(screen.getByText("100%")).toBeInTheDocument(); // grounded
    expect(screen.getByText("250ms")).toBeInTheDocument();
  });

  it("renders a results row per case with question and latency", () => {
    render(<EvalReport report={makeReport()} />);
    expect(screen.getByText("What colour is the sky?")).toBeInTheDocument();
    expect(screen.getByText("300ms")).toBeInTheDocument();
  });

  it("falls back to the case id when the case was deleted (null question)", () => {
    render(
      <EvalReport
        report={makeReport({ results: [makeResult({ question: undefined, case_id: "gone-1" })] })}
      />,
    );
    expect(screen.getByText("gone-1")).toBeInTheDocument();
  });

  it("notes when a run has no summary", () => {
    const r = makeReport();
    r.run.summary = undefined;
    render(<EvalReport report={r} />);
    expect(screen.getByText(/no summary/i)).toBeInTheDocument();
  });

  it("notes when a run recorded no results", () => {
    render(<EvalReport report={makeReport({ results: [] })} />);
    expect(screen.getByText(/no case results/i)).toBeInTheDocument();
  });
});
