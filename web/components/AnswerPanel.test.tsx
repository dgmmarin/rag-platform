import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";

import { AnswerPanel } from "./AnswerPanel";
import type { QueryResult } from "@/lib/query";

function makeResult(over: Partial<QueryResult> = {}): QueryResult {
  return {
    id: "q1",
    answer: "The sky is blue [1].",
    grounded: true,
    citations: [
      {
        n: 1,
        document_id: "d1",
        title: "Sky facts",
        uri: "https://x/sky",
        heading_path: ["Colours"],
        snippet: "the sky is blue",
      },
    ],
    usage: { retrieval_ms: 10, generation_ms: 20, in_tokens: 5, out_tokens: 8 },
    model: "claude-sonnet-5",
    ...over,
  };
}

describe("AnswerPanel", () => {
  it("renders the answer, grounded badge, model and citations", () => {
    render(<AnswerPanel result={makeResult()} onFeedback={vi.fn()} />);

    expect(screen.getByText("The sky is blue [1].")).toBeInTheDocument();
    expect(screen.getByText(/grounded/i)).toBeInTheDocument();
    expect(screen.getByText("claude-sonnet-5")).toBeInTheDocument();
    expect(screen.getByText("Sky facts")).toBeInTheDocument();
    expect(screen.getByText(/citations \(1\)/i)).toBeInTheDocument();
  });

  it("shows a not-grounded badge and no-citations message when ungrounded", () => {
    render(
      <AnswerPanel
        result={makeResult({ grounded: false, citations: [] })}
        onFeedback={vi.fn()}
      />,
    );
    expect(screen.getByText(/not grounded/i)).toBeInTheDocument();
    expect(screen.getByText(/no citations/i)).toBeInTheDocument();
  });

  it("wires the thumbs-up and thumbs-down feedback callbacks", () => {
    const onFeedback = vi.fn();
    render(<AnswerPanel result={makeResult()} onFeedback={onFeedback} />);

    fireEvent.click(screen.getByRole("button", { name: /thumbs up/i }));
    expect(onFeedback).toHaveBeenCalledWith(1);

    fireEvent.click(screen.getByRole("button", { name: /thumbs down/i }));
    expect(onFeedback).toHaveBeenCalledWith(-1);
  });

  it("disables feedback once a rating is recorded", () => {
    render(<AnswerPanel result={makeResult()} onFeedback={vi.fn()} rating={1} />);
    expect(screen.getByRole("button", { name: /thumbs up/i })).toBeDisabled();
    expect(screen.getByRole("button", { name: /thumbs down/i })).toBeDisabled();
    expect(screen.getByText(/thanks for the feedback/i)).toBeInTheDocument();
  });
});
