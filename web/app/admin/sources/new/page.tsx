"use client";

import Link from "next/link";
import { SourceForm } from "@/components/SourceForm";

export default function NewSourcePage() {
  return (
    <div className="flex flex-col gap-1">
      <p className="font-mono text-xs uppercase tracking-wide text-fg-muted">/admin/sources/new</p>

      <div className="flex items-center justify-between gap-4">
        <h1 className="text-lg font-semibold tracking-tight text-fg">New source</h1>
        <Link
          href="/admin/sources"
          className="text-sm font-medium text-fg-muted transition-colors hover:text-fg"
        >
          Back to sources
        </Link>
      </div>

      <SourceForm />
    </div>
  );
}
