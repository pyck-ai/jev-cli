// Package classify implements the jev_classify MCP tool: assigns each of a
// list of items to exactly one of a fixed set of classes, using
// TypeSafe's Jev judgment model's "choice" question type, via
// OpenRouter's SystemOne API.
//
// This package is a self-registering plugin (see internal/registry's
// package doc comment for the overall mechanism).
//
// # Batching and self-sufficiency
//
// One named "choice" question per item ("item0", "item1", ...) is asked
// in a single client.Ask call. Every item's question shares the same
// `criteria` (class id -> description, since the class set is global to
// the whole call) and the same shared `state` (Purpose, if given); each
// item's own text is embedded directly in that item's `instructions`, per
// this codebase's "every question must be self-sufficient" principle
// (see internal/tools/match's package doc comment).
//
// # What is and isn't independently verified
//
// The "choice" question/answer shape is a verified-live wire fact from
// 2026-09-26 (see internal/openrouter's package doc comment) -- not
// independently re-verified here (no OPENROUTER_API_KEY was available in
// this implementation environment). auto_accept's default of 0.85 and
// minimum_margin's default of 0.5 are the project brief's literal
// defaults for this tool.
package classify

import (
	"context"
	"fmt"
	"sort"
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

// ToolNameClassify is the MCP tool name registered for ClassifyHandler.
const ToolNameClassify = "jev_classify"

// Caps named verbatim in the project brief.
const (
	maxItems           = 64
	maxClasses         = 250
	maxItemClassBudget = 8000 // len(items) * len(classes)
)

// Default thresholds, both the project brief's literal defaults for this
// tool.
const (
	defaultAutoAccept    = 0.85
	defaultMinimumMargin = 0.5
)

// Status values for ItemResult.Status.
const (
	StatusOK              = "ok"
	StatusInvalidResponse = "invalid_response"
)

// Decision values for ItemResult.Decision.
const (
	DecisionAuto   = "auto"
	DecisionReview = "review"
)

// Usage mirrors the token accounting reported by OpenRouter for a call.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Item is one item to classify.
type Item struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// Class is one candidate classification.
type Class struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

// ClassifyInput is the jev_classify tool's input schema.
type ClassifyInput struct {
	Purpose       string  `json:"purpose,omitempty" jsonschema:"Optional shared context for why these items are being classified."`
	Items         []Item  `json:"items" jsonschema:"Items to classify. Capped at 64."`
	Classes       []Class `json:"classes" jsonschema:"Candidate classes every item is classified against. Capped at 250; len(items)*len(classes) is capped at 8000."`
	AutoAccept    float64 `json:"auto_accept,omitempty" jsonschema:"Confidence bar in (0.5, 1] for decision to be 'auto'. Default 0.85."`
	MinimumMargin float64 `json:"minimum_margin,omitempty" jsonschema:"Minimum gap in [0,1] between the top and runner-up class probability for decision to be 'auto'. Default 0.5."`
}

// ItemResult is one item's classification result.
//
// Callers MUST check Status == "ok" before trusting Classification,
// Margin, Confidence, or Probabilities: when Status == "invalid_response"
// those fields are zero-valued placeholders and Decision is forced to
// "review", never a fabricated classification.
type ItemResult struct {
	ID             string             `json:"id"`
	Classification string             `json:"classification,omitempty"`
	Margin         float64            `json:"margin,omitempty"`
	Confidence     float64            `json:"confidence,omitempty"`
	Probabilities  map[string]float64 `json:"probabilities,omitempty"`
	Decision       string             `json:"decision"`
	Status         string             `json:"status"`
}

// ClassifyOutput is the jev_classify tool's output schema.
type ClassifyOutput struct {
	Results        []ItemResult `json:"results"`
	Model          string       `json:"model"`
	Usage          *Usage       `json:"usage"`
	LatencyMs      int64        `json:"latency_ms"`
	BudgetExceeded bool         `json:"budget_exceeded,omitempty"`
}

// ClassifyHandler implements the jev_classify tool.
type ClassifyHandler struct {
	client           *openrouter.Client
	model            string
	timeout          time.Duration
	budget           *budget.Tracker
	maxUSDPerCall    float64
	maxUSDPerSession float64
	auditLog         *audit.Logger
}

// NewClassifyHandler builds a ClassifyHandler from application dependencies.
func NewClassifyHandler(client *openrouter.Client, cfg config.Config, tracker *budget.Tracker, auditLog *audit.Logger) *ClassifyHandler {
	return &ClassifyHandler{
		client:           client,
		model:            cfg.ModelForTool(ToolNameClassify),
		timeout:          time.Duration(cfg.RequestTimeoutMs) * time.Millisecond,
		budget:           tracker,
		maxUSDPerCall:    cfg.Budget.MaxUSDPerCall,
		maxUSDPerSession: cfg.Budget.MaxUSDPerSession,
		auditLog:         auditLog,
	}
}

func init() {
	description := "Classify each of a list of items into exactly one of a fixed set of classes, using " +
		"TypeSafe's Jev judgment model. Fails closed per item: a malformed or missing answer is " +
		"reported as status=\"invalid_response\" with decision=\"review\", never a fabricated " +
		"classification."
	registry.Register(registry.Tool{
		Name:        "classify",
		MCPName:     ToolNameClassify,
		Description: description,
		RegisterMCP: func(server *mcp.Server, deps *registry.Deps) {
			h := NewClassifyHandler(deps.Client, deps.Config, deps.Budget, deps.Audit)
			mcp.AddTool(server, &mcp.Tool{
				Name:        ToolNameClassify,
				Description: description,
			}, h.Handle)
		},
		RegisterCLI: func(root *cobra.Command, provider registry.DepsProvider) {
			root.AddCommand(newCLICommand(provider, description))
		},
	})
}

// Handle implements mcp.ToolHandlerFor[ClassifyInput, ClassifyOutput]: a
// thin adapter over run (the transport-agnostic core both the MCP
// handler and the CLI subcommand share).
func (h *ClassifyHandler) Handle(ctx context.Context, _ *mcp.CallToolRequest, in ClassifyInput) (*mcp.CallToolResult, ClassifyOutput, error) {
	out, err := h.run(ctx, in)
	if err != nil {
		return nil, ClassifyOutput{}, err
	}
	return nil, out, nil
}

// run is jev_classify's transport-agnostic core: everything from input
// validation through the SystemOne call, budget accounting, and audit
// logging, independent of whether the caller is the MCP handler (Handle)
// or the CLI subcommand (cli.go).
func (h *ClassifyHandler) run(ctx context.Context, in ClassifyInput) (ClassifyOutput, error) {
	start := time.Now()

	if err := validateInput(in); err != nil {
		return ClassifyOutput{}, err
	}
	autoAccept := answers.ResolveThreshold(in.AutoAccept, defaultAutoAccept)
	minimumMargin := answers.ResolveThreshold(in.MinimumMargin, defaultMinimumMargin)

	inputHash := audit.HashValue(in)

	if h.budget.SessionBudgetExceeded() {
		refuseErr := fmt.Errorf("jev_classify: refusing call: session budget exhausted (spent $%.6f, cap $%.6f)", h.budget.Total(), h.maxUSDPerSession)
		h.auditLog.Log(audit.Entry{
			Tool: ToolNameClassify, Model: h.model, InputStateSHA256: inputHash,
			Status: "error", LatencyMs: time.Since(start).Milliseconds(), Error: refuseErr.Error(),
			ItemCount: len(in.Items),
		})
		return ClassifyOutput{}, refuseErr
	}

	classCriteria := make(map[string]string, len(in.Classes))
	validOptions := make(map[string]bool, len(in.Classes))
	for _, c := range in.Classes {
		classCriteria[c.ID] = c.Description
		validOptions[c.ID] = true
	}

	questions := make(map[string]openrouter.Question, len(in.Items))
	for i, item := range in.Items {
		questions[itemKey(i)] = openrouter.Question{
			Type: "choice",
			Instructions: fmt.Sprintf(
				"Classify the following item into exactly one of the classes in `criteria` (considering "+
					"`state` as shared context, if any).\n\nItem: %s",
				item.Text,
			),
			Criteria: classCriteria,
		}
	}

	resp, callErr := h.client.Ask(ctx, h.model, questions, in.Purpose, h.timeout)
	latencyMs := time.Since(start).Milliseconds()

	if callErr != nil {
		h.auditLog.Log(audit.Entry{
			Tool: ToolNameClassify, Model: h.model, InputStateSHA256: inputHash,
			Status: "error", LatencyMs: latencyMs, Error: callErr.Error(),
			ItemCount: len(in.Items),
		})
		return ClassifyOutput{}, fmt.Errorf("jev_classify: %w", callErr)
	}

	out := ClassifyOutput{Model: resp.Model, LatencyMs: latencyMs}
	if resp.Usage != nil {
		out.Usage = &Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens}
	}

	invalidCount := 0
	out.Results = make([]ItemResult, len(in.Items))
	for i, item := range in.Items {
		res := ItemResult{ID: item.ID}
		raw, present := resp.Answers[itemKey(i)]
		var choice string
		var confidence float64
		var probs map[string]float64
		ok := false
		if present {
			choice, confidence, probs, ok = answers.Choice(raw, validOptions)
		}
		if !ok {
			res.Status = StatusInvalidResponse
			res.Decision = DecisionReview
			invalidCount++
		} else {
			res.Status = StatusOK
			res.Classification = choice
			res.Confidence = confidence
			res.Probabilities = probs
			res.Margin = margin(probs)
			if confidence >= autoAccept && res.Margin >= minimumMargin {
				res.Decision = DecisionAuto
			} else {
				res.Decision = DecisionReview
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
		Tool: ToolNameClassify, Model: out.Model, InputStateSHA256: inputHash,
		Status: status, CostUSD: costUSD, LatencyMs: out.LatencyMs, BudgetExceeded: out.BudgetExceeded,
		ItemCount: len(in.Items), InvalidCount: invalidCount,
	})

	return out, nil
}

func itemKey(i int) string { return "item" + strconv.Itoa(i) }

// margin returns the gap between the highest and second-highest
// probability in probs (0 if there are fewer than two entries, i.e. a
// single-class call).
func margin(probs map[string]float64) float64 {
	if len(probs) == 0 {
		return 0
	}
	values := make([]float64, 0, len(probs))
	for _, v := range probs {
		values = append(values, v)
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(values)))
	if len(values) == 1 {
		return values[0]
	}
	return values[0] - values[1]
}

