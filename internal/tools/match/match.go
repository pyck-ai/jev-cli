// Package match implements the jev_match MCP tool: given a query and a
// list of candidates, picks the single best-matching candidate (and
// reports whether any candidate actually answers the query at all), using
// TypeSafe's Jev judgment model's "choice" and "noul" question types, via
// OpenRouter's SystemOne API.
//
// This package is a self-registering plugin (see internal/registry's
// package doc comment for the overall mechanism).
//
// # Batching and self-sufficiency
//
// Exactly two named questions are asked in a single client.Ask call: one
// "choice" question ("pick", options = every candidate id) and one "noul"
// question ("exists", "does any candidate actually answer the query?").
// Both the query text and the full (truncated) candidate list are put in
// the shared "state" object, and both questions' instructions refer to
// `state` explicitly -- rather than relying on one question's `criteria`
// being visible while answering a different, sibling question in the same
// request, which is not a documented or verified SystemOne behavior. Every
// multi-question tool in this codebase follows this same
// "each question is self-sufficient via shared state" principle.
//
// # What is and isn't independently verified
//
// The "choice" and "noul" question/answer shapes are verified-live wire
// facts from 2026-09-26 (see internal/openrouter's package doc comment) --
// not independently re-verified here (no OPENROUTER_API_KEY was available
// in this implementation environment). The exists_verdict thresholds
// (0.7 "answered", 0.3 "partial", else "absent") are this implementation's
// own invention: the project brief explicitly says "pick and document your
// own thresholds since none are specified verbatim for this exact tool,
// note it's inferred from jkudish's jev_find docs" -- so these are
// documented here as invented, not verified or brief-specified, values.
package match

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/pyck-ai/jev-mcp/internal/answers"
	"github.com/pyck-ai/jev-mcp/internal/audit"
	"github.com/pyck-ai/jev-mcp/internal/budget"
	"github.com/pyck-ai/jev-mcp/internal/capstring"
	"github.com/pyck-ai/jev-mcp/internal/config"
	"github.com/pyck-ai/jev-mcp/internal/openrouter"
	"github.com/pyck-ai/jev-mcp/internal/registry"
)

// ToolNameMatch is the MCP tool name registered for MatchHandler.
const ToolNameMatch = "jev_match"

// Caps named verbatim in the project brief.
const (
	maxCandidates         = 250
	maxCandidateTextChars = 2000
)

// exists_verdict thresholds -- this implementation's own invention, see
// package doc comment.
const (
	existsAnsweredAt = 0.7
	existsPartialAt  = 0.3
)

// ExistsVerdict values.
const (
	VerdictAnswered = "answered"
	VerdictPartial  = "partial"
	VerdictAbsent   = "absent"
)

// Status values for MatchOutput.Status.
const (
	StatusOK              = "ok"
	StatusInvalidResponse = "invalid_response"
)

// Usage mirrors the token accounting reported by OpenRouter for a call.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Candidate is one candidate to match the query against.
type Candidate struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// MatchInput is the jev_match tool's input schema.
type MatchInput struct {
	Query      string      `json:"query" jsonschema:"The query to find the best-matching candidate for."`
	Candidates []Candidate `json:"candidates" jsonschema:"Candidates to search over. Capped at 250; each candidate's text is truncated at 2000 characters before being sent to the model."`
	TopK       int         `json:"top_k,omitempty" jsonschema:"Return at most this many top candidates, sorted by probability descending. Omitted or <= 0 returns every candidate."`
}

// TopMatch is one candidate's probability of being the single best match.
type TopMatch struct {
	ID          string  `json:"id"`
	Probability float64 `json:"probability"`
}

