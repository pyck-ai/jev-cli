// Package ask implements the jev_ask MCP tool: the escape hatch. Unlike
// every other tool in this codebase, which builds a fixed, purpose-specific
// question shape from typed input, jev_ask accepts a caller-supplied
// SystemOne question map almost verbatim -- matching OpenRouter's own
// "questions" wire shape (see internal/openrouter's package doc comment)
// nearly 1:1 -- and passes it straight through to a single client.Ask
// call.
//
// This package is a self-registering plugin (see internal/registry's
// package doc comment for the overall mechanism).
//
// # Validate before forwarding
//
// Because the caller supplies the raw question shape directly, this
// handler validates every question's "type" (must be exactly "noul",
// "choice", or "score") AND that its "criteria" has the shape that type
// requires (a non-empty JSON object for "noul"/"choice", a non-empty JSON
// array of strings for "score") BEFORE sending anything to OpenRouter --
// rejecting the whole call with a clear Go error otherwise, per the
// project brief's "reject with a clear error otherwise rather than
// forwarding garbage to the API". maxQuestions (64) is this
// implementation's own invented safety cap, reusing the same number as
// internal/tools/check/internal/tools/verify for consistency; the brief
// did not state one for this tool.
//
// # Output: pass through, fail closed per answer
//
// Per the project brief, "the same fail-closed validation per answer that
// jev_score already does for its own type" is applied to every requested
// question's answer independently: a malformed or missing answer for one
// question key is reported as that key's AskAnswer.Status ==
// "invalid_response" (with only Type preserved, so the caller can still
// see what kind of question it was), while every other key's valid answer
// is preserved unaffected. Output is built by iterating the ORIGINAL
// REQUESTED question ids (not resp.Answers' own keys), so a model
// response naming some unexpected extra key never leaks into the output.
//
// # What is and isn't independently verified
//
// The "noul"/"choice"/"score" question/answer shapes are verified-live
// wire facts from 2026-09-26 (see internal/openrouter's package doc
// comment) -- not independently re-verified here (no OPENROUTER_API_KEY
// was available in this implementation environment).
package ask

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
)

// ToolNameAsk is the MCP tool name registered for AskHandler.
const ToolNameAsk = "jev_ask"

// maxQuestions is this implementation's own invented safety cap (see
// package doc comment).
const maxQuestions = 64

// The three known SystemOne question types.
const (
	TypeNoul   = "noul"
	TypeChoice = "choice"
	TypeScore  = "score"
)

// Status values for AskAnswer.Status.
const (
	StatusOK              = "ok"
	StatusInvalidResponse = "invalid_response"
)

// Usage mirrors the token accounting reported by OpenRouter for a call.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// CostUSD is the real cost of the call in USD as reported by the API;
	// omitted when the API returned no cost.
	CostUSD *float64 `json:"cost_usd,omitempty"`
}

// AskQuestion is one caller-supplied SystemOne question, matching
// internal/openrouter.Question's wire shape almost 1:1 (see package doc
// comment). Criteria's required shape depends on Type: a non-empty JSON
// object ({"<value_or_option_id>": "<description>"}) for "noul"/"choice",
// or a non-empty JSON array of level-description strings for "score".
type AskQuestion struct {
	Type         string `json:"type" jsonschema:"One of \"noul\" (probability that a statement is true), \"choice\" (pick exactly one of several options), or \"score\" (place on an ordered scale)."`
	Instructions string `json:"instructions" jsonschema:"The question to answer about state, e.g. \"Does the PRD state the customer impact?\"."`
	Criteria     any    `json:"criteria" jsonschema:"Required and non-empty; shape depends on type. noul: an object mapping the two answers to descriptions, exactly {\"true\": \"<when true>\", \"false\": \"<when false>\"}. choice: an object mapping each option id to its description, e.g. {\"auth\": \"authentication gap\", \"perf\": \"performance gap\"}; the answer is one of these ids (there is no \"options\" key or list). score: an array of level descriptions, lowest first, e.g. [\"missing\", \"partial\", \"complete\"]; the answer is a 0-based index into this array."`
}

