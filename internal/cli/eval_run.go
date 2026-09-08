package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/alecthomas/kong"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rag-platform/ragctl/internal/answer"
	"github.com/rag-platform/ragctl/internal/cp/tenants"
	"github.com/rag-platform/ragctl/internal/eval"
	"github.com/rag-platform/ragctl/internal/llm"
	"github.com/rag-platform/ragctl/internal/query"
	"github.com/rag-platform/ragctl/internal/retrieve"
	"github.com/rag-platform/ragctl/internal/tenant"
)

// defaultEvalFinalK is the fallback recall@k when neither the tenant settings nor
// the --config-file overlay pin retrieval.final_k. It matches the retrieval
// default (ADR-0007).
const defaultEvalFinalK = 8

// EvalRunCmd runs a tenant's eval cases through the real retrieval + answering
// pipeline and prints recall@k, grounded rate and mean latency (STORY-12.2,
// SPEC-06 §8). It records an eval_run plus one eval_result per case in the tenant
// database. LLM-as-judge correctness is STORY-12.3 and is not computed here.
//
// The optional --config-file is a partial settings document (same shape as a
// tenant's settings, SPEC-02 §5) overlaid on the tenant's live settings FOR THIS
// RUN ONLY — it never mutates stored settings. The effective (merged) settings are
// stored in eval_runs.config. The flag is named --config-file rather than
// --config because the global --config (ADR-0009) is the ragctl config-file flag.
//
// --judge (STORY-12.3, ADR-0071) is opt-in LLM-as-judge correctness: when set,
// each case with an expected_answer has its produced answer scored correct/
// incorrect by the tenant's LLM (or the --judge-model override), populating
// eval_results.judged_correct and a correctness rate in the summary. Without it
// the run is unchanged: no judging, no judge cost, judged_correct NULL.
type EvalRunCmd struct {
	Slug       string `arg:"" help:"Tenant slug."`
	ConfigFile string `help:"Optional JSON settings overlay applied to this run only (retrieval/answering/reranker/llm)." name:"config-file" type:"existingfile"`
	Limit      int    `help:"Maximum cases to run (0 = all)." default:"0"`
	Judge      bool   `help:"Score answer correctness with an LLM judge (opt-in; only cases with an expected_answer)." name:"judge"`
	JudgeModel string `help:"Judge model id (defaults to the tenant's settings.llm.model)." name:"judge-model"`
	JSON       bool   `help:"Emit the run summary (and gate verdict) as JSON to stdout instead of text." name:"json"`
	Gate       string `help:"Gate the run against a minimum-thresholds JSON policy file; non-zero exit on regression (STORY-12.4)." name:"gate" type:"existingfile"`
}

