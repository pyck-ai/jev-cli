// Package review implements the jev_review MCP tool: a weighted,
// multi-rubric code-review assessment (correctness, spec match, test
// gap, blast radius, and a safe-to-apply signal) of a diff against a
// request, using the configured SystemOne decision model's "score" and "noul"
// question types, via OpenRouter's SystemOne API.
//
// This package is a self-registering plugin (see internal/registry's
// package doc comment for the overall mechanism). The actual scoring
// logic -- question construction, answer parsing, composite computation,
// and the auto/review/escalate decision rule -- lives in
// internal/tools/reviewcore, shared with internal/tools/gate (jev_gate is
// jev_review plus claim verification, folded into the same SystemOne
// call; see reviewcore's package doc comment for why the shared logic
// lives in its own package rather than gate importing review directly).
// This package is a thin wrapper: input validation/truncation, the
// client.Ask call, and the usual budget/audit plumbing.
//
// # What is and isn't independently verified
//
// See internal/tools/reviewcore's package doc comment: the "score" and
// "noul" wire shapes are verified-live facts from 2026-09-26, not
// independently re-verified here (no OPENROUTER_API_KEY was available in
// this implementation environment).
package review

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/pyck-ai/jev-cli/internal/answers"
	"github.com/pyck-ai/jev-cli/internal/audit"
	"github.com/pyck-ai/jev-cli/internal/budget"
	"github.com/pyck-ai/jev-cli/internal/config"
	"github.com/pyck-ai/jev-cli/internal/openrouter"
	"github.com/pyck-ai/jev-cli/internal/registry"
	"github.com/pyck-ai/jev-cli/internal/tools/reviewcore"
)

// ToolNameReview is the MCP tool name registered for ReviewHandler.
const ToolNameReview = "jev_review"

// Usage mirrors the token accounting reported by OpenRouter for a call.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// CostUSD is the real cost of the call in USD as reported by the API;
	// omitted when the API returned no cost.
	CostUSD *float64 `json:"cost_usd,omitempty"`
}

// ReviewInput is the jev_review tool's input schema.
type ReviewInput struct {
	Request        string             `json:"request" jsonschema:"Original task the diff should satisfy; max 50,000 chars."`
	Diff           string             `json:"diff" jsonschema:"Diff to review; max 50,000 chars."`
	Tests          string             `json:"tests,omitempty" jsonschema:"Test output/description; max 50,000 chars."`
	AutoAccept     float64            `json:"auto_accept,omitempty" jsonschema:"Confidence bar in (0.5, 1] each rubric must meet for auto. Default 0.8."`
	CompositeFloor float64            `json:"composite_floor,omitempty" jsonschema:"Minimum weighted composite in [0,1] for auto. Default 0.7."`
	Weights        reviewcore.Weights `json:"weights,omitempty" jsonschema:"Rubric weights override; default correctness 0.4, spec_match 0.3, test_gap 0.15, blast_radius 0.15."`
}

// ReviewOutput is the jev_review tool's output schema. It embeds
// reviewcore.Assessment directly (flattening correctness/spec_match/
// test_gap/blast_radius/safe_to_apply/composite/truncated/action/
// reason_codes to the top level of the JSON object) alongside this tool's
// own call metadata.
type ReviewOutput struct {
	reviewcore.Assessment
	Model          string `json:"model"`
	Usage          *Usage `json:"usage"`
	LatencyMs      int64  `json:"latency_ms"`
	BudgetExceeded bool   `json:"budget_exceeded,omitempty"`
}

// ReviewHandler implements the jev_review tool.
type ReviewHandler struct {
	client           *openrouter.Client
	model            string
	timeout          time.Duration
	budget           *budget.Tracker
	maxUSDPerCall    float64
	maxUSDPerSession float64
	auditLog         *audit.Logger
}

// NewReviewHandler builds a ReviewHandler from application dependencies.
func NewReviewHandler(client *openrouter.Client, cfg config.Config, tracker *budget.Tracker, auditLog *audit.Logger) *ReviewHandler {
	return &ReviewHandler{
		client:           client,
		model:            cfg.ModelForTool(ToolNameReview),
		timeout:          time.Duration(cfg.RequestTimeoutMs) * time.Millisecond,
		budget:           tracker,
		maxUSDPerCall:    cfg.Budget.MaxUSDPerCall,
		maxUSDPerSession: cfg.Budget.MaxUSDPerSession,
		auditLog:         auditLog,
	}
}

func init() {
	description := "Assess a diff against its request on four weighted rubrics (correctness, spec_match, test_gap, " +
		"blast_radius) plus safe_to_apply; returns action auto/review/escalate and a composite in [0,1]. " +
		"Use jev_gate instead when you also have factual claims to verify. Fails closed: bad rubric " +
		"answers force escalate."
	registry.Register(registry.Tool{
		Name:        "review",
		MCPName:     ToolNameReview,
		Description: description,
		RegisterMCP: func(server *mcp.Server, deps *registry.Deps) {
			h := NewReviewHandler(deps.Client, deps.Config, deps.Budget, deps.Audit)
			mcp.AddTool(server, &mcp.Tool{
				Name:        ToolNameReview,
				Description: description,
			}, h.Handle)
		},
		RegisterCLI: func(root *cobra.Command, provider registry.DepsProvider) {
			root.AddCommand(newCLICommand(provider, description))
		},
		Run: registry.Runner(func(ctx context.Context, d *registry.Deps, in ReviewInput) (ReviewOutput, error) {
			return NewReviewHandler(d.Client, d.Config, d.Budget, d.Audit).run(ctx, in)
		}, exitCode),
	})
}

