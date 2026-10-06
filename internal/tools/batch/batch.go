// Package batch implements jev_batch: run several independent jev tool calls
// in one call. Each item names a registered tool and carries that tool's own
// input object; the item is dispatched to the tool's run core through
// registry.Tool.Run (looked up at run time, so this package imports no tool
// package and no tool imports it), fanned out concurrently behind a channel
// semaphore. Nothing is merged on the wire: one item is exactly one direct
// call, with the same validation, audit line, budget accounting and
// recording call_id.
//
// Items fail independently: an item's error (invalid input, session-budget
// refusal, preflight, HTTP) becomes that item's status "error" and never
// touches the others. The batch itself returns an error (CLI exit 3) only
// when it is malformed: no items, more than MaxItems, a bad max_concurrency,
// an unknown tool, a nested batch, or doctor. Results come back in input
// order; summary.exit_code is the maximum of the item exit codes. The
// session budget is re-checked before every launch. Audit keeps each item's
// own line plus one extra "jev_batch" summary line.
package batch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/pyck-ai/jev-cli/internal/audit"
	"github.com/pyck-ai/jev-cli/internal/registry"
)

// ToolNameBatch is the MCP tool name for jev_batch.
const ToolNameBatch = "jev_batch"

const (
	// MaxItems is the largest number of items one batch may carry.
	MaxItems = 32
	// DefaultConcurrency is how many items run at once when
	// max_concurrency is not given.
	DefaultConcurrency = 4
	// MaxConcurrency is the largest accepted max_concurrency.
	MaxConcurrency = 8
)

// Item statuses.
const (
	StatusOK    = "ok"
	StatusError = "error"
)

// Item is one tool call inside a batch.
type Item struct {
	ID    string         `json:"id,omitempty" jsonschema:"Optional label echoed back in results[].id."`
	Tool  string         `json:"tool" jsonschema:"Tool name without the jev_ prefix, e.g. decide or check. Not batch or doctor."`
	Input map[string]any `json:"input" jsonschema:"That tool's own input object, exactly as in a direct call."`
}

// BatchInput is the jev_batch tool's input schema.
type BatchInput struct {
	Items          []Item `json:"items" jsonschema:"1-32 independent calls, run concurrently; results keep this order."`
	MaxConcurrency int    `json:"max_concurrency,omitempty" jsonschema:"Optional, 1-8 (default 4)."`
}

