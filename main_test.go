package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/pyck-ai/jev-mcp/internal/audit"
	"github.com/pyck-ai/jev-mcp/internal/budget"
	"github.com/pyck-ai/jev-mcp/internal/config"
	"github.com/pyck-ai/jev-mcp/internal/openrouter"
	"github.com/pyck-ai/jev-mcp/internal/registry"
	"github.com/pyck-ai/jev-mcp/internal/tools/score"
)

// TestEndToEnd_MCPWireProtocol drives the jev_score tool through a real MCP
// client/server session (in-memory transport, but still real JSON-RPC
// framing and JSON marshal/unmarshal on both ends -- see
// mcp.NewInMemoryTransports), against a fake OpenRouter SystemOne server.
//
// This is the one test in this codebase that exercises the go-sdk's own
// input/output JSON Schema inference and validation (mcp.AddTool), which
// the more targeted tests in internal/tools/score bypass by calling
// ScoreHandler.Handle directly. In particular it confirms:
//   - ScoreInput's required fields (state, scale_min, scale_max,
//     instructions) are correctly inferred as required from the struct's
//     json tags (no `omitempty`).
//   - ScoreOutput -- including the nullable *Usage pointer field -- passes
//     the SDK's own output-schema validation.
//   - A Go error returned from the handler surfaces as IsError=true on the
//     wire, not as a fabricated structured result.
//
// It also, incidentally, exercises the real registry/blank-import plugin
// wiring end to end (newServer below loops over registry.All() exactly as
// main() does): every tool package main.go blank-imports is registered
// against the server this test drives, not just jev_score, though only
// jev_score is actually called here.
func TestEndToEnd_MCPWireProtocol(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers: map[string]json.RawMessage{
				"score": json.RawMessage(`{"type":"score","score":1.99,"confidence":0.99,"probabilities":{"0":0,"1":0.01,"2":0.99}}`),
			},
			Model: "typesafe/jev-1.13-20260917",
			Usage: &openrouter.Usage{Cost: 0.000019992, InputTokens: 476, OutputTokens: 70},
		})
	}))
	defer srv.Close()

	cfg := config.Default()
	client := openrouter.NewClientWithEndpoint("test-key", srv.URL, openrouter.RetryPolicy{
		MaxAttempts: cfg.Retry.MaxAttempts, BaseBackoffMs: 1, MaxBackoffMs: 5,
	})
	auditLog := audit.NewLogger(t.TempDir() + "/audit.jsonl")
	deps := &registry.Deps{
		Client: client,
		Config: cfg,
		Budget: budget.NewTracker(cfg.Budget.MaxUSDPerSession),
		Audit:  auditLog,
	}

	server := newServer(deps)
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0.0.1"}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t1, t2 := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, t1, nil)
	if err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	defer serverSession.Close()

	clientSession, err := mcpClient.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	defer clientSession.Close()

	t.Run("valid call round-trips a structured, schema-valid result", func(t *testing.T) {
		res, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
			Name: score.ToolNameScore,
			Arguments: map[string]any{
				"state":        "some diff to judge",
				"scale_min":    0,
				"scale_max":    2,
				"instructions": "0=incorrect, 1=partially correct, 2=fully correct",
			},
		})
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if res.IsError {
			t.Fatalf("expected success, got IsError=true, content=%+v", res.Content)
		}

		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatalf("marshaling StructuredContent: %v", err)
		}
		var out score.ScoreOutput
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("unmarshaling StructuredContent into ScoreOutput: %v\nraw: %s", err, raw)
		}
		if out.Status != score.StatusOK {
			t.Errorf("status = %q, want %q", out.Status, score.StatusOK)
		}
		if out.Score != 1.99 {
			t.Errorf("score = %v, want 1.99", out.Score)
		}
		if out.Usage == nil || out.Usage.InputTokens != 476 {
			t.Errorf("usage = %+v, want input_tokens=476", out.Usage)
		}
	})

	t.Run("missing required field is rejected by input schema validation before the handler runs", func(t *testing.T) {
		res, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
			Name: score.ToolNameScore,
			Arguments: map[string]any{
				"scale_min":    0,
				"scale_max":    2,
				"instructions": "missing state field entirely",
			},
		})
		// Schema-validation failures surface as a tool error result
		// (IsError=true), not a transport-level Go error, consistent with
		// how the SDK reports other handler-level failures.
		if err != nil {
			t.Fatalf("CallTool transport error: %v", err)
		}
		if !res.IsError {
			t.Fatalf("expected IsError=true for missing required field 'state'")
		}
	})

	t.Run("scale_max <= scale_min surfaces as a tool error, not a fabricated score", func(t *testing.T) {
		res, err := clientSession.CallTool(ctx, &mcp.CallToolParams{
			Name: score.ToolNameScore,
			Arguments: map[string]any{
				"state":        "x",
				"scale_min":    5,
				"scale_max":    2,
				"instructions": "y",
			},
		})
		if err != nil {
			t.Fatalf("CallTool transport error: %v", err)
		}
		if !res.IsError {
			t.Fatalf("expected IsError=true for scale_max <= scale_min")
		}
		if res.StructuredContent != nil {
			t.Errorf("expected no structured content alongside a tool error, got %#v", res.StructuredContent)
		}
	})
}

// TestNewServer_RegistersEveryToolExactlyOnce is a regression guard for
// the self-registering plugin architecture itself (see internal/registry's
// package doc comment): it lists every tool newServer actually registered
// against a real MCP server/client session and confirms it's exactly the
// expected 14-tool roster (jev_score plus the 13 tools added on top of the
// plugin architecture), with no duplicate names. mcp.Server.AddTool's own
// documented behavior for a name collision is to silently REPLACE the
// earlier tool, not error or panic -- so this test, not just a successful
// build, is what actually catches two tool packages accidentally
// registering the same name.
func TestNewServer_RegistersEveryToolExactlyOnce(t *testing.T) {
	deps := &registry.Deps{
		Client: openrouter.NewClientWithEndpoint("test-key", "http://127.0.0.1:0", openrouter.RetryPolicy{MaxAttempts: 1}),
		Config: config.Default(),
		Budget: budget.NewTracker(0),
		Audit:  audit.NewLogger(t.TempDir() + "/audit.jsonl"),
	}
	server := newServer(deps)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t1, t2 := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, t1, nil)
	if err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	defer serverSession.Close()
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0.0.1"}, nil)
	clientSession, err := mcpClient.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	defer clientSession.Close()

	res, err := clientSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	want := []string{
		"jev_ask", "jev_check", "jev_classify", "jev_compare", "jev_decide",
		"jev_doctor", "jev_extract", "jev_gate", "jev_match", "jev_rerank",
		"jev_review", "jev_score", "jev_screen", "jev_verify",
	}

	seen := make(map[string]int, len(res.Tools))
	for _, tool := range res.Tools {
		seen[tool.Name]++
	}
	if len(seen) != len(res.Tools) {
		t.Errorf("server reported %d tools but only %d distinct names -- a name collision silently replaced a tool: %v", len(res.Tools), len(seen), seen)
	}
	for _, name := range want {
		if seen[name] != 1 {
			t.Errorf("expected exactly 1 tool named %q, got %d", name, seen[name])
		}
	}
	if len(res.Tools) != len(want) {
		gotNames := make([]string, len(res.Tools))
		for i, tool := range res.Tools {
			gotNames[i] = tool.Name
		}
		t.Errorf("len(Tools) = %d, want %d\ngot:  %v\nwant: %v", len(res.Tools), len(want), gotNames, want)
	}
}
