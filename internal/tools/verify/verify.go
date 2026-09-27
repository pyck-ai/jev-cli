// Package verify implements the jev_verify MCP tool: batched
// support/contradict/says-nothing verification of a list of claims against
// supplied evidence, using TypeSafe's Jev judgment model's "choice"
// question type, via OpenRouter's SystemOne API.
//
// This package is a self-registering plugin (see internal/registry's
// package doc comment for the overall mechanism).
//
// # Batching
//
// Every claim is asked as a separate named "choice" question ("c0", "c1",
// ...) in a single client.Ask call, all sharing the same evidence as
// "state" -- one HTTP request regardless of how many claims are checked.
//
// # Evidence's dual shape
//
// Per the project brief, Evidence may be "a single text blob, or a list of
// [EvidenceItem]": VerifyInput.Evidence is typed `any` to accept either a
// JSON string or a JSON array of {id, text} objects (normalizeEvidence
// disambiguates by attempting each in turn), matching OpenRouter's own
// documented "state" field, which "accepts a plain string, or a JSON
// object or array of related context" (see internal/openrouter's package
// doc comment). Whichever shape is supplied is forwarded to SystemOne
// as-is as the shared "state" for every claim's question.
//
// # What is and isn't independently verified
//
// The "choice" question/answer shape is a verified-live wire fact from
// 2026-09-26 (see internal/openrouter's package doc comment) -- not
// independently re-verified here (no OPENROUTER_API_KEY was available in
// this implementation environment). auto_accept's default of 0.8 is the
// project brief's literal default for this tool. maxClaims (64) is this
// implementation's own invented safety cap: the brief did not state one
// for jev_verify specifically (unlike e.g. jev_check's explicit "cap 64"),
// so this package reuses that same number for consistency across the
// tool suite.
package verify

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
	"github.com/pyck-ai/jev-mcp/internal/config"
	"github.com/pyck-ai/jev-mcp/internal/openrouter"
	"github.com/pyck-ai/jev-mcp/internal/registry"
)

// ToolNameVerify is the MCP tool name registered for VerifyHandler.
const ToolNameVerify = "jev_verify"

// maxClaims is this implementation's own invented cap (see package doc
// comment).
const maxClaims = 64

// defaultAutoAccept is the project brief's literal default for
// jev_verify's auto_accept.
const defaultAutoAccept = 0.8

// The three verdict options every claim's "choice" question offers,
// verbatim from the project brief.
const (
	VerdictSupports    = "supports"
	VerdictContradicts = "contradicts"
	VerdictSaysNothing = "says_nothing"
)

var verdictOptions = map[string]bool{VerdictSupports: true, VerdictContradicts: true, VerdictSaysNothing: true}

// Status values for ClaimResult.Status.
const (
	StatusOK              = "ok"
	StatusInvalidResponse = "invalid_response"
)

// Action values for ClaimResult.Action.
const (
	ActionAuto   = "auto"
	ActionReview = "review"
)

// Usage mirrors the token accounting reported by OpenRouter for a call.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// EvidenceItem is one item of a structured (as opposed to single-blob)
// Evidence array.
type EvidenceItem struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// VerifyInput is the jev_verify tool's input schema.
type VerifyInput struct {
	Claims []string `json:"claims" jsonschema:"Claims to verify against the supplied evidence. Capped at 64."`
	// Evidence is a single text blob (JSON string), or a list of
	// {id, text} items (JSON array of objects) -- see package doc comment.
	Evidence   any     `json:"evidence" jsonschema:"Evidence to check claims against: either a single text blob (a string), or an array of {id, text} objects."`
	AutoAccept float64 `json:"auto_accept,omitempty" jsonschema:"Confidence bar in (0.5, 1] for action to be 'auto' rather than 'review'. Default 0.8."`
}

