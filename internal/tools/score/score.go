// Package score implements the jev_score MCP tool: judging a single piece
// of text/data against a numeric [scale_min, scale_max] rubric using
// TypeSafe's Jev judgment model, via OpenRouter's SystemOne API's "score"
// question type.
//
// This package is a self-registering plugin (see internal/registry's
// package doc comment for the overall mechanism): its init() function
// registers a registry.Tool whose RegisterMCP wires jev_score onto
// whatever *mcp.Server cmd/jev/main.go passes it at startup. cmd/jev/main.go activates it
// with a single blank import,
// `_ "github.com/pyck-ai/jev-cli/internal/tools/score"` -- deleting this
// directory and that one line is sufficient to remove the tool entirely;
// no other file needs to change.
//
// This package is a structural move of what was previously
// internal/tools/jev_score.go (package tools, the sole tool at the time):
// behavior, input/output schema, and wire format are unchanged from before
// the move to a per-tool-package plugin architecture.
package score

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/pyck-ai/jev-cli/internal/audit"
	"github.com/pyck-ai/jev-cli/internal/budget"
	"github.com/pyck-ai/jev-cli/internal/config"
	"github.com/pyck-ai/jev-cli/internal/openrouter"
	"github.com/pyck-ai/jev-cli/internal/registry"
)

// ToolNameScore is the MCP tool name registered for ScoreHandler, and the
// key used in config.Config.ToolModelOverrides and audit.Entry.Tool.
const ToolNameScore = "jev_score"

// Status values for ScoreOutput.Status. Exactly the two values named in the
// project brief: never a third value, even when a call errors out entirely
// -- outright errors are surfaced as a Go error (see ScoreHandler.Handle),
// which the MCP SDK turns into a CallToolResult with IsError set, rather
// than as a ScoreOutput with some third status string.
const (
	StatusOK              = "ok"
	StatusInvalidResponse = "invalid_response"
)

// maxScaleLevels is a sanity cap on scale_max-scale_min+1. It is this
// implementation's own guard against pathological input (e.g. scale_max =
// 1_000_000), not a documented OpenRouter/Jev limit -- none was found for
// the "score" question type's criteria array length.
const maxScaleLevels = 1000

// Usage mirrors the token accounting reported by OpenRouter for a call.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// CostUSD is the real cost of the call in USD as reported by the API;
	// omitted when the API returned no cost.
	CostUSD *float64 `json:"cost_usd,omitempty"`
}

// ScoreInput is the jev_score tool's input schema.
type ScoreInput struct {
	State        string `json:"state" jsonschema:"The text or data to be judged, e.g. \"The PRD covers scope, metrics, and risks.\". Required, non-empty plain string (not an object or array)."`
	ScaleMin     int    `json:"scale_min" jsonschema:"Inclusive lower bound of the integer scoring rubric, e.g. 0. Required integer; must be strictly less than scale_max."`
	ScaleMax     int    `json:"scale_max" jsonschema:"Inclusive upper bound of the integer scoring rubric, e.g. 2 or 10. Required integer; must be strictly greater than scale_min, and scale_max-scale_min+1 (the number of levels) must be at most 1000."`
	Instructions string `json:"instructions" jsonschema:"What the score means and how to judge state: describe every level from scale_min to scale_max, e.g. \"0=incomplete, 1=partial, 2=complete\". Required, non-empty string."`
}