// MatchOutput is the jev_match tool's output schema.
//
// Callers MUST check Status == "ok" before trusting Top, Exists, or
// ExistsVerdict: this tool's "pick" and "exists" answers are used together
// to build one coherent recommendation, so if EITHER is malformed the
// whole result is reported as Status == "invalid_response" (fail-closed at
// the call level, since match -- unlike e.g. jev_check or jev_verify --
// isn't a batch of independent per-item judgments where one bad item
// shouldn't taint the others).
type MatchOutput struct {
	Top    []TopMatch `json:"top"`
	Exists float64    `json:"exists"`
	// ExistsVerdict is one of VerdictAnswered/VerdictPartial/VerdictAbsent
	// when Status == "ok", else "".
	ExistsVerdict  string `json:"exists_verdict"`
	Status         string `json:"status"`
	Model          string `json:"model"`
	Usage          *Usage `json:"usage"`
	LatencyMs      int64  `json:"latency_ms"`
	BudgetExceeded bool   `json:"budget_exceeded,omitempty"`
}

// MatchHandler implements the jev_match tool.
type MatchHandler struct {
	client           *openrouter.Client
	model            string
	timeout          time.Duration
	budget           *budget.Tracker
	maxUSDPerCall    float64
	maxUSDPerSession float64
	auditLog         *audit.Logger
}

// NewMatchHandler builds a MatchHandler from application dependencies.
func NewMatchHandler(client *openrouter.Client, cfg config.Config, tracker *budget.Tracker, auditLog *audit.Logger) *MatchHandler {
	return &MatchHandler{
		client:           client,
		model:            cfg.ModelForTool(ToolNameMatch),
		timeout:          time.Duration(cfg.RequestTimeoutMs) * time.Millisecond,
		budget:           tracker,
		maxUSDPerCall:    cfg.Budget.MaxUSDPerCall,
		maxUSDPerSession: cfg.Budget.MaxUSDPerSession,
		auditLog:         auditLog,
	}
}

func init() {
	registry.Register(func(server *mcp.Server, deps *registry.Deps) {
		h := NewMatchHandler(deps.Client, deps.Config, deps.Budget, deps.Audit)
		mcp.AddTool(server, &mcp.Tool{
			Name: ToolNameMatch,
			Description: "Find the single best-matching candidate for a query, and whether any candidate " +
				"actually answers it at all, using TypeSafe's Jev judgment model. Fails closed: a malformed " +
				"or missing model answer is reported as status=\"invalid_response\", never a fabricated match.",
		}, h.Handle)
	})
}

