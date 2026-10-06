// Package check implements the jev_check MCP tool: batched true/false
// judgment of a list of independent propositions using TypeSafe's Jev
// judgment model's "noul" question type, via OpenRouter's SystemOne API.
//
// This is functionally jkudish's jev_noul tool (see the project brief),
// renamed jev_check in this codebase; "check" was chosen as the package
// name for the same reason as internal/tools/score's package name
// matches its tool's short identity, not the full jev_ MCP tool name.
//
// This package is a self-registering plugin (see internal/registry's
// package doc comment for the overall mechanism): its init() function
// registers a registry.Tool whose RegisterMCP wires jev_check onto
// whatever *mcp.Server cmd/jev/main.go passes it at startup.
//
// # Batching
//
// Every proposition is asked as a separate named "noul" question
// ("p0", "p1", ...) in a single SystemOne request/client.Ask call, per the
// project brief's "one HTTP call, many questions" batching pattern -- not
// one HTTP round trip per proposition.
//
// # What is and isn't independently verified
//
// The "noul" question/answer shape (criteria as a small label map, answer
// {"type":"noul","noul":<float>} with no confidence/probabilities field)
// is exactly what internal/openrouter's package doc comment already
// documents as a verified-live wire fact from 2026-09-26 -- not
// independently re-verified here (no OPENROUTER_API_KEY was available in
// this implementation environment). The auto_accept default of 0.85 and
// the "likely"/"unlikely"/"uncertain" labeling scheme are taken verbatim
// from the project brief's citation of jkudish's jev_noul docs.
package check

import (
	"context"
	"fmt"
	"strconv"
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
)

// ToolNameCheck is the MCP tool name registered for CheckHandler, and the
// key used in config.Config.ToolModelOverrides and audit.Entry.Tool.
const ToolNameCheck = "jev_check"

// maxPropositions is the project brief's literal cap ("propositions
// []string (cap 64)").
const maxPropositions = 64

// defaultAutoAccept is the project brief's literal default for jev_check's
// auto_accept ("default 0.85, must be > 0.5"). Exposed as an optional
// input field (AutoAccept); see internal/answers.ResolveThreshold's doc
// comment for why this codebase treats every "default 0.NN" threshold in
// the project brief as caller-overridable via an optional input field.
const defaultAutoAccept = 0.85

// Status values for PropositionResult.Status.
const (
	StatusOK              = "ok"
	StatusInvalidResponse = "invalid_response"
)

// Action values for PropositionResult.Action.
const (
	ActionAuto   = "auto"
	ActionReview = "review"
)

// Usage mirrors the token accounting reported by OpenRouter for a call.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// CostUSD is the real cost of the call in USD as reported by the API;
	// omitted when the API returned no cost.
	CostUSD *float64 `json:"cost_usd,omitempty"`
}

// CheckInput is the jev_check tool's input schema.
type CheckInput struct {
	Propositions []string `json:"propositions" jsonschema:"List of standalone propositions to check, each judged independently as true or false. Required; 1 to 64 non-empty strings, e.g. [\"the invoice total is $500\"]."`
	Context      string   `json:"context,omitempty" jsonschema:"Optional shared background/context every proposition is judged against, e.g. \"Invoice #1: total $500.00\". A plain string; omit or leave empty if no shared context is needed."`
	AutoAccept   float64  `json:"auto_accept,omitempty" jsonschema:"Confidence threshold for a proposition's label to count as 'likely'/'unlikely' (action 'auto') rather than 'uncertain' (action 'review'). Optional; must be > 0.5 and <= 1 if set, e.g. 0.9; defaults to 0.85 when omitted or 0."`
}

