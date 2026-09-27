// Package gate implements the jev_gate MCP tool: jev_review's four-rubric
// code-review assessment PLUS claim verification against caller-supplied
// evidence, combined into a single stricter auto/review/escalate gate
// decision, using TypeSafe's Jev judgment model via OpenRouter's
// SystemOne API. All of it -- the review rubrics and every claim
// verification -- happens in ONE client.Ask call.
//
// This package is a self-registering plugin (see internal/registry's
// package doc comment for the overall mechanism). The review half's
// question-building/answer-parsing/composite logic is shared with
// internal/tools/review via internal/tools/reviewcore (see that package's
// doc comment for why the shared logic lives there rather than this
// package importing internal/tools/review directly).
//
// # Claim verification is evidence-only
//
// Per the project brief, every claim's "choice" question is explicitly
// instructed to use ONLY the supplied Evidence, never Request/Diff/Tests
// -- deliberately keeping "is this diff good" (the review half) and "are
// these specific factual claims about it true" (the verification half)
// epistemically separate, even though both are answered in the same HTTP
// call and share one `state` object (which is why the instructions
// mention state.evidence, not just implicit access to all of `state`).
//
// # Action decision rule (verbatim priority order from the project brief)
//
//  1. A confidently contradicted claim (Status ok, Verdict
//     VerdictContradicts, Action ActionAuto) forces Action ==
//     ActionEscalate, regardless of anything else -- "a confidently
//     contradicted claim forces escalate".
//  2. Otherwise, if the review half's own Action is ActionEscalate,
//     GateOutput.Action is also ActionEscalate (this implementation's own
//     addition: the brief doesn't say explicitly what happens when review
//     itself escalates, but "auto only if review action is auto" already
//     implies non-auto review can't produce an auto gate, and propagating
//     an escalate-worthy review as an escalate-worthy gate, rather than
//     downgrading it to merely "review", was judged the safer choice).
//  3. Otherwise, Action == ActionAuto only if the review half's Action is
//     ActionAuto AND every claim's own Action is ActionAuto ("auto only
//     if review action is auto AND every claim verifies auto").
//  4. Otherwise, Action == ActionReview.
//
// # What is and isn't independently verified
//
// See internal/tools/reviewcore's and internal/tools/verify's package doc
// comments: the "score"/"noul"/"choice" wire shapes are verified-live
// facts from 2026-09-26, not independently re-verified here (no
// OPENROUTER_API_KEY was available in this implementation environment).
package gate

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/pyck-ai/jev-mcp/internal/answers"
	"github.com/pyck-ai/jev-mcp/internal/audit"
	"github.com/pyck-ai/jev-mcp/internal/budget"
	"github.com/pyck-ai/jev-mcp/internal/config"
	"github.com/pyck-ai/jev-mcp/internal/openrouter"
	"github.com/pyck-ai/jev-mcp/internal/registry"
	"github.com/pyck-ai/jev-mcp/internal/tools/reviewcore"
)

// ToolNameGate is the MCP tool name registered for GateHandler.
const ToolNameGate = "jev_gate"

// Caps named verbatim in the project brief for jev_gate's claims/evidence
// (distinct from, and stricter than, internal/tools/verify's own invented
// cap for its general-purpose claims list).
const (
	maxClaims           = 16
	maxEvidenceItems    = 16
	maxEvidenceAggChars = 200_000
)

// Verdict/status/action constants, shared vocabulary with
// internal/tools/verify (not imported from there -- see package doc
// comment on why gate and verify don't share a dependency, only
// review/gate do via reviewcore).
const (
	VerdictSupports    = "supports"
	VerdictContradicts = "contradicts"
	VerdictSaysNothing = "says_nothing"

	StatusOK              = "ok"
	StatusInvalidResponse = "invalid_response"

	ActionAuto     = "auto"
	ActionReview   = "review"
	ActionEscalate = "escalate"
)

var verdictOptions = map[string]bool{VerdictSupports: true, VerdictContradicts: true, VerdictSaysNothing: true}

