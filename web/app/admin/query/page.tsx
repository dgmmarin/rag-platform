"use client";

import { useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { useAuth } from "@/lib/auth";
import { useTenant } from "@/lib/tenant";
import { AnswerPanel } from "@/components/AnswerPanel";
import { runQuery, sendFeedback, type QueryResult } from "@/lib/query";

export default function QueryPage() {
  const { me } = useAuth();
  const tenantId = useTenant().current?.id;
  const csrf = me?.csrf_token;

  const [question, setQuestion] = useState("");
  const [result, setResult] = useState<QueryResult | null>(null);
  const [rating, setRating] = useState<1 | -1 | null>(null);

  const ask = useMutation({
    mutationFn: (q: string) => runQuery(tenantId as string, q, csrf),
    onSuccess: (r) => {
      setResult(r);
      setRating(null);
    },
  });

  const feedback = useMutation({
    mutationFn: (r: 1 | -1) => sendFeedback(tenantId as string, result!.id, r, csrf),
    onSuccess: (_data, r) => setRating(r),
  });

  return (
    <div className="flex flex-col gap-1">
      <p className="font-mono text-xs uppercase tracking-wide text-fg-muted">/admin/query</p>
      <h1 className="text-lg font-semibold tracking-tight text-fg">Query playground</h1>

      <form
        className="mt-4 flex flex-col gap-3"
        onSubmit={(e) => {
          e.preventDefault();
          const q = question.trim();
          if (q) ask.mutate(q);
        }}
      >
        <textarea
          value={question}
          onChange={(e) => setQuestion(e.target.value)}
          placeholder="Ask a grounded question over this tenant's content…"
          aria-label="Question"
          rows={3}
          className="w-full resize-y rounded-md border border-border bg-bg px-3 py-2 text-sm text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg"
        />
        <div className="flex items-center gap-3">
          <button
            type="submit"
            disabled={ask.isPending || !question.trim()}
            className="h-9 rounded-md bg-accent-text px-4 text-sm font-medium text-bg transition-opacity hover:opacity-90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg disabled:cursor-not-allowed disabled:opacity-50"
          >
            {ask.isPending ? "Asking…" : "Ask"}
          </button>
        </div>
      </form>

      {ask.isError ? (
        <div className="mt-6 rounded-2xl border border-border bg-danger-tint px-4 py-4 text-sm text-danger-text">
          Query failed: {ask.error instanceof Error ? ask.error.message : "unknown error"}
        </div>
      ) : null}

      {feedback.isError ? (
        <p className="mt-4 rounded-md border border-border bg-danger-tint px-3 py-2 text-sm text-danger-text">
          Could not record feedback: {feedback.error instanceof Error ? feedback.error.message : "unknown error"}
        </p>
      ) : null}

      {result ? (
        <AnswerPanel
          result={result}
          onFeedback={(r) => feedback.mutate(r)}
          rating={rating}
          busy={feedback.isPending}
        />
      ) : null}
    </div>
  );
}