// PropositionResult is one proposition's judged result.
//
// Callers MUST check Status == "ok" before trusting Probability or Label:
// per the project's fail-closed requirement, when Status ==
// "invalid_response" those fields are zero-valued placeholders and Action
// is forced to "review", never a fabricated verdict.
type PropositionResult struct {
	Proposition string  `json:"proposition"`
	Probability float64 `json:"probability"`
	// Label is "likely" (Probability >= auto_accept), "unlikely"
	// (Probability <= 1-auto_accept), or "uncertain" (otherwise) -- see
	// internal/answers.NoulLabel. Empty when Status != "ok".
	Label string `json:"label,omitempty"`
	// Action is "auto" when Label is "likely" or "unlikely" (a confident
	// verdict either way), else "review" -- including always "review"
	// when Status == "invalid_response".
	Action string `json:"action"`
	Status string `json:"status"`
}

// CheckOutput is the jev_check tool's output schema.
type CheckOutput struct {
	Results        []PropositionResult `json:"results"`
	Model          string              `json:"model"`
	Usage          *Usage              `json:"usage"`
	LatencyMs      int64               `json:"latency_ms"`
	BudgetExceeded bool                `json:"budget_exceeded,omitempty"`
}

// CheckHandler implements the jev_check tool.
type CheckHandler struct {
	client           *openrouter.Client
	model            string
	timeout          time.Duration
	budget           *budget.Tracker
	maxUSDPerCall    float64
	maxUSDPerSession float64
	auditLog         *audit.Logger
}

// NewCheckHandler builds a CheckHandler from application dependencies.
func NewCheckHandler(client *openrouter.Client, cfg config.Config, tracker *budget.Tracker, auditLog *audit.Logger) *CheckHandler {
	return &CheckHandler{
		client:           client,
		model:            cfg.ModelForTool(ToolNameCheck),
		timeout:          time.Duration(cfg.RequestTimeoutMs) * time.Millisecond,
		budget:           tracker,
		maxUSDPerCall:    cfg.Budget.MaxUSDPerCall,
		maxUSDPerSession: cfg.Budget.MaxUSDPerSession,
		auditLog:         auditLog,
	}
}

func init() {
	description := "Batch-check a list of independent true/false propositions, each judged separately " +
		"against optional shared context. Use this for standalone propositions with no distinct " +
		"evidence text; use jev_verify instead when checking claims against separate evidence " +
		"(support/contradict/says_nothing). Fails closed per proposition: a malformed answer is " +
		"status=\"invalid_response\" with action=\"review\", never a fabricated verdict. Example: " +
		`{"propositions": ["the invoice total is $500"]}. ` +
		"Output: one {probability, label, action} per proposition."
	registry.Register(registry.Tool{
		Name:        "check",
		MCPName:     ToolNameCheck,
		Description: description,
		RegisterMCP: func(server *mcp.Server, deps *registry.Deps) {
			h := NewCheckHandler(deps.Client, deps.Config, deps.Budget, deps.Audit)
			mcp.AddTool(server, &mcp.Tool{
				Name:        ToolNameCheck,
				Description: description,
			}, h.Handle)
		},
		RegisterCLI: func(root *cobra.Command, provider registry.DepsProvider) {
			root.AddCommand(newCLICommand(provider, description))
		},
	})
}

// Handle implements mcp.ToolHandlerFor[CheckInput, CheckOutput]: a thin
// adapter over run (the transport-agnostic core both the MCP handler and
// the CLI subcommand share).
func (h *CheckHandler) Handle(ctx context.Context, _ *mcp.CallToolRequest, in CheckInput) (*mcp.CallToolResult, CheckOutput, error) {
	out, err := h.run(ctx, in)
	if err != nil {
		return nil, CheckOutput{}, err
	}
	return nil, out, nil
}

