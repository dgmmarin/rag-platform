"use client";

import { useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { useAuth } from "@/lib/auth";
import { useTenant } from "@/lib/tenant";
import {
  updateSettings,
  useSettings,
  type FieldError,
  type Settings,
  type SettingsPatch,
} from "@/lib/settings";

const inputClass =
  "h-9 w-full rounded-md border border-border bg-bg px-3 text-sm text-fg placeholder:text-fg-subtle focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text";

const readOnlyClass =
  "h-9 w-full rounded-md border border-border bg-bg-subtle px-3 text-sm text-fg-muted focus-visible:outline-none";

const textareaClass =
  "w-full rounded-md border border-border bg-bg px-3 py-2 text-sm text-fg placeholder:text-fg-subtle focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text";

// FieldErr renders the per-field message for a dotted settings path, if any.
function FieldErr({ error }: { error?: string }) {
  if (!error) return null;
  return <p className="text-xs text-danger-text">{error}</p>;
}

// Help renders the muted one-line description under a field's control.
function Help({ text }: { text?: string }) {
  if (!text) return null;
  return <p className="text-xs text-fg-subtle">{text}</p>;
}

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section className="flex flex-col gap-4 border-t border-border pt-5">
      <h2 className="text-sm font-semibold tracking-tight text-fg">{title}</h2>
      <div className="grid gap-4 sm:grid-cols-2">{children}</div>
    </section>
  );
}

function TextRow(props: {
  id: string;
  label: string;
  value: string;
  onChange: (v: string) => void;
  error?: string;
  readOnly?: boolean;
  description?: string;
}) {
  return (
    <div className="flex flex-col gap-1.5">
      <label htmlFor={props.id} className="text-sm font-medium text-fg">
        {props.label}
      </label>
      <input
        id={props.id}
        type="text"
        className={props.readOnly ? readOnlyClass : inputClass}
        value={props.value}
        onChange={(e) => props.onChange(e.target.value)}
        readOnly={props.readOnly}
      />
      <Help text={props.description} />
      <FieldErr error={props.error} />
    </div>
  );
}

function NumberRow(props: {
  id: string;
  label: string;
  value: number;
  onChange: (v: number) => void;
  error?: string;
  step?: string;
  readOnly?: boolean;
  description?: string;
}) {
  return (
    <div className="flex flex-col gap-1.5">
      <label htmlFor={props.id} className="text-sm font-medium text-fg">
        {props.label}
      </label>
      <input
        id={props.id}
        type="number"
        step={props.step}
        className={props.readOnly ? readOnlyClass : inputClass}
        value={props.value}
        onChange={(e) => props.onChange(Number(e.target.value))}
        readOnly={props.readOnly}
      />
      <Help text={props.description} />
      <FieldErr error={props.error} />
    </div>
  );
}

function CheckRow(props: {
  id: string;
  label: string;
  checked: boolean;
  onChange: (v: boolean) => void;
  error?: string;
  description?: string;
}) {
  return (
    <div className="flex flex-col gap-1.5">
      <div className="flex items-center gap-2">
        <input
          id={props.id}
          type="checkbox"
          className="h-4 w-4 rounded border-border text-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text"
          checked={props.checked}
          onChange={(e) => props.onChange(e.target.checked)}
        />
        <label htmlFor={props.id} className="text-sm font-medium text-fg">
          {props.label}
        </label>
      </div>
      <Help text={props.description} />
      <FieldErr error={props.error} />
    </div>
  );
}

// SelectRow is a labelled dropdown over a fixed option set (e.g. the tenant's
// providers_allowed). The current value is always included so a value outside the
// allowed list still shows rather than rendering blank.
function SelectRow(props: {
  id: string;
  label: string;
  value: string;
  options: string[];
  onChange: (v: string) => void;
  error?: string;
  description?: string;
}) {
  const options = props.options.includes(props.value)
    ? props.options
    : [props.value, ...props.options];
  return (
    <div className="flex flex-col gap-1.5">
      <label htmlFor={props.id} className="text-sm font-medium text-fg">
        {props.label}
      </label>
      <select
        id={props.id}
        className={inputClass}
        value={props.value}
        onChange={(e) => props.onChange(e.target.value)}
      >
        {options.map((o) => (
          <option key={o} value={o}>
            {o}
          </option>
        ))}
      </select>
      <Help text={props.description} />
      <FieldErr error={props.error} />
    </div>
  );
}

