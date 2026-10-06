// Package doctor implements the jev_doctor MCP tool (renamed from
// blakestone's jev_health per the project brief): a minimal, cheap
// SystemOne round-trip against the currently configured (or
// caller-overridden) model, reporting reachability and latency, plus a
// snapshot of the currently active configuration (resolved model,
// credential source, budget caps, and current session spend) so this
// doubles as a configuration-sanity tool, not just a network ping. The
// snapshot includes the active route (LiteLLM proxy or direct OpenRouter,
// see internal/route), why it was chosen, the base URL and the credential
// source -- never a key.
//
// This package is a self-registering plugin (see internal/registry's
// package doc comment for the overall mechanism).
//
// # Never a Go error: reachability problems ARE the useful result
//
// Unlike every other tool in this codebase, DoctorHandler.Handle
// essentially never returns a Go error (which the MCP SDK would surface
// as IsError=true, hiding any structured content): a network failure, a
// refused call (session budget exhausted), or an unreachable endpoint are
// exactly the situations jev_doctor exists to diagnose, so they are
// reported as a normal, successful tool result with Reachable=false and a
// human-readable Error string -- not as a tool failure. The only
// exception is if building the underlying HTTP request itself panics or
// similarly can't be represented at all, which is not expected in
// practice.
//
// Reachable reflects whether client.Ask completed successfully (a 2xx
// HTTP response whose envelope parsed) -- NOT whether the "noul" probe
// answer itself was well-formed; this tool doesn't otherwise care about
// the probe's semantic content; see internal/openrouter's package doc
// comment for the underlying wire format this relies on.
//
// # What is and isn't independently verified
//
// The "noul" question/answer shape is a verified-live wire fact from
// 2026-09-26 (see internal/openrouter's package doc comment) -- not
// independently re-verified here (no OPENROUTER_API_KEY was available in
// this implementation environment). Usage/BudgetExceeded on
// DoctorOutput are additions beyond the project brief's minimal literal
// field list ({model, reachable, latency_ms, error}), following the same
// "document additions beyond the brief" precedent as
// internal/tools/score.ScoreOutput.BudgetExceeded, since a real,
// billable SystemOne call IS made here and every other tool in this
// codebase surfaces that same accounting.
package doctor

import (
	"context"
	"fmt"
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

// ToolNameDoctor is the MCP tool name registered for DoctorHandler.
const ToolNameDoctor = "jev_doctor"

// Usage mirrors the token accounting reported by OpenRouter for a call.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	// CostUSD is the real cost of the call in USD as reported by the API;
	// omitted when the API returned no cost.
	CostUSD *float64 `json:"cost_usd,omitempty"`
}

// DoctorInput is the jev_doctor tool's input schema. Every field is
// optional -- an empty DoctorInput{} is a complete, valid call.
type DoctorInput struct {
	ProbeModel string `json:"probe_model,omitempty" jsonschema:"Optional model slug to probe instead of the configured default_model/tool_model_overrides, e.g. \"anthropic/claude-3.7-sonnet\". Omit to probe the currently configured model."`
}

// ConfigSnapshot reports whatever configuration is currently active, so
// jev_doctor doubles as a configuration-sanity check.
type ConfigSnapshot struct {
	// ResolvedModel is config.Config.DefaultModel: the model every OTHER
	// tool falls back to absent its own tool_model_overrides entry (see
	// config.Config.ModelForTool). This is deliberately the single
	// headline "what model is this server using" figure, rather than the
	// full tool_model_overrides map, which this tool does not surface.
	ResolvedModel string `json:"resolved_model"`
	// Route is "proxy" (local LiteLLM proxy) or "direct" (OpenRouter);
	// RouteWhy is the probe result that chose it (or the failover cause);
	// BaseURL is the API base calls go to. CredentialSource names where
	// the bearer credential came from (env PYCKLLM_API_KEY, config
	// proxy_api_key, env OPENROUTER_API_KEY, opencode auth store at
	// <path>), never the key itself. All four describe the route active at
	// the time of the call, so a mid-session proxy failover shows up here.
	Route                  string  `json:"route"`
	RouteWhy               string  `json:"route_why"`
	BaseURL                string  `json:"base_url"`
	CredentialSource       string  `json:"credential_source"`
	BudgetMaxUSDPerCall    float64 `json:"budget_max_usd_per_call"`
	BudgetMaxUSDPerSession float64 `json:"budget_max_usd_per_session"`
	SessionSpendUSD        float64 `json:"session_spend_usd"`
}

// DoctorOutput is the jev_doctor tool's output schema.
type DoctorOutput struct {
	Model          string         `json:"model"`
	Reachable      bool           `json:"reachable"`
	LatencyMs      int64          `json:"latency_ms"`
	Error          *string        `json:"error"`
	Config         ConfigSnapshot `json:"config"`
	Usage          *Usage         `json:"usage,omitempty"`
	BudgetExceeded bool           `json:"budget_exceeded,omitempty"`
}

// DoctorHandler implements the jev_doctor tool.
type DoctorHandler struct {
	client   *openrouter.Client
	model    string
	timeout  time.Duration
	budget   *budget.Tracker
	cfg      config.Config
	auditLog *audit.Logger
}

// NewDoctorHandler builds a DoctorHandler from application dependencies.
func NewDoctorHandler(client *openrouter.Client, cfg config.Config, tracker *budget.Tracker, auditLog *audit.Logger) *DoctorHandler {
	return &DoctorHandler{
		client:   client,
		model:    cfg.ModelForTool(ToolNameDoctor),
		timeout:  time.Duration(cfg.RequestTimeoutMs) * time.Millisecond,
		budget:   tracker,
		cfg:      cfg,
		auditLog: auditLog,
	}
}