// ClaimResult is one claim's verification result.
//
// Callers MUST check Status == "ok" before trusting Verdict, Confidence,
// or Probabilities: when Status == "invalid_response" those fields are
// zero-valued placeholders and Action is forced to "review", never a
// fabricated verdict.
type ClaimResult struct {
	Claim         string             `json:"claim"`
	Verdict       string             `json:"verdict,omitempty"`
	Confidence    float64            `json:"confidence,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Action        string             `json:"action"`
	Status        string             `json:"status"`
}

// VerifyOutput is the jev_verify tool's output schema.
type VerifyOutput struct {
	Results        []ClaimResult `json:"results"`
	Model          string        `json:"model"`
	Usage          *Usage        `json:"usage"`
	LatencyMs      int64         `json:"latency_ms"`
	BudgetExceeded bool          `json:"budget_exceeded,omitempty"`
}

// VerifyHandler implements the jev_verify tool.
type VerifyHandler struct {
	client           *openrouter.Client
	model            string
	timeout          time.Duration
	budget           *budget.Tracker
	maxUSDPerCall    float64
	maxUSDPerSession float64
	auditLog         *audit.Logger
}

// NewVerifyHandler builds a VerifyHandler from application dependencies.
func NewVerifyHandler(client *openrouter.Client, cfg config.Config, tracker *budget.Tracker, auditLog *audit.Logger) *VerifyHandler {
	return &VerifyHandler{
		client:           client,
		model:            cfg.ModelForTool(ToolNameVerify),
		timeout:          time.Duration(cfg.RequestTimeoutMs) * time.Millisecond,
		budget:           tracker,
		maxUSDPerCall:    cfg.Budget.MaxUSDPerCall,
		maxUSDPerSession: cfg.Budget.MaxUSDPerSession,
		auditLog:         auditLog,
	}
}

func init() {
	description := "Batch-verify a list of claims against supplied evidence using TypeSafe's Jev " +
		"judgment model (via OpenRouter's SystemOne API's \"choice\" question type): each claim is " +
		"judged as supports/contradicts/says_nothing. Fails closed per claim: a malformed or missing " +
		"answer is reported as status=\"invalid_response\" with action=\"review\", never a fabricated " +
		"verdict."
	registry.Register(registry.Tool{
		Name:        "verify",
		MCPName:     ToolNameVerify,
		Description: description,
		RegisterMCP: func(server *mcp.Server, deps *registry.Deps) {
			h := NewVerifyHandler(deps.Client, deps.Config, deps.Budget, deps.Audit)
			mcp.AddTool(server, &mcp.Tool{
				Name:        ToolNameVerify,
				Description: description,
			}, h.Handle)
		},
		RegisterCLI: func(root *cobra.Command, provider registry.DepsProvider) {
			root.AddCommand(newCLICommand(provider, description))
		},
	})
}

// Handle implements mcp.ToolHandlerFor[VerifyInput, VerifyOutput]: a thin
// adapter over run (the transport-agnostic core both the MCP handler and
// the CLI subcommand share).
func (h *VerifyHandler) Handle(ctx context.Context, _ *mcp.CallToolRequest, in VerifyInput) (*mcp.CallToolResult, VerifyOutput, error) {
	out, err := h.run(ctx, in)
	if err != nil {
		return nil, VerifyOutput{}, err
	}
	return nil, out, nil
}

// run is jev_verify's transport-agnostic core: everything from input
// validation through the SystemOne call, budget accounting, and audit
// logging, independent of whether the caller is the MCP handler (Handle)
// or the CLI subcommand (cli.go).
func (h *VerifyHandler) run(ctx context.Context, in VerifyInput) (VerifyOutput, error) {
	start := time.Now()

	state, err := validateInput(in)
	if err != nil {
		return VerifyOutput{}, err
	}
	autoAccept := answers.ResolveThreshold(in.AutoAccept, defaultAutoAccept)

	inputHash := audit.HashValue(in)

	if h.budget.SessionBudgetExceeded() {
		refuseErr := fmt.Errorf("jev_verify: refusing call: session budget exhausted (spent $%.6f, cap $%.6f)", h.budget.Total(), h.maxUSDPerSession)
		h.auditLog.Log(audit.Entry{
			Tool: ToolNameVerify, Model: h.model, InputStateSHA256: inputHash,
			Status: "error", LatencyMs: time.Since(start).Milliseconds(), Error: refuseErr.Error(),
			ItemCount: len(in.Claims),
		})
		return VerifyOutput{}, refuseErr
	}

	questions := make(map[string]openrouter.Question, len(in.Claims))
	for i, claim := range in.Claims {
		questions[claimKey(i)] = openrouter.Question{
			Type: "choice",
			Instructions: fmt.Sprintf(
				"Does the evidence in `state` support, contradict, or say nothing about this claim?\n\nClaim: %s",
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
			Tool: ToolNameVerify, Model: h.model, InputStateSHA256: inputHash,
			Status: "error", LatencyMs: latencyMs, Error: callErr.Error(),
			ItemCount: len(in.Claims),
		})
		return VerifyOutput{}, fmt.Errorf("jev_verify: %w", callErr)
	}

	out := VerifyOutput{Model: resp.Model, LatencyMs: latencyMs}
	if resp.Usage != nil {
		out.Usage = &Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens}
	}

	invalidCount := 0
	out.Results = make([]ClaimResult, len(in.Claims))
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
			invalidCount++
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
		out.Results[i] = res
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
		Tool: ToolNameVerify, Model: out.Model, InputStateSHA256: inputHash,
		Status: status, CostUSD: costUSD, LatencyMs: out.LatencyMs, BudgetExceeded: out.BudgetExceeded,
		ItemCount: len(in.Claims), InvalidCount: invalidCount,
	})

	return out, nil
}

func claimKey(i int) string { return "c" + strconv.Itoa(i) }

// validateInput rejects obviously-unusable input before spending any
// budget or making a network call, and normalizes Evidence into whatever
// shape (string or []EvidenceItem) should be sent as SystemOne's "state".
func validateInput(in VerifyInput) (state any, err error) {
	if len(in.Claims) == 0 {
		return nil, fmt.Errorf("jev_verify: claims must not be empty")
	}
	if len(in.Claims) > maxClaims {
		return nil, fmt.Errorf("jev_verify: too many claims (%d, max %d)", len(in.Claims), maxClaims)
	}
	for i, c := range in.Claims {
		if strings.TrimSpace(c) == "" {
			return nil, fmt.Errorf("jev_verify: claims[%d] must not be empty", i)
		}
	}
	if err := answers.ValidateAutoAccept("auto_accept", in.AutoAccept); err != nil {
		return nil, fmt.Errorf("jev_verify: %w", err)
	}
	return normalizeEvidence(in.Evidence)
}

// normalizeEvidence accepts either a JSON string or a JSON array of
// {id, text} objects for in.Evidence (unmarshaled into `any`, so a JSON
// string decodes as a Go string and a JSON array decodes as []any -- see
// this package's doc comment), and returns whichever shape it actually is,
// ready to use as SystemOne's "state" verbatim.
func normalizeEvidence(raw any) (any, error) {
	if raw == nil {
		return nil, fmt.Errorf("evidence must be a non-empty string or a non-empty array of {id, text} objects")
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("evidence: %w", err)
	}

	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		if strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("evidence must not be empty")
		}
		return s, nil
	}

	var items []EvidenceItem
	if err := json.Unmarshal(b, &items); err == nil && len(items) > 0 {
		for i, it := range items {
			if strings.TrimSpace(it.Text) == "" {
				return nil, fmt.Errorf("evidence[%d].text must not be empty", i)
			}
		}
		return items, nil
	}

	return nil, fmt.Errorf("evidence must be a non-empty string, or a non-empty array of {id, text} objects")
}