// DatalistRow is a text input with dropdown suggestions. Used for the LLM model,
// where models_allowed may hold wildcard patterns (e.g. "gpt-*"): concrete entries
// become pickable suggestions, and the free-text input still accepts a specific
// model for a wildcard-allowed provider.
function DatalistRow(props: {
  id: string;
  label: string;
  value: string;
  suggestions: string[];
  onChange: (v: string) => void;
  error?: string;
  description?: string;
}) {
  const listId = `${props.id}-options`;
  return (
    <div className="flex flex-col gap-1.5">
      <label htmlFor={props.id} className="text-sm font-medium text-fg">
        {props.label}
      </label>
      <input
        id={props.id}
        type="text"
        list={listId}
        className={inputClass}
        value={props.value}
        onChange={(e) => props.onChange(e.target.value)}
      />
      <datalist id={listId}>
        {props.suggestions.map((s) => (
          <option key={s} value={s} />
        ))}
      </datalist>
      <Help text={props.description} />
      <FieldErr error={props.error} />
    </div>
  );
}

// arraysEqual compares two string arrays element-wise. Used to decide whether the
// allowed-providers list changed and so belongs in the patch.
function arraysEqual(a: string[], b: string[]): boolean {
  return a.length === b.length && a.every((v, i) => v === b[i]);
}

// buildPatch diffs the draft against the loaded document and returns only the
// changed sections (a partial document). embedding.dim is never part of the diff
// because it is read-only, so it is never sent (ADR-0022).
function buildPatch(orig: Settings, draft: Draft, providers: string[]): SettingsPatch {
  const patch: SettingsPatch = {};

  const emb: SettingsPatch["embedding"] = {};
  if (draft.embeddingProvider !== orig.embedding.provider) emb.provider = draft.embeddingProvider;
  if (draft.embeddingModel !== orig.embedding.model) emb.model = draft.embeddingModel;
  if (Object.keys(emb).length) patch.embedding = emb;

  const llm: SettingsPatch["llm"] = {};
  if (draft.llmProvider !== orig.llm.provider) llm.provider = draft.llmProvider;
  if (draft.llmModel !== orig.llm.model) llm.model = draft.llmModel;
  if (Object.keys(llm).length) patch.llm = llm;

  const rr: SettingsPatch["reranker"] = {};
  if (draft.rerankerEnabled !== orig.reranker.enabled) rr.enabled = draft.rerankerEnabled;
  if (draft.rerankerProvider !== orig.reranker.provider) rr.provider = draft.rerankerProvider;
  if (draft.rerankerTopN !== orig.reranker.top_n) rr.top_n = draft.rerankerTopN;
  if (Object.keys(rr).length) patch.reranker = rr;

  const rt: SettingsPatch["retrieval"] = {};
  if (draft.kVector !== orig.retrieval.k_vector) rt.k_vector = draft.kVector;
  if (draft.kText !== orig.retrieval.k_text) rt.k_text = draft.kText;
  if (draft.finalK !== orig.retrieval.final_k) rt.final_k = draft.finalK;
  if (draft.minScore !== orig.retrieval.min_score) rt.min_score = draft.minScore;
  if (Object.keys(rt).length) patch.retrieval = rt;

  const ch: SettingsPatch["chunking"] = {};
  if (draft.targetTokens !== orig.chunking.target_tokens) ch.target_tokens = draft.targetTokens;
  if (draft.overlapTokens !== orig.chunking.overlap_tokens) ch.overlap_tokens = draft.overlapTokens;
  if (Object.keys(ch).length) patch.chunking = ch;

  const rw: SettingsPatch["rewrite"] = {};
  if (draft.rewriteEnabled !== orig.rewrite.enabled) rw.enabled = draft.rewriteEnabled;
  if (Object.keys(rw).length) patch.rewrite = rw;

  const ex: SettingsPatch["expansion"] = {};
  if (draft.expansionMode !== orig.expansion.mode) ex.mode = draft.expansionMode;
  if (Object.keys(ex).length) patch.expansion = ex;

  if (!arraysEqual(providers, orig.providers_allowed)) patch.providers_allowed = providers;

  return patch;
}

// Draft is the flat editable projection of the settings sections the form
// exposes. embedding.dim is deliberately absent — it is read-only.
type Draft = {
  embeddingProvider: string;
  embeddingModel: string;
  llmProvider: string;
  llmModel: string;
  rerankerEnabled: boolean;
  rerankerProvider: string;
  rerankerTopN: number;
  kVector: number;
  kText: number;
  finalK: number;
  minScore: number;
  targetTokens: number;
  overlapTokens: number;
  rewriteEnabled: boolean;
  expansionMode: string;
};

function draftFrom(s: Settings): Draft {
  return {
    embeddingProvider: s.embedding.provider,
    embeddingModel: s.embedding.model,
    llmProvider: s.llm.provider,
    llmModel: s.llm.model,
    rerankerEnabled: s.reranker.enabled,
    rerankerProvider: s.reranker.provider,
    rerankerTopN: s.reranker.top_n,
    kVector: s.retrieval.k_vector,
    kText: s.retrieval.k_text,
    finalK: s.retrieval.final_k,
    minScore: s.retrieval.min_score,
    targetTokens: s.chunking.target_tokens,
    overlapTokens: s.chunking.overlap_tokens,
    rewriteEnabled: s.rewrite.enabled,
    expansionMode: s.expansion.mode,
  };
}

