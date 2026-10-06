// Package rerank implements the jev_rerank MCP tool: scores every
// candidate's relevance to a query independently and returns them sorted
// descending, using TypeSafe's Jev judgment model's "noul" question type,
// via OpenRouter's SystemOne API.
//
// This package is a self-registering plugin (see internal/registry's
// package doc comment for the overall mechanism).
//
// # Batching and self-sufficiency
//
// One named "noul" question per candidate ("cand0", "cand1", ...) is
// asked in a single client.Ask call. Since SystemOne's request shape has
// exactly one shared "state" value for the WHOLE request (see
// internal/openrouter.Request), not one per question, per-candidate text
// cannot be embedded in a per-question state -- so, following this
// codebase's "every question must be self-sufficient" principle (see
// internal/tools/match's package doc comment), the query AND the full
// candidate list are both put in the single shared "state" object, and
// each per-candidate question's instructions refer to that specific
// candidate by id (e.g. "the candidate with id \"cand3\" in
// `state.candidates`"), rather than repeating that candidate's full text
// redundantly inside 250 separate instruction strings.
//
// # Fail-closed at the call level, not per-candidate
//
// Per the project brief: "If any candidate's answer is malformed, the
// whole call reports status: invalid_response rather than silently
// treating a missing score as zero." Unlike internal/tools/check or
// internal/tools/verify (where one bad item doesn't taint the others),
// jev_rerank's whole point is a relative ORDERING across all candidates --
// silently scoring one malformed candidate as 0.0 could badly distort
// that ordering (a candidate that was actually highly relevant, but whose
// answer merely failed to parse, would look like the least relevant one)
// -- so this tool fails the entire call closed instead.
//
// # What is and isn't independently verified
//
// The "noul" question/answer shape is a verified-live wire fact from
// 2026-09-26 (see internal/openrouter's package doc comment) -- not
// independently re-verified here (no OPENROUTER_API_KEY was available in
// this implementation environment). The "aggregate cap is enforced by
// rejecting the call outright" behavior (as opposed to e.g. truncating
// individual candidates until the aggregate fits) is this implementation's
// own choice, made because the brief specifies a per-tool aggregate
// character cap without specifying a truncation strategy, and silently
// dropping/truncating an unspecified subset of candidates would change
// the very ranking this tool exists to produce, in a way a caller could
// easily miss.
package rerank

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

// ToolNameRerank is the MCP tool name registered for RerankHandler.
const ToolNameRerank = "jev_rerank"

// Caps named verbatim in the project brief.
const (
	maxCandidates     = 250
	maxAggregateChars = 100_000
)

// Status values for RerankOutput.Status.
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

// Candidate is one candidate to rank against the query.
type Candidate struct {
	ID   string `json:"id" jsonschema:"Caller-chosen identifier for this candidate, e.g. \"a\" or \"doc-1\". Required, must be a non-empty string, and must be unique among all candidates in this call; echoed back in the output ranking to identify this candidate."`
	Text string `json:"text" jsonschema:"This candidate's text content, judged for relevance against query. Required, must be a non-empty string."`
}

// RerankInput is the jev_rerank tool's input schema.
type RerankInput struct {
	Query      string      `json:"query" jsonschema:"The query every candidate is scored for relevance against, e.g. \"vendor security posture\". Required, must be a non-empty string."`
	Candidates []Candidate `json:"candidates" jsonschema:"Candidates to rank, e.g. [{\"id\": \"a\", \"text\": \"...\"}, {\"id\": \"b\", \"text\": \"...\"}]. Required, an array of 1-250 {id, text} objects, with combined text across all candidates not exceeding 100,000 characters."`
}

// Ranked is one candidate's position in the final descending-relevance
// ordering. Only populated when RerankOutput.Status == "ok".
type Ranked struct {
	Rank      int     `json:"rank"`
	ID        string  `json:"id"`
	Relevance float64 `json:"relevance"`
}

// RerankOutput is the jev_rerank tool's output schema.
//
// Callers MUST check Status == "ok" before trusting Ranked: if ANY
// candidate's answer was malformed, Ranked is nil and Status is
// "invalid_response" for the WHOLE call (see package doc comment for why
// this is call-level, not per-candidate, fail-closed behavior).
type RerankOutput struct {
	Ranked         []Ranked `json:"ranked"`
	Status         string   `json:"status"`
	Model          string   `json:"model"`
	Usage          *Usage   `json:"usage"`
	LatencyMs      int64    `json:"latency_ms"`
	BudgetExceeded bool     `json:"budget_exceeded,omitempty"`
}

