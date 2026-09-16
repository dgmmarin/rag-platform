"use client";

import { useState } from "react";
import { KEY_SCOPES, type CreateKeyInput, type CreateKeyResult } from "@/lib/apiKeys";

type CreateKeyDialogProps = {
  // onCreate mints the key and returns the plaintext secret once. The page wires
  // this to the create mutation; the dialog owns the form and the reveal panel.
  onCreate: (input: CreateKeyInput) => Promise<CreateKeyResult>;
};

const inputClass =
  "h-9 w-full rounded-md border border-border bg-bg px-3 text-sm text-fg placeholder:text-fg-subtle focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text";

// toRFC3339 converts a date/datetime input value to an RFC3339 timestamp the API
// accepts. Returns undefined for a blank input so `expires_at` is omitted.
function toRFC3339(value: string): string | undefined {
  if (value.trim() === "") return undefined;
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return undefined;
  return d.toISOString();
}

// copyToClipboard feature-detects the Clipboard API — it is undefined in jsdom
// and on non-secure origins — so a missing clipboard is a no-op, not a throw.
async function copyToClipboard(text: string): Promise<void> {
  if (typeof navigator !== "undefined" && navigator.clipboard?.writeText) {
    await navigator.clipboard.writeText(text);
  }
}

export function CreateKeyDialog({ onCreate }: CreateKeyDialogProps) {
  const [name, setName] = useState("");
  const [scopes, setScopes] = useState<string[]>([]);
  const [expiry, setExpiry] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [pending, setPending] = useState(false);
  // created holds the one-time secret after a successful mint. Once set, the
  // form is replaced by the reveal panel; the secret is never stored elsewhere
  // and never refetchable (FR-ACC-04).
  const [created, setCreated] = useState<CreateKeyResult | null>(null);
  const [copied, setCopied] = useState(false);

  function toggleScope(scope: string) {
    setScopes((prev) => (prev.includes(scope) ? prev.filter((s) => s !== scope) : [...prev, scope]));
  }

  async function onSubmit(e: React.FormEvent) {
    e.preventDefault();
    setError(null);
    if (name.trim() === "") {
      setError("Name is required.");
      return;
    }
    if (scopes.length === 0) {
      setError("Select at least one scope.");
      return;
    }
    const input: CreateKeyInput = { name: name.trim(), scopes };
    const expires = toRFC3339(expiry);
    if (expires) input.expires_at = expires;

    setPending(true);
    try {
      const res = await onCreate(input);
      setCreated(res);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Could not create the key.");
    } finally {
      setPending(false);
    }
  }

  // Reveal panel: the plaintext secret, shown once, with a copy button and a
  // clear warning that it cannot be seen again.
  if (created) {
    return (
      <div className="mt-6 flex max-w-xl flex-col gap-4 rounded-2xl border border-border bg-bg-elevated p-5">
        <div className="flex flex-col gap-1">
          <h2 className="text-sm font-semibold text-fg">Key created</h2>
          <p className="text-sm text-danger-text">
            Copy this secret now. You will not be able to see it again.
          </p>
        </div>
        <div className="flex items-center gap-2">
          <code className="flex-1 overflow-x-auto rounded-md border border-border bg-bg px-3 py-2 font-mono text-sm text-fg">
            {created.key}
          </code>
          <button
            type="button"
            onClick={async () => {
              await copyToClipboard(created.key);
              setCopied(true);
            }}
            className="inline-flex h-9 shrink-0 items-center rounded-md border border-border px-3 text-sm font-medium text-fg transition-colors hover:border-border-strong hover:bg-bg-subtle focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg"
          >
            {copied ? "Copied" : "Copy"}
          </button>
        </div>
        <p className="text-xs text-fg-muted">
          Key <span className="font-mono">{created.record.prefix}</span> · scopes{" "}
          <span className="font-mono">{created.record.scopes.join(", ")}</span>
        </p>
      </div>
    );
  }

  return (
    <form onSubmit={onSubmit} className="mt-6 flex max-w-xl flex-col gap-5">
      <div className="flex flex-col gap-1.5">
        <label htmlFor="key-name" className="text-sm font-medium text-fg">
          Name
        </label>
        <input
          id="key-name"
          className={inputClass}
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="e.g. CI pipeline"
        />
      </div>

      <fieldset className="flex flex-col gap-2">
        <legend className="text-sm font-medium text-fg">Scopes</legend>
        {KEY_SCOPES.map((scope) => {
          const id = `scope-${scope}`;
          return (
            <div key={scope} className="flex items-center gap-2">
              <input
                id={id}
                type="checkbox"
                className="h-4 w-4 rounded border-border text-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text"
                checked={scopes.includes(scope)}
                onChange={() => toggleScope(scope)}
              />
              <label htmlFor={id} className="font-mono text-sm text-fg">
                {scope}
              </label>
            </div>
          );
        })}
      </fieldset>

      <div className="flex flex-col gap-1.5">
        <label htmlFor="key-expiry" className="text-sm font-medium text-fg">
          Expiry (optional)
        </label>
        <input
          id="key-expiry"
          type="date"
          className={inputClass}
          value={expiry}
          onChange={(e) => setExpiry(e.target.value)}
        />
        <p className="text-xs text-fg-subtle">Leave blank for a key that never expires.</p>
      </div>

      {error ? (
        <p className="rounded-md border border-border bg-danger-tint px-3 py-2 text-sm text-danger-text">
          {error}
        </p>
      ) : null}

      <div>
        <button
          type="submit"
          disabled={pending}
          className="inline-flex h-9 items-center rounded-md bg-accent px-3 text-sm font-medium text-accent-fg transition-colors hover:bg-accent-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg disabled:cursor-not-allowed disabled:opacity-50"
        >
          {pending ? "Creating…" : "Create key"}
        </button>
      </div>
    </form>
  );
}
