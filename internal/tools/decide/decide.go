// Package decide implements the jev_decide MCP tool: recommends which of
// several candidate options best satisfies a decision (given evidence and
// priorities), optionally checking a list of requirements against every
// candidate, using TypeSafe's Jev judgment model's "choice" question
// type, via OpenRouter's SystemOne API.
//
// This package is a self-registering plugin (see internal/registry's
// package doc comment for the overall mechanism).
//
// # Question-type mapping (one of two designs offered by the project brief)
//
// The project brief offered two designs for requirement checking: "one
// choice question per requirement per candidate" (a full
// requirement x candidate matrix), or "one question per requirement asking
// which candidates it's supported/contradicted for" (one question per
// requirement, answering for all candidates at once). This package
// implements the FIRST design (one "choice" question per (requirement,
// candidate) pair, options supported/contradicted/unclear), because the
// brief's own output shape -- `checks []{candidate, requirement index,
// answer}`, i.e. one entry per (candidate, requirement) pair -- is exactly
// that matrix; a "choice" question only ever returns ONE selected option,
// so the second design (one question per requirement) could only name a
// single candidate per requirement, silently discarding per-candidate
// detail for every other candidate on that same requirement. The first
// design maps directly onto both the "choice" primitive (one answer type,
// three options, no partial/multi-select needed) and the brief's own
// output shape with no reshaping required.
//
// # Batching and self-sufficiency
//
// One main "choice" question ("decision", options = every candidate id
// plus, if EscapeHatches, the three escape-hatch ids) plus one "choice"
// question per (requirement, candidate) pair are all asked in a single
// client.Ask call, sharing one `state` object
// ({"decision", "evidence", "priorities"}), per this codebase's "every
// question must be self-sufficient" principle (see
// internal/tools/match's package doc comment).
//
// # What is and isn't independently verified
//
// The "choice" question/answer shape is a verified-live wire fact from
// 2026-09-26 (see internal/openrouter's package doc comment) -- not
// independently re-verified here (no OPENROUTER_API_KEY was available in
// this implementation environment). Unlike most of this tool suite,
// jev_decide's own project brief names NO confidence/auto-accept
// threshold for its Recommendation at all (only "recommendation{selected,
// escaped, confidence, probabilities}" -- no "action" or "decision"
// field) -- so, deliberately, this implementation adds none: Recommendation
// exposes Confidence/Probabilities for the CALLER to interpret, rather
// than inventing an auto_accept-driven verdict field the brief didn't ask
// for. maxRequirements (20) is this implementation's own invented safety
// cap: the brief did not state one (unlike jev_gate's explicit
// "claims []string (cap 16)" for a similar list).
package decide

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
)

// ToolNameDecide is the MCP tool name registered for DecideHandler.
const ToolNameDecide = "jev_decide"

// Candidate count bounds, verbatim from the project brief
// ("candidates []{id, description} (2-6)").
const (
	minCandidates = 2
	maxCandidates = 6
)

// maxRequirements is this implementation's own invented safety cap (see
// package doc comment).
const maxRequirements = 20

// Escape-hatch option ids, offered alongside candidate ids in the main
// "decision" question when EscapeHatches is true. Reserved: no candidate
// may use one of these as its own id (see validateInput), so Escaped can
// always be computed unambiguously from Recommendation.Selected.
const (
	EscapeAskUser     = "ask_user"
	EscapeInvestigate = "investigate"
	EscapeNone        = "none"
)

var escapeHatchIDs = map[string]bool{EscapeAskUser: true, EscapeInvestigate: true, EscapeNone: true}

var escapeHatchCriteria = map[string]string{
	EscapeAskUser:     "escalate: ask the user for more input before deciding",
	EscapeInvestigate: "escalate: gather more evidence/investigate further before deciding",
	EscapeNone:        "none of the candidates are acceptable; do not proceed with any of them",
}

// Requirement-check answer options, verbatim from the project brief,
// plus AnswerInvalidResponse for a malformed/missing answer (fail-closed;
// mirrors internal/tools/extract's FieldResult.Status, which folds
// "invalid_response" into the same enum as its other outcome values
// rather than a separate parallel field).
const (
	AnswerSupported       = "supported"
	AnswerContradicted    = "contradicted"
	AnswerUnclear         = "unclear"
	AnswerInvalidResponse = "invalid_response"
)

