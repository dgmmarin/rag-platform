"use client";

import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useAuth } from "@/lib/auth";
import { useTenant } from "@/lib/tenant";
import { ApiKeysTable } from "@/components/ApiKeysTable";
import { CreateKeyDialog } from "@/components/CreateKeyDialog";
import {
  createApiKey,
  revokeApiKey,
  useApiKeys,
  type ApiKey,
  type CreateKeyInput,
} from "@/lib/apiKeys";

function LoadingSkeleton() {
  return (
    <div className="mt-6 overflow-hidden rounded-2xl border border-border">
      {[0, 1, 2].map((i) => (
        <div key={i} className="flex items-center gap-4 border-b border-border px-4 py-4 last:border-b-0">
          <div className="h-4 w-40 animate-pulse rounded bg-bg-subtle" />
          <div className="h-4 w-24 animate-pulse rounded bg-bg-subtle" />
          <div className="h-4 w-20 animate-pulse rounded bg-bg-subtle" />
          <div className="ml-auto h-8 w-20 animate-pulse rounded bg-bg-subtle" />
        </div>
      ))}
    </div>
  );
}

export default function ApiKeysPage() {
  const { me } = useAuth();
  const { current } = useTenant();
  const tenantId = current?.id;
  const csrf = me?.csrf_token;
  const qc = useQueryClient();

  const [notice, setNotice] = useState<{ tone: "ok" | "error"; text: string } | null>(null);
  const { data, isLoading, isError, error } = useApiKeys();

  function invalidate() {
    void qc.invalidateQueries({ queryKey: ["api-keys", tenantId] });
  }

  const create = useMutation({
    mutationFn: (input: CreateKeyInput) => createApiKey(tenantId as string, csrf, input),
    onSuccess: (res) => {
      setNotice({ tone: "ok", text: `Key "${res.record.name}" created.` });
      invalidate();
    },
    // The dialog surfaces the create error inline; the notice covers success.
  });

  const revoke = useMutation({
    mutationFn: (k: ApiKey) => revokeApiKey(tenantId as string, k.id, csrf),
    onSuccess: (_r, k) => {
      setNotice({ tone: "ok", text: `Key "${k.name}" revoked.` });
      invalidate();
    },
    onError: (e: Error) => setNotice({ tone: "error", text: e.message }),
  });

  const busyId: string | null = (revoke.isPending ? revoke.variables?.id : undefined) ?? null;

  return (
    <div className="flex flex-col gap-1">
      <p className="font-mono text-xs uppercase tracking-wide text-fg-muted">/admin/keys</p>
      <h1 className="text-lg font-semibold tracking-tight text-fg">API Keys</h1>
      <p className="max-w-xl text-sm text-fg-muted">
        Keys let a service query or ingest for this tenant. A new key&rsquo;s secret is shown once
        and cannot be retrieved later.
      </p>

      {notice ? (
        <p
          className={`mt-4 rounded-md border px-3 py-2 text-sm ${
            notice.tone === "error"
              ? "border-border bg-danger-tint text-danger-text"
              : "border-border bg-accent-tint text-accent-text"
          }`}
        >
          {notice.text}
        </p>
      ) : null}

      <CreateKeyDialog onCreate={(input) => create.mutateAsync(input)} />

      {isLoading ? (
        <LoadingSkeleton />
      ) : isError ? (
        <div className="mt-6 rounded-2xl border border-border bg-danger-tint px-4 py-4 text-sm text-danger-text">
          Could not load API keys: {error instanceof Error ? error.message : "unknown error"}
        </div>
      ) : (
        <ApiKeysTable keys={data ?? []} onRevoke={(k) => revoke.mutate(k)} busyId={busyId} />
      )}
    </div>
  );
}