// Run wires the pipeline (mirroring the serve composition root), resolves the
// effective config + k, and executes the run.
func (c *EvalRunCmd) Run(k *kong.Context, g *Globals) error {
	ctx := context.Background()

	if g.ControlPlaneURL == "" {
		return fmt.Errorf("eval run: no control-plane URL (set --control-plane-url or CONTROL_PLANE_URL)")
	}
	cipher, err := LoadStartupKeyring(ctx, g.Secrets)
	if err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, g.ControlPlaneURL)
	if err != nil {
		return fmt.Errorf("eval run: open control-plane pool: %w", err)
	}
	defer pool.Close()

	tid, err := tenantIDForSlug(ctx, pool, c.Slug)
	if err != nil {
		return err
	}

	overlay, err := readConfigOverlay(c.ConfigFile)
	if err != nil {
		return err
	}

	// --- Compose the pipeline exactly as `serve` does (retrieve + query), so an
	// eval run exercises the real retrieval and answering path. ---
	resolver := tenant.NewResolver(tenant.Config{ControlPool: pool, Decrypter: cipher})
	baseSettings := tenants.NewSettingsService(tenants.SettingsFromPool(pool))

	// Effective settings = tenant settings merged with the run overlay. Both the
	// retrieval and answering stages read settings through this one source, so the
	// overlay applies uniformly; the merged doc is stored as the run config.
	base, err := baseSettings.Get(ctx, tid.String())
	if err != nil {
		return fmt.Errorf("eval run: load settings: %w", err)
	}
	effective := deepMerge(base, overlay)
	settings := fixedSettings{doc: effective}
	kValue := finalKFromSettings(effective)

	llmFactory := llm.Factory{Keys: llm.Keys{
		Anthropic:     g.Config.AnthropicAPIKey,
		OpenAI:        g.Config.OpenAIAPIKey,
		OpenAIBaseURL: g.Config.OpenAIBaseURL,
	}}
	retrieveSvc := retrieve.NewService(resolver, settings,
		retrieve.KeyedEmbedderFactory{APIKey: g.Config.EmbeddingAPIKey, BaseURL: g.Config.EmbeddingBaseURL})
	retrieveSvc.Reranker = retrieve.KeyedRerankerFactory{
		CohereAPIKey:  g.Config.CohereAPIKey,
		CohereBaseURL: g.Config.CohereBaseURL,
		LLM:           llmFactory,
	}
	providerFactory := answer.KeyedProviderFactory{LLM: llmFactory}
	querySvc := &query.Service{
		Retrieve:  retrieveSvc,
		Answer:    &answer.Service{Providers: providerFactory},
		Settings:  settings,
		Names:     tenants.NewNameService(tenants.SettingsFromPool(pool)),
		Providers: providerFactory,
	}

	svc := eval.NewService(resolver, eval.NewTenantStore())
	svc.Runs = eval.NewRunStore()

	// Optional LLM judge (STORY-12.3): build the provider through the same factory
	// as the answer path, fail-closed on the tenant's provider + model allowlists.
	var judge eval.Judge
	if c.Judge {
		judge, err = buildJudge(llmFactory, effective, c.JudgeModel)
		if err != nil {
			return err
		}
	}

	summary, err := svc.Run(ctx, tid, eval.RunOptions{
		Pipeline: evalPipeline{retrieve: retrieveSvc, query: querySvc, tid: tid},
		Judge:    judge,
		K:        kValue,
		Config:   effective,
		Limit:    c.Limit,
	})
	if err != nil {
		return err
	}

	// Optional gate (STORY-12.4): compare the run against a minimum-thresholds
	// policy. A regression returns an error (non-zero exit) AFTER the output is
	// written, so both a human and CI see the metrics and the verdict.
	var gate *eval.GateResult
	if c.Gate != "" {
		policyRaw, rerr := os.ReadFile(c.Gate)
		if rerr != nil {
			return fmt.Errorf("eval run: read gate policy: %w", rerr)
		}
		policy, perr := eval.ParseGatePolicy(policyRaw)
		if perr != nil {
			return perr
		}
		res := eval.CheckGate(summary, policy)
		gate = &res
	}

	if c.JSON {
		if err := printEvalRunJSON(k, summary, gate); err != nil {
			return err
		}
	} else if err := printEvalSummary(k, summary, gate); err != nil {
		return err
	}

	if gate != nil && !gate.Passed && !gate.Skipped {
		return fmt.Errorf("eval gate: quality regression: %s", strings.Join(gate.Failures, "; "))
	}
	return nil
}

// evalPipeline adapts the retrieve + query services to the eval.Pipeline port.
// Retrieve returns the retrieved document ids (rank order) for recall@k; Answer
// returns grounded-ness + the answer text; the runner times the Answer call.
type evalPipeline struct {
	retrieve *retrieve.Service
	query    *query.Service
	tid      tenant.ID
}

func (p evalPipeline) Retrieve(ctx context.Context, question string, k int) ([]string, error) {
	res, err := p.retrieve.Search(ctx, p.tid, retrieve.Request{Query: question, TopK: k})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(res))
	for _, r := range res {
		ids = append(ids, r.DocumentID)
	}
	return ids, nil
}

func (p evalPipeline) Answer(ctx context.Context, question string, k int) (bool, string, error) {
	res, err := p.query.Query(ctx, p.tid, query.Request{Question: question, TopK: k})
	if err != nil {
		return false, "", err
	}
	return res.Grounded, res.Answer, nil
}

// buildJudge constructs the LLM judge from the effective settings.llm block
// (provider/model + the provider & model allowlists), fail-closed inside the
// factory exactly like the answer path. --judge-model overrides the model; an
// unset model (no override, none in settings) is an actionable error rather than
// a silent default.
func buildJudge(factory llm.Factory, effective map[string]any, modelOverride string) (eval.Judge, error) {
	provider, model, modelsAllowed := llmSelection(effective)
	if modelOverride != "" {
		model = modelOverride
	}
	if provider == "" || model == "" {
		return nil, fmt.Errorf("eval run: --judge needs an llm provider and model (set settings.llm or --judge-model)")
	}
	p, err := factory.Provider(provider, model, providersAllowed(effective), modelsAllowed)
	if err != nil {
		return nil, fmt.Errorf("eval run: build judge provider: %w", err)
	}
	return eval.NewLLMJudge(p, model), nil
}

// llmSelection extracts settings.llm.{provider,model,models_allowed} from a
// settings document.
func llmSelection(doc map[string]any) (provider, model string, modelsAllowed []string) {
	l, ok := doc["llm"].(map[string]any)
	if !ok {
		return "", "", nil
	}
	provider, _ = l["provider"].(string)
	model, _ = l["model"].(string)
	return provider, model, stringSlice(l["models_allowed"])
}

// providersAllowed extracts settings.providers_allowed from a settings document.
func providersAllowed(doc map[string]any) []string { return stringSlice(doc["providers_allowed"]) }

