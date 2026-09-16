import { humanDuration } from "./JobsTable";
import { isCancellable, type Job } from "@/lib/jobs";

type JobDetailProps = {
  job: Job;
  onCancel: (job: Job) => void;
  // busy disables the cancel action while the mutation runs.
  busy?: boolean;
};

function fmt(ts?: string): string {
  if (!ts) return "—";
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return ts;
  return d.toLocaleString();
}

function StatusBadge({ status }: { status: string }) {
  const tone =
    status === "failed"
      ? "bg-danger-tint text-danger-text"
      : status === "succeeded" || status === "running"
        ? "bg-accent-tint text-accent-text"
        : "bg-bg-subtle text-fg-muted";
  return (
    <span className={`inline-flex rounded-full px-2 py-0.5 text-xs font-medium ${tone}`}>{status}</span>
  );
}

// Field renders one label/value pair in the summary definition list.
function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="flex flex-col gap-1">
      <dt className="text-xs uppercase tracking-wide text-fg-muted">{label}</dt>
      <dd className="text-sm text-fg">{children}</dd>
    </div>
  );
}

// friendly labels for the known numeric stat keys (SPEC-08 §3 sync stats). Any
// other key is shown with its raw name so new stats are never dropped.
const KNOWN_STAT_LABELS: Record<string, string> = {
  docs_seen: "Documents seen",
  docs_changed: "Documents changed",
  chunks_written: "Chunks written",
  embed_tokens: "Embed tokens",
  duration_ms: "Duration (ms)",
};

// renderStatValue prints scalars directly; the errors[] array is handled
// separately, so any remaining array/object is JSON-stringified rather than
// dropped.
function renderStatValue(value: unknown): string {
  if (value == null) return "—";
  if (typeof value === "object") return JSON.stringify(value);
  return String(value);
}

function Stats({ stats }: { stats: Record<string, unknown> }) {
  const errors = Array.isArray(stats.errors) ? (stats.errors as unknown[]) : null;
  // every stat key except the errors[] array, ordered known-first for stability.
  const entries = Object.entries(stats).filter(([k]) => k !== "errors");
  const known = entries.filter(([k]) => k in KNOWN_STAT_LABELS);
  const extra = entries.filter(([k]) => !(k in KNOWN_STAT_LABELS));
  const ordered = [...known, ...extra];

  return (
    <div className="mt-6 rounded-2xl border border-border px-4 py-4">
      <h2 className="text-sm font-semibold tracking-tight text-fg">Statistics</h2>
      {ordered.length > 0 ? (
        <dl className="mt-4 grid grid-cols-2 gap-4 sm:grid-cols-3">
          {ordered.map(([key, value]) => (
            <Field key={key} label={KNOWN_STAT_LABELS[key] ?? key}>
              {renderStatValue(value)}
            </Field>
          ))}
        </dl>
      ) : null}
      {errors && errors.length > 0 ? (
        <div className="mt-4">
          <p className="text-xs uppercase tracking-wide text-fg-muted">Errors</p>
          <ul className="mt-2 flex flex-col gap-1">
            {errors.map((e, i) => (
              <li key={i} className="text-sm text-danger-text">
                {typeof e === "string" ? e : JSON.stringify(e)}
              </li>
            ))}
          </ul>
        </div>
      ) : null}
    </div>
  );
}

export function JobDetail({ job, onCancel, busy }: JobDetailProps) {
  return (
    <div className="mt-6 flex flex-col gap-6">
      <div className="rounded-2xl border border-border px-4 py-4">
        <div className="flex items-start justify-between gap-4">
          <div className="flex items-center gap-3">
            <span className="font-mono text-sm text-fg">{job.kind}</span>
            <StatusBadge status={job.status} />
          </div>
          {isCancellable(job.status) ? (
            <button
              type="button"
              onClick={() => onCancel(job)}
              disabled={busy}
              className="h-8 rounded-md border border-border px-2.5 text-xs font-medium text-danger-text transition-colors hover:border-danger-text hover:bg-danger-tint focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg disabled:cursor-not-allowed disabled:opacity-50"
            >
              Cancel
            </button>
          ) : null}
        </div>

        <dl className="mt-6 grid grid-cols-2 gap-4 sm:grid-cols-3">
          <Field label="Source">
            <span className="font-mono text-xs">{job.source_id ?? "—"}</span>
          </Field>
          <Field label="Attempt">
            {job.attempt}/{job.max_attempts}
          </Field>
          <Field label="Duration">{humanDuration(job.duration_ms)}</Field>
          <Field label="Queued">{fmt(job.queued_at)}</Field>
          <Field label="Started">{fmt(job.started_at)}</Field>
          <Field label="Finished">{fmt(job.finished_at)}</Field>
          {job.worker_id ? (
            <Field label="Worker">
              <span className="font-mono text-xs">{job.worker_id}</span>
            </Field>
          ) : null}
        </dl>

        {job.error ? (
          <div className="mt-6 rounded-md border border-border bg-danger-tint px-3 py-2 text-sm text-danger-text">
            {job.error}
          </div>
        ) : null}
      </div>

      {job.stats ? <Stats stats={job.stats} /> : null}
    </div>
  );
}