var requirementAnswerOptions = map[string]bool{AnswerSupported: true, AnswerContradicted: true, AnswerUnclear: true}

// Status values for Recommendation.Status.
const (
	StatusOK              = "ok"
	StatusInvalidResponse = "invalid_response"
)

// Usage mirrors the token accounting reported by OpenRouter for a call.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Candidate is one candidate option for the decision.
type Candidate struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

// DecideInput is the jev_decide tool's input schema.
type DecideInput struct {
	Decision     string      `json:"decision" jsonschema:"The decision to be made."`
	Evidence     string      `json:"evidence" jsonschema:"Evidence relevant to the decision."`
	Priorities   string      `json:"priorities" jsonschema:"Priorities/tradeoffs that should guide the decision."`
	Candidates   []Candidate `json:"candidates" jsonschema:"Candidate options, 2-6 of them."`
	Requirements []string    `json:"requirements,omitempty" jsonschema:"Optional requirements to check every candidate against. Capped at 20."`
	// EscapeHatches is a *bool (not bool) specifically so this handler can
	// tell "omitted" (nil -> defaults to true) apart from "explicitly
	// false": a plain bool field cannot distinguish those two cases after
	// JSON unmarshaling, since both leave the Go field at its zero value.
	EscapeHatches *bool `json:"escape_hatches,omitempty" jsonschema:"Whether to offer ask_user/investigate/none escape hatches alongside the real candidates in the main recommendation. Default true."`
}