// Usage mirrors the token accounting reported by OpenRouter for a call.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// EvidenceItem is one item of gate's (always structured, unlike
// jev_verify's dual-shape Evidence) evidence list.
type EvidenceItem struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// GateInput is the jev_gate tool's input schema: jev_review's input
// (request/diff/tests plus thresholds) plus claims/evidence.
type GateInput struct {
	Request        string             `json:"request" jsonschema:"The original request/task the diff is meant to satisfy. Capped at 50,000 characters."`
	Diff           string             `json:"diff" jsonschema:"The diff to review. Capped at 50,000 characters."`
	Tests          string             `json:"tests,omitempty" jsonschema:"Optional test output/description. Capped at 50,000 characters."`
	Claims         []string           `json:"claims" jsonschema:"Claims to verify against evidence (NOT against request/diff/tests). Capped at 16."`
	Evidence       []EvidenceItem     `json:"evidence" jsonschema:"Evidence claims are verified against. Capped at 16 items, 200,000 characters aggregate."`
	AutoAccept     float64            `json:"auto_accept,omitempty" jsonschema:"Confidence bar in (0.5, 1] for both review rubrics and claim verification. Default 0.8."`
	CompositeFloor float64            `json:"composite_floor,omitempty" jsonschema:"Minimum weighted review composite in [0,1] for the review half to be 'auto'. Default 0.7."`
	Weights        reviewcore.Weights `json:"weights,omitempty" jsonschema:"Optional override of the default review rubric weights; normalized to sum to 1."`
}

