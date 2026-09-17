import { useMemo, useState } from "react";

// usePagination slices a loaded list into fixed-size pages for the admin tables.
// It is client-side: it paginates whatever the list query returned (the tables'
// datasets are bounded per tenant). The effective page is clamped into range at
// render time (no effect), so a shrinking list — a filter or refetch — never shows
// an out-of-range empty page.
export function usePagination<T>(items: T[], pageSize = 20) {
  const [page, setPage] = useState(0);
  const total = items.length;
  const pageCount = Math.max(1, Math.ceil(total / pageSize));
  const current = Math.min(Math.max(page, 0), pageCount - 1);

  const pageItems = useMemo(
    () => items.slice(current * pageSize, current * pageSize + pageSize),
    [items, current, pageSize],
  );

  return {
    page: current,
    pageCount,
    total,
    pageItems,
    setPage,
    next: () => setPage(Math.min(current + 1, pageCount - 1)),
    prev: () => setPage(Math.max(current - 1, 0)),
  };
}