// SettingsForm loads the current tenant's settings and, once loaded, renders the
// editable form. It owns the GET load/error states so the page stays thin.
export function SettingsForm() {
  const { data, isLoading, isError, error } = useSettings();

  if (isLoading) {
    return (
      <div
        data-testid="settings-loading"
        className="mt-6 h-64 animate-pulse rounded-2xl border border-border bg-bg-subtle"
      />
    );
  }
  if (isError || !data) {
    return (
      <div className="mt-6 rounded-2xl border border-border bg-danger-tint px-4 py-4 text-sm text-danger-text">
        Could not load the settings: {error instanceof Error ? error.message : "unknown error"}
      </div>
    );
  }
  return <SettingsFormInner settings={data} />;
}

function SettingsFormInner({ settings }: { settings: Settings }) {
  const csrf = useAuth().me?.csrf_token;
  const tenantId = useTenant().current?.id;

  const [draft, setDraft] = useState<Draft>(() => draftFrom(settings));
  const [providersText, setProvidersText] = useState<string>(() =>
    settings.providers_allowed.join("\n"),
  );
  const [formError, setFormError] = useState<string | null>(null);
  const [fieldErrors, setFieldErrors] = useState<FieldError[]>([]);
  const [saved, setSaved] = useState(false);

  function set<K extends keyof Draft>(key: K, value: Draft[K]) {
    setDraft((prev) => ({ ...prev, [key]: value }));
    setSaved(false);
  }

  function errFor(path: string): string | undefined {
    return fieldErrors.find((f) => f.field === path)?.message;
  }

  const save = useMutation({
    mutationFn: (patch: SettingsPatch) => updateSettings(tenantId as string, csrf, patch),
    onSuccess: () => {
      setFormError(null);
      setFieldErrors([]);
      setSaved(true);
    },
    onError: (e: Error & { fields?: FieldError[] }) => {
      setFormError(e.message);
      setFieldErrors(e.fields ?? []);
      setSaved(false);
    },
  });

  function onSubmit(e: React.FormEvent) {
    e.preventDefault();
    setFormError(null);
    setFieldErrors([]);
    setSaved(false);
    const providers = providersText
      .split("\n")
      .map((line) => line.trim())
      .filter((line) => line !== "");
    save.mutate(buildPatch(settings, draft, providers));
  }

  return (
    <form onSubmit={onSubmit} className="mt-6 flex max-w-2xl flex-col gap-6">
      <Section title="Embedding">
        <TextRow
          id="embedding-provider"
          label="Embedding provider"
          value={draft.embeddingProvider}
          onChange={(v) => set("embeddingProvider", v)}
          error={errFor("embedding.provider")}
          description="The service that turns document and query text into vectors for search (for example openai, voyage)."
        />
        <TextRow
          id="embedding-model"
          label="Embedding model"
          value={draft.embeddingModel}
          onChange={(v) => set("embeddingModel", v)}
          error={errFor("embedding.model")}
          description="The embedding model name. It must match the vectors already stored; a change needs a full reindex."
        />
        <NumberRow
          id="embedding-dim"
          label="Embedding dimension"
          value={settings.embedding.dim}
          onChange={() => {}}
          error={errFor("embedding.dim")}
          readOnly
          description="The vector size for this tenant. It is fixed when the tenant is created and cannot change."
        />
      </Section>

      <Section title="LLM">
        <SelectRow
          id="llm-provider"
          label="LLM provider"
          value={draft.llmProvider}
          options={settings.providers_allowed}
          onChange={(v) => set("llmProvider", v)}
          error={errFor("llm.provider")}
          description="The service that writes the answer (for example anthropic, openai). It must be in the allowed providers below and have a key on the platform."
        />
        <DatalistRow
          id="llm-model"
          label="LLM model"
          value={draft.llmModel}
          suggestions={settings.llm.models_allowed.filter((m) => !m.includes("*"))}
          onChange={(v) => set("llmModel", v)}
          error={errFor("llm.model")}
          description="The model that writes the answer. Pick one the provider offers and the allowed models permit."
        />
      </Section>

      <Section title="Reranker">
        <CheckRow
          id="reranker-enabled"
          label="Reranker enabled"
          checked={draft.rerankerEnabled}
          onChange={(v) => set("rerankerEnabled", v)}
          error={errFor("reranker.enabled")}
          description="Re-score the retrieved chunks with a reranker model for better ordering before the answer. Off by default."
        />
        <TextRow
          id="reranker-provider"
          label="Reranker provider"
          value={draft.rerankerProvider}
          onChange={(v) => set("rerankerProvider", v)}
          error={errFor("reranker.provider")}
          description="The reranker service to use when the reranker is on (for example cohere)."
        />
        <NumberRow
          id="reranker-top-n"
          label="Reranker top N"
          value={draft.rerankerTopN}
          onChange={(v) => set("rerankerTopN", v)}
          error={errFor("reranker.top_n")}
          description="How many top chunks the reranker keeps after re-scoring."
        />
      </Section>

      <Section title="Retrieval">
        <NumberRow
          id="retrieval-k-vector"
          label="Vector k"
          value={draft.kVector}
          onChange={(v) => set("kVector", v)}
          error={errFor("retrieval.k_vector")}
          description="How many chunks the vector (meaning-based) search returns before the two searches are merged."
        />
        <NumberRow
          id="retrieval-k-text"
          label="Text k"
          value={draft.kText}
          onChange={(v) => set("kText", v)}
          error={errFor("retrieval.k_text")}
          description="How many chunks the keyword (text) search returns before the two searches are merged."
        />
        <NumberRow
          id="retrieval-final-k"
          label="Final k"
          value={draft.finalK}
          onChange={(v) => set("finalK", v)}
          error={errFor("retrieval.final_k")}
          description="How many merged chunks are passed to the answer step as context."
        />
        <NumberRow
          id="retrieval-min-score"
          label="Minimum score"
          value={draft.minScore}
          onChange={(v) => set("minScore", v)}
          error={errFor("retrieval.min_score")}
          step="0.01"
          description="Grounding floor on the reranker's relevance score: a chunk below it is dropped, and if none remain the answer refuses. It applies only when the reranker is enabled; without a reranker the model judges the retrieved context instead."
        />
      </Section>

      <Section title="Chunking">
        <NumberRow
          id="chunking-target-tokens"
          label="Chunk target tokens"
          value={draft.targetTokens}
          onChange={(v) => set("targetTokens", v)}
          error={errFor("chunking.target_tokens")}
          description="The target size of each chunk, in tokens. It takes effect on the next reindex or sync."
        />
        <NumberRow
          id="chunking-overlap-tokens"
          label="Chunk overlap tokens"
          value={draft.overlapTokens}
          onChange={(v) => set("overlapTokens", v)}
          error={errFor("chunking.overlap_tokens")}
          description="How many tokens each chunk shares with the next, to keep context across the split."
        />
      </Section>

      <Section title="Rewrite">
        <CheckRow
          id="rewrite-enabled"
          label="Query rewrite enabled"
          checked={draft.rewriteEnabled}
          onChange={(v) => set("rewriteEnabled", v)}
          error={errFor("rewrite.enabled")}
          description="Rewrite a follow-up question into a standalone one, using the chat history, before search. Off by default."
        />
      </Section>

      <Section title="Query expansion">
        <SelectRow
          id="expansion-mode"
          label="Expansion mode"
          value={draft.expansionMode}
          options={["off", "hyde"]}
          onChange={(v) => set("expansionMode", v)}
          error={errFor("expansion.mode")}
          description="Broaden the search when the question and the documents use different words. off: search the question as typed. hyde: an LLM drafts a short hypothetical answer and the search matches on that (better for wording mismatches), at the cost of one extra LLM call per query."
        />
      </Section>

      <Section title="Providers">
        <div className="flex flex-col gap-1.5 sm:col-span-2">
          <label htmlFor="providers-allowed" className="text-sm font-medium text-fg">
            Allowed providers
          </label>
          <textarea
            id="providers-allowed"
            className={textareaClass}
            rows={4}
            value={providersText}
            onChange={(e) => {
              setProvidersText(e.target.value);
              setSaved(false);
            }}
            placeholder="One provider per line"
          />
          <p className="text-xs text-fg-subtle">
            The providers this tenant may use for embedding, LLM and reranking. One per line. A
            provider not listed here is refused, even if a key exists on the platform.
          </p>
          <FieldErr error={errFor("providers_allowed")} />
        </div>
      </Section>

      {formError ? (
        <p className="rounded-md border border-border bg-danger-tint px-3 py-2 text-sm text-danger-text">
          {formError}
        </p>
      ) : null}

      {saved ? (
        <p className="rounded-md border border-border bg-accent-tint px-3 py-2 text-sm text-accent-text">
          Settings saved.
        </p>
      ) : null}

      <div className="flex items-center gap-2 border-t border-border pt-5">
        <button
          type="submit"
          disabled={save.isPending}
          className="inline-flex h-9 items-center rounded-md bg-accent px-3 text-sm font-medium text-accent-fg transition-colors hover:bg-accent-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent-text focus-visible:ring-offset-2 focus-visible:ring-offset-bg disabled:cursor-not-allowed disabled:opacity-50"
        >
          {save.isPending ? "Saving…" : "Save"}
        </button>
      </div>
    </form>
  );
}
