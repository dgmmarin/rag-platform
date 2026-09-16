"use client";

import { useState } from "react";
import { useRouter } from "next/navigation";
import { useMutation } from "@tanstack/react-query";
import { useAuth } from "@/lib/auth";
import { useTenant } from "@/lib/tenant";
import { useConnectorKinds } from "@/lib/connectorKinds";
import {
  createSource,
  updateSource,
  testSource,
  type ConnectorField,
  type ConnectorKind,
  type Source,
  type SourceInput,
} from "@/lib/sources";

// FieldValue is what one input holds while editing: a string for
// text/url/number/secret, a boolean for a checkbox (bool).
type FieldValue = string | boolean;

// initialValue is the pre-fill for one field. Non-secret fields on edit come
// from the source's config; secret fields are always blank (the API never
// returns a stored secret — it is write-only). Create mode starts empty.
function initialValue(field: ConnectorField, source?: Source): FieldValue {
  if (field.type === "bool") return Boolean(source?.config?.[field.name]);
  if (field.type === "secret" || !source) return "";
  const v = source.config?.[field.name];
  if (v == null) return "";
  // stringlist edits as one entry per line; json edits as pretty-printed text.
  if (field.type === "stringlist") return Array.isArray(v) ? (v as unknown[]).join("\n") : String(v);
  if (field.type === "json") return typeof v === "string" ? v : JSON.stringify(v, null, 2);
  return String(v);
}

function fieldInputType(type: ConnectorField["type"]): string {
  switch (type) {
    case "url":
      return "url";
    case "number":
      return "number";
    case "secret":
      return "password";
    default:
      return "text";
  }
}

const inputClass =
  "h-9 w-full rounded-md border border-border bg-bg px-3 text-sm text-fg placeholder:text-fg-subtle focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text";

const textareaClass =
  "w-full rounded-md border border-border bg-bg px-3 py-2 text-sm text-fg placeholder:text-fg-subtle focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text";