// ScoreOutput is the jev_score tool's output schema.
//
// Callers MUST check Status == "ok" before trusting Score, Confidence, or
// Probabilities: per the project brief's fail-closed requirement, when
// Status == "invalid_response" those three fields are zero-valued
// placeholders, not a real (if low-confidence) judgment -- this
// implementation never fabricates a score.
type ScoreOutput struct {
	Score         float64            `json:"score"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
	// Status is "ok" or "invalid_response" (see the Status* constants),
	// exactly as specified: it does not encode budget state (see
	// BudgetExceeded below).
	Status    string `json:"status"`
	Usage     *Usage `json:"usage"`
	Model     string `json:"model"`
	LatencyMs int64  `json:"latency_ms"`
	// BudgetExceeded is an addition beyond the exact field list in the
	// project brief. It is set when this specific call's actual cost (from
	// OpenRouter's response) exceeded config Budget.MaxUSDPerCall.
	//
	// Rationale: the brief asks per-call budget overruns to be "refused
	// before sending" if cost is predictable pre-call, or "rejected/flagged
	// after the fact" if only knowable post-hoc, and to document whichever
	// is feasible. OpenRouter only reports a call's cost in its response
	// (see internal/openrouter's package doc comment), so pre-call refusal
	// is not feasible for a *single* call's cost. This implementation
	// therefore flags rather than rejects: Status stays "ok" (the model's
	// answer itself is valid) and BudgetExceeded is set to true, because
	// discarding an already-paid-for, valid judgment would waste the spend
	// without actually protecting the budget -- the spend already happened.
	// json:"omitempty" so existing consumers that only know the brief's
	// literal field list see no change in the (expected common) case where
	// the cap was not exceeded.
	BudgetExceeded bool `json:"budget_exceeded,omitempty"`
}

// ScoreHandler implements the jev_score tool.
type ScoreHandler struct {
	client           *openrouter.Client
	model            string
	timeout          time.Duration
	budget           *budget.Tracker
	maxUSDPerCall    float64
	maxUSDPerSession float64
	auditLog         *audit.Logger
}

// NewScoreHandler builds a ScoreHandler from application dependencies. cfg
// is read once here (model + budget + timeout); it is not retained or
// re-read per call.
func NewScoreHandler(client *openrouter.Client, cfg config.Config, tracker *budget.Tracker, auditLog *audit.Logger) *ScoreHandler {
	return &ScoreHandler{
		client:           client,
		model:            cfg.ModelForTool(ToolNameScore),
		timeout:          time.Duration(cfg.RequestTimeoutMs) * time.Millisecond,
		budget:           tracker,
		maxUSDPerCall:    cfg.Budget.MaxUSDPerCall,
		maxUSDPerSession: cfg.Budget.MaxUSDPerSession,
		auditLog:         auditLog,
	}
}

// init registers jev_score with internal/registry, so a blank import of
// this package (see cmd/jev/main.go) is all that's needed to activate it -- see
// this package's doc comment and internal/registry's for the full
// mechanism.
func init() {
	description := "Judge a single piece of text/data against a numeric [scale_min, scale_max] rubric, " +
		"returning a calibrated probability distribution over every integer level, not just a bare " +
		"number. Use jev_check instead for a plain true/false judgment, or jev_verify for claims " +
		"against separate evidence. Fails closed: always check `status` before trusting " +
		"`score`/`confidence`/`probabilities` -- a malformed model answer is " +
		"status=\"invalid_response\", never a fabricated score. Example: " +
		`{"state": "The PRD covers scope, metrics, and risks.", "scale_min": 0, "scale_max": 2, ` +
		`"instructions": "0=incomplete, 1=partial, 2=complete"}. ` +
		"Output: score + confidence + full probability distribution."
	registry.Register(registry.Tool{
		Name:        "score",
		MCPName:     ToolNameScore,
		Description: description,
		RegisterMCP: func(server *mcp.Server, deps *registry.Deps) {
			h := NewScoreHandler(deps.Client, deps.Config, deps.Budget, deps.Audit)
			mcp.AddTool(server, &mcp.Tool{
				Name:        ToolNameScore,
				Description: description,
			}, h.Handle)
		},
		RegisterCLI: func(root *cobra.Command, provider registry.DepsProvider) {
			root.AddCommand(newCLICommand(provider, description))
		},
	})
}

// Handle implements mcp.ToolHandlerFor[ScoreInput, ScoreOutput]: a thin
// adapter over run (the transport-agnostic core both the MCP handler and
// the CLI subcommand share).
func (h *ScoreHandler) Handle(ctx context.Context, _ *mcp.CallToolRequest, in ScoreInput) (*mcp.CallToolResult, ScoreOutput, error) {
	out, err := h.run(ctx, in)
	if err != nil {
		return nil, ScoreOutput{}, err
	}
	return nil, out, nil
}

// run is jev_score's transport-agnostic core: everything from input
// validation through the SystemOne call, budget accounting, and audit
// logging, independent of whether the caller is the MCP handler (Handle)
// or the CLI subcommand (cli.go).
func (h *ScoreHandler) run(ctx context.Context, in ScoreInput) (ScoreOutput, error) {
	start := time.Now()

	if err := validateInput(in); err != nil {
		return ScoreOutput{}, err
	}

	stateHash := audit.HashState(in.State)
	levels := in.ScaleMax - in.ScaleMin + 1

	// Pre-call session budget refusal. See internal/budget's package doc
	// comment for why only the session cap (not the per-call cap) can be
	// enforced before sending the request.
	if h.budget.SessionBudgetExceeded() {
		err := fmt.Errorf("jev_score: refusing call: session budget exhausted (spent $%.6f, cap $%.6f)", h.budget.Total(), h.maxUSDPerSession)
		h.auditLog.Log(audit.Entry{
			Tool:             ToolNameScore,
			Model:            h.model,
			InputStateSHA256: stateHash,
			ScaleMin:         in.ScaleMin,
			ScaleMax:         in.ScaleMax,
			Status:           "error",
			LatencyMs:        time.Since(start).Milliseconds(),
			Error:            err.Error(),
		})
		return ScoreOutput{}, err
	}

	criteria := make([]string, levels)
	for i := range levels {
		criteria[i] = strconv.Itoa(in.ScaleMin + i)
	}

	resp, callErr := h.client.Score(ctx, h.model, in.Instructions, criteria, in.State, h.timeout)
	latencyMs := time.Since(start).Milliseconds()

	if callErr != nil {
		h.auditLog.Log(audit.Entry{
			Tool:             ToolNameScore,
			Model:            h.model,
			InputStateSHA256: stateHash,
			ScaleMin:         in.ScaleMin,
			ScaleMax:         in.ScaleMax,
			Status:           "error",
			LatencyMs:        latencyMs,
			Error:            callErr.Error(),
		})
		return ScoreOutput{}, fmt.Errorf("jev_score: %w", callErr)
	}

	out := ScoreOutput{
		Model:     resp.Model,
		LatencyMs: latencyMs,
	}
	if resp.Usage != nil {
		out.Usage = &Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens, CostUSD: resp.Usage.CostUSD()}
	}

	score, confidence, probabilities, valid := parseScoreAnswer(resp.Answers, in.ScaleMin, levels)
	if valid {
		out.Status = StatusOK
		out.Score = score
		out.Confidence = confidence
		out.Probabilities = probabilities
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

	entry := audit.Entry{
		Tool:             ToolNameScore,
		Model:            out.Model,
		InputStateSHA256: stateHash,
		ScaleMin:         in.ScaleMin,
		ScaleMax:         in.ScaleMax,
		Status:           out.Status,
		CostUSD:          costUSD,
		LatencyMs:        out.LatencyMs,
		BudgetExceeded:   out.BudgetExceeded,
	}
	if out.Usage != nil {
		entry.Usage = &audit.Usage{InputTokens: out.Usage.InputTokens, OutputTokens: out.Usage.OutputTokens}
	}
	if valid {
		s, c := out.Score, out.Confidence
		entry.Score = &s
		entry.Confidence = &c
	}
	h.auditLog.Log(entry)

	return out, nil
}

// validateInput rejects obviously-unusable input before spending any
// budget or making a network call.
//
// Every error names the offending field, says what's wrong, and says
// what's expected (including the limit/example): agents calling this
// tool over MCP only ever see the error text, so the message has to
// carry enough information to fix the call on the next try.
func validateInput(in ScoreInput) error {
	if strings.TrimSpace(in.State) == "" {
		return fmt.Errorf("jev_score: state: must not be empty; provide the text or data to judge as a non-empty string, e.g. \"The PRD covers scope, metrics, and risks.\"")
	}
	if strings.TrimSpace(in.Instructions) == "" {
		return fmt.Errorf("jev_score: instructions: must not be empty; describe what each scale level means, e.g. \"0=incorrect, 1=partially correct, 2=fully correct\"")
	}
	if in.ScaleMax <= in.ScaleMin {
		return fmt.Errorf("jev_score: scale_max: must be greater than scale_min (got scale_max=%d, scale_min=%d); e.g. scale_min=0, scale_max=2", in.ScaleMax, in.ScaleMin)
	}
	if levels := in.ScaleMax - in.ScaleMin + 1; levels > maxScaleLevels {
		return fmt.Errorf("jev_score: scale_min/scale_max: range too large (%d levels; max %d); narrow the gap between scale_min and scale_max so it spans at most %d levels", levels, maxScaleLevels, maxScaleLevels)
	}
	return nil
}

// parseScoreAnswer extracts and validates the "score" answer from a
// SystemOne response's Answers map, remapping OpenRouter's 0-based level
// indices back to the caller's [scaleMin, scaleMin+levels-1] range (see
// internal/openrouter's package doc comment for why this remapping is
// necessary and how it was derived).
//
// Returns valid=false -- triggering ScoreOutput.Status ==
// StatusInvalidResponse, i.e. "fail closed, never fabricate a score" --
// exactly when the project brief says to: the "score" key is missing or
// doesn't parse as a score-type answer, a probability for some expected
// level is missing or out of the plausible [0,1] range, or the
// probabilities do not sum to ~1 (tolerance 0.01).
func parseScoreAnswer(answers map[string]json.RawMessage, scaleMin, levels int) (score, confidence float64, probabilities map[string]float64, valid bool) {
	raw, ok := answers["score"]
	if !ok {
		return 0, 0, nil, false
	}

	var ans openrouter.ScoreAnswer
	if err := json.Unmarshal(raw, &ans); err != nil {
		return 0, 0, nil, false
	}
	if ans.Type != "score" {
		return 0, 0, nil, false
	}

	remapped := make(map[string]float64, levels)
	sum := 0.0
	for i := range levels {
		v, present := ans.Probabilities[strconv.Itoa(i)]
		if !present || v < -0.001 || v > 1.001 {
			return 0, 0, nil, false
		}
		remapped[strconv.Itoa(scaleMin+i)] = v
		sum += v
	}
	if math.Abs(sum-1.0) > 0.01 {
		return 0, 0, nil, false
	}

	return float64(scaleMin) + ans.Score, ans.Confidence, remapped, true
}