// Handle implements mcp.ToolHandlerFor[ReviewInput, ReviewOutput]: a
// thin adapter over run (the transport-agnostic core both the MCP
// handler and the CLI subcommand share).
func (h *ReviewHandler) Handle(ctx context.Context, _ *mcp.CallToolRequest, in ReviewInput) (*mcp.CallToolResult, ReviewOutput, error) {
	out, err := h.run(ctx, in)
	if err != nil {
		return nil, ReviewOutput{}, err
	}
	return nil, out, nil
}

// run is jev_review's transport-agnostic core: everything from input
// validation through the SystemOne call, budget accounting, and audit
// logging, independent of whether the caller is the MCP handler (Handle)
// or the CLI subcommand (cli.go).
func (h *ReviewHandler) run(ctx context.Context, in ReviewInput) (ReviewOutput, error) {
	start := time.Now()

	if err := validateInput(in); err != nil {
		return ReviewOutput{}, err
	}

	prepared, truncated := reviewcore.Prepare(in.Request, in.Diff, in.Tests)
	state := reviewcore.State(prepared)
	inputHash := audit.HashValue(in)

	if h.budget.SessionBudgetExceeded() {
		refuseErr := fmt.Errorf("jev_review: refusing call: session budget exhausted (spent $%.6f, cap $%.6f)", h.budget.Total(), h.maxUSDPerSession)
		h.auditLog.Log(audit.Entry{
			Tool: ToolNameReview, Model: h.model, InputStateSHA256: inputHash,
			Status: "error", LatencyMs: time.Since(start).Milliseconds(), Error: refuseErr.Error(),
			ItemCount: 5,
		})
		return ReviewOutput{}, refuseErr
	}

	resp, callErr := h.client.Ask(ctx, h.model, reviewcore.Questions(), state, h.timeout)
	latencyMs := time.Since(start).Milliseconds()

	if callErr != nil {
		h.auditLog.Log(audit.Entry{
			Tool: ToolNameReview, Model: h.model, InputStateSHA256: inputHash,
			Status: "error", LatencyMs: latencyMs, Error: callErr.Error(),
			ItemCount: 5,
		})
		return ReviewOutput{}, fmt.Errorf("jev_review: %w", callErr)
	}

	assessment := reviewcore.ParseAssessment(resp.Answers, truncated, in.AutoAccept, in.CompositeFloor, in.Weights)
	out := ReviewOutput{Assessment: assessment, Model: resp.Model, LatencyMs: latencyMs}
	if resp.Usage != nil {
		out.Usage = &Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens, CostUSD: resp.Usage.CostUSD()}
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

	invalidCount := assessment.InvalidCount()
	status := "ok"
	if invalidCount > 0 {
		status = "invalid_response"
	}
	h.auditLog.Log(audit.Entry{
		Tool: ToolNameReview, Model: out.Model, InputStateSHA256: inputHash,
		Status: status, CostUSD: costUSD, LatencyMs: out.LatencyMs, BudgetExceeded: out.BudgetExceeded,
		ItemCount: assessment.ItemCount(), InvalidCount: invalidCount,
	})

	return out, nil
}

// validateInput rejects obviously-unusable input before spending any
// budget or making a network call. Note: Request/Diff/Tests are NOT
// length-checked here (over-cap is handled by silent truncation in
// reviewcore.Prepare, not rejection) -- only emptiness and threshold
// shapes are validated up front.
func validateInput(in ReviewInput) error {
	if strings.TrimSpace(in.Request) == "" {
		return fmt.Errorf("jev_review: request must not be empty; describe the original task/request the diff is meant to satisfy (a non-empty string, up to 50,000 characters)")
	}
	if strings.TrimSpace(in.Diff) == "" {
		return fmt.Errorf("jev_review: diff must not be empty; provide the diff/patch text to review (a non-empty string, up to 50,000 characters)")
	}
	if err := answers.ValidateAutoAccept("auto_accept", in.AutoAccept); err != nil {
		return fmt.Errorf(`jev_review: %w (0 means "use the default 0.8")`, err)
	}
	if in.CompositeFloor != 0 && (in.CompositeFloor < 0 || in.CompositeFloor > 1) {
		return fmt.Errorf(`jev_review: composite_floor must be in the range [0,1] if set (0 means use the default 0.7), got %v; this is the minimum weighted composite required for action to be "auto"`, in.CompositeFloor)
	}
	for name, w := range map[string]float64{
		"weights.correctness": in.Weights.Correctness, "weights.spec_match": in.Weights.SpecMatch,
		"weights.test_gap": in.Weights.TestGap, "weights.blast_radius": in.Weights.BlastRadius,
	} {
		if w < 0 {
			return fmt.Errorf("jev_review: %s must be >= 0, got %v; the four weights are normalized to sum to 1, so only non-negative values make sense", name, w)
		}
	}
	return nil
}