// Recommendation is jev_decide's main recommendation.
//
// Callers MUST check Status == "ok" before trusting Selected, Escaped,
// Confidence, or Probabilities: when Status == "invalid_response" those
// fields are zero-valued placeholders, never a fabricated recommendation.
type Recommendation struct {
	Selected      string             `json:"selected,omitempty"`
	Escaped       bool               `json:"escaped"`
	Confidence    float64            `json:"confidence,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Status        string             `json:"status"`
}

// Check is one (candidate, requirement) pair's requirement-check result.
// Requirement is the 0-based index into DecideInput.Requirements. Answer
// is one of AnswerSupported/AnswerContradicted/AnswerUnclear/
// AnswerInvalidResponse (the last one meaning a malformed/missing
// answer -- fail closed, never fabricated).
type Check struct {
	Candidate   string `json:"candidate"`
	Requirement int    `json:"requirement"`
	Answer      string `json:"answer"`
}

// DecideOutput is the jev_decide tool's output schema.
type DecideOutput struct {
	Recommendation Recommendation `json:"recommendation"`
	Checks         []Check        `json:"checks,omitempty"`
	Model          string         `json:"model"`
	Usage          *Usage         `json:"usage"`
	LatencyMs      int64          `json:"latency_ms"`
	BudgetExceeded bool           `json:"budget_exceeded,omitempty"`
}

// DecideHandler implements the jev_decide tool.
type DecideHandler struct {
	client           *openrouter.Client
	model            string
	timeout          time.Duration
	budget           *budget.Tracker
	maxUSDPerCall    float64
	maxUSDPerSession float64
	auditLog         *audit.Logger
}

// NewDecideHandler builds a DecideHandler from application dependencies.
func NewDecideHandler(client *openrouter.Client, cfg config.Config, tracker *budget.Tracker, auditLog *audit.Logger) *DecideHandler {
	return &DecideHandler{
		client:           client,
		model:            cfg.ModelForTool(ToolNameDecide),
		timeout:          time.Duration(cfg.RequestTimeoutMs) * time.Millisecond,
		budget:           tracker,
		maxUSDPerCall:    cfg.Budget.MaxUSDPerCall,
		maxUSDPerSession: cfg.Budget.MaxUSDPerSession,
		auditLog:         auditLog,
	}
}

func init() {
	description := "Recommend which of 2-6 candidate options best satisfies a decision (given evidence " +
		"and priorities), with optional escape hatches (ask_user/investigate/none) and optional " +
		"per-requirement, per-candidate checks, using TypeSafe's Jev judgment model. Fails closed: a " +
		"malformed or missing answer is reported as status=\"invalid_response\" (recommendation) or " +
		"answer=\"invalid_response\" (a requirement check), never fabricated."
	registry.Register(registry.Tool{
		Name:        "decide",
		MCPName:     ToolNameDecide,
		Description: description,
		RegisterMCP: func(server *mcp.Server, deps *registry.Deps) {
			h := NewDecideHandler(deps.Client, deps.Config, deps.Budget, deps.Audit)
			mcp.AddTool(server, &mcp.Tool{
				Name:        ToolNameDecide,
				Description: description,
			}, h.Handle)
		},
		RegisterCLI: func(root *cobra.Command, provider registry.DepsProvider) {
			root.AddCommand(newCLICommand(provider, description))
		},
	})
}

// Handle implements mcp.ToolHandlerFor[DecideInput, DecideOutput]: a
// thin adapter over run (the transport-agnostic core both the MCP
// handler and the CLI subcommand share).
func (h *DecideHandler) Handle(ctx context.Context, _ *mcp.CallToolRequest, in DecideInput) (*mcp.CallToolResult, DecideOutput, error) {
	out, err := h.run(ctx, in)
	if err != nil {
		return nil, DecideOutput{}, err
	}
	return nil, out, nil
}

// run is jev_decide's transport-agnostic core: everything from input
// validation through the SystemOne call, budget accounting, and audit
// logging, independent of whether the caller is the MCP handler (Handle)
// or the CLI subcommand (cli.go).
func (h *DecideHandler) run(ctx context.Context, in DecideInput) (DecideOutput, error) {
	start := time.Now()

	if err := validateInput(in); err != nil {
		return DecideOutput{}, err
	}
	escapeHatches := true
	if in.EscapeHatches != nil {
		escapeHatches = *in.EscapeHatches
	}

	inputHash := audit.HashValue(in)
	itemCount := 1 + len(in.Requirements)*len(in.Candidates)

	if h.budget.SessionBudgetExceeded() {
		refuseErr := fmt.Errorf("jev_decide: refusing call: session budget exhausted (spent $%.6f, cap $%.6f)", h.budget.Total(), h.maxUSDPerSession)
		h.auditLog.Log(audit.Entry{
			Tool: ToolNameDecide, Model: h.model, InputStateSHA256: inputHash,
			Status: "error", LatencyMs: time.Since(start).Milliseconds(), Error: refuseErr.Error(),
			ItemCount: itemCount,
		})
		return DecideOutput{}, refuseErr
	}

	state := map[string]any{"decision": in.Decision, "evidence": in.Evidence, "priorities": in.Priorities}

	mainCriteria := make(map[string]string, len(in.Candidates)+len(escapeHatchCriteria))
	validMainOptions := make(map[string]bool, len(in.Candidates)+len(escapeHatchCriteria))
	for _, c := range in.Candidates {
		mainCriteria[c.ID] = c.Description
		validMainOptions[c.ID] = true
	}
	if escapeHatches {
		for id, desc := range escapeHatchCriteria {
			mainCriteria[id] = desc
			validMainOptions[id] = true
		}
	}

	questions := map[string]openrouter.Question{
		"decision": {
			Type: "choice",
			Instructions: "Considering `state.decision`, `state.evidence`, and `state.priorities`, which " +
				"option in `criteria` best satisfies the decision?",
			Criteria: mainCriteria,
		},
	}
	for ri, req := range in.Requirements {
		for _, c := range in.Candidates {
			questions[checkKey(ri, c.ID)] = openrouter.Question{
				Type: "choice",
				Instructions: fmt.Sprintf(
					"Considering `state.evidence`, is this requirement supported, contradicted, or unclear "+
						"for this candidate?\n\nRequirement: %s\n\nCandidate: %s - %s",
					req, c.ID, c.Description,
				),
				Criteria: map[string]string{
					AnswerSupported:    "the evidence supports this candidate meeting the requirement",
					AnswerContradicted: "the evidence contradicts this candidate meeting the requirement",
					AnswerUnclear:      "the evidence is unclear/insufficient to tell",
				},
			}
		}
	}

	resp, callErr := h.client.Ask(ctx, h.model, questions, state, h.timeout)
	latencyMs := time.Since(start).Milliseconds()

	if callErr != nil {
		h.auditLog.Log(audit.Entry{
			Tool: ToolNameDecide, Model: h.model, InputStateSHA256: inputHash,
			Status: "error", LatencyMs: latencyMs, Error: callErr.Error(),
			ItemCount: itemCount,
		})
		return DecideOutput{}, fmt.Errorf("jev_decide: %w", callErr)
	}

	out := DecideOutput{Model: resp.Model, LatencyMs: latencyMs}
	if resp.Usage != nil {
		out.Usage = &Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens}
	}

	invalidCount := 0

	var selected string
	var confidence float64
	var probs map[string]float64
	mainOK := false
	if raw, present := resp.Answers["decision"]; present {
		selected, confidence, probs, mainOK = answers.Choice(raw, validMainOptions)
	}
	if !mainOK {
		out.Recommendation = Recommendation{Status: StatusInvalidResponse}
		invalidCount++
	} else {
		out.Recommendation = Recommendation{
			Selected: selected, Escaped: escapeHatchIDs[selected],
			Confidence: confidence, Probabilities: probs, Status: StatusOK,
		}
	}

	if len(in.Requirements) > 0 {
		out.Checks = make([]Check, 0, len(in.Requirements)*len(in.Candidates))
		for ri := range in.Requirements {
			for _, c := range in.Candidates {
				answer := AnswerInvalidResponse
				if raw, present := resp.Answers[checkKey(ri, c.ID)]; present {
					if choice, _, _, ok := answers.Choice(raw, requirementAnswerOptions); ok {
						answer = choice
					}
				}
				if answer == AnswerInvalidResponse {
					invalidCount++
				}
				out.Checks = append(out.Checks, Check{Candidate: c.ID, Requirement: ri, Answer: answer})
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
		Tool: ToolNameDecide, Model: out.Model, InputStateSHA256: inputHash,
		Status: status, CostUSD: costUSD, LatencyMs: out.LatencyMs, BudgetExceeded: out.BudgetExceeded,
		ItemCount: itemCount, InvalidCount: invalidCount,
	})

	return out, nil
}

func checkKey(requirementIndex int, candidateID string) string {
	return "req" + strconv.Itoa(requirementIndex) + "_" + candidateID
}

// validateInput rejects obviously-unusable input before spending any
// budget or making a network call.
func validateInput(in DecideInput) error {
	if strings.TrimSpace(in.Decision) == "" {
		return fmt.Errorf("jev_decide: decision must not be empty")
	}
	if strings.TrimSpace(in.Evidence) == "" {
		return fmt.Errorf("jev_decide: evidence must not be empty")
	}
	if strings.TrimSpace(in.Priorities) == "" {
		return fmt.Errorf("jev_decide: priorities must not be empty")
	}
	if len(in.Candidates) < minCandidates || len(in.Candidates) > maxCandidates {
		return fmt.Errorf("jev_decide: candidates must number %d-%d, got %d", minCandidates, maxCandidates, len(in.Candidates))
	}
	seen := make(map[string]bool, len(in.Candidates))
	for i, c := range in.Candidates {
		if strings.TrimSpace(c.ID) == "" {
			return fmt.Errorf("jev_decide: candidates[%d].id must not be empty", i)
		}
		if escapeHatchIDs[c.ID] {
			return fmt.Errorf("jev_decide: candidates[%d].id %q is reserved for an escape hatch", i, c.ID)
		}
		if seen[c.ID] {
			return fmt.Errorf("jev_decide: duplicate candidate id %q", c.ID)
		}
		seen[c.ID] = true
		if strings.TrimSpace(c.Description) == "" {
			return fmt.Errorf("jev_decide: candidates[%d].description must not be empty", i)
		}
	}
	if len(in.Requirements) > maxRequirements {
		return fmt.Errorf("jev_decide: too many requirements (%d, max %d)", len(in.Requirements), maxRequirements)
	}
	for i, r := range in.Requirements {
		if strings.TrimSpace(r) == "" {
			return fmt.Errorf("jev_decide: requirements[%d] must not be empty", i)
		}
	}
	return nil
}
