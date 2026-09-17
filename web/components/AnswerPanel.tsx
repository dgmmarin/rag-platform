import type { Citation, QueryResult } from "@/lib/query";

type AnswerPanelProps = {
  result: QueryResult;
  onFeedback: (rating: 1 | -1) => void;
  // rating already recorded for this answer, or null; disables the buttons once set.
  rating?: 1 | -1 | null;
  busy?: boolean;
};

function CitationRow({ c }: { c: Citation }) {
  const heading = c.heading_path.length > 0 ? c.heading_path.join(" › ") : null;
  return (
    <li className="rounded-md border border-border px-3 py-3">
      <div className="flex items-baseline gap-2">
        <span className="font-mono text-xs text-accent-text">[{c.n}]</span>
        <span className="text-sm text-fg">{c.title || c.uri || c.document_id}</span>
      </div>
      {c.uri ? <p className="mt-1 break-all text-xs text-fg-muted">{c.uri}</p> : null}
      {heading ? <p className="mt-1 text-xs text-fg-muted">{heading}</p> : null}
      {c.snippet ? <p className="mt-2 text-sm text-fg-muted">{c.snippet}</p> : null}
    </li>
  );
}

export function AnswerPanel({ result, onFeedback, rating, busy }: AnswerPanelProps) {
  return (
    <div className="mt-6 flex flex-col gap-6">
      <div className="rounded-2xl border border-border px-4 py-4">
        <div className="flex items-center justify-between gap-4">
          <span
            className={`inline-flex rounded-full px-2 py-0.5 text-xs font-medium ${
              result.grounded ? "bg-accent-tint text-accent-text" : "bg-danger-tint text-danger-text"
            }`}
          >
            {result.grounded ? "Grounded" : "Not grounded"}
          </span>
          <span className="font-mono text-xs text-fg-muted">{result.model}</span>
        </div>

        <p className="mt-4 whitespace-pre-wrap text-sm text-fg">{result.answer}</p>

        <div className="mt-4 flex items-center gap-2 border-t border-border pt-4">
          <span className="text-xs uppercase tracking-wide text-fg-muted">Feedback</span>
          <button
            type="button"
            onClick={() => onFeedback(1)}
            disabled={busy || rating != null}
            aria-pressed={rating === 1}
            aria-label="Thumbs up"
            className={`h-8 rounded-md border px-2.5 text-xs font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg disabled:cursor-not-allowed disabled:opacity-50 ${
              rating === 1 ? "border-accent-text bg-accent-tint text-accent-text" : "border-border text-fg hover:bg-bg-subtle"
            }`}
          >
            👍
          </button>
          <button
            type="button"
            onClick={() => onFeedback(-1)}
            disabled={busy || rating != null}
            aria-pressed={rating === -1}
            aria-label="Thumbs down"
            className={`h-8 rounded-md border px-2.5 text-xs font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg disabled:cursor-not-allowed disabled:opacity-50 ${
              rating === -1 ? "border-danger-text bg-danger-tint text-danger-text" : "border-border text-fg hover:bg-bg-subtle"
            }`}
          >
            👎
          </button>
          {rating != null ? <span className="text-xs text-fg-muted">Thanks for the feedback.</span> : null}
        </div>
      </div>

      <div className="rounded-2xl border border-border px-4 py-4">
        <h2 className="text-sm font-semibold tracking-tight text-fg">
          Citations{result.citations.length > 0 ? ` (${result.citations.length})` : ""}
        </h2>
        {result.citations.length === 0 ? (
          <p className="mt-4 text-sm text-fg-muted">No citations for this answer.</p>
        ) : (
          <ul className="mt-4 flex flex-col gap-3">
            {result.citations.map((c) => (
              <CitationRow key={c.n} c={c} />
            ))}
          </ul>
        )}
      </div>
    </div>
  );
}
