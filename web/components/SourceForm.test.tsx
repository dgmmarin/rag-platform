import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";

import type { ConnectorKind, Source } from "@/lib/sources";

// --- mocks -----------------------------------------------------------------
const push = vi.fn();
vi.mock("next/navigation", () => ({ useRouter: () => ({ push }) }));

vi.mock("@/lib/auth", () => ({ useAuth: () => ({ me: { csrf_token: "tok" } }) }));
vi.mock("@/lib/tenant", () => ({ useTenant: () => ({ current: { id: "t1" } }) }));

const useConnectorKindsMock = vi.fn();
vi.mock("@/lib/connectorKinds", () => ({ useConnectorKinds: () => useConnectorKindsMock() }));

const createSource = vi.fn();
const updateSource = vi.fn();
const testSource = vi.fn();
vi.mock("@/lib/sources", () => ({
  createSource: (...a: unknown[]) => createSource(...a),
  updateSource: (...a: unknown[]) => updateSource(...a),
  testSource: (...a: unknown[]) => testSource(...a),
}));

import { SourceForm } from "./SourceForm";

// Two kinds with disjoint fields so "exactly this kind's fields" is observable.
const KINDS: ConnectorKind[] = [
  {
    kind: "webcrawl",
    label: "Web crawl",
    fields: [
      { name: "start_url", label: "Start URL", type: "url", required: true },
      { name: "max_depth", label: "Max depth", type: "number", required: false },
    ],
  },
  {
    kind: "api",
    label: "API",
    fields: [
      { name: "endpoint", label: "Endpoint", type: "url", required: true },
      { name: "token", label: "API token", type: "secret", required: true },
      { name: "verify_tls", label: "Verify TLS", type: "bool", required: false },
    ],
  },
  {
    kind: "crawl",
    label: "Crawl",
    fields: [
      { name: "start_urls", label: "Start URLs", type: "stringlist", required: true },
      { name: "auth", label: "Auth Config", type: "json", required: false },
    ],
  },
];