export function SourceForm({ source }: { source?: Source }) {
  const editing = !!source;
  const router = useRouter();
  const csrf = useAuth().me?.csrf_token;
  const tenantId = useTenant().current?.id;
  const kindsQuery = useConnectorKinds();
  const kinds: ConnectorKind[] = kindsQuery.data ?? [];

  const [kind, setKind] = useState<string>(source?.kind ?? "");
  const [name, setName] = useState<string>(source?.name ?? "");
  // values holds edits keyed by field name; a field not present here falls back
  // to its initial value, so pre-fill works even before the schema loads.
  const [values, setValues] = useState<Record<string, FieldValue>>({});
  const [formError, setFormError] = useState<string | null>(null);
  const [testResult, setTestResult] = useState<{ ok: boolean; text: string } | null>(null);

  const selected = kinds.find((k) => k.kind === kind);
  const fields = selected?.fields ?? [];

  function valueOf(field: ConnectorField): FieldValue {
    return field.name in values ? values[field.name] : initialValue(field, source);
  }

  function setValue(field: ConnectorField, v: FieldValue) {
    setValues((prev) => ({ ...prev, [field.name]: v }));
  }

  const save = useMutation({
    mutationFn: (input: SourceInput) =>
      editing
        ? updateSource(tenantId as string, source!.id, csrf, {
            name: input.name,
            config: input.config,
            credentials: input.credentials,
          })
        : createSource(tenantId as string, csrf, input),
    onSuccess: () => router.push("/admin/sources"),
    onError: (e: Error) => setFormError(e.message),
  });

  const test = useMutation({
    mutationFn: () => testSource(tenantId as string, source!.id, csrf),
    onSuccess: () => setTestResult({ ok: true, text: "Connection test passed." }),
    onError: (e: Error) => setTestResult({ ok: false, text: e.message }),
  });

  function assemble(): { input: SourceInput; missing: string[]; invalid: string[] } {
    const config: Record<string, unknown> = {};
    const credentials: Record<string, string> = {};
    const missing: string[] = [];
    const invalid: string[] = []; // fields whose JSON did not parse

    for (const field of fields) {
      const v = valueOf(field);
      if (field.type === "secret") {
        const s = typeof v === "string" ? v : "";
        // On edit a blank secret means "leave unchanged" — omit it.
        if (s.trim() !== "") credentials[field.name] = s;
        else if (field.required && !editing) missing.push(field.label);
        continue;
      }
      if (field.type === "bool") {
        config[field.name] = Boolean(v);
        continue;
      }
      const s = typeof v === "string" ? v : "";
      if (field.type === "stringlist") {
        // One entry per line; blanks trimmed away. The config value is an ARRAY,
        // not a string (the connector's ValidateConfig requires it — ISSUE-0061).
        const items = s
          .split("\n")
          .map((line) => line.trim())
          .filter((line) => line !== "");
        if (items.length === 0) {
          if (field.required) missing.push(field.label);
          continue;
        }
        config[field.name] = items;
        continue;
      }
      if (field.type === "json") {
        if (s.trim() === "") {
          if (field.required) missing.push(field.label);
          continue;
        }
        try {
          config[field.name] = JSON.parse(s);
        } catch {
          invalid.push(field.label);
        }
        continue;
      }
      if (field.type === "number") {
        if (s.trim() === "") {
          if (field.required) missing.push(field.label);
          continue;
        }
        config[field.name] = Number(s);
        continue;
      }
      if (s.trim() === "" && field.required) missing.push(field.label);
      config[field.name] = s;
    }

    const input: SourceInput = { kind, name, config };
    if (Object.keys(credentials).length > 0) input.credentials = credentials;
    return { input, missing, invalid };
  }

  function onSubmit(e: React.FormEvent) {
    e.preventDefault();
    setFormError(null);
    if (name.trim() === "") {
      setFormError("Name is required.");
      return;
    }
    if (!editing && kind === "") {
      setFormError("Select a connector kind.");
      return;
    }
    const { input, missing, invalid } = assemble();
    if (invalid.length > 0) {
      setFormError(`Enter valid JSON for: ${invalid.join(", ")}.`);
      return;
    }
    if (missing.length > 0) {
      setFormError(`Fill the required fields: ${missing.join(", ")}.`);
      return;
    }
    save.mutate(input);
  }

  return (
    <form onSubmit={onSubmit} className="mt-6 flex max-w-xl flex-col gap-5">
      {/* Name */}
      <div className="flex flex-col gap-1.5">
        <label htmlFor="source-name" className="text-sm font-medium text-fg">
          Name
        </label>
        <input
          id="source-name"
          className={inputClass}
          value={name}
          onChange={(e) => setName(e.target.value)}
          required
        />
      </div>

      {/* Kind: a picker on create, read-only on edit (kind is immutable). */}
      {editing ? (
        <div className="flex flex-col gap-1.5">
          <span className="text-sm font-medium text-fg">Connector kind</span>
          <p className="font-mono text-sm text-fg-muted">{selected?.label ?? kind}</p>
        </div>
      ) : (
        <div className="flex flex-col gap-1.5">
          <label htmlFor="source-kind" className="text-sm font-medium text-fg">
            Connector kind
          </label>
          <select
            id="source-kind"
            className={inputClass}
            value={kind}
            onChange={(e) => {
              setKind(e.target.value);
              setValues({});
              setFormError(null);
            }}
          >
            <option value="">Select a kind…</option>
            {kinds.map((k) => (
              <option key={k.kind} value={k.kind}>
                {k.label}
              </option>
            ))}
          </select>
        </div>
      )}

      {/* Schema-driven fields for the selected kind. */}
      {fields.map((field) => {
        const id = `field-${field.name}`;
        const v = valueOf(field);
        if (field.type === "bool") {
          return (
            <div key={field.name} className="flex items-center gap-2">
              <input
                id={id}
                type="checkbox"
                className="h-4 w-4 rounded border-border text-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text"
                checked={Boolean(v)}
                onChange={(e) => setValue(field, e.target.checked)}
              />
              <label htmlFor={id} className="text-sm font-medium text-fg">
                {field.label}
              </label>
            </div>
          );
        }
        const labelBlock = (
          <div className="flex items-center gap-1">
            <label htmlFor={id} className="text-sm font-medium text-fg">
              {field.label}
            </label>
            {field.required ? (
              <span aria-hidden className="text-danger-text">
                *
              </span>
            ) : null}
          </div>
        );
        // stringlist (array of strings, one per line) and json (raw JSON
        // object/array) render as a textarea and submit a composite value, not a
        // scalar string — a scalar would fail the connector's ValidateConfig
        // ("got string, want array", ISSUE-0061).
        if (field.type === "stringlist" || field.type === "json") {
          const isJSON = field.type === "json";
          return (
            <div key={field.name} className="flex flex-col gap-1.5">
              {labelBlock}
              <textarea
                id={id}
                className={`${textareaClass} ${isJSON ? "font-mono" : ""}`}
                rows={isJSON ? 6 : 3}
                value={typeof v === "string" ? v : ""}
                onChange={(e) => setValue(field, e.target.value)}
                required={field.required}
                placeholder={isJSON ? '{ "type": "bearer" }' : "One entry per line"}
              />
              <p className="text-xs text-fg-subtle">
                {isJSON ? "Enter valid JSON." : "One entry per line."}
              </p>
            </div>
          );
        }
        return (
          <div key={field.name} className="flex flex-col gap-1.5">
            {labelBlock}
            <input
              id={id}
              type={fieldInputType(field.type)}
              className={inputClass}
              value={typeof v === "string" ? v : ""}
              onChange={(e) => setValue(field, e.target.value)}
              // A required secret on edit may stay blank (unchanged), so only the
              // browser-required attribute is dropped there; create still checks.
              required={field.required && !(field.type === "secret" && editing)}
              autoComplete={field.type === "secret" ? "new-password" : undefined}
              placeholder={field.type === "secret" && editing ? "Leave blank to keep current" : undefined}
            />
          </div>
        );
      })}

      {formError ? (
        <p className="rounded-md border border-border bg-danger-tint px-3 py-2 text-sm text-danger-text">
          {formError}
        </p>
      ) : null}

      {testResult ? (
        <p
          className={`rounded-md border border-border px-3 py-2 text-sm ${
            testResult.ok ? "bg-accent-tint text-accent-text" : "bg-danger-tint text-danger-text"
          }`}
        >
          {testResult.text}
        </p>
      ) : null}

      <div className="flex items-center gap-2">
        <button
          type="submit"
          disabled={save.isPending}
          className="inline-flex h-9 items-center rounded-md bg-accent px-3 text-sm font-medium text-accent-fg transition-colors hover:bg-accent-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg disabled:cursor-not-allowed disabled:opacity-50"
        >
          {editing ? "Save" : "Create"}
        </button>

        {/* Test connection only makes sense once the source exists (edit mode). */}
        {editing ? (
          <button
            type="button"
            onClick={() => {
              setTestResult(null);
              test.mutate();
            }}
            disabled={test.isPending}
            className="inline-flex h-9 items-center rounded-md border border-border px-3 text-sm font-medium text-fg transition-colors hover:border-border-strong hover:bg-bg-subtle focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg disabled:cursor-not-allowed disabled:opacity-50"
          >
            {test.isPending ? "Testing…" : "Test connection"}
          </button>
        ) : null}
      </div>
    </form>
  );
}
