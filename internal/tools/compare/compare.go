// Package compare implements the jev_compare MCP tool: judges the
// factual relation between two passages -- overall, and optionally for
// each of several specific aspects -- using TypeSafe's Jev judgment
// model's "choice" question type, via OpenRouter's SystemOne API.
//
// This package is a self-registering plugin (see internal/registry's
// package doc comment for the overall mechanism).
//
// # Batching and self-sufficiency
//
// One "overall" "choice" question plus one additional "choice" question
// per aspect ("aspect0", "aspect1", ...) are asked in a single client.Ask
// call, all three-option (see RelationSameFact/RelationContradicts/
// RelationDifferentFacts) and all sharing the same `state`
// ({"passage_a", "passage_b"}), per this codebase's "every question must
// be self-sufficient" principle (see internal/tools/match's package doc
// comment).
//
// # What is and isn't independently verified
//
// The "choice" question/answer shape is a verified-live wire fact from
// 2026-09-26 (see internal/openrouter's package doc comment) -- not
// independently re-verified here (no OPENROUTER_API_KEY was available in
// this implementation environment). auto_accept's default of 0.8 is the
// project brief's literal default for this tool. maxAspects (32) is this
// implementation's own invented safety cap: the brief did not state one.
// Truncated is an addition beyond the project brief's literal output
// field list for this tool (which only names overall/aspects), following
// the same "document additions beyond the brief" precedent already
// established by internal/tools/score.ScoreOutput.BudgetExceeded; unlike
// internal/tools/reviewcore, truncation does NOT block Decision ==
// "auto" here -- the brief only specifies that blocking behavior for
// jev_review/jev_gate, not for jev_compare.
package compare

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/pyck-ai/jev-mcp/internal/answers"
	"github.com/pyck-ai/jev-mcp/internal/audit"
	"github.com/pyck-ai/jev-mcp/internal/budget"
	"github.com/pyck-ai/jev-mcp/internal/capstring"
	"github.com/pyck-ai/jev-mcp/internal/config"
	"github.com/pyck-ai/jev-mcp/internal/openrouter"
	"github.com/pyck-ai/jev-mcp/internal/registry"
)

// ToolNameCompare is the MCP tool name registered for CompareHandler.
const ToolNameCompare = "jev_compare"

// Caps named verbatim in the project brief (passage size), plus this
// implementation's own invented cap on aspect count (see package doc
// comment).
const (
	maxPassageChars = 20_000
	maxAspects      = 32
)

// defaultAutoAccept is the project brief's literal default for
// jev_compare's auto_accept.
const defaultAutoAccept = 0.8

// The three relation options every question offers, verbatim from the
// project brief.
const (
	RelationSameFact       = "same_fact"
	RelationContradicts    = "contradicts"
	RelationDifferentFacts = "different_facts"
)

var relationOptions = map[string]bool{RelationSameFact: true, RelationContradicts: true, RelationDifferentFacts: true}

var relationCriteria = map[string]string{
	RelationSameFact:       "both passages describe the same fact/claim, even if worded differently",
	RelationContradicts:    "the passages contradict each other",
	RelationDifferentFacts: "the passages are simply about different facts/claims -- neither supporting nor contradicting one another",
}

// Status values.
const (
	StatusOK              = "ok"
	StatusInvalidResponse = "invalid_response"
)

// Decision values.
const (
	DecisionAuto   = "auto"
	DecisionReview = "review"
)

// Usage mirrors the token accounting reported by OpenRouter for a call.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// CompareInput is the jev_compare tool's input schema.
type CompareInput struct {
	PassageA   string   `json:"passage_a" jsonschema:"First passage to compare. Capped at 20,000 characters."`
	PassageB   string   `json:"passage_b" jsonschema:"Second passage to compare. Capped at 20,000 characters."`
	Aspects    []string `json:"aspects,omitempty" jsonschema:"Optional specific aspects to additionally compare the passages on, each judged independently. Capped at 32."`
	AutoAccept float64  `json:"auto_accept,omitempty" jsonschema:"Confidence bar in (0.5, 1] for decision to be 'auto'. Default 0.8."`
}

// OverallResult is the passages' overall relation.
//
// Callers MUST check Status == "ok" before trusting Relation or
// Confidence: when Status == "invalid_response" those fields are
// zero-valued placeholders and Decision is forced to "review", never a
// fabricated relation.
type OverallResult struct {
	Relation   string  `json:"relation,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
	Decision   string  `json:"decision"`
	Status     string  `json:"status"`
}

// AspectResult is one aspect's relation, framed to that aspect
// specifically. Same fail-closed contract as OverallResult.
type AspectResult struct {
	Aspect     string  `json:"aspect"`
	Relation   string  `json:"relation,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
	Decision   string  `json:"decision"`
	Status     string  `json:"status"`
}