// ClaimResult is one claim's verification result, evidence-only (see
// package doc comment). Same fail-closed contract as
// internal/tools/verify.ClaimResult: Status != "ok" means Verdict/
// Confidence/Probabilities are zero-valued placeholders and Action is
// forced to ActionReview.
type ClaimResult struct {
	Claim         string             `json:"claim"`
	Verdict       string             `json:"verdict,omitempty"`
	Confidence    float64            `json:"confidence,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Action        string             `json:"action"`
	Status        string             `json:"status"`
}

// VerificationSummary tallies ClaimResult.Action/Status across every
// claim, for a caller that doesn't want to re-scan Results itself.
type VerificationSummary struct {
	Auto         int `json:"auto"`
	Review       int `json:"review"`
	Invalid      int `json:"invalid"`
	Contradicted int `json:"contradicted"`
}

// Verification is the claim-verification half of GateOutput.
type Verification struct {
	Summary VerificationSummary `json:"summary"`
	Results []ClaimResult       `json:"results"`
}

// GateOutput is the jev_gate tool's output schema.
type GateOutput struct {
	Action         string                `json:"action"`
	Review         reviewcore.Assessment `json:"review"`
	Verification   Verification          `json:"verification"`
	ReasonCodes    []string              `json:"reason_codes,omitempty"`
	Model          string                `json:"model"`
	Usage          *Usage                `json:"usage"`
	LatencyMs      int64                 `json:"latency_ms"`
	BudgetExceeded bool                  `json:"budget_exceeded,omitempty"`
}

// GateHandler implements the jev_gate tool.
type GateHandler struct {
	client           *openrouter.Client
	model            string
	timeout          time.Duration
	budget           *budget.Tracker
	maxUSDPerCall    float64
	maxUSDPerSession float64
	auditLog         *audit.Logger
}

// NewGateHandler builds a GateHandler from application dependencies.
func NewGateHandler(client *openrouter.Client, cfg config.Config, tracker *budget.Tracker, auditLog *audit.Logger) *GateHandler {
	return &GateHandler{
		client:           client,
		model:            cfg.ModelForTool(ToolNameGate),
		timeout:          time.Duration(cfg.RequestTimeoutMs) * time.Millisecond,
		budget:           tracker,
		maxUSDPerCall:    cfg.Budget.MaxUSDPerCall,
		maxUSDPerSession: cfg.Budget.MaxUSDPerSession,
		auditLog:         auditLog,
	}
}

func init() {
	description := "jev_review plus claim verification against supplied evidence (evidence-only, never " +
		"against request/diff/tests), combined into one stricter gate decision using TypeSafe's Jev " +
		"judgment model: action=\"auto\" only if the review half is auto AND every claim verifies auto; " +
		"a confidently contradicted claim forces action=\"escalate\" regardless of anything else. Fails " +
		"closed throughout, never a fabricated verdict."
	registry.Register(registry.Tool{
		Name:        "gate",
		MCPName:     ToolNameGate,
		Description: description,
		RegisterMCP: func(server *mcp.Server, deps *registry.Deps) {
			h := NewGateHandler(deps.Client, deps.Config, deps.Budget, deps.Audit)
			mcp.AddTool(server, &mcp.Tool{
				Name:        ToolNameGate,
				Description: description,
			}, h.Handle)
		},
		RegisterCLI: func(root *cobra.Command, provider registry.DepsProvider) {
			root.AddCommand(newCLICommand(provider, description))
		},
	})
}

// Handle implements mcp.ToolHandlerFor[GateInput, GateOutput]: a thin
// adapter over run (the transport-agnostic core both the MCP handler and
// the CLI subcommand share).
func (h *GateHandler) Handle(ctx context.Context, _ *mcp.CallToolRequest, in GateInput) (*mcp.CallToolResult, GateOutput, error) {
	out, err := h.run(ctx, in)
	if err != nil {
		return nil, GateOutput{}, err
	}
	return nil, out, nil
}

// run is jev_gate's transport-agnostic core: everything from input
// validation through the SystemOne call, budget accounting, and audit
// logging, independent of whether the caller is the MCP handler (Handle)
// or the CLI subcommand (cli.go).
func (h *GateHandler) run(ctx context.Context, in GateInput) (GateOutput, error) {
	start := time.Now()

	if err := validateInput(in); err != nil {
		return GateOutput{}, err
	}

	prepared, truncated := reviewcore.Prepare(in.Request, in.Diff, in.Tests)
	state := map[string]any{"request": prepared.Request, "diff": prepared.Diff, "evidence": in.Evidence}
	if prepared.Tests != "" {
		state["tests"] = prepared.Tests
	}

	inputHash := audit.HashValue(in)
	itemCount := 5 + len(in.Claims)

	if h.budget.SessionBudgetExceeded() {
		refuseErr := fmt.Errorf("jev_gate: refusing call: session budget exhausted (spent $%.6f, cap $%.6f)", h.budget.Total(), h.maxUSDPerSession)
		h.auditLog.Log(audit.Entry{
			Tool: ToolNameGate, Model: h.model, InputStateSHA256: inputHash,
			Status: "error", LatencyMs: time.Since(start).Milliseconds(), Error: refuseErr.Error(),
			ItemCount: itemCount,
		})
		return GateOutput{}, refuseErr
	}

	questions := reviewcore.Questions()
	for i, claim := range in.Claims {
		questions[claimKey(i)] = openrouter.Question{
			Type: "choice",
			Instructions: fmt.Sprintf(
				"Using ONLY the evidence in `state.evidence` (NOT `state.request`, `state.diff`, or "+
					"`state.tests`), does the evidence support, contradict, or say nothing about this claim?"+
					"\n\nClaim: %s",
				claim,
			),
			Criteria: map[string]string{
				VerdictSupports:    "the evidence supports this claim",
				VerdictContradicts: "the evidence contradicts this claim",
				VerdictSaysNothing: "the evidence says nothing about this claim (neither supports nor contradicts it)",
			},
		}
	}

	resp, callErr := h.client.Ask(ctx, h.model, questions, state, h.timeout)
	latencyMs := time.Since(start).Milliseconds()

	if callErr != nil {
		h.auditLog.Log(audit.Entry{
			Tool: ToolNameGate, Model: h.model, InputStateSHA256: inputHash,
			Status: "error", LatencyMs: latencyMs, Error: callErr.Error(),
			ItemCount: itemCount,
		})
		return GateOutput{}, fmt.Errorf("jev_gate: %w", callErr)
	}

	autoAccept := answers.ResolveThreshold(in.AutoAccept, reviewcore.DefaultAutoAccept)
	reviewAssessment := reviewcore.ParseAssessment(resp.Answers, truncated, in.AutoAccept, in.CompositeFloor, in.Weights)

	claimResults := make([]ClaimResult, len(in.Claims))
	claimInvalid := 0
	for i, claim := range in.Claims {
		res := ClaimResult{Claim: claim}
		raw, present := resp.Answers[claimKey(i)]
		var choice string
		var confidence float64
		var probs map[string]float64
		ok := false
		if present {
			choice, confidence, probs, ok = answers.Choice(raw, verdictOptions)
		}
		if !ok {
			res.Status = StatusInvalidResponse
			res.Action = ActionReview
			claimInvalid++
		} else {
			res.Status = StatusOK
			res.Verdict = choice
			res.Confidence = confidence
			res.Probabilities = probs
			if confidence >= autoAccept {
				res.Action = ActionAuto
			} else {
				res.Action = ActionReview
			}
		}
		claimResults[i] = res
	}

	action, reasonCodes := computeAction(reviewAssessment, claimResults)

	out := GateOutput{
		Action:      action,
		Review:      reviewAssessment,
		ReasonCodes: reasonCodes,
		Verification: Verification{
			Summary: summarize(claimResults),
			Results: claimResults,
		},
		Model:     resp.Model,
		LatencyMs: latencyMs,
	}
	if resp.Usage != nil {
		out.Usage = &Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens}
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

	invalidCount := reviewAssessment.InvalidCount() + claimInvalid
	status := "ok"
	if invalidCount > 0 {
		status = "invalid_response"
	}
	h.auditLog.Log(audit.Entry{
		Tool: ToolNameGate, Model: out.Model, InputStateSHA256: inputHash,
		Status: status, CostUSD: costUSD, LatencyMs: out.LatencyMs, BudgetExceeded: out.BudgetExceeded,
		ItemCount: itemCount, InvalidCount: invalidCount,
	})

	return out, nil
}

func claimKey(i int) string { return "claim" + strconv.Itoa(i) }

// computeAction implements this package's doc comment's "Action decision
// rule" exactly, in priority order, and collects human-readable
// ReasonCodes explaining the result (review's own reason codes prefixed
// with "review:", plus gate-specific claim-level codes).
func computeAction(reviewAssessment reviewcore.Assessment, claims []ClaimResult) (action string, reasonCodes []string) {
	for _, code := range reviewAssessment.ReasonCodes {
		reasonCodes = append(reasonCodes, "review:"+code)
	}

	contradicted := false
	allClaimsAuto := true
	for i, c := range claims {
		switch {
		case c.Status != StatusOK:
			reasonCodes = append(reasonCodes, fmt.Sprintf("claim_invalid_response:%d", i))
			allClaimsAuto = false
		case c.Action != ActionAuto:
			reasonCodes = append(reasonCodes, fmt.Sprintf("claim_needs_review:%d", i))
			allClaimsAuto = false
		}
		if c.Status == StatusOK && c.Verdict == VerdictContradicts && c.Action == ActionAuto {
			contradicted = true
			reasonCodes = append(reasonCodes, fmt.Sprintf("confidently_contradicted_claim:%d", i))
		}
	}

	switch {
	case contradicted:
		action = ActionEscalate
	case reviewAssessment.Action == reviewcore.ActionEscalate:
		action = ActionEscalate
	case reviewAssessment.Action == reviewcore.ActionAuto && allClaimsAuto:
		action = ActionAuto
	default:
		action = ActionReview
	}
	return action, reasonCodes
}

func summarize(claims []ClaimResult) VerificationSummary {
	var s VerificationSummary
	for _, c := range claims {
		switch {
		case c.Status != StatusOK:
			s.Invalid++
		case c.Action == ActionAuto:
			s.Auto++
		default:
			s.Review++
		}
		if c.Status == StatusOK && c.Verdict == VerdictContradicts {
			s.Contradicted++
		}
	}
	return s
}

// validateInput rejects obviously-unusable input before spending any
// budget or making a network call.
func validateInput(in GateInput) error {
	if strings.TrimSpace(in.Request) == "" {
		return fmt.Errorf("jev_gate: request must not be empty")
	}
	if strings.TrimSpace(in.Diff) == "" {
		return fmt.Errorf("jev_gate: diff must not be empty")
	}
	if len(in.Claims) == 0 {
		return fmt.Errorf("jev_gate: claims must not be empty")
	}
	if len(in.Claims) > maxClaims {
		return fmt.Errorf("jev_gate: too many claims (%d, max %d)", len(in.Claims), maxClaims)
	}
	for i, c := range in.Claims {
		if strings.TrimSpace(c) == "" {
			return fmt.Errorf("jev_gate: claims[%d] must not be empty", i)
		}
	}
	if len(in.Evidence) == 0 {
		return fmt.Errorf("jev_gate: evidence must not be empty")
	}
	if len(in.Evidence) > maxEvidenceItems {
		return fmt.Errorf("jev_gate: too many evidence items (%d, max %d)", len(in.Evidence), maxEvidenceItems)
	}
	seen := make(map[string]bool, len(in.Evidence))
	aggregate := 0
	for i, e := range in.Evidence {
		if strings.TrimSpace(e.ID) == "" {
			return fmt.Errorf("jev_gate: evidence[%d].id must not be empty", i)
		}
		if seen[e.ID] {
			return fmt.Errorf("jev_gate: duplicate evidence id %q", e.ID)
		}
		seen[e.ID] = true
		if strings.TrimSpace(e.Text) == "" {
			return fmt.Errorf("jev_gate: evidence[%d].text must not be empty", i)
		}
		aggregate += len([]rune(e.Text))
	}
	if aggregate > maxEvidenceAggChars {
		return fmt.Errorf("jev_gate: aggregate evidence text too long (%d chars, max %d)", aggregate, maxEvidenceAggChars)
	}
	if err := answers.ValidateAutoAccept("auto_accept", in.AutoAccept); err != nil {
		return fmt.Errorf("jev_gate: %w", err)
	}
	if in.CompositeFloor != 0 && (in.CompositeFloor < 0 || in.CompositeFloor > 1) {
		return fmt.Errorf("jev_gate: composite_floor must be in [0,1] if set, got %v", in.CompositeFloor)
	}
	for name, w := range map[string]float64{
		"weights.correctness": in.Weights.Correctness, "weights.spec_match": in.Weights.SpecMatch,
		"weights.test_gap": in.Weights.TestGap, "weights.blast_radius": in.Weights.BlastRadius,
	} {
		if w < 0 {
			return fmt.Errorf("jev_gate: %s must not be negative, got %v", name, w)
		}
	}
	return nil
}
