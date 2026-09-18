import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";

import type { Settings } from "@/lib/settings";

// --- mocks -----------------------------------------------------------------
vi.mock("@/lib/auth", () => ({ useAuth: () => ({ me: { csrf_token: "tok" } }) }));
vi.mock("@/lib/tenant", () => ({ useTenant: () => ({ current: { id: "t1" } }) }));

const useSettingsMock = vi.fn();
const updateSettings = vi.fn();
vi.mock("@/lib/settings", () => ({
  useSettings: () => useSettingsMock(),
  updateSettings: (...a: unknown[]) => updateSettings(...a),
}));

import { SettingsForm } from "./SettingsForm";

function settings(over: Partial<Settings> = {}): Settings {
  return {
    embedding: { provider: "voyage", model: "voyage-3", dim: 1024 },
    llm: { provider: "anthropic", model: "claude-sonnet-5", max_tokens: 1024, models_allowed: ["claude-opus-5"] },
    reranker: { enabled: false, provider: "cohere", model: "rerank-v3.5", top_n: 20 },
    rewrite: { enabled: false },
    expansion: { mode: "off" },
    chunking: { target_tokens: 512, overlap_tokens: 64 },
    retrieval: { k_vector: 40, k_text: 40, final_k: 8, min_score: 0.02 },
    answering: { token_budget: 6000, history_n: 6 },
    limits: { qps: 10, max_upload_mb: 50, max_pages_per_crawl: 5000 },
    providers_allowed: ["anthropic", "voyage", "cohere"],
    ...over,
  };
}