// CompareOutput is the jev_compare tool's output schema.
type CompareOutput struct {
	Overall OverallResult  `json:"overall"`
	Aspects []AspectResult `json:"aspects,omitempty"`
	// Truncated is true if passage_a and/or passage_b exceeded the
	// 20,000-character cap -- an addition beyond the project brief's
	// literal field list for this tool; see package doc comment.
	Truncated      bool   `json:"truncated,omitempty"`
	Model          string `json:"model"`
	Usage          *Usage `json:"usage"`
	LatencyMs      int64  `json:"latency_ms"`
	BudgetExceeded bool   `json:"budget_exceeded,omitempty"`
}

// CompareHandler implements the jev_compare tool.
type CompareHandler struct {
	client           *openrouter.Client
	model            string
	timeout          time.Duration
	budget           *budget.Tracker
	maxUSDPerCall    float64
	maxUSDPerSession float64
	auditLog         *audit.Logger
}

// NewCompareHandler builds a CompareHandler from application dependencies.
func NewCompareHandler(client *openrouter.Client, cfg config.Config, tracker *budget.Tracker, auditLog *audit.Logger) *CompareHandler {
	return &CompareHandler{
		client:           client,
		model:            cfg.ModelForTool(ToolNameCompare),
		timeout:          time.Duration(cfg.RequestTimeoutMs) * time.Millisecond,
		budget:           tracker,
		maxUSDPerCall:    cfg.Budget.MaxUSDPerCall,
		maxUSDPerSession: cfg.Budget.MaxUSDPerSession,
		auditLog:         auditLog,
	}
}

func init() {
	description := "Compare two passages' factual relation (same_fact/contradicts/different_facts), " +
		"overall and optionally per specific aspect, using TypeSafe's Jev judgment model. Fails " +
		"closed: a malformed or missing answer is reported as status=\"invalid_response\" with " +
		"decision=\"review\", never a fabricated relation."
	registry.Register(registry.Tool{
		Name:        "compare",
		MCPName:     ToolNameCompare,
		Description: description,
		RegisterMCP: func(server *mcp.Server, deps *registry.Deps) {
			h := NewCompareHandler(deps.Client, deps.Config, deps.Budget, deps.Audit)
			mcp.AddTool(server, &mcp.Tool{
				Name:        ToolNameCompare,
				Description: description,
			}, h.Handle)
		},
		RegisterCLI: func(root *cobra.Command, provider registry.DepsProvider) {
			root.AddCommand(newCLICommand(provider, description))
		},
	})
}

// Handle implements mcp.ToolHandlerFor[CompareInput, CompareOutput]: a
// thin adapter over run (the transport-agnostic core both the MCP
// handler and the CLI subcommand share).
func (h *CompareHandler) Handle(ctx context.Context, _ *mcp.CallToolRequest, in CompareInput) (*mcp.CallToolResult, CompareOutput, error) {
	out, err := h.run(ctx, in)
	if err != nil {
		return nil, CompareOutput{}, err
	}
	return nil, out, nil
}