// RerankHandler implements the jev_rerank tool.
type RerankHandler struct {
	client           *openrouter.Client
	model            string
	timeout          time.Duration
	budget           *budget.Tracker
	maxUSDPerCall    float64
	maxUSDPerSession float64
	auditLog         *audit.Logger
}

// NewRerankHandler builds a RerankHandler from application dependencies.
func NewRerankHandler(client *openrouter.Client, cfg config.Config, tracker *budget.Tracker, auditLog *audit.Logger) *RerankHandler {
	return &RerankHandler{
		client:           client,
		model:            cfg.ModelForTool(ToolNameRerank),
		timeout:          time.Duration(cfg.RequestTimeoutMs) * time.Millisecond,
		budget:           tracker,
		maxUSDPerCall:    cfg.Budget.MaxUSDPerCall,
		maxUSDPerSession: cfg.Budget.MaxUSDPerSession,
		auditLog:         auditLog,
	}
}

func init() {
	description := "Rank a list of candidates by relevance to a query, using TypeSafe's Jev judgment " +
		"model. Returns EVERY candidate sorted descending by relevance, not just the top one -- use " +
		"jev_match instead if you only need the single best match and whether anything actually " +
		"matches at all. Fails closed at the WHOLE-CALL level: if any candidate's answer is malformed, " +
		"status=\"invalid_response\" and no ranking is returned at all, rather than silently treating " +
		"a missing score as zero (which could badly distort the ordering). Example: " +
		`{"query": "vendor security posture", "candidates": [{"id": "a", "text": "..."}, ` +
		`{"id": "b", "text": "..."}]}. ` +
		"Output: a ranked list of {rank, id, relevance} plus status."
	registry.Register(registry.Tool{
		Name:        "rerank",
		MCPName:     ToolNameRerank,
		Description: description,
		RegisterMCP: func(server *mcp.Server, deps *registry.Deps) {
			h := NewRerankHandler(deps.Client, deps.Config, deps.Budget, deps.Audit)
			mcp.AddTool(server, &mcp.Tool{
				Name:        ToolNameRerank,
				Description: description,
			}, h.Handle)
		},
		RegisterCLI: func(root *cobra.Command, provider registry.DepsProvider) {
			root.AddCommand(newCLICommand(provider, description))
		},
	})
}

// Handle implements mcp.ToolHandlerFor[RerankInput, RerankOutput]: a thin
// adapter over run (the transport-agnostic core both the MCP handler and
// the CLI subcommand share).
func (h *RerankHandler) Handle(ctx context.Context, _ *mcp.CallToolRequest, in RerankInput) (*mcp.CallToolResult, RerankOutput, error) {
	out, err := h.run(ctx, in)
	if err != nil {
		return nil, RerankOutput{}, err
	}
	return nil, out, nil
}

