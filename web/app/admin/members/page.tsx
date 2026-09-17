"use client";

import { useState, type FormEvent } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useAuth } from "@/lib/auth";
import { useTenant } from "@/lib/tenant";
import { MembersTable } from "@/components/MembersTable";
import { Pagination } from "@/components/Pagination";
import { usePagination } from "@/lib/pagination";
import {
  addMember,
  removeMember,
  setMemberRole,
  useMembers,
  ROLES,
  type Member,
  type Role,
} from "@/lib/members";

function LoadingSkeleton() {
  return (
    <div className="mt-6 overflow-hidden rounded-2xl border border-border">
      {[0, 1, 2].map((i) => (
        <div key={i} className="flex items-center gap-4 border-b border-border px-4 py-4 last:border-b-0">
          <div className="h-4 w-56 animate-pulse rounded bg-bg-subtle" />
          <div className="h-8 w-28 animate-pulse rounded bg-bg-subtle" />
          <div className="ml-auto h-8 w-20 animate-pulse rounded bg-bg-subtle" />
        </div>
      ))}
    </div>
  );
}

export default function MembersPage() {
  const { me } = useAuth();
  const { current } = useTenant();
  const tenantId = current?.id;
  const csrf = me?.csrf_token;
  const qc = useQueryClient();

  const [notice, setNotice] = useState<{ tone: "ok" | "error"; text: string } | null>(null);
  const [email, setEmail] = useState("");
  const [role, setRole] = useState<Role>("viewer");
  const [addError, setAddError] = useState<string | null>(null);

  const { data, isLoading, isError, error } = useMembers();
  const paged = usePagination(data ?? []);

  function invalidate() {
    void qc.invalidateQueries({ queryKey: ["members", tenantId] });
  }

  const add = useMutation({
    mutationFn: () => addMember(tenantId as string, csrf, { email: email.trim(), role }),
    onSuccess: (m) => {
      setNotice({ tone: "ok", text: `Added "${m.email}" as ${m.role}.` });
      setEmail("");
      setRole("viewer");
      setAddError(null);
      invalidate();
    },
    // The add form surfaces the server message inline (404: user must sign up
    // first; 409: already a member; 400: invalid role) rather than in the banner.
    onError: (e: Error) => setAddError(e.message),
  });

  const setRoleMut = useMutation({
    mutationFn: (v: { member: Member; role: Role }) =>
      setMemberRole(tenantId as string, v.member.userId, csrf, v.role),
    onSuccess: (_r, v) => {
      setNotice({ tone: "ok", text: `Changed "${v.member.email}" to ${v.role}.` });
      invalidate();
    },
    onError: (e: Error) => setNotice({ tone: "error", text: e.message }),
  });

  const remove = useMutation({
    mutationFn: (m: Member) => removeMember(tenantId as string, m.userId, csrf),
    onSuccess: (_r, m) => {
      setNotice({ tone: "ok", text: `Removed "${m.email}".` });
      invalidate();
    },
    onError: (e: Error) => setNotice({ tone: "error", text: e.message }),
  });

  const busyId: string | null =
    (setRoleMut.isPending ? setRoleMut.variables?.member.userId : undefined) ??
    (remove.isPending ? remove.variables?.userId : undefined) ??
    null;

  function onSubmit(e: FormEvent) {
    e.preventDefault();
    setAddError(null);
    add.mutate();
  }

  return (
    <div className="flex flex-col gap-1">
      <p className="font-mono text-xs uppercase tracking-wide text-fg-muted">/admin/members</p>
      <h1 className="text-lg font-semibold tracking-tight text-fg">Members</h1>

      <form
        onSubmit={onSubmit}
        className="mt-4 flex flex-wrap items-end gap-3 rounded-2xl border border-border bg-bg-subtle px-4 py-4"
      >
        <div className="flex flex-col gap-1">
          <label htmlFor="add-email" className="text-xs font-medium text-fg-muted">
            Email
          </label>
          <input
            id="add-email"
            type="email"
            required
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            placeholder="person@example.com"
            className="h-9 w-64 rounded-md border border-border bg-bg px-3 text-sm text-fg placeholder:text-fg-subtle focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg"
          />
        </div>
        <div className="flex flex-col gap-1">
          <label htmlFor="add-role" className="text-xs font-medium text-fg-muted">
            Role
          </label>
          <select
            id="add-role"
            value={role}
            onChange={(e) => setRole(e.target.value as Role)}
            className="h-9 rounded-md border border-border bg-bg px-2 text-sm text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg"
          >
            {ROLES.map((r) => (
              <option key={r} value={r}>
                {r}
              </option>
            ))}
          </select>
        </div>
        <button
          type="submit"
          disabled={add.isPending}
          className="inline-flex h-9 items-center rounded-md bg-accent px-3 text-sm font-medium text-accent-fg transition-colors hover:bg-accent-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg disabled:cursor-not-allowed disabled:opacity-50"
        >
          Add member
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
          Could not load members: {error instanceof Error ? error.message : "unknown error"}
        </div>
      ) : (
        <>
          <MembersTable
            members={paged.pageItems}
            onSetRole={(member, newRole) => setRoleMut.mutate({ member, role: newRole })}
            onRemove={(m) => remove.mutate(m)}
            busyId={busyId}
          />
          <Pagination
            page={paged.page}
            pageCount={paged.pageCount}
            total={paged.total}
            onPrev={paged.prev}
            onNext={paged.next}
          />
        </>
      )}
    </div>
  );
}