func init() {
	description := "Check connectivity to OpenRouter's SystemOne API with a minimal, cheap probe call, " +
		"and report the active configuration (resolved model, credential source, budget caps, session " +
		"spend) plus which route is active (proxy|direct), why, the base URL and credential source (never a key). Run this first when another tool call fails, to rule out a connectivity/config " +
		"problem before assuming the tool itself is broken. Never fails as a Go error: an unreachable " +
		"endpoint is reported as reachable=false with a human-readable error, since that IS the " +
		"useful result. Example (every field optional): {} or {\"probe_model\": \"some/other-model\"}. " +
		"Returns model, reachable (bool), latency_ms, error (or null), and the config snapshot."
	registry.Register(registry.Tool{
		Name:        "doctor",
		MCPName:     ToolNameDoctor,
		Description: description,
		RegisterMCP: func(server *mcp.Server, deps *registry.Deps) {
			h := NewDoctorHandler(deps.Client, deps.Config, deps.Budget, deps.Audit)
			mcp.AddTool(server, &mcp.Tool{
				Name:        ToolNameDoctor,
				Description: description,
			}, h.Handle)
		},
		RegisterCLI: func(root *cobra.Command, provider registry.DepsProvider) {
			root.AddCommand(newCLICommand(provider, description))
		},
	})
}

// Handle implements mcp.ToolHandlerFor[DoctorInput, DoctorOutput]: a
// thin adapter over run (the transport-agnostic core both the MCP
// handler and the CLI subcommand share). See package doc comment for
// why run practically never returns a Go error.
func (h *DoctorHandler) Handle(ctx context.Context, _ *mcp.CallToolRequest, in DoctorInput) (*mcp.CallToolResult, DoctorOutput, error) {
	out, err := h.run(ctx, in)
	if err != nil {
		return nil, DoctorOutput{}, err
	}
	return nil, out, nil
}

// run is jev_doctor's transport-agnostic core.
func (h *DoctorHandler) run(ctx context.Context, in DoctorInput) (DoctorOutput, error) {
	start := time.Now()

	model := in.ProbeModel
	if model == "" {
		model = h.model
	}

	ri := h.client.RouteInfo()
	snapshot := ConfigSnapshot{
		ResolvedModel:          h.cfg.DefaultModel,
		Route:                  ri.Route,
		RouteWhy:               ri.Why,
		BaseURL:                ri.BaseURL,
		CredentialSource:       ri.CredentialSource,
		BudgetMaxUSDPerCall:    h.cfg.Budget.MaxUSDPerCall,
		BudgetMaxUSDPerSession: h.cfg.Budget.MaxUSDPerSession,
		SessionSpendUSD:        h.budget.Total(),
	}

	inputHash := audit.HashValue(in)

	if h.budget.SessionBudgetExceeded() {
		errMsg := fmt.Sprintf("refusing probe: session budget exhausted (spent $%.6f, cap $%.6f)", h.budget.Total(), h.cfg.Budget.MaxUSDPerSession)
		out := DoctorOutput{Model: model, Reachable: false, LatencyMs: time.Since(start).Milliseconds(), Error: &errMsg, Config: snapshot}
		h.auditLog.Log(audit.Entry{
			Tool: ToolNameDoctor, Model: model, InputStateSHA256: inputHash,
			Status: "error", LatencyMs: out.LatencyMs, Error: errMsg,
		})
		return out, nil
	}

	resp, callErr := h.client.Ask(ctx, model, map[string]openrouter.Question{
		"ping": {
			Type:         "noul",
			Instructions: "Is the following state a non-empty string?",
			Criteria:     map[string]string{"true": "non-empty", "false": "empty"},
		},
	}, "ping", h.timeout)
	latencyMs := time.Since(start).Milliseconds()

	if callErr != nil {
		errMsg := callErr.Error()
		out := DoctorOutput{Model: model, Reachable: false, LatencyMs: latencyMs, Error: &errMsg, Config: snapshot}
		h.auditLog.Log(audit.Entry{
			Tool: ToolNameDoctor, Model: model, InputStateSHA256: inputHash,
			Status: "error", LatencyMs: latencyMs, Error: errMsg,
		})
		return out, nil
	}

	out := DoctorOutput{Model: model, Reachable: true, LatencyMs: latencyMs, Error: nil, Config: snapshot}
	if resp.Usage != nil {
		out.Usage = &Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens, CostUSD: resp.Usage.CostUSD()}
	}

	var costUSD *float64
	if resp.Usage != nil {
		cost := resp.Usage.Cost
		h.budget.Add(cost)
		costUSD = &cost
		out.Config.SessionSpendUSD = h.budget.Total()
		if h.cfg.Budget.MaxUSDPerCall > 0 && cost > h.cfg.Budget.MaxUSDPerCall {
			out.BudgetExceeded = true
		}
	}

	// Reachable is about the round trip, not the probe answer's validity
	// (see package doc comment) -- but we still fail-closed-parse it so a
	// malformed probe answer isn't silently ignored in the audit log's
	// status.
	status := "invalid_response"
	if raw, present := resp.Answers["ping"]; present {
		if _, ok := answers.Noul(raw); ok {
			status = "ok"
		}
	}

	h.auditLog.Log(audit.Entry{
		Tool: ToolNameDoctor, Model: out.Model, InputStateSHA256: inputHash,
		Status: status, CostUSD: costUSD, LatencyMs: out.LatencyMs, BudgetExceeded: out.BudgetExceeded,
	})

	return out, nil
}
