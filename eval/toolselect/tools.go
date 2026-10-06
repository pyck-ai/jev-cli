package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/pyck-ai/jev-cli/internal/registry"
	// Blank imports register every tool, exactly like cmd/jev/main.go.
	_ "github.com/pyck-ai/jev-cli/internal/tools/ask"
	_ "github.com/pyck-ai/jev-cli/internal/tools/batch"
	_ "github.com/pyck-ai/jev-cli/internal/tools/check"
	_ "github.com/pyck-ai/jev-cli/internal/tools/classify"
	_ "github.com/pyck-ai/jev-cli/internal/tools/compare"
	_ "github.com/pyck-ai/jev-cli/internal/tools/decide"
	_ "github.com/pyck-ai/jev-cli/internal/tools/doctor"
	_ "github.com/pyck-ai/jev-cli/internal/tools/extract"
	_ "github.com/pyck-ai/jev-cli/internal/tools/gate"
	_ "github.com/pyck-ai/jev-cli/internal/tools/match"
	_ "github.com/pyck-ai/jev-cli/internal/tools/rerank"
	_ "github.com/pyck-ai/jev-cli/internal/tools/review"
	_ "github.com/pyck-ai/jev-cli/internal/tools/score"
	_ "github.com/pyck-ai/jev-cli/internal/tools/screen"
	_ "github.com/pyck-ai/jev-cli/internal/tools/verify"
)

// mcpToolPrefix is the prefix every jev tool already carries in its MCP name
// ("jev_score"). Bare names ("score") are what cases.json uses.
const mcpToolPrefix = "jev_"

// Function is one OpenAI-style function tool, as sent in the request.
type Function struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// bareName strips an agent-side prefix and the MCP "jev_" prefix:
// "jev_jev_decide" (prefix "jev_") -> "decide".
func bareName(name, prefix string) string {
	name = strings.TrimPrefix(name, prefix)
	return strings.TrimPrefix(name, mcpToolPrefix)
}

// loadTools registers every tool from internal/registry on an in-process MCP
// server, connects an in-memory client and returns the tools/list result as
// OpenAI-style functions. Descriptions are therefore always the current code.
// Handlers are never invoked, so zero-value deps are enough at registration.
func loadTools(ctx context.Context, prefix string) ([]Function, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	server := mcp.NewServer(&mcp.Implementation{Name: "jev-cli", Version: "eval"}, nil)
	deps := &registry.Deps{}
	for _, t := range registry.All() {
		t.RegisterMCP(server, deps)
	}

	t1, t2 := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, t1, nil)
	if err != nil {
		return nil, fmt.Errorf("server connect: %w", err)
	}
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "toolselect-eval", Version: "v0"}, nil)
	cs, err := client.Connect(ctx, t2, nil)
	if err != nil {
		return nil, fmt.Errorf("client connect: %w", err)
	}
	defer cs.Close()

	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	return toFunctions(res.Tools, prefix)
}

// toFunctions converts MCP tools to OpenAI functions: name = prefix+name,
// parameters = inputSchema (round-tripped through JSON into a plain map).
func toFunctions(tools []*mcp.Tool, prefix string) ([]Function, error) {
	out := make([]Function, 0, len(tools))
	for _, t := range tools {
		raw, err := json.Marshal(t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("tool %s: marshal inputSchema: %w", t.Name, err)
		}
		params := map[string]any{}
		if string(raw) != "null" {
			if err := json.Unmarshal(raw, &params); err != nil {
				return nil, fmt.Errorf("tool %s: inputSchema is not an object: %w", t.Name, err)
			}
		}
		if _, ok := params["type"]; !ok {
			params["type"] = "object"
		}
		out = append(out, Function{Name: prefix + t.Name, Description: t.Description, Parameters: params})
	}
	return out, nil
}
