import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { apiFetch } from "./api";
import { useTenant } from "./tenant";

// The types mirror the control-plane JSON (internal/documents/documents.go). The
// documents browser is read-only (FR-ADM-03): list, detail with current-version
// metadata, and the chunk debug view. Upload/delete stay on the Bearer surface.
export type DocumentStatus = "active" | "deleted";

export type Document = {
  id: string;
  source_id: string;
  external_id: string;
  title?: string;
  uri?: string;
  mime_type?: string;
  status: DocumentStatus;
  current_version?: string;
  metadata?: Record<string, unknown>;
  first_seen_at: string;
  last_seen_at: string;
  deleted_at?: string;
};

// VersionMeta is the current-version metadata. `content` is present only when the
// detail was fetched with ?content=true (not used by the browser's default view).
export type VersionMeta = {
  id: string;
  content_hash: string;
  char_count: number;
  parser?: string;
  created_at: string;
  content?: string;
};

export type DocumentDetail = Document & { current_version_meta?: VersionMeta };

// Chunk is the debug view; the embedding vector is never returned by the API.
export type Chunk = {
  id: string;
  position: number;
  heading_path: string[];
  content: string;
  token_count: number;
  embedding_model: string;
  metadata?: Record<string, unknown>;
  created_at: string;
};

export type DocumentListPage = { items: Document[]; next_cursor?: string };
export type ChunkPage = { items: Chunk[]; next_cursor?: string };

// DocumentFilter carries the optional list filters (SPEC-07 §2: source, status,
// q). An empty filter lists the whole tenant, newest first.
export type DocumentFilter = {
  source?: string;
  status?: DocumentStatus;
  q?: string;
};

// ensureOk turns any non-2xx into an Error carrying the envelope's error.message
// (falling back to the status). apiFetch already throws Unauthorized on 401.
async function ensureOk(res: Response): Promise<Response> {
  if (res.ok) return res;
  let message = `request failed with status ${res.status}`;
  try {
    const body = (await res.json()) as { error?: { message?: string } };
    if (body?.error?.message) message = body.error.message;
  } catch {
    // non-JSON body — keep the status-based default
  }
  throw new Error(message);
}

function base(tenantId: string): string {
  return `/admin/tenants/${tenantId}/documents`;
}

export async function listDocuments(
  tenantId: string,
  filter: DocumentFilter = {},
  opts: { limit?: number; cursor?: string } = {},
): Promise<DocumentListPage> {
  const q = new URLSearchParams();
  if (filter.source) q.set("source", filter.source);
  if (filter.status) q.set("status", filter.status);
  if (filter.q) q.set("q", filter.q);
  if (opts.limit != null) q.set("limit", String(opts.limit));
  if (opts.cursor) q.set("cursor", opts.cursor);
  const qs = q.toString();
  const res = await ensureOk(await apiFetch(`${base(tenantId)}${qs ? `?${qs}` : ""}`));
  return (await res.json()) as DocumentListPage;
}

// listAllDocuments follows next_cursor to load EVERY matching document, so the
// admin table (which paginates client-side, usePagination) shows the true total
// instead of just the server's first page. Requests the server max (200) per round
// trip. ponytail: capped at 100 pages (20k docs); a larger corpus should move the
// table to true server-side cursor paging rather than lifting this cap.
export async function listAllDocuments(
  tenantId: string,
  filter: DocumentFilter = {},
): Promise<DocumentListPage> {
  const items: Document[] = [];
  let cursor: string | undefined;
  for (let i = 0; i < 100; i++) {
    const page = await listDocuments(tenantId, filter, { limit: 200, cursor });
    items.push(...page.items);
    if (!page.next_cursor) break;
    cursor = page.next_cursor;
  }
  return { items };
}

export async function getDocument(tenantId: string, id: string): Promise<DocumentDetail> {
  const res = await ensureOk(await apiFetch(`${base(tenantId)}/${id}`));
  return (await res.json()) as DocumentDetail;
}

export async function getChunks(
  tenantId: string,
  id: string,
  opts: { limit?: number; cursor?: string } = {},
): Promise<ChunkPage> {
  const q = new URLSearchParams();
  if (opts.limit != null) q.set("limit", String(opts.limit));
  if (opts.cursor) q.set("cursor", opts.cursor);
  const qs = q.toString();
  const res = await ensureOk(await apiFetch(`${base(tenantId)}/${id}/chunks${qs ? `?${qs}` : ""}`));
  return (await res.json()) as ChunkPage;
}

// useDocuments loads the current tenant's documents for the given filter, keyed by
// tenant and filter so switching either refetches; disabled until a tenant is known.
export function useDocuments(filter: DocumentFilter = {}): UseQueryResult<DocumentListPage, Error> {
  const tenantId = useTenant().current?.id;
  return useQuery({
    queryKey: ["documents", tenantId, filter],
    queryFn: () => listAllDocuments(tenantId as string, filter),
    enabled: !!tenantId,
  });
}

// useDocument loads one document by id, keyed by tenant and id.
export function useDocument(id: string): UseQueryResult<DocumentDetail, Error> {
  const tenantId = useTenant().current?.id;
  return useQuery({
    queryKey: ["document", tenantId, id],
    queryFn: () => getDocument(tenantId as string, id),
    enabled: !!tenantId && !!id,
  });
}

// useChunks loads one document's chunks, keyed by tenant and id.
export function useChunks(id: string): UseQueryResult<ChunkPage, Error> {
  const tenantId = useTenant().current?.id;
  return useQuery({
    queryKey: ["chunks", tenantId, id],
    queryFn: () => getChunks(tenantId as string, id),
    enabled: !!tenantId && !!id,
  });
}
