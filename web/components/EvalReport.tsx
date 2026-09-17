import { pct, type Report, type ResultView, type Summary } from "@/lib/eval";

type EvalReportProps = {
  report: Report;
};

function fmt(ts?: string): string {
  if (!ts) return "—";
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return ts;
  return d.toLocaleString();
}

// Tri maps a nullable boolean result to a mark: true → ✓ (accent), false → ✗
// (danger), null → — (muted, "not scored / not judged").
function Tri({ value }: { value: boolean | null }) {
  if (value == null) return <span className="text-fg-muted">—</span>;
  return value ? (
    <span className="text-accent-text">✓</span>
  ) : (
    <span className="text-danger-text">✗</span>
  );
}

function Tile({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex flex-col gap-1 rounded-md border border-border px-3 py-3">
      <span className="text-xs uppercase tracking-wide text-fg-muted">{label}</span>
      <span className="text-lg font-semibold text-fg">{value}</span>
    </div>
  );
}

function SummaryTiles({ summary }: { summary: Summary }) {
  return (
    <div className="mt-4 grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-6">
      <Tile label="Cases" value={String(summary.cases)} />
      <Tile label={`Recall@${summary.k}`} value={pct(summary.recall_at_k)} />
      <Tile label="Grounded" value={pct(summary.grounded_rate)} />
      <Tile
        label="Correct"
        value={summary.correctness_rate != null ? pct(summary.correctness_rate) : "—"}
      />
      <Tile label="Mean latency" value={`${summary.mean_latency_ms}ms`} />
      <Tile label="Errors" value={String(summary.errors)} />
    </div>
  );
}

function ResultsTable({ results }: { results: ResultView[] }) {
  return (
    <div className="mt-6 overflow-x-auto rounded-2xl border border-border">
      <table className="w-full border-collapse text-left text-sm">
        <thead>
          <tr className="border-b border-border text-xs uppercase tracking-wide text-fg-muted">
            <th className="px-4 py-3 font-medium">Case</th>
            <th className="px-4 py-3 font-medium">Recall</th>
            <th className="px-4 py-3 font-medium">Correct</th>
            <th className="px-4 py-3 font-medium">Docs</th>
            <th className="px-4 py-3 font-medium">Latency</th>
          </tr>
        </thead>
        <tbody>
          {results.map((r) => (
            <tr key={r.case_id} className="border-b border-border last:border-b-0 align-top">
              <td className="px-4 py-3 text-fg">
                {r.question ?? <span className="font-mono text-xs text-fg-muted">{r.case_id}</span>}
              </td>
              <td className="px-4 py-3 text-center">
                <Tri value={r.recall_hit} />
              </td>
              <td className="px-4 py-3 text-center">
                <Tri value={r.judged_correct} />
              </td>
              <td className="px-4 py-3 text-xs text-fg-muted">{r.retrieved_doc_ids.length}</td>
              <td className="px-4 py-3 text-xs text-fg-muted">{r.latency_ms}ms</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export function EvalReport({ report }: EvalReportProps) {
  const { run, results } = report;
  return (
    <div className="mt-6 flex flex-col gap-2">
      <div className="rounded-2xl border border-border px-4 py-4">
        <div className="flex flex-wrap items-baseline justify-between gap-2">
          <span className="font-mono text-xs text-fg-muted">{run.id}</span>
          <span className="text-xs text-fg-muted">
            {fmt(run.started_at)} → {fmt(run.finished_at)}
          </span>
        </div>
        {run.summary ? (
          <SummaryTiles summary={run.summary} />
        ) : (
          <p className="mt-4 text-sm text-fg-muted">This run has no summary (it may not have finished).</p>
        )}
      </div>

      {results.length === 0 ? (
        <div className="mt-6 rounded-2xl border border-dashed border-border px-6 py-12 text-center text-sm text-fg-muted">
          This run recorded no case results.
        </div>
      ) : (
        <ResultsTable results={results} />
      )}
    </div>
  );
}
