import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { listConnectorKinds, type ConnectorKind } from "./sources";

// useConnectorKinds loads the platform-global connector-kind form schemas
// (GET /admin/connector-kinds). The list is the same for every tenant, so the
// query is not tenant-scoped and its key carries no tenant id. The fetch and the
// ConnectorKind type stay in sources.ts as the single source of truth; this hook
// only wraps them for the schema-driven source form (STORY-11.2, SPEC-11 §10).
export function useConnectorKinds(): UseQueryResult<ConnectorKind[], Error> {
  return useQuery({
    queryKey: ["connector-kinds"],
    queryFn: listConnectorKinds,
  });
}