// run is jev_check's transport-agnostic core: everything from input
// validation through the SystemOne call, budget accounting, and audit
// logging, independent of whether the caller is the MCP handler (Handle)
// or the CLI subcommand (cli.go).
func (h *CheckHandler) run(ctx context.Context, in CheckInput) (CheckOutput, error) {
	start := time.Now()

	if err := validateInput(in); err != nil {
		return CheckOutput{}, err
	}
	autoAccept := answers.ResolveThreshold(in.AutoAccept, defaultAutoAccept)

	inputHash := audit.HashValue(in)

	if h.budget.SessionBudgetExceeded() {
		err := fmt.Errorf("jev_check: refusing call: session budget exhausted (spent $%.6f, cap $%.6f)", h.budget.Total(), h.maxUSDPerSession)
		h.auditLog.Log(audit.Entry{
			Tool: ToolNameCheck, Model: h.model, InputStateSHA256: inputHash,
			Status: "error", LatencyMs: time.Since(start).Milliseconds(), Error: err.Error(),
			ItemCount: len(in.Propositions),
		})
		return CheckOutput{}, err
	}

	questions := make(map[string]openrouter.Question, len(in.Propositions))
	for i, prop := range in.Propositions {
		questions[questionKey(i)] = openrouter.Question{
			Type:         "noul",
			Instructions: fmt.Sprintf("Is the following proposition true?\n\n%s", prop),
			Criteria: map[string]string{
				"true":  "the proposition is true",
				"false": "the proposition is false",
			},
		}
	}

	resp, callErr := h.client.Ask(ctx, h.model, questions, in.Context, h.timeout)
	latencyMs := time.Since(start).Milliseconds()

	if callErr != nil {
		h.auditLog.Log(audit.Entry{
			Tool: ToolNameCheck, Model: h.model, InputStateSHA256: inputHash,
			Status: "error", LatencyMs: latencyMs, Error: callErr.Error(),
			ItemCount: len(in.Propositions),
		})
		return CheckOutput{}, fmt.Errorf("jev_check: %w", callErr)
	}

	out := CheckOutput{Model: resp.Model, LatencyMs: latencyMs}
	if resp.Usage != nil {
		out.Usage = &Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens, CostUSD: resp.Usage.CostUSD()}
	}

	invalidCount := 0
	out.Results = make([]PropositionResult, len(in.Propositions))
	for i, prop := range in.Propositions {
		res := PropositionResult{Proposition: prop}
		raw, present := resp.Answers[questionKey(i)]
		v, ok := 0.0, false
		if present {
			v, ok = answers.Noul(raw)
		}
		if !ok {
			res.Status = StatusInvalidResponse
			res.Action = ActionReview
			invalidCount++
		} else {
			res.Status = StatusOK
			res.Probability = v
			res.Label = answers.NoulLabel(v, autoAccept)
			if res.Label == "uncertain" {
				res.Action = ActionReview
			} else {
				res.Action = ActionAuto
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
		Tool: ToolNameCheck, Model: out.Model, InputStateSHA256: inputHash,
		Status: status, CostUSD: costUSD, LatencyMs: out.LatencyMs, BudgetExceeded: out.BudgetExceeded,
		ItemCount: len(in.Propositions), InvalidCount: invalidCount,
	})

	return out, nil
}

func questionKey(i int) string {
	return "p" + strconv.Itoa(i)
}

// validateInput rejects obviously-unusable input before spending any
// budget or making a network call.
//
// Every error names the offending field by its JSON path (e.g.
// "propositions[2]"), says what's wrong, and says what's expected
// (including the limit/example): agents calling this tool over MCP only
// ever see the error text, so the message has to carry enough
// information to fix the call on the next try.
func validateInput(in CheckInput) error {
	if len(in.Propositions) == 0 {
		return fmt.Errorf("jev_check: propositions: must not be empty; provide 1 to %d propositions as an array of strings, e.g. [\"the invoice total is $500\"]", maxPropositions)
	}
	if len(in.Propositions) > maxPropositions {
		return fmt.Errorf("jev_check: propositions: too many entries (%d), more than the maximum of %d; reduce the list to at most %d propositions", len(in.Propositions), maxPropositions, maxPropositions)
	}
	for i, p := range in.Propositions {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("jev_check: propositions[%d]: must not be empty; provide a non-empty proposition string, e.g. \"the invoice total is $500\"", i)
		}
	}
	if err := answers.ValidateAutoAccept("auto_accept", in.AutoAccept); err != nil {
		return fmt.Errorf("jev_check: %w", err)
	}
	return nil
}