function renderForm(ui: ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

beforeEach(() => {
  updateSettings.mockReset();
  useSettingsMock.mockReset();
  useSettingsMock.mockReturnValue({ data: settings(), isLoading: false, isError: false, error: null });
});

describe("SettingsForm (load states)", () => {
  it("shows a loading skeleton while the GET is in flight", () => {
    useSettingsMock.mockReturnValue({ data: undefined, isLoading: true, isError: false, error: null });
    renderForm(<SettingsForm />);
    expect(screen.getByTestId("settings-loading")).toBeInTheDocument();
  });

  it("shows an error state when the GET fails", () => {
    useSettingsMock.mockReturnValue({
      data: undefined,
      isLoading: false,
      isError: true,
      error: new Error("boom"),
    });
    renderForm(<SettingsForm />);
    expect(screen.getByText(/boom/i)).toBeInTheDocument();
  });
});

describe("SettingsForm (render)", () => {
  it("renders current values, with embedding.dim read-only", () => {
    renderForm(<SettingsForm />);

    expect(screen.getByLabelText("Embedding provider")).toHaveValue("voyage");
    expect(screen.getByLabelText("Embedding model")).toHaveValue("voyage-3");
    // dim is immutable (ADR-0022): shown, read-only, never editable.
    const dim = screen.getByLabelText("Embedding dimension");
    expect(dim).toHaveValue(1024);
    expect(dim).toHaveAttribute("readonly");

    expect(screen.getByLabelText("LLM provider")).toHaveValue("anthropic");
    expect(screen.getByLabelText("Reranker enabled")).not.toBeChecked();
    expect(screen.getByLabelText("Reranker top N")).toHaveValue(20);
    expect(screen.getByLabelText("Minimum score")).toHaveAttribute("step", "0.01");
    expect(screen.getByLabelText("Query rewrite enabled")).not.toBeChecked();
    expect(screen.getByLabelText("Expansion mode")).toHaveValue("off");
    // providers_allowed renders as one entry per line.
    expect(screen.getByLabelText("Allowed providers")).toHaveValue("anthropic\nvoyage\ncohere");
  });
});

describe("SettingsForm (submit)", () => {
  it("PATCHes only the changed sections, coercing numbers and the stringlist, never sending dim", async () => {
    updateSettings.mockResolvedValue(settings());
    renderForm(<SettingsForm />);

    fireEvent.click(screen.getByLabelText("Reranker enabled"));
    fireEvent.change(screen.getByLabelText("Minimum score"), { target: { value: "0.5" } });
    fireEvent.change(screen.getByLabelText("Allowed providers"), {
      target: { value: "anthropic\nvoyage\n" },
    });

    fireEvent.click(screen.getByRole("button", { name: /save/i }));

    await waitFor(() => expect(updateSettings).toHaveBeenCalledTimes(1));
    expect(updateSettings).toHaveBeenCalledWith("t1", "tok", {
      reranker: { enabled: true },
      retrieval: { min_score: 0.5 },
      providers_allowed: ["anthropic", "voyage"],
    });
    // the changed sections carry no embedding key at all -> dim never sent.
    const body = updateSettings.mock.calls[0][2];
    expect(body.embedding).toBeUndefined();
  });

  it("PATCHes the expansion mode when changed to hyde", async () => {
    updateSettings.mockResolvedValue(settings());
    renderForm(<SettingsForm />);

    fireEvent.change(screen.getByLabelText("Expansion mode"), { target: { value: "hyde" } });
    fireEvent.click(screen.getByRole("button", { name: /save/i }));

    await waitFor(() => expect(updateSettings).toHaveBeenCalledTimes(1));
    expect(updateSettings).toHaveBeenCalledWith("t1", "tok", { expansion: { mode: "hyde" } });
  });

  it("sends an empty patch untouched (no spurious sections)", async () => {
    updateSettings.mockResolvedValue(settings());
    renderForm(<SettingsForm />);

    fireEvent.click(screen.getByRole("button", { name: /save/i }));

    await waitFor(() => expect(updateSettings).toHaveBeenCalledTimes(1));
    expect(updateSettings).toHaveBeenCalledWith("t1", "tok", {});
  });

  it("shows a saved notice on success", async () => {
    updateSettings.mockResolvedValue(settings());
    renderForm(<SettingsForm />);

    fireEvent.change(screen.getByLabelText("Embedding provider"), { target: { value: "openai" } });
    fireEvent.click(screen.getByRole("button", { name: /save/i }));

    expect(await screen.findByText(/saved/i)).toBeInTheDocument();
  });
});

describe("SettingsForm (errors)", () => {
  it("surfaces the top-level banner and the per-field messages", async () => {
    updateSettings.mockRejectedValue(
      Object.assign(new Error("settings failed validation"), {
        fields: [{ field: "retrieval.min_score", message: "must be between 0 and 1" }],
      }),
    );
    renderForm(<SettingsForm />);

    fireEvent.change(screen.getByLabelText("Minimum score"), { target: { value: "9" } });
    fireEvent.click(screen.getByRole("button", { name: /save/i }));

    expect(await screen.findByText(/settings failed validation/i)).toBeInTheDocument();
    expect(await screen.findByText(/must be between 0 and 1/i)).toBeInTheDocument();
  });

  it("surfaces the immutable-dim conflict message", async () => {
    updateSettings.mockRejectedValue(
      Object.assign(new Error("embedding.dim is immutable; reindex to change"), {
        fields: [{ field: "embedding.dim", message: "immutable after provisioning" }],
      }),
    );
    renderForm(<SettingsForm />);

    fireEvent.change(screen.getByLabelText("Reranker top N"), { target: { value: "25" } });
    fireEvent.click(screen.getByRole("button", { name: /save/i }));

    // Top-level banner carries the conflict message…
    expect(await screen.findByText(/reindex to change/i)).toBeInTheDocument();
    // …and the per-field note sits next to the read-only dimension input.
    expect(screen.getByText(/immutable after provisioning/i)).toBeInTheDocument();
  });
});

describe("SettingsForm (LLM model selection)", () => {
  it("offers the LLM provider as a select over providers_allowed", () => {
    renderForm(<SettingsForm />);
    const provider = screen.getByLabelText("LLM provider") as HTMLSelectElement;
    expect(provider.tagName).toBe("SELECT");
    const opts = Array.from(provider.options).map((o) => o.value);
    expect(opts).toEqual(expect.arrayContaining(["anthropic", "voyage", "cohere"]));
    expect(provider.value).toBe("anthropic");
  });

  it("offers models_allowed as datalist suggestions for the model and patches the choice", async () => {
    useSettingsMock.mockReturnValue({
      data: settings({
        llm: { provider: "anthropic", model: "claude-sonnet-5", max_tokens: 1024, models_allowed: ["claude-opus-5", "claude-sonnet-5", "gpt-*"] },
      }),
      isLoading: false,
      isError: false,
      error: null,
    });
    updateSettings.mockResolvedValue(undefined);
    renderForm(<SettingsForm />);

    const model = screen.getByLabelText("LLM model") as HTMLInputElement;
    // Wildcard entries are not offered as concrete suggestions.
    const listId = model.getAttribute("list")!;
    const optionValues = Array.from(document.getElementById(listId)!.querySelectorAll("option")).map(
      (o) => (o as HTMLOptionElement).value,
    );
    expect(optionValues).toEqual(["claude-opus-5", "claude-sonnet-5"]);

    fireEvent.change(model, { target: { value: "claude-opus-5" } });
    fireEvent.click(screen.getByRole("button", { name: /save/i }));

    await waitFor(() => expect(updateSettings).toHaveBeenCalled());
    const patch = updateSettings.mock.calls[0][2];
    expect(patch.llm).toEqual({ model: "claude-opus-5" });
  });
});
