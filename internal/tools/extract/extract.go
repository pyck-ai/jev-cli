// Package extract implements the jev_extract MCP tool: for each of a list
// of named fields, runs a caller-supplied regular expression against a
// document to find candidate substrings, then -- only if there are any --
// asks the configured SystemOne decision model's "choice" question type (via
// OpenRouter's SystemOne API) to pick which candidate is the field's real
// value.
//
// This package is a self-registering plugin (see internal/registry's
// package doc comment for the overall mechanism).
//
// # Regex-first, model-second, and the zero-call fast path
//
// Every field's regex runs BEFORE any network call. A field with zero
// matches becomes "not_found" with NO model call for that field at all
// (per the project brief). If, after running every field's regex, NOT A
// SINGLE field has any candidates needing a model decision, this handler
// makes NO API call whatsoever and returns immediately -- no budget
// check, no cost, per the project brief's "If a call has zero fields
// needing the model (all zero-match), make no API call at all and return
// immediately."
//
// When at least one field does need a decision, every such field's
// "choice" question is batched into a single client.Ask call (one per
// field needing it, keyed by that field's own ID, since field ids are
// already required to be unique -- see validateInput), all sharing the
// document as `state`, per this codebase's "every question must be
// self-sufficient" principle (see internal/tools/match's package doc
// comment).
//
// Per the project brief, each such question's `criteria` uses "each
// candidate match's text as its own option" -- i.e. the option id IS the
// matched substring itself (deduplicated), not a synthetic id -- so the
// model's returned `choice` can be used directly as FieldResult.Value with
// no separate id-to-text lookup table.
//
// # Regex engine and the timeout
//
// Field.Pattern is compiled with Go's stdlib regexp package, i.e. RE2
// syntax (not PCRE/ECMA -- no backreferences or lookaround; see
// regexp/syntax's docs). RE2 is specifically designed to run in time
// linear in the size of the input, so it is NOT susceptible to the
// classic catastrophic-backtracking ReDoS that a hard timeout usually
// defends against. This implementation still enforces the project
// brief's explicit "hard 1-second timeout per field" requirement anyway,
// as defense in depth against a large document x many fields combination
// taking longer than expected in aggregate -- implemented as a goroutine
// racing a time.After via select (Go's regexp API offers no cancellation
// hook to abort an in-flight match, so -- unlike an HTTP call bounded by
// context -- "timeout" here means "stop waiting for an answer", not
// "abort the computation"; the goroutine itself keeps running against the
// buffered result channel and exits harmlessly once RE2 does finish, it
// is just not waited on). A field that times out is reported as
// status = StatusInvalidResponse (see FieldResult.Status): there is no
// dedicated status value for "regex timed out" in the project brief's
// literal 5-value enum (auto/review/not_found/invalid_pattern/
// invalid_response), and "we could not produce a trustworthy candidate
// set for this field" is closest in spirit to invalid_response among
// those five -- documented here as an interpretation, not a brief-specified
// mapping.
//
// # What is and isn't independently verified
//
// The "choice" question/answer shape is a verified-live wire fact from
// 2026-09-26 (see internal/openrouter's package doc comment) -- not
// independently re-verified here (no OPENROUTER_API_KEY was available in
// this implementation environment). auto_accept (default 0.8, exposed as
// an optional input field per this codebase's general convention -- see
// internal/answers.ResolveThreshold's doc comment) is this
// implementation's own invention: the brief did not state a confidence
// threshold or default for jev_extract's auto/review split at all.
package extract

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/pyck-ai/jev-cli/internal/answers"
	"github.com/pyck-ai/jev-cli/internal/audit"
	"github.com/pyck-ai/jev-cli/internal/budget"
	"github.com/pyck-ai/jev-cli/internal/capstring"
	"github.com/pyck-ai/jev-cli/internal/config"
	"github.com/pyck-ai/jev-cli/internal/openrouter"
	"github.com/pyck-ai/jev-cli/internal/registry"
)

// ToolNameExtract is the MCP tool name registered for ExtractHandler.
const ToolNameExtract = "jev_extract"

// Caps named verbatim in the project brief.
const (
	maxDocumentChars   = 50_000
	maxFields          = 32
	maxMatchesPerField = 20
)

// regexTimeout is the project brief's literal "hard 1-second timeout per
// field" (see package doc comment for why Go's RE2 engine makes this
// belt-and-suspenders rather than a ReDoS necessity).
const regexTimeout = 1 * time.Second

// defaultAutoAccept is this implementation's own invented default (see
// package doc comment).
const defaultAutoAccept = 0.8

