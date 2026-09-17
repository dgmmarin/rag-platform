"use client";

import { useState, type FormEvent } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useAuth } from "@/lib/auth";
import { TenantsTable } from "@/components/TenantsTable";
import {
  createTenant,
  deleteTenant,
  setTenantStatus,
  useTenants,
  type PlatformTenant,
} from "@/lib/tenants";

function LoadingSkeleton() {
  return (
    <div className="mt-6 overflow-hidden rounded-2xl border border-border">
      {[0, 1, 2].map((i) => (
        <div key={i} className="flex items-center gap-4 border-b border-border px-4 py-4 last:border-b-0">
          <div className="h-4 w-48 animate-pulse rounded bg-bg-subtle" />
          <div className="h-4 w-24 animate-pulse rounded bg-bg-subtle" />
          <div className="h-4 w-16 animate-pulse rounded bg-bg-subtle" />
          <div className="ml-auto h-8 w-24 animate-pulse rounded bg-bg-subtle" />
        </div>
      ))}
    </div>
  );
}

export default function TenantsPage() {
  const { me } = useAuth();
  const csrf = me?.csrf_token;
  const qc = useQueryClient();

  const [notice, setNotice] = useState<{ tone: "ok" | "error"; text: string } | null>(null);
  const [slug, setSlug] = useState("");
  const [name, setName] = useState("");
  const [region, setRegion] = useState("");
  const [embeddingDim, setEmbeddingDim] = useState("1536");
  const [addError, setAddError] = useState<string | null>(null);

  const { data, isLoading, isError, error } = useTenants();

  function invalidate() {
    void qc.invalidateQueries({ queryKey: ["platform-tenants"] });
  }

  const enrol = useMutation({
    mutationFn: () =>
      createTenant(csrf, {
        slug: slug.trim(),
        name: name.trim(),
        region: region.trim(),
        embedding_dim: Number(embeddingDim),
      }),
    onSuccess: (r) => {
      setNotice({ tone: "ok", text: `Enrolled "${r.tenant.name}" — provisioning job ${r.job_id}.` });
      setSlug("");
      setName("");
      setRegion("");
      setEmbeddingDim("1536");
      setAddError(null);
      invalidate();
    },
    onError: (e: Error) => setAddError(e.message),
  });

  const suspend = useMutation({
    mutationFn: (t: PlatformTenant) => setTenantStatus(t.id, csrf, "suspended"),
    onSuccess: (_r, t) => {
      setNotice({ tone: "ok", text: `Suspended "${t.name}".` });
      invalidate();
    },
    onError: (e: Error) => setNotice({ tone: "error", text: e.message }),
  });

  const activate = useMutation({
    mutationFn: (t: PlatformTenant) => setTenantStatus(t.id, csrf, "active"),
    onSuccess: (_r, t) => {
      setNotice({ tone: "ok", text: `Activated "${t.name}".` });
      invalidate();
    },
    onError: (e: Error) => setNotice({ tone: "error", text: e.message }),
  });

  const remove = useMutation({
    mutationFn: (t: PlatformTenant) => deleteTenant(t.id, csrf),
    onSuccess: (_r, t) => {
      setNotice({ tone: "ok", text: `Scheduled "${t.name}" for deletion.` });
      invalidate();
    },
    onError: (e: Error) => setNotice({ tone: "error", text: e.message }),
  });

  const busyId: string | null =
    (suspend.isPending ? suspend.variables?.id : undefined) ??
    (activate.isPending ? activate.variables?.id : undefined) ??
    (remove.isPending ? remove.variables?.id : undefined) ??
    null;

  function onEnrol(e: FormEvent) {
    e.preventDefault();
    setAddError(null);
    enrol.mutate();
  }

  // A direct visit by a non-platform-admin still gets a clear message; the server
  // enforces the real gate (RequirePlatformAdmin) regardless.
  if (me && !me.is_platform_admin) {
    return (
      <div className="flex flex-col gap-1">
        <p className="font-mono text-xs uppercase tracking-wide text-fg-muted">/admin/tenants</p>
        <h1 className="text-lg font-semibold tracking-tight text-fg">Tenants</h1>
        <div className="mt-6 rounded-2xl border border-border bg-bg-subtle px-4 py-4 text-sm text-fg-muted">
          Only platform admins can manage tenants.
        </div>
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-1">
      <p className="font-mono text-xs uppercase tracking-wide text-fg-muted">/admin/tenants</p>
      <h1 className="text-lg font-semibold tracking-tight text-fg">Tenants</h1>

      <form
        onSubmit={onEnrol}
        className="mt-4 flex flex-wrap items-end gap-3 rounded-2xl border border-border bg-bg-subtle px-4 py-4"
      >
        <div className="flex flex-col gap-1">
          <label htmlFor="t-slug" className="text-xs font-medium text-fg-muted">
            Slug
          </label>
          <input
            id="t-slug"
            required
            value={slug}
            onChange={(e) => setSlug(e.target.value)}
            placeholder="acme"
            className="h-9 w-40 rounded-md border border-border bg-bg px-3 text-sm text-fg placeholder:text-fg-subtle focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg"
          />
        </div>
        <div className="flex flex-col gap-1">
          <label htmlFor="t-name" className="text-xs font-medium text-fg-muted">
            Name
          </label>
          <input
            id="t-name"
            required
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="Acme Inc."
            className="h-9 w-56 rounded-md border border-border bg-bg px-3 text-sm text-fg placeholder:text-fg-subtle focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg"
          />
        </div>
        <div className="flex flex-col gap-1">
          <label htmlFor="t-region" className="text-xs font-medium text-fg-muted">
            Region
          </label>
          <input
            id="t-region"
            required
            value={region}
            onChange={(e) => setRegion(e.target.value)}
            placeholder="eu-west-1"
            className="h-9 w-40 rounded-md border border-border bg-bg px-3 text-sm text-fg placeholder:text-fg-subtle focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg"
          />
        </div>
        <div className="flex flex-col gap-1">
          <label htmlFor="t-dim" className="text-xs font-medium text-fg-muted">
            Embedding dim
          </label>
          <input
            id="t-dim"
            type="number"
            min={1}
            required
            value={embeddingDim}
            onChange={(e) => setEmbeddingDim(e.target.value)}
            className="h-9 w-32 rounded-md border border-border bg-bg px-3 text-sm text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg"
          />
        </div>
        <button
          type="submit"
          disabled={enrol.isPending}
          className="inline-flex h-9 items-center rounded-md bg-accent px-3 text-sm font-medium text-accent-fg transition-colors hover:bg-accent-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg disabled:cursor-not-allowed disabled:opacity-50"
        >
          Enrol tenant
        </button>
        {addError ? <p className="w-full text-sm text-danger-text">{addError}</p> : null}
      </form>

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

      {isLoading ? (
        <LoadingSkeleton />
      ) : isError ? (
        <div className="mt-6 rounded-2xl border border-border bg-danger-tint px-4 py-4 text-sm text-danger-text">
          Could not load tenants: {error instanceof Error ? error.message : "unknown error"}
        </div>
      ) : (
        <TenantsTable
          tenants={data?.items ?? []}
          onSuspend={(t) => suspend.mutate(t)}
          onActivate={(t) => activate.mutate(t)}
          onDelete={(t) => {
            if (window.confirm(`Schedule "${t.name}" for deletion? Its data is removed after the grace window.`)) {
              remove.mutate(t);
            }
          }}
          busyId={busyId}
        />
      )}
    </div>
  );
}