function renderForm(ui: ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

beforeEach(() => {
  push.mockReset();
  createSource.mockReset();
  updateSource.mockReset();
  testSource.mockReset();
  useConnectorKindsMock.mockReset();
  useConnectorKindsMock.mockReturnValue({ data: KINDS, isLoading: false, isError: false });
});

describe("SourceForm (create)", () => {
  it("renders exactly the picked kind's fields with the right input types", () => {
    renderForm(<SourceForm />);

    fireEvent.change(screen.getByLabelText(/connector kind/i), { target: { value: "api" } });

    // api's fields present…
    expect(screen.getByLabelText("Endpoint")).toHaveAttribute("type", "url");
    expect(screen.getByLabelText("API token")).toHaveAttribute("type", "password");
    expect(screen.getByLabelText("Verify TLS")).toHaveAttribute("type", "checkbox");
    // required marker on the required field
    expect(screen.getByLabelText("Endpoint")).toBeRequired();
    // …and webcrawl's fields absent
    expect(screen.queryByLabelText("Start URL")).not.toBeInTheDocument();
  });

  it("splits secrets into credentials and non-secrets into config, coerces types, then navigates", async () => {
    createSource.mockResolvedValue({ id: "s9" });
    renderForm(<SourceForm />);

    fireEvent.change(screen.getByLabelText(/connector kind/i), { target: { value: "api" } });
    fireEvent.change(screen.getByLabelText(/^name$/i), { target: { value: "My API" } });
    fireEvent.change(screen.getByLabelText("Endpoint"), { target: { value: "https://api.example.com" } });
    fireEvent.change(screen.getByLabelText("API token"), { target: { value: "s3cr3t" } });
    fireEvent.click(screen.getByLabelText("Verify TLS"));

    fireEvent.click(screen.getByRole("button", { name: /save|create/i }));

    await waitFor(() => expect(createSource).toHaveBeenCalledTimes(1));
    expect(createSource).toHaveBeenCalledWith("t1", "tok", {
      kind: "api",
      name: "My API",
      config: { endpoint: "https://api.example.com", verify_tls: true },
      credentials: { token: "s3cr3t" },
    });
    await waitFor(() => expect(push).toHaveBeenCalledWith("/admin/sources"));
  });

  it("blocks submit and flags the missing required field", async () => {
    renderForm(<SourceForm />);

    fireEvent.change(screen.getByLabelText(/connector kind/i), { target: { value: "api" } });
    fireEvent.change(screen.getByLabelText(/^name$/i), { target: { value: "My API" } });
    // leave required Endpoint + token empty
    fireEvent.click(screen.getByRole("button", { name: /save|create/i }));

    expect(await screen.findByText(/endpoint/i)).toBeInTheDocument();
    expect(createSource).not.toHaveBeenCalled();
  });

  it("offers no test-connection control before the source exists", () => {
    renderForm(<SourceForm />);
    fireEvent.change(screen.getByLabelText(/connector kind/i), { target: { value: "api" } });
    expect(screen.queryByRole("button", { name: /test connection/i })).not.toBeInTheDocument();
  });

  it("submits a stringlist field as an array and a json field as a parsed object", async () => {
    createSource.mockResolvedValue({ id: "s10" });
    renderForm(<SourceForm />);

    fireEvent.change(screen.getByLabelText(/connector kind/i), { target: { value: "crawl" } });
    fireEvent.change(screen.getByLabelText(/^name$/i), { target: { value: "Docs crawl" } });
    // one URL per line, with a trailing blank line to prove blanks are dropped
    fireEvent.change(screen.getByLabelText("Start URLs"), {
      target: { value: "https://a.example.com\nhttps://b.example.com\n" },
    });
    fireEvent.change(screen.getByLabelText("Auth Config"), {
      target: { value: '{ "type": "bearer" }' },
    });

    fireEvent.click(screen.getByRole("button", { name: /save|create/i }));

    await waitFor(() => expect(createSource).toHaveBeenCalledTimes(1));
    expect(createSource).toHaveBeenCalledWith("t1", "tok", {
      kind: "crawl",
      name: "Docs crawl",
      config: {
        start_urls: ["https://a.example.com", "https://b.example.com"],
        auth: { type: "bearer" },
      },
    });
  });

  it("blocks submit when a json field does not parse", async () => {
    renderForm(<SourceForm />);

    fireEvent.change(screen.getByLabelText(/connector kind/i), { target: { value: "crawl" } });
    fireEvent.change(screen.getByLabelText(/^name$/i), { target: { value: "Docs crawl" } });
    fireEvent.change(screen.getByLabelText("Start URLs"), { target: { value: "https://a.example.com" } });
    fireEvent.change(screen.getByLabelText("Auth Config"), { target: { value: "{ not json" } });

    fireEvent.click(screen.getByRole("button", { name: /save|create/i }));

    expect(await screen.findByText(/valid json for:/i)).toBeInTheDocument();
    expect(createSource).not.toHaveBeenCalled();
  });
});

function apiSource(over: Partial<Source> = {}): Source {
  return {
    id: "s1",
    tenant_id: "t1",
    kind: "api",
    name: "My API",
    status: "active",
    config: { endpoint: "https://api.example.com", verify_tls: true },
    created_at: "2026-09-01T00:00:00Z",
    updated_at: "2026-09-01T00:00:00Z",
    ...over,
  };
}

describe("SourceForm (edit)", () => {
  it("pre-fills non-secret config, blanks secrets, and fixes the kind (no picker)", () => {
    renderForm(<SourceForm source={apiSource()} />);

    expect(screen.queryByLabelText(/connector kind/i)).not.toBeInTheDocument();
    expect(screen.getByLabelText("Endpoint")).toHaveValue("https://api.example.com");
    // secret never round-trips: rendered blank + write-only
    expect(screen.getByLabelText("API token")).toHaveValue("");
    expect(screen.getByLabelText("Verify TLS")).toBeChecked();
  });

  it("omits an unchanged secret but sends a filled one on update", async () => {
    updateSource.mockResolvedValue(apiSource());
    renderForm(<SourceForm source={apiSource()} />);

    // submit without touching the secret -> credentials omitted
    fireEvent.click(screen.getByRole("button", { name: /save|update/i }));
    await waitFor(() => expect(updateSource).toHaveBeenCalledTimes(1));
    const firstBody = updateSource.mock.calls[0][3];
    expect(firstBody.credentials).toBeUndefined();
    expect(firstBody.kind).toBeUndefined(); // kind is immutable on edit

    // now change the secret -> credentials sent
    fireEvent.change(screen.getByLabelText("API token"), { target: { value: "rotated" } });
    fireEvent.click(screen.getByRole("button", { name: /save|update/i }));
    await waitFor(() => expect(updateSource).toHaveBeenCalledTimes(2));
    expect(updateSource.mock.calls[1][3].credentials).toEqual({ token: "rotated" });
  });

  it("runs test-connection and shows the inline result", async () => {
    testSource.mockResolvedValueOnce({ ok: true });
    renderForm(<SourceForm source={apiSource()} />);

    fireEvent.click(screen.getByRole("button", { name: /test connection/i }));
    await waitFor(() => expect(testSource).toHaveBeenCalledWith("t1", "s1", "tok"));
    expect(await screen.findByText(/passed|success/i)).toBeInTheDocument();

    testSource.mockRejectedValueOnce(new Error("connection refused"));
    fireEvent.click(screen.getByRole("button", { name: /test connection/i }));
    expect(await screen.findByText(/connection refused/i)).toBeInTheDocument();
  });
});