// noneOfThem is the sentinel option offered alongside every field's real
// candidates, verbatim from the project brief.
const noneOfThem = "none_of_them"

// Status values for FieldResult.Status, verbatim from the project brief's
// 5-value enum.
const (
	StatusAuto            = "auto"
	StatusReview          = "review"
	StatusNotFound        = "not_found"
	StatusInvalidPattern  = "invalid_pattern"
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

// Field is one named field to extract, defined by a regular expression
// (Go/RE2 syntax) run against Document.
type Field struct {
	ID          string `json:"id" jsonschema:"Unique field id, echoed as the output key"`
	Pattern     string `json:"pattern" jsonschema:"RE2 regex (no backreferences or lookaround); up to 20 matches become candidates; invalid gives status invalid_pattern"`
	Description string `json:"description" jsonschema:"What the field is, shown to the model to pick among candidates"`
}

// ExtractInput is the jev_extract tool's input schema.
type ExtractInput struct {
	Document   string  `json:"document" jsonschema:"The text itself, pasted in full (never a file path or a request to read one); truncated at 50,000 chars"`
	Fields     []Field `json:"fields" jsonschema:"1-32 fields, each with id, pattern, description"`
	AutoAccept float64 `json:"auto_accept,omitempty" jsonschema:"Confidence in (0.5, 1] needed for status auto vs review; default 0.8"`
}

// FieldResult is one field's extraction result.
//
// Value is non-nil only when Status == StatusAuto or StatusReview (a real
// candidate was picked); it is nil for StatusNotFound, StatusInvalidPattern,
// and StatusInvalidResponse -- and also nil when the model confidently
// picked "none of the candidates", which is itself reported as
// StatusReview (a human should look, since regex DID find candidates but
// the model rejected all of them) rather than a fabricated value.
type FieldResult struct {
	ID                   string  `json:"id"`
	Value                *string `json:"value"`
	Status               string  `json:"status"`
	CandidatesConsidered int     `json:"candidates_considered"`
	CandidatesTruncated  bool    `json:"candidates_truncated"`
}

// ExtractOutput is the jev_extract tool's output schema.
//
// Model/Usage are nil and LatencyMs still reflects real elapsed regex-only
// processing time when every field resolved without a model call at all
// (see package doc comment).
type ExtractOutput struct {
	Fields         []FieldResult `json:"fields"`
	Model          string        `json:"model,omitempty"`
	Usage          *Usage        `json:"usage,omitempty"`
	LatencyMs      int64         `json:"latency_ms"`
	BudgetExceeded bool          `json:"budget_exceeded,omitempty"`
}

// ExtractHandler implements the jev_extract tool.
type ExtractHandler struct {
	client           *openrouter.Client
	model            string
	timeout          time.Duration
	budget           *budget.Tracker
	maxUSDPerCall    float64
	maxUSDPerSession float64
	auditLog         *audit.Logger
}

// NewExtractHandler builds an ExtractHandler from application dependencies.
func NewExtractHandler(client *openrouter.Client, cfg config.Config, tracker *budget.Tracker, auditLog *audit.Logger) *ExtractHandler {
	return &ExtractHandler{
		client:           client,
		model:            cfg.ModelForTool(ToolNameExtract),
		timeout:          time.Duration(cfg.RequestTimeoutMs) * time.Millisecond,
		budget:           tracker,
		maxUSDPerCall:    cfg.Budget.MaxUSDPerCall,
		maxUSDPerSession: cfg.Budget.MaxUSDPerSession,
		auditLog:         auditLog,
	}
}

func init() {
	description := "Disambiguate regex matches in text you ALREADY HAVE: per field a Go/RE2 regex finds candidates in the " +
		"given text, then the model picks the right one. Not for reading files, editing code or general tasks. " +
		"Returns fields[] with id, value (or null), status (auto/review/not_found/invalid_pattern/" +
		"invalid_response); never fabricates a value. Use jev_ask for open-ended questions."
	registry.Register(registry.Tool{
		Name:        "extract",
		MCPName:     ToolNameExtract,
		Description: description,
		RegisterMCP: func(server *mcp.Server, deps *registry.Deps) {
			h := NewExtractHandler(deps.Client, deps.Config, deps.Budget, deps.Audit)
			mcp.AddTool(server, &mcp.Tool{
				Name:        ToolNameExtract,
				Description: description,
			}, h.Handle)
		},
		RegisterCLI: func(root *cobra.Command, provider registry.DepsProvider) {
			root.AddCommand(newCLICommand(provider, description))
		},
		Run: registry.Runner(func(ctx context.Context, d *registry.Deps, in ExtractInput) (ExtractOutput, error) {
			return NewExtractHandler(d.Client, d.Config, d.Budget, d.Audit).run(ctx, in)
		}, exitCode),
	})
}

// Handle implements mcp.ToolHandlerFor[ExtractInput, ExtractOutput]: a
// thin adapter over run (the transport-agnostic core both the MCP
// handler and the CLI subcommand share).
func (h *ExtractHandler) Handle(ctx context.Context, _ *mcp.CallToolRequest, in ExtractInput) (*mcp.CallToolResult, ExtractOutput, error) {
	out, err := h.run(ctx, in)
	if err != nil {
		return nil, ExtractOutput{}, err
	}
	return nil, out, nil
}

// run is jev_extract's transport-agnostic core: everything from input
// validation through the SystemOne call, budget accounting, and audit
// logging, independent of whether the caller is the MCP handler (Handle)
// or the CLI subcommand (cli.go).
func (h *ExtractHandler) run(ctx context.Context, in ExtractInput) (ExtractOutput, error) {
	start := time.Now()

	if err := validateInput(in); err != nil {
		return ExtractOutput{}, err
	}
	autoAccept := answers.ResolveThreshold(in.AutoAccept, defaultAutoAccept)

	document, _ := capstring.Truncate(in.Document, maxDocumentChars)

	results := make([]FieldResult, len(in.Fields))
	// pending indexes (into in.Fields/results) of fields that found at
	// least one candidate and therefore need a model decision.
	var pending []int
	candidatesByField := make(map[int][]string, len(in.Fields))

	for i, f := range in.Fields {
		re, err := regexp.Compile(f.Pattern)
		if err != nil {
			results[i] = FieldResult{ID: f.ID, Status: StatusInvalidPattern}
			continue
		}
		candidates, truncated, timedOut := findCandidates(re, document, maxMatchesPerField, regexTimeout)
		if timedOut {
			results[i] = FieldResult{ID: f.ID, Status: StatusInvalidResponse}
			continue
		}
		if len(candidates) == 0 {
			results[i] = FieldResult{ID: f.ID, Status: StatusNotFound}
			continue
		}
		results[i] = FieldResult{ID: f.ID, CandidatesConsidered: len(candidates), CandidatesTruncated: truncated}
		candidatesByField[i] = candidates
		pending = append(pending, i)
	}

	inputHash := audit.HashValue(in)

	if len(pending) == 0 {
		// Per the project brief: zero fields need the model -> no API
		// call at all, not even a budget check (nothing would be spent).
		out := ExtractOutput{Fields: results, LatencyMs: time.Since(start).Milliseconds()}
		h.auditLog.Log(audit.Entry{
			Tool: ToolNameExtract, Model: h.model, InputStateSHA256: inputHash,
			Status: "ok", LatencyMs: out.LatencyMs, ItemCount: len(in.Fields),
		})
		return out, nil
	}

	if h.budget.SessionBudgetExceeded() {
		refuseErr := fmt.Errorf("jev_extract: refusing call: session budget exhausted (spent $%.6f, cap $%.6f)", h.budget.Total(), h.maxUSDPerSession)
		h.auditLog.Log(audit.Entry{
			Tool: ToolNameExtract, Model: h.model, InputStateSHA256: inputHash,
			Status: "error", LatencyMs: time.Since(start).Milliseconds(), Error: refuseErr.Error(),
			ItemCount: len(in.Fields),
		})
		return ExtractOutput{}, refuseErr
	}

	questions := make(map[string]openrouter.Question, len(pending))
	validOptionsByField := make(map[int]map[string]bool, len(pending))
	for _, i := range pending {
		f := in.Fields[i]
		criteria := make(map[string]string, len(candidatesByField[i])+1)
		validOptions := make(map[string]bool, len(candidatesByField[i])+1)
		for _, c := range candidatesByField[i] {
			criteria[c] = fmt.Sprintf("the substring %q found in the document", c)
			validOptions[c] = true
		}
		criteria[noneOfThem] = "none of the candidate substrings above is the correct value for this field"
		validOptions[noneOfThem] = true
		validOptionsByField[i] = validOptions

		questions[f.ID] = openrouter.Question{
			Type: "choice",
			Instructions: fmt.Sprintf(
				"Considering the document in `state`, which candidate substring in `criteria` is the correct "+
					"value for this field (or is it none of them)?\n\nField: %s - %s",
				f.ID, f.Description,
			),
			Criteria: criteria,
		}
	}

	resp, callErr := h.client.Ask(ctx, h.model, questions, document, h.timeout)
	latencyMs := time.Since(start).Milliseconds()

	if callErr != nil {
		h.auditLog.Log(audit.Entry{
			Tool: ToolNameExtract, Model: h.model, InputStateSHA256: inputHash,
			Status: "error", LatencyMs: latencyMs, Error: callErr.Error(),
			ItemCount: len(in.Fields),
		})
		return ExtractOutput{}, fmt.Errorf("jev_extract: %w", callErr)
	}

	out := ExtractOutput{Model: resp.Model, LatencyMs: latencyMs}
	if resp.Usage != nil {
		out.Usage = &Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens, CostUSD: resp.Usage.CostUSD()}
	}

	invalidCount := 0
	for _, i := range pending {
		f := in.Fields[i]
		raw, present := resp.Answers[f.ID]
		var choice string
		var confidence float64
		ok := false
		if present {
			choice, confidence, _, ok = answers.Choice(raw, validOptionsByField[i])
		}
		res := results[i]
		switch {
		case !ok:
			res.Status = StatusInvalidResponse
			invalidCount++
		case choice == noneOfThem:
			res.Status = StatusReview
		default:
			v := choice
			res.Value = &v
			if confidence >= autoAccept {
				res.Status = StatusAuto
			} else {
				res.Status = StatusReview
			}
		}
		results[i] = res
	}
	out.Fields = results

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
		Tool: ToolNameExtract, Model: out.Model, InputStateSHA256: inputHash,
		Status: status, CostUSD: costUSD, LatencyMs: out.LatencyMs, BudgetExceeded: out.BudgetExceeded,
		ItemCount: len(in.Fields), InvalidCount: invalidCount,
	})

	return out, nil
}