// ItemResult is the outcome of one item. Output is the tool's own output
// value, so its JSON is byte-identical to a direct call's.
type ItemResult struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Tool     string `json:"tool"`
	Status   string `json:"status"`
	ExitCode int    `json:"exit_code"`
	Output   any    `json:"output,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Summary aggregates a batch. ExitCode is the maximum item exit code.
type Summary struct {
	Items          int      `json:"items"`
	OK             int      `json:"ok"`
	Error          int      `json:"error"`
	ExitCode       int      `json:"exit_code"`
	CostUSD        *float64 `json:"cost_usd,omitempty"`
	LatencyMs      int64    `json:"latency_ms"`
	BudgetExceeded bool     `json:"budget_exceeded,omitempty"`
}

// BatchOutput is the jev_batch tool's output schema.
type BatchOutput struct {
	Results []ItemResult `json:"results"`
	Summary Summary      `json:"summary"`
}

// BatchHandler implements the jev_batch tool.
type BatchHandler struct {
	deps   *registry.Deps
	lookup func(name string) (registry.Tool, bool)
}

// NewBatchHandler builds a handler that dispatches items through the
// registry using deps.
func NewBatchHandler(deps *registry.Deps) *BatchHandler {
	return &BatchHandler{deps: deps, lookup: registry.Lookup}
}

func init() {
	description := "Run several independent jev tool calls in one call, e.g. several decisions at once (one " +
		"jev_decide input per decision, such as a confidence for every option of several questions) or a " +
		"decision plus jev_check sanity checks. Items run concurrently " +
		"and fail independently; each result is exactly what that tool returns alone. Use the single tool " +
		"when there is only one call."
	registry.Register(registry.Tool{
		Name:        "batch",
		MCPName:     ToolNameBatch,
		Description: description,
		RegisterMCP: func(server *mcp.Server, deps *registry.Deps) {
			h := NewBatchHandler(deps)
			mcp.AddTool(server, &mcp.Tool{
				Name:        ToolNameBatch,
				Description: description,
			}, h.Handle)
		},
		RegisterCLI: func(root *cobra.Command, provider registry.DepsProvider) {
			root.AddCommand(newCLICommand(provider, description))
		},
	})
}

// Handle implements mcp.ToolHandlerFor[BatchInput, BatchOutput]: a thin
// adapter over run.
func (h *BatchHandler) Handle(ctx context.Context, _ *mcp.CallToolRequest, in BatchInput) (*mcp.CallToolResult, BatchOutput, error) {
	out, err := h.run(ctx, in)
	if err != nil {
		return nil, BatchOutput{}, err
	}
	return nil, out, nil
}

// normalizeTool strips an optional jev_ prefix.
func normalizeTool(name string) string {
	return strings.TrimPrefix(strings.TrimSpace(name), "jev_")
}

// validate checks the batch is well formed and resolves every item's tool.
// It returns the resolved tools (nil Run is allowed here: that is a
// per-item error, not a batch error).
func (h *BatchHandler) validate(in BatchInput) ([]registry.Tool, error) {
	if len(in.Items) == 0 {
		return nil, fmt.Errorf("jev_batch: items must contain at least 1 item")
	}
	if len(in.Items) > MaxItems {
		return nil, fmt.Errorf("jev_batch: items has %d entries, max is %d", len(in.Items), MaxItems)
	}
	if in.MaxConcurrency < 0 || in.MaxConcurrency > MaxConcurrency {
		return nil, fmt.Errorf("jev_batch: max_concurrency must be 1-%d, got %d", MaxConcurrency, in.MaxConcurrency)
	}
	tools := make([]registry.Tool, len(in.Items))
	for i, it := range in.Items {
		name := normalizeTool(it.Tool)
		switch name {
		case "":
			return nil, fmt.Errorf("jev_batch: items[%d].tool is required", i)
		case "batch":
			return nil, fmt.Errorf("jev_batch: items[%d].tool: batches cannot be nested", i)
		case "doctor":
			return nil, fmt.Errorf("jev_batch: items[%d].tool: doctor is not a judgment and cannot be batched", i)
		}
		t, ok := h.lookup(name)
		if !ok {
			return nil, fmt.Errorf("jev_batch: items[%d].tool: unknown tool %q", i, it.Tool)
		}
		tools[i] = t
	}
	return tools, nil
}

// run is jev_batch's transport-agnostic core.
func (h *BatchHandler) run(ctx context.Context, in BatchInput) (BatchOutput, error) {
	start := time.Now()
	tools, err := h.validate(in)
	if err != nil {
		return BatchOutput{}, err
	}
	conc := in.MaxConcurrency
	if conc == 0 {
		conc = DefaultConcurrency
	}

	results := make([]ItemResult, len(in.Items))
	costs := make([]float64, len(in.Items))
	exceeded := make([]bool, len(in.Items))
	var wg sync.WaitGroup
	sem := make(chan struct{}, conc)

	for i := range in.Items {
		sem <- struct{}{} // blocks until a slot is free, so launches happen in input order
		results[i] = ItemResult{Index: i, ID: in.Items[i].ID, Tool: tools[i].Name}
		if h.deps.Budget.SessionBudgetExceeded() {
			<-sem
			fail(&results[i], fmt.Errorf("jev_batch: refusing item: session budget exhausted (spent $%.6f, cap $%.6f)",
				h.deps.Budget.Total(), h.deps.Config.Budget.MaxUSDPerSession))
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			costs[i], exceeded[i] = h.runItem(ctx, tools[i], in.Items[i], &results[i])
		}()
	}
	wg.Wait()

	sum := Summary{Items: len(results)}
	var cost float64
	haveCost := false
	for i, r := range results {
		if r.Status == StatusOK {
			sum.OK++
		} else {
			sum.Error++
		}
		sum.ExitCode = max(sum.ExitCode, r.ExitCode)
		if costs[i] > 0 {
			cost += costs[i]
			haveCost = true
		}
		sum.BudgetExceeded = sum.BudgetExceeded || exceeded[i]
	}
	if haveCost {
		sum.CostUSD = &cost
	}
	sum.LatencyMs = time.Since(start).Milliseconds()

	entry := audit.Entry{
		Tool: ToolNameBatch, InputStateSHA256: audit.HashValue(in), Status: StatusOK,
		CostUSD: sum.CostUSD, LatencyMs: sum.LatencyMs, BudgetExceeded: sum.BudgetExceeded,
		ItemCount: sum.Items, InvalidCount: sum.Error,
	}
	if sum.Error > 0 {
		entry.Status = StatusError
		entry.Error = fmt.Sprintf("%d of %d items failed", sum.Error, sum.Items)
	}
	h.deps.Audit.Log(entry)

	return BatchOutput{Results: results, Summary: sum}, nil
}

// fail marks r as an errored item.
func fail(r *ItemResult, err error) {
	r.Status = StatusError
	r.ExitCode = 3
	r.Error = err.Error()
}

// runItem runs one item into r and returns its cost and budget_exceeded
// flag (read generically from the marshaled output's usage.cost_usd and
// budget_exceeded keys, which every tool's output exposes).
func (h *BatchHandler) runItem(ctx context.Context, t registry.Tool, it Item, r *ItemResult) (cost float64, exceeded bool) {
	defer func() {
		if p := recover(); p != nil {
			*r = ItemResult{Index: r.Index, ID: r.ID, Tool: r.Tool}
			fail(r, fmt.Errorf("jev_batch: item panicked: %v", p))
		}
	}()
	if t.Run == nil {
		fail(r, fmt.Errorf("tool %q does not support batch yet", t.Name))
		return 0, false
	}
	input := it.Input
	if input == nil {
		input = map[string]any{}
	}
	raw, err := json.Marshal(input)
	if err != nil {
		fail(r, fmt.Errorf("marshalling input: %w", err))
		return 0, false
	}
	out, code, err := t.Run(ctx, h.deps, raw)
	if err != nil {
		fail(r, err)
		return 0, false
	}
	r.Status, r.ExitCode, r.Output = StatusOK, code, out

	if enc, err := json.Marshal(out); err == nil {
		var meta struct {
			Usage *struct {
				CostUSD *float64 `json:"cost_usd"`
			} `json:"usage"`
			BudgetExceeded bool `json:"budget_exceeded"`
		}
		if json.Unmarshal(enc, &meta) == nil {
			if meta.Usage != nil && meta.Usage.CostUSD != nil {
				cost = *meta.Usage.CostUSD
			}
			exceeded = meta.BudgetExceeded
		}
	}
	return cost, exceeded
}
