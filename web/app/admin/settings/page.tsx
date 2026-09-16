"use client";

import { SettingsForm } from "@/components/SettingsForm";

export default function SettingsPage() {
  return (
    <div className="flex flex-col gap-1">
      <p className="font-mono text-xs uppercase tracking-wide text-fg-muted">/admin/settings</p>
      <h1 className="text-lg font-semibold tracking-tight text-fg">Settings</h1>
      <p className="max-w-2xl text-sm text-fg-muted">
        Tune retrieval, models, and limits for this tenant. The embedding dimension is fixed at
        provisioning and can change only through a reindex.
      </p>
      <SettingsForm />
    </div>
  );
}