// validateInput rejects obviously-unusable input before spending any
// budget or making a network call.
func validateInput(in ClassifyInput) error {
	if len(in.Items) == 0 {
		return fmt.Errorf("jev_classify: items must not be empty")
	}
	if len(in.Items) > maxItems {
		return fmt.Errorf("jev_classify: too many items (%d, max %d)", len(in.Items), maxItems)
	}
	if len(in.Classes) == 0 {
		return fmt.Errorf("jev_classify: classes must not be empty")
	}
	if len(in.Classes) > maxClasses {
		return fmt.Errorf("jev_classify: too many classes (%d, max %d)", len(in.Classes), maxClasses)
	}
	if itemClassBudget := len(in.Items) * len(in.Classes); itemClassBudget > maxItemClassBudget {
		return fmt.Errorf("jev_classify: items*classes too large (%d, max %d)", itemClassBudget, maxItemClassBudget)
	}
	seenItems := make(map[string]bool, len(in.Items))
	for i, item := range in.Items {
		if strings.TrimSpace(item.ID) == "" {
			return fmt.Errorf("jev_classify: items[%d].id must not be empty", i)
		}
		if seenItems[item.ID] {
			return fmt.Errorf("jev_classify: duplicate item id %q", item.ID)
		}
		seenItems[item.ID] = true
		if strings.TrimSpace(item.Text) == "" {
			return fmt.Errorf("jev_classify: items[%d].text must not be empty", i)
		}
	}
	seenClasses := make(map[string]bool, len(in.Classes))
	for i, c := range in.Classes {
		if strings.TrimSpace(c.ID) == "" {
			return fmt.Errorf("jev_classify: classes[%d].id must not be empty", i)
		}
		if seenClasses[c.ID] {
			return fmt.Errorf("jev_classify: duplicate class id %q", c.ID)
		}
		seenClasses[c.ID] = true
		if strings.TrimSpace(c.Description) == "" {
			return fmt.Errorf("jev_classify: classes[%d].description must not be empty", i)
		}
	}
	if err := answers.ValidateAutoAccept("auto_accept", in.AutoAccept); err != nil {
		return fmt.Errorf("jev_classify: %w", err)
	}
	if in.MinimumMargin != 0 && (in.MinimumMargin < 0 || in.MinimumMargin > 1) {
		return fmt.Errorf("jev_classify: minimum_margin must be in [0,1] if set, got %v", in.MinimumMargin)
	}
	return nil
}
