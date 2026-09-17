type PaginationProps = {
  page: number; // 0-based
  pageCount: number;
  total: number;
  onPrev: () => void;
  onNext: () => void;
};

// Pagination renders the shared prev / page-indicator / next control for the admin
// tables. It hides itself when everything fits on one page.
export function Pagination({ page, pageCount, total, onPrev, onNext }: PaginationProps) {
  if (pageCount <= 1) return null;
  const btn =
    "h-8 rounded-md border border-border px-2.5 text-xs font-medium text-fg transition-colors hover:border-border-strong hover:bg-bg-subtle focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg disabled:cursor-not-allowed disabled:opacity-50";
  return (
    <div className="mt-4 flex items-center justify-between text-xs text-fg-muted">
      <span>{total} total</span>
      <div className="flex items-center gap-2">
        <button type="button" onClick={onPrev} disabled={page === 0} className={btn}>
          Previous
        </button>
        <span>
          Page {page + 1} of {pageCount}
        </span>
        <button type="button" onClick={onNext} disabled={page >= pageCount - 1} className={btn}>
          Next
        </button>
      </div>
    </div>
  );
}