// run is jev_rerank's transport-agnostic core: everything from input
// validation through the SystemOne call, budget accounting, and audit
// logging, independent of whether the caller is the MCP handler (Handle)
// or the CLI subcommand (cli.go).
func (h *RerankHandler) run(ctx context.Context, in RerankInput) (RerankOutput, error) {
	start := time.Now()

	if err := validateInput(in); err != nil {
		return RerankOutput{}, err
	}

	inputHash := audit.HashValue(in)

	if h.budget.SessionBudgetExceeded() {
		refuseErr := fmt.Errorf("jev_rerank: refusing call: session budget exhausted (spent $%.6f, cap $%.6f)", h.budget.Total(), h.maxUSDPerSession)
		h.auditLog.Log(audit.Entry{
			Tool: ToolNameRerank, Model: h.model, InputStateSHA256: inputHash,
			Status: "error", LatencyMs: time.Since(start).Milliseconds(), Error: refuseErr.Error(),
			ItemCount: len(in.Candidates),
		})
		return RerankOutput{}, refuseErr
	}

	state := map[string]any{"query": in.Query, "candidates": in.Candidates}
	questions := make(map[string]openrouter.Question, len(in.Candidates))
	for i, c := range in.Candidates {
		questions[candKey(i)] = openrouter.Question{
			Type: "noul",
			Instructions: fmt.Sprintf(
				"Considering `state.query`, is the candidate with id %q in `state.candidates` relevant to the query?",
				c.ID,
			),
			Criteria: map[string]string{
				"true":  "relevant to the query",
				"false": "not relevant to the query",
			},
		}
	}

	resp, callErr := h.client.Ask(ctx, h.model, questions, state, h.timeout)
	latencyMs := time.Since(start).Milliseconds()

	if callErr != nil {
		h.auditLog.Log(audit.Entry{
			Tool: ToolNameRerank, Model: h.model, InputStateSHA256: inputHash,
			Status: "error", LatencyMs: latencyMs, Error: callErr.Error(),
			ItemCount: len(in.Candidates),
		})
		return RerankOutput{}, fmt.Errorf("jev_rerank: %w", callErr)
	}

	out := RerankOutput{Model: resp.Model, LatencyMs: latencyMs}
	if resp.Usage != nil {
		out.Usage = &Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens, CostUSD: resp.Usage.CostUSD()}
	}

	relevance := make([]float64, len(in.Candidates))
	invalidCount := 0
	allValid := true
	for i := range in.Candidates {
		raw, present := resp.Answers[candKey(i)]
		v, ok := 0.0, false
		if present {
			v, ok = answers.Noul(raw)
		}
		if !ok {
			allValid = false
			invalidCount++
			continue
		}
		relevance[i] = v
	}

	if allValid {
		out.Status = StatusOK
		ranked := make([]Ranked, len(in.Candidates))
		for i, c := range in.Candidates {
			ranked[i] = Ranked{ID: c.ID, Relevance: relevance[i]}
		}
		sort.Slice(ranked, func(a, b int) bool {
			if ranked[a].Relevance != ranked[b].Relevance {
				return ranked[a].Relevance > ranked[b].Relevance
			}
			return ranked[a].ID < ranked[b].ID
		})
		for i := range ranked {
			ranked[i].Rank = i + 1
		}
		out.Ranked = ranked
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
		Tool: ToolNameRerank, Model: out.Model, InputStateSHA256: inputHash,
		Status: out.Status, CostUSD: costUSD, LatencyMs: out.LatencyMs, BudgetExceeded: out.BudgetExceeded,
		ItemCount: len(in.Candidates), InvalidCount: invalidCount,
	})

	return out, nil
}

func candKey(i int) string { return "cand" + strconv.Itoa(i) }

// validateInput rejects obviously-unusable input before spending any
// budget or making a network call, including both caps named in the
// project brief (candidate count and aggregate text length -- see
// package doc comment for why the aggregate cap rejects rather than
// truncates).
func validateInput(in RerankInput) error {
	if strings.TrimSpace(in.Query) == "" {
		return fmt.Errorf("jev_rerank: query must not be empty; provide the text every candidate is scored for relevance against, e.g. \"vendor security posture\"")
	}
	if len(in.Candidates) == 0 {
		return fmt.Errorf("jev_rerank: candidates must not be empty; provide an array of 1-250 {\"id\": <string>, \"text\": <string>} objects, e.g. [{\"id\": \"a\", \"text\": \"...\"}]")
	}
	if len(in.Candidates) > maxCandidates {
		return fmt.Errorf("jev_rerank: candidates has %d entries, exceeding the max of %d; send fewer candidates or split the call", len(in.Candidates), maxCandidates)
	}
	seen := make(map[string]bool, len(in.Candidates))
	aggregate := 0
	for i, c := range in.Candidates {
		if strings.TrimSpace(c.ID) == "" {
			return fmt.Errorf("jev_rerank: candidates[%d].id must not be empty; each candidate needs a unique non-empty string id, e.g. \"a\"", i)
		}
		if seen[c.ID] {
			return fmt.Errorf("jev_rerank: candidates[%d].id: %q is already used by another candidate; every candidate needs a unique id", i, c.ID)
		}
		seen[c.ID] = true
		if strings.TrimSpace(c.Text) == "" {
			return fmt.Errorf("jev_rerank: candidates[%d].text must not be empty; provide this candidate's text content to compare against query", i)
		}
		aggregate += len([]rune(c.Text))
	}
	if aggregate > maxAggregateChars {
		return fmt.Errorf("jev_rerank: candidates[].text: aggregate length across all candidates is %d characters, exceeding the %d character cap; shorten candidates' text or send fewer candidates", aggregate, maxAggregateChars)
	}
	return nil
}
