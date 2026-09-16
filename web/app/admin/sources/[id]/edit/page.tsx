"use client";

import Link from "next/link";
import { useParams } from "next/navigation";
import { useQuery } from "@tanstack/react-query";
import { useTenant } from "@/lib/tenant";
import { SourceForm } from "@/components/SourceForm";
import { getSource } from "@/lib/sources";

export default function EditSourcePage() {
  const params = useParams<{ id: string }>();
  const id = params.id;
  const tenantId = useTenant().current?.id;

  const { data, isLoading, isError, error } = useQuery({
    queryKey: ["source", tenantId, id],
    queryFn: () => getSource(tenantId as string, id),
    enabled: !!tenantId && !!id,
  });

  return (
    <div className="flex flex-col gap-1">
      <p className="font-mono text-xs uppercase tracking-wide text-fg-muted">
        /admin/sources/{id}/edit
      </p>

      <div className="flex items-center justify-between gap-4">
        <h1 className="text-lg font-semibold tracking-tight text-fg">Edit source</h1>
        <Link
          href="/admin/sources"
          className="text-sm font-medium text-fg-muted transition-colors hover:text-fg"
        >
          Back to sources
        </Link>
      </div>

      {isLoading ? (
        <div className="mt-6 h-40 animate-pulse rounded-2xl border border-border bg-bg-subtle" />
      ) : isError ? (
        <div className="mt-6 rounded-2xl border border-border bg-danger-tint px-4 py-4 text-sm text-danger-text">
          Could not load the source: {error instanceof Error ? error.message : "unknown error"}
        </div>
      ) : data ? (
        <SourceForm source={data} />
      ) : null}
    </div>
  );
}