// run is jev_compare's transport-agnostic core: everything from input
// validation through the SystemOne call, budget accounting, and audit
// logging, independent of whether the caller is the MCP handler (Handle)
// or the CLI subcommand (cli.go).
func (h *CompareHandler) run(ctx context.Context, in CompareInput) (CompareOutput, error) {
	start := time.Now()

	if err := validateInput(in); err != nil {
		return CompareOutput{}, err
	}
	autoAccept := answers.ResolveThreshold(in.AutoAccept, defaultAutoAccept)

	passageA, truncA := capstring.Truncate(in.PassageA, maxPassageChars)
	passageB, truncB := capstring.Truncate(in.PassageB, maxPassageChars)
	truncated := truncA || truncB
	state := map[string]any{"passage_a": passageA, "passage_b": passageB}

	inputHash := audit.HashValue(in)

	if h.budget.SessionBudgetExceeded() {
		refuseErr := fmt.Errorf("jev_compare: refusing call: session budget exhausted (spent $%.6f, cap $%.6f)", h.budget.Total(), h.maxUSDPerSession)
		h.auditLog.Log(audit.Entry{
			Tool: ToolNameCompare, Model: h.model, InputStateSHA256: inputHash,
			Status: "error", LatencyMs: time.Since(start).Milliseconds(), Error: refuseErr.Error(),
			ItemCount: 1 + len(in.Aspects),
		})
		return CompareOutput{}, refuseErr
	}

	questions := map[string]openrouter.Question{
		"overall": {
			Type:         "choice",
			Instructions: "Considering `state.passage_a` and `state.passage_b` as a whole, what is their relation?",
			Criteria:     relationCriteria,
		},
	}
	for i, aspect := range in.Aspects {
		questions[aspectKey(i)] = openrouter.Question{
			Type: "choice",
			Instructions: fmt.Sprintf(
				"Considering `state.passage_a` and `state.passage_b`, specifically regarding this aspect, "+
					"what is their relation?\n\nAspect: %s",
				aspect,
			),
			Criteria: relationCriteria,
		}
	}

	resp, callErr := h.client.Ask(ctx, h.model, questions, state, h.timeout)
	latencyMs := time.Since(start).Milliseconds()

	if callErr != nil {
		h.auditLog.Log(audit.Entry{
			Tool: ToolNameCompare, Model: h.model, InputStateSHA256: inputHash,
			Status: "error", LatencyMs: latencyMs, Error: callErr.Error(),
			ItemCount: 1 + len(in.Aspects),
		})
		return CompareOutput{}, fmt.Errorf("jev_compare: %w", callErr)
	}

	out := CompareOutput{Model: resp.Model, LatencyMs: latencyMs, Truncated: truncated}
	if resp.Usage != nil {
		out.Usage = &Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens}
	}

	invalidCount := 0
	relation, confidence, ok := parseRelation(resp.Answers, "overall")
	if !ok {
		out.Overall = OverallResult{Status: StatusInvalidResponse, Decision: DecisionReview}
		invalidCount++
	} else {
		out.Overall = OverallResult{Relation: relation, Confidence: confidence, Status: StatusOK, Decision: decisionFor(confidence, autoAccept)}
	}

	if len(in.Aspects) > 0 {
		out.Aspects = make([]AspectResult, len(in.Aspects))
		for i, aspect := range in.Aspects {
			relation, confidence, ok := parseRelation(resp.Answers, aspectKey(i))
			if !ok {
				out.Aspects[i] = AspectResult{Aspect: aspect, Status: StatusInvalidResponse, Decision: DecisionReview}
				invalidCount++
			} else {
				out.Aspects[i] = AspectResult{
					Aspect: aspect, Relation: relation, Confidence: confidence,
					Status: StatusOK, Decision: decisionFor(confidence, autoAccept),
				}
			}
		}
	}

	var costUSD *float64
	if resp.Usage != nil {
		cost := resp.Usage.Cost
		h.budget.Add(cost)
		costUSD = &cost
		if h.maxUSDPerCall > 0 && cost > h.maxUSDPerCall {
			out.BudgetExceeded = true
		}
	}

	status := "ok"
	if invalidCount > 0 {
		status = StatusInvalidResponse
	}
	h.auditLog.Log(audit.Entry{
		Tool: ToolNameCompare, Model: out.Model, InputStateSHA256: inputHash,
		Status: status, CostUSD: costUSD, LatencyMs: out.LatencyMs, BudgetExceeded: out.BudgetExceeded,
		ItemCount: 1 + len(in.Aspects), InvalidCount: invalidCount,
	})

	return out, nil
}

func aspectKey(i int) string { return "aspect" + strconv.Itoa(i) }

func decisionFor(confidence, autoAccept float64) string {
	if confidence >= autoAccept {
		return DecisionAuto
	}
	return DecisionReview
}

// parseRelation looks up key in answersMap and parses it as a "choice"
// answer restricted to relationOptions, returning ok=false -- never a
// fabricated relation -- if the key is missing or the answer is malformed
// or names an option outside relationOptions.
func parseRelation(answersMap map[string]json.RawMessage, key string) (relation string, confidence float64, ok bool) {
	raw, present := answersMap[key]
	if !present {
		return "", 0, false
	}
	relation, confidence, _, ok = answers.Choice(raw, relationOptions)
	return relation, confidence, ok
}

// validateInput rejects obviously-unusable input before spending any
// budget or making a network call.
func validateInput(in CompareInput) error {
	if strings.TrimSpace(in.PassageA) == "" {
		return fmt.Errorf("jev_compare: passage_a must not be empty")
	}
	if strings.TrimSpace(in.PassageB) == "" {
		return fmt.Errorf("jev_compare: passage_b must not be empty")
	}
	if len(in.Aspects) > maxAspects {
		return fmt.Errorf("jev_compare: too many aspects (%d, max %d)", len(in.Aspects), maxAspects)
	}
	for i, a := range in.Aspects {
		if strings.TrimSpace(a) == "" {
			return fmt.Errorf("jev_compare: aspects[%d] must not be empty", i)
		}
	}
	if err := answers.ValidateAutoAccept("auto_accept", in.AutoAccept); err != nil {
		return fmt.Errorf("jev_compare: %w", err)
	}
	return nil
}