// AskInput is the jev_ask tool's input schema.
type AskInput struct {
	// State is a string, or an arbitrary JSON object/array of related
	// context -- matching SystemOne's own "state" field flexibility (see
	// internal/openrouter.Request.State's doc comment).
	State     any                    `json:"state" jsonschema:"Text or data to be judged: a string, or an arbitrary JSON object/array of related context."`
	Questions map[string]AskQuestion `json:"questions" jsonschema:"Named SystemOne questions to ask in a single call, keyed by caller-chosen id. Capped at 64 questions."`
}

// AskAnswer is one requested question's parsed (or fail-closed) answer.
//
// Type always reflects what was REQUESTED for this key (from
// AskInput.Questions), even when Status == "invalid_response". Only the
// fields relevant to Type are ever populated, and only when
// Status == "ok": Noul for "noul"; Choice/Confidence/Probabilities for
// "choice"; Score/Confidence/Probabilities for "score". Every other field
// is the zero value / omitted -- never a fabricated answer.
type AskAnswer struct {
	Type          string             `json:"type"`
	Status        string             `json:"status"`
	Noul          *float64           `json:"noul,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}

// AskOutput is the jev_ask tool's output schema.
type AskOutput struct {
	Answers        map[string]AskAnswer `json:"answers"`
	Model          string               `json:"model"`
	Usage          *Usage               `json:"usage"`
	LatencyMs      int64                `json:"latency_ms"`
	BudgetExceeded bool                 `json:"budget_exceeded,omitempty"`
}

// AskHandler implements the jev_ask tool.
type AskHandler struct {
	client           *openrouter.Client
	model            string
	timeout          time.Duration
	budget           *budget.Tracker
	maxUSDPerCall    float64
	maxUSDPerSession float64
	auditLog         *audit.Logger
}

// NewAskHandler builds an AskHandler from application dependencies.
func NewAskHandler(client *openrouter.Client, cfg config.Config, tracker *budget.Tracker, auditLog *audit.Logger) *AskHandler {
	return &AskHandler{
		client:           client,
		model:            cfg.ModelForTool(ToolNameAsk),
		timeout:          time.Duration(cfg.RequestTimeoutMs) * time.Millisecond,
		budget:           tracker,
		maxUSDPerCall:    cfg.Budget.MaxUSDPerCall,
		maxUSDPerSession: cfg.Budget.MaxUSDPerSession,
		auditLog:         auditLog,
	}
}

func init() {
	description := "Escape hatch: ask TypeSafe's Jev judgment model an arbitrary set of named " +
		"noul/choice/score questions in a single SystemOne call, matching OpenRouter's own wire shape " +
		"almost 1:1. Every question needs type, instructions and a non-empty criteria whose shape " +
		"depends on type. Example questions: " +
		`{"impact_stated": {"type": "noul", "instructions": "Does the PRD state the customer impact?", ` +
		`"criteria": {"true": "customer impact is stated", "false": "customer impact is missing"}}, ` +
		`"top_gap": {"type": "choice", "instructions": "What is the biggest gap?", ` +
		`"criteria": {"scope": "unclear scope", "metrics": "no success metrics", "risks": "risks not covered"}}, ` +
		`"completeness": {"type": "score", "instructions": "How complete is the PRD?", ` +
		`"criteria": ["missing", "partial", "complete"]}}. ` +
		"Rejects a malformed question before sending, and fails closed per answer: a malformed or " +
		"missing answer for one key is status=\"invalid_response\", every other key's valid answer is unaffected."
	registry.Register(registry.Tool{
		Name:        "ask",
		MCPName:     ToolNameAsk,
		Description: description,
		RegisterMCP: func(server *mcp.Server, deps *registry.Deps) {
			h := NewAskHandler(deps.Client, deps.Config, deps.Budget, deps.Audit)
			mcp.AddTool(server, &mcp.Tool{
				Name:        ToolNameAsk,
				Description: description,
			}, h.Handle)
		},
		RegisterCLI: func(root *cobra.Command, provider registry.DepsProvider) {
			root.AddCommand(newCLICommand(provider, description))
		},
	})
}

// Handle implements mcp.ToolHandlerFor[AskInput, AskOutput]: a thin
// adapter over run (the transport-agnostic core both the MCP handler and
// the CLI subcommand share).
func (h *AskHandler) Handle(ctx context.Context, _ *mcp.CallToolRequest, in AskInput) (*mcp.CallToolResult, AskOutput, error) {
	out, err := h.run(ctx, in)
	if err != nil {
		return nil, AskOutput{}, err
	}
	return nil, out, nil
}

// run is jev_ask's transport-agnostic core: everything from input
// validation through the SystemOne call, budget accounting, and audit
// logging, independent of whether the caller is the MCP handler (Handle)
// or the CLI subcommand (cli.go).
func (h *AskHandler) run(ctx context.Context, in AskInput) (AskOutput, error) {
	start := time.Now()

	if err := validateInput(in); err != nil {
		return AskOutput{}, err
	}

	inputHash := audit.HashValue(in)

	if h.budget.SessionBudgetExceeded() {
		refuseErr := fmt.Errorf("jev_ask: refusing call: session budget exhausted (spent $%.6f, cap $%.6f)", h.budget.Total(), h.maxUSDPerSession)
		h.auditLog.Log(audit.Entry{
			Tool: ToolNameAsk, Model: h.model, InputStateSHA256: inputHash,
			Status: "error", LatencyMs: time.Since(start).Milliseconds(), Error: refuseErr.Error(),
			ItemCount: len(in.Questions),
		})
		return AskOutput{}, refuseErr
	}

	questions := make(map[string]openrouter.Question, len(in.Questions))
	for id, q := range in.Questions {
		questions[id] = openrouter.Question{Type: q.Type, Instructions: q.Instructions, Criteria: q.Criteria}
	}

	resp, callErr := h.client.Ask(ctx, h.model, questions, in.State, h.timeout)
	latencyMs := time.Since(start).Milliseconds()

	if callErr != nil {
		h.auditLog.Log(audit.Entry{
			Tool: ToolNameAsk, Model: h.model, InputStateSHA256: inputHash,
			Status: "error", LatencyMs: latencyMs, Error: callErr.Error(),
			ItemCount: len(in.Questions),
		})
		return AskOutput{}, fmt.Errorf("jev_ask: %w", callErr)
	}

	out := AskOutput{Model: resp.Model, LatencyMs: latencyMs, Answers: make(map[string]AskAnswer, len(in.Questions))}
	if resp.Usage != nil {
		out.Usage = &Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens, CostUSD: resp.Usage.CostUSD()}
	}

	invalidCount := 0
	for id, q := range in.Questions {
		raw, present := resp.Answers[id]
		ans := AskAnswer{Type: q.Type}
		ok := false
		if present {
			switch q.Type {
			case TypeNoul:
				var v float64
				if v, ok = answers.Noul(raw); ok {
					ans.Noul = &v
				}
			case TypeChoice:
				validOptions := stringKeySet(q.Criteria)
				var choice string
				var confidence float64
				var probs map[string]float64
				if choice, confidence, probs, ok = answers.Choice(raw, validOptions); ok {
					ans.Choice = choice
					ans.Confidence = &confidence
					ans.Probabilities = probs
				}
			case TypeScore:
				levels := criteriaArrayLen(q.Criteria)
				var score, confidence float64
				var probs map[string]float64
				if score, confidence, probs, ok = answers.Score(raw, levels); ok {
					ans.Score = &score
					ans.Confidence = &confidence
					ans.Probabilities = probs
				}
			}
		}
		if ok {
			ans.Status = StatusOK
		} else {
			ans.Status = StatusInvalidResponse
			invalidCount++
		}
		out.Answers[id] = ans
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
		Tool: ToolNameAsk, Model: out.Model, InputStateSHA256: inputHash,
		Status: status, CostUSD: costUSD, LatencyMs: out.LatencyMs, BudgetExceeded: out.BudgetExceeded,
		ItemCount: len(in.Questions), InvalidCount: invalidCount,
	})

	return out, nil
}

// stringKeySet returns the key set of criteria (already validated to be a
// map[string]any by validateInput) as a map[string]bool suitable for
// answers.Choice's validOptions parameter.
func stringKeySet(criteria any) map[string]bool {
	obj, _ := criteria.(map[string]any)
	set := make(map[string]bool, len(obj))
	for k := range obj {
		set[k] = true
	}
	return set
}

// criteriaArrayLen returns len(criteria) for criteria already validated
// to be a []any by validateInput.
func criteriaArrayLen(criteria any) int {
	arr, _ := criteria.([]any)
	return len(arr)
}

// validateInput rejects obviously-unusable input -- including every
// question's type/criteria shape (see package doc comment) -- before
// spending any budget or making a network call.
func validateInput(in AskInput) error {
	if in.State == nil {
		return fmt.Errorf("jev_ask: state must not be omitted")
	}
	if len(in.Questions) == 0 {
		return fmt.Errorf("jev_ask: questions must not be empty")
	}
	if len(in.Questions) > maxQuestions {
		return fmt.Errorf("jev_ask: too many questions (%d, max %d)", len(in.Questions), maxQuestions)
	}
	for id, q := range in.Questions {
		if err := validateQuestion(id, q); err != nil {
			return fmt.Errorf("jev_ask: %w", err)
		}
	}
	return nil
}

// jsonKindName describes v's JSON kind the way a caller thinks about their
// own request (an "object", "array", "string", etc.), never as a Go
// runtime type: %T on a value decoded from JSON into `any` would otherwise
// leak internals like "map[string]interface {}" or "[]interface {}" into
// a user-facing error message, which is meaningless to a caller who only
// ever wrote JSON. v is always something encoding/json produced for an
// `any`-typed field, so the switch above is exhaustive for that domain.
func jsonKindName(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case map[string]any:
		return "an object"
	case []any:
		return "an array"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case float64:
		return "a number"
	default:
		return "an unrecognized value"
	}
}

// criteriaHint shows the expected criteria shape for a question type, so a
// validation error tells the caller how to fix the call.
func criteriaHint(questionType string) string {
	switch questionType {
	case TypeNoul:
		return `for "noul" use {"true": "<when true>", "false": "<when false>"}`
	case TypeChoice:
		return `for "choice" map each option id to its description, e.g. {"a": "<option a>", "b": "<option b>"}; there is no "options" list`
	case TypeScore:
		return `for "score" use an array of level descriptions, lowest first, e.g. ["missing", "partial", "complete"]`
	}
	return ""
}

// validateQuestion validates one question's type and criteria shape.
func validateQuestion(id string, q AskQuestion) error {
	if strings.TrimSpace(q.Instructions) == "" {
		return fmt.Errorf("questions[%q].instructions must not be empty", id)
	}
	switch q.Type {
	case TypeNoul, TypeChoice:
		obj, ok := q.Criteria.(map[string]any)
		if !ok {
			return fmt.Errorf("questions[%q]: criteria must be an object for type %q, got %s instead; %s", id, q.Type, jsonKindName(q.Criteria), criteriaHint(q.Type))
		}
		if len(obj) == 0 {
			return fmt.Errorf("questions[%q]: criteria must not be empty; %s", id, criteriaHint(q.Type))
		}
		for k, v := range obj {
			if _, ok := v.(string); !ok {
				return fmt.Errorf("questions[%q]: criteria[%q] must be a string description, got %s instead; %s", id, k, jsonKindName(v), criteriaHint(q.Type))
			}
		}
		// SystemOne requires exactly these two keys for noul; anything
		// else is rejected by OpenRouter with an opaque HTTP 400
		// (verified live 2026-09-28 with {"yes":...,"no":...}).
		if q.Type == TypeNoul {
			_, hasTrue := obj["true"]
			_, hasFalse := obj["false"]
			if !hasTrue || !hasFalse || len(obj) != 2 {
				return fmt.Errorf("questions[%q]: criteria for \"noul\" must have exactly the keys \"true\" and \"false\"; %s", id, criteriaHint(q.Type))
			}
		}
	case TypeScore:
		arr, ok := q.Criteria.([]any)
		if !ok {
			return fmt.Errorf("questions[%q]: criteria must be an array for type \"score\", got %s instead; %s", id, jsonKindName(q.Criteria), criteriaHint(q.Type))
		}
		if len(arr) == 0 {
			return fmt.Errorf("questions[%q]: criteria must not be empty; %s", id, criteriaHint(q.Type))
		}
		for i, v := range arr {
			if _, ok := v.(string); !ok {
				return fmt.Errorf("questions[%q]: criteria[%d] must be a level-description string, got %s instead; %s", id, i, jsonKindName(v), criteriaHint(q.Type))
			}
		}
	default:
		return fmt.Errorf("questions[%q]: type must be \"noul\", \"choice\", or \"score\", got %q", id, q.Type)
	}
	return nil
}