// stringSlice coerces a JSON []any of strings to []string.
func stringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, a := range arr {
		if s, ok := a.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// fixedSettings is a SettingsSource returning a precomputed (merged) settings
// document — the tenant settings with the run overlay applied.
type fixedSettings struct{ doc map[string]any }

func (f fixedSettings) Get(context.Context, string) (map[string]any, error) { return f.doc, nil }

// tenantIDForSlug resolves a tenant slug to its id via the control plane.
func tenantIDForSlug(ctx context.Context, pool *pgxpool.Pool, slug string) (tenant.ID, error) {
	if slug == "" {
		return tenant.ID{}, fmt.Errorf("eval run: a tenant slug is required")
	}
	var idStr string
	err := pool.QueryRow(ctx, `select id::text from tenants where slug = $1`, slug).Scan(&idStr)
	if errors.Is(err, pgx.ErrNoRows) {
		return tenant.ID{}, fmt.Errorf("eval run: unknown tenant slug %q", slug)
	}
	if err != nil {
		return tenant.ID{}, fmt.Errorf("eval run: look up tenant %q: %w", slug, err)
	}
	u, err := uuid.Parse(idStr)
	if err != nil {
		return tenant.ID{}, fmt.Errorf("eval run: tenant %q has an invalid id: %w", slug, err)
	}
	return tenant.ID(u), nil
}

// readConfigOverlay parses the optional --config-file JSON into a settings map.
// An empty path returns a nil overlay (use tenant settings unchanged).
func readConfigOverlay(path string) (map[string]any, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("eval run: read config file: %w", err)
	}
	var overlay map[string]any
	if err := json.Unmarshal(raw, &overlay); err != nil {
		return nil, fmt.Errorf("eval run: parse config file: %w", err)
	}
	return overlay, nil
}

// deepMerge overlays src onto dst, recursing into nested objects. src wins on
// conflicts; nested maps merge rather than replace. dst is not mutated.
func deepMerge(dst, src map[string]any) map[string]any {
	out := make(map[string]any, len(dst)+len(src))
	for k, v := range dst {
		out[k] = v
	}
	for k, v := range src {
		if sv, ok := v.(map[string]any); ok {
			if dv, ok := out[k].(map[string]any); ok {
				out[k] = deepMerge(dv, sv)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// finalKFromSettings reads retrieval.final_k from a settings document, falling
// back to the retrieval default.
func finalKFromSettings(doc map[string]any) int {
	if ret, ok := doc["retrieval"].(map[string]any); ok {
		switch n := ret["final_k"].(type) {
		case float64:
			if n > 0 {
				return int(n)
			}
		case int:
			if n > 0 {
				return n
			}
		case json.Number:
			if i, _ := n.Int64(); i > 0 {
				return int(i)
			}
		}
	}
	return defaultEvalFinalK
}

// evalRunJSON is the machine-readable output of `eval run --json` (STORY-12.4):
// the run summary plus an optional gate verdict. It is the small data contract the
// mise-tasks/eval-gate script parses and the EPIC-11 admin UI will render.
type evalRunJSON struct {
	Summary eval.Summary     `json:"summary"`
	Gate    *eval.GateResult `json:"gate,omitempty"`
}

// printEvalRunJSON writes the summary (+ gate) as JSON.
func printEvalRunJSON(k *kong.Context, s eval.Summary, gate *eval.GateResult) error {
	body, err := json.MarshalIndent(evalRunJSON{Summary: s, Gate: gate}, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(k.Stdout, string(body))
	return err
}

// printEvalSummary writes the human-readable run summary to stdout. The
// correctness line is printed only when the LLM judge actually scored cases
// (STORY-12.3); a non-judged run prints exactly as it did before. The gate line
// is printed only when a gate policy was applied (STORY-12.4).
func printEvalSummary(k *kong.Context, s eval.Summary, gate *eval.GateResult) error {
	if _, err := fmt.Fprintf(k.Stdout,
		"ragctl eval run: run %s\n"+
			"  cases:         %d (%d errored)\n"+
			"  recall@%d:      %.3f (over %d cases with expected docs)\n"+
			"  grounded rate: %.3f\n"+
			"  mean latency:  %d ms\n",
		s.RunID, s.Cases, s.Errors, s.K, s.RecallAtK, s.CasesScoredForRecall, s.GroundedRate, s.MeanLatencyMs); err != nil {
		return err
	}
	if s.CasesJudged > 0 {
		if _, err := fmt.Fprintf(k.Stdout,
			"  correctness:   %.3f (over %d cases judged)\n", s.CorrectnessRate, s.CasesJudged); err != nil {
			return err
		}
	}
	if gate != nil {
		verdict := "PASS"
		switch {
		case gate.Skipped:
			verdict = "SKIP (" + gate.Reason + ")"
		case !gate.Passed:
			verdict = "FAIL: " + strings.Join(gate.Failures, "; ")
		}
		if _, err := fmt.Fprintf(k.Stdout, "  gate:          %s\n", verdict); err != nil {
			return err
		}
	}
	return nil
}