// Handle implements mcp.ToolHandlerFor[MatchInput, MatchOutput].
func (h *MatchHandler) Handle(ctx context.Context, _ *mcp.CallToolRequest, in MatchInput) (*mcp.CallToolResult, MatchOutput, error) {
	start := time.Now()

	if err := validateInput(in); err != nil {
		return nil, MatchOutput{}, err
	}

	inputHash := audit.HashValue(in)

	if h.budget.SessionBudgetExceeded() {
		refuseErr := fmt.Errorf("jev_match: refusing call: session budget exhausted (spent $%.6f, cap $%.6f)", h.budget.Total(), h.maxUSDPerSession)
		h.auditLog.Log(audit.Entry{
			Tool: ToolNameMatch, Model: h.model, InputStateSHA256: inputHash,
			Status: "error", LatencyMs: time.Since(start).Milliseconds(), Error: refuseErr.Error(),
			ItemCount: len(in.Candidates),
		})
		return nil, MatchOutput{}, refuseErr
	}

	truncated := make([]Candidate, len(in.Candidates))
	criteria := make(map[string]string, len(in.Candidates))
	validOptions := make(map[string]bool, len(in.Candidates))
	for i, c := range in.Candidates {
		t, _ := capstring.Truncate(c.Text, maxCandidateTextChars)
		truncated[i] = Candidate{ID: c.ID, Text: t}
		criteria[c.ID] = t
		validOptions[c.ID] = true
	}
	state := map[string]any{"query": in.Query, "candidates": truncated}

	questions := map[string]openrouter.Question{
		"pick": {
			Type:         "choice",
			Instructions: "Considering `state.query` and `state.candidates`, which single candidate id (from `criteria`) best answers the query?",
			Criteria:     criteria,
		},
		"exists": {
			Type:         "noul",
			Instructions: "Considering `state.query` and `state.candidates`, does ANY of the candidates actually answer the query?",
			Criteria: map[string]string{
				"true":  "at least one candidate answers the query",
				"false": "no candidate answers the query",
			},
		},
	}

	resp, callErr := h.client.Ask(ctx, h.model, questions, state, h.timeout)
	latencyMs := time.Since(start).Milliseconds()

	if callErr != nil {
		h.auditLog.Log(audit.Entry{
			Tool: ToolNameMatch, Model: h.model, InputStateSHA256: inputHash,
			Status: "error", LatencyMs: latencyMs, Error: callErr.Error(),
			ItemCount: len(in.Candidates),
		})
		return nil, MatchOutput{}, fmt.Errorf("jev_match: %w", callErr)
	}

	out := MatchOutput{Model: resp.Model, LatencyMs: latencyMs}
	if resp.Usage != nil {
		out.Usage = &Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens}
	}

	var pickProbs map[string]float64
	pickOK := false
	if raw, present := resp.Answers["pick"]; present {
		_, _, pickProbs, pickOK = answers.Choice(raw, validOptions)
	}
	var exists float64
	existsOK := false
	if raw, present := resp.Answers["exists"]; present {
		exists, existsOK = answers.Noul(raw)
	}

	invalidCount := 0
	if !pickOK {
		invalidCount++
	}
	if !existsOK {
		invalidCount++
	}

	if pickOK && existsOK {
		out.Status = StatusOK
		out.Exists = exists
		out.ExistsVerdict = existsVerdict(exists)
		out.Top = topMatches(pickProbs, in.TopK)
	} else {
		out.Status = StatusInvalidResponse
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

	h.auditLog.Log(audit.Entry{
		Tool: ToolNameMatch, Model: out.Model, InputStateSHA256: inputHash,
		Status: out.Status, CostUSD: costUSD, LatencyMs: out.LatencyMs, BudgetExceeded: out.BudgetExceeded,
		ItemCount: 2, InvalidCount: invalidCount,
	})

	return nil, out, nil
}

// existsVerdict classifies a validated "exists" noul probability using
// this package's own invented thresholds (see package doc comment).
func existsVerdict(exists float64) string {
	switch {
	case exists >= existsAnsweredAt:
		return VerdictAnswered
	case exists >= existsPartialAt:
		return VerdictPartial
	default:
		return VerdictAbsent
	}
}

// topMatches sorts probs (candidate id -> probability) descending by
// probability and returns at most topK entries (all of them if
// topK <= 0). Ties are broken by candidate id for deterministic output.
func topMatches(probs map[string]float64, topK int) []TopMatch {
	out := make([]TopMatch, 0, len(probs))
	for id, p := range probs {
		out = append(out, TopMatch{ID: id, Probability: p})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Probability != out[j].Probability {
			return out[i].Probability > out[j].Probability
		}
		return out[i].ID < out[j].ID
	})
	if topK > 0 && topK < len(out) {
		out = out[:topK]
	}
	return out
}

// validateInput rejects obviously-unusable input before spending any
// budget or making a network call.
func validateInput(in MatchInput) error {
	if strings.TrimSpace(in.Query) == "" {
		return fmt.Errorf("jev_match: query must not be empty")
	}
	if len(in.Candidates) == 0 {
		return fmt.Errorf("jev_match: candidates must not be empty")
	}
	if len(in.Candidates) > maxCandidates {
		return fmt.Errorf("jev_match: too many candidates (%d, max %d)", len(in.Candidates), maxCandidates)
	}
	seen := make(map[string]bool, len(in.Candidates))
	for i, c := range in.Candidates {
		if strings.TrimSpace(c.ID) == "" {
			return fmt.Errorf("jev_match: candidates[%d].id must not be empty", i)
		}
		if seen[c.ID] {
			return fmt.Errorf("jev_match: duplicate candidate id %q", c.ID)
		}
		seen[c.ID] = true
		if strings.TrimSpace(c.Text) == "" {
			return fmt.Errorf("jev_match: candidates[%d].text must not be empty", i)
		}
	}
	return nil
}