// findCandidates runs re against document, bounded by timeout (see
// package doc comment for why this is "stop waiting", not "abort the
// match"). It returns at most maxMatches deduplicated (first-occurrence
// order preserved) matched substrings, and whether more than maxMatches
// raw matches existed (by asking for one more than the cap and checking
// whether that extra one came back).
func findCandidates(re *regexp.Regexp, document string, maxMatches int, timeout time.Duration) (candidates []string, truncated bool, timedOut bool) {
	ch := make(chan []string, 1)
	go func() {
		ch <- re.FindAllString(document, maxMatches+1)
	}()

	select {
	case raw := <-ch:
		truncated = len(raw) > maxMatches
		if truncated {
			raw = raw[:maxMatches]
		}
		seen := make(map[string]bool, len(raw))
		for _, m := range raw {
			if !seen[m] {
				seen[m] = true
				candidates = append(candidates, m)
			}
		}
		return candidates, truncated, false
	case <-time.After(timeout):
		return nil, false, true
	}
}

// validateInput rejects obviously-unusable input before running any
// regex or spending any budget.
func validateInput(in ExtractInput) error {
	if strings.TrimSpace(in.Document) == "" {
		return fmt.Errorf("jev_extract: document must not be empty; provide the text to extract fields from (a non-empty string, up to 50,000 characters)")
	}
	if len(in.Fields) == 0 {
		return fmt.Errorf(`jev_extract: fields must not be empty; provide 1-32 fields, each an object with id, pattern, and description, e.g. [{"id":"email","pattern":"[0-9]{3}-[0-9]{4}","description":"the contact phone extension"}]`)
	}
	if len(in.Fields) > maxFields {
		return fmt.Errorf("jev_extract: fields has %d entries, more than the max of %d; remove some fields or split the document across multiple calls", len(in.Fields), maxFields)
	}
	seen := make(map[string]bool, len(in.Fields))
	for i, f := range in.Fields {
		if strings.TrimSpace(f.ID) == "" {
			return fmt.Errorf(`jev_extract: fields[%d].id must not be empty; give this field a short unique identifier, e.g. "email"`, i)
		}
		if seen[f.ID] {
			return fmt.Errorf("jev_extract: fields[%d].id: %q is already used by another field; every fields[].id must be unique within one call", i, f.ID)
		}
		seen[f.ID] = true
		if strings.TrimSpace(f.Pattern) == "" {
			return fmt.Errorf(`jev_extract: fields[%d].pattern must not be empty; provide a Go/RE2 regular expression (no backreferences or lookaround) whose matches become this field's candidates, e.g. "[0-9]{3}-[0-9]{4}"`, i)
		}
		if strings.TrimSpace(f.Description) == "" {
			return fmt.Errorf(`jev_extract: fields[%d].description must not be empty; describe in plain language what this field represents so the model can pick the right match, e.g. "the customer's phone number"`, i)
		}
	}
	if err := answers.ValidateAutoAccept("auto_accept", in.AutoAccept); err != nil {
		return fmt.Errorf(`jev_extract: %w (0 means "use the default 0.8")`, err)
	}
	return nil
}
