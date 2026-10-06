package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pyck-ai/jev-cli/internal/registry"
)

func TestVerdictFor(t *testing.T) {
	c := Case{Ideal: "decide", Acceptable: []string{"ask"}}
	strict := Case{Ideal: "decide", Acceptable: []string{"check"}}
	control := Case{Ideal: "none"}
	tests := []struct {
		name   string
		c      Case
		chosen string
		want   string
	}{
		{"ideal", c, "decide", VerdictIdeal},
		{"acceptable", c, "ask", VerdictAcceptable},
		{"escape", strict, "ask", VerdictEscape},
		{"none", c, "none", VerdictNone},
		{"empty is none", c, "", VerdictNone},
		{"wrong", c, "score", VerdictWrong},
		{"unknown name is wrong", c, "frobnicate", VerdictWrong},
		{"control ideal none", control, "none", VerdictIdeal},
		{"control wrong", control, "ask", VerdictEscape},
	}
	for _, tt := range tests {
		if got := verdictFor(tt.c, tt.chosen); got != tt.want {
			t.Errorf("%s: verdictFor(%q) = %s, want %s", tt.name, tt.chosen, got, tt.want)
		}
	}
}

func TestValidateArgs(t *testing.T) {
	schema := map[string]any{
		"type":     "object",
		"required": []any{"state", "scale_max"},
		"properties": map[string]any{
			"state":     map[string]any{"type": "string"},
			"scale_max": map[string]any{"type": "integer"},
			"tags":      map[string]any{"type": []any{"array", "null"}},
		},
	}
	tests := []struct {
		name string
		args map[string]any
		want int
	}{
		{"valid", map[string]any{"state": "x", "scale_max": 3.0}, 0},
		{"missing required", map[string]any{"state": "x"}, 1},
		{"wrong type", map[string]any{"state": 5.0, "scale_max": 3.0}, 1},
		{"non integer", map[string]any{"state": "x", "scale_max": 2.5}, 1},
		{"union type ok", map[string]any{"state": "x", "scale_max": 1.0, "tags": nil}, 0},
		{"union type bad", map[string]any{"state": "x", "scale_max": 1.0, "tags": "a"}, 1},
		{"unknown key ignored", map[string]any{"state": "x", "scale_max": 1.0, "extra": true}, 0},
		{"nil args", nil, 1},
	}
	for _, tt := range tests {
		if got := validateArgs(schema, tt.args); len(got) != tt.want {
			t.Errorf("%s: got %v, want %d problems", tt.name, got, tt.want)
		}
	}
}

func TestBareName(t *testing.T) {
	for in, want := range map[string]string{"jev_jev_decide": "decide", "jev_decide": "decide", "decide": "decide"} {
		if got := bareName(in, "jev_"); got != want {
			t.Errorf("bareName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := bareName("srv_jev_ask", "srv_"); got != "ask" {
		t.Errorf("custom prefix: got %q", got)
	}
}

func TestLoadToolsEveryRegisteredToolAppears(t *testing.T) {
	fns, err := loadTools(context.Background(), "jev_")
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.All()
	if len(reg) == 0 {
		t.Fatal("registry is empty: tool packages not imported")
	}
	got := map[string]Function{}
	for _, f := range fns {
		got[f.Name] = f
	}
	if len(got) != len(fns) {
		t.Errorf("duplicate function names in %d functions", len(fns))
	}
	for _, rt := range reg {
		f, ok := got["jev_"+rt.MCPName]
		if !ok {
			t.Errorf("registered tool %s missing from tools/list (have %d functions)", rt.MCPName, len(fns))
			continue
		}
		if f.Description != rt.Description {
			t.Errorf("%s: description is not the live registry description", rt.MCPName)
		}
		if f.Parameters["type"] != "object" {
			t.Errorf("%s: parameters.type = %v, want object", rt.MCPName, f.Parameters["type"])
		}
	}
	if len(fns) != len(reg) {
		t.Errorf("got %d functions for %d registered tools", len(fns), len(reg))
	}
	// A custom prefix is applied verbatim.
	fns2, err := loadTools(context.Background(), "x_")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(fns2[0].Name, "x_jev_") {
		t.Errorf("custom prefix not applied: %s", fns2[0].Name)
	}
}

func TestParseResponse(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		tool     string
		wantArgs string // key expected in args, "" for none
		wantErr  bool
		cost     float64
	}{
		{
			name:     "string arguments",
			body:     `{"choices":[{"finish_reason":"tool_calls","message":{"content":null,"tool_calls":[{"function":{"name":"jev_jev_decide","arguments":"{\"decision\":\"x\"}"}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"cost":0.0012}}`,
			tool:     "decide",
			wantArgs: "decision",
			cost:     0.0012,
		},
		{
			name:     "object arguments and extra reasoning fields",
			body:     `{"choices":[{"finish_reason":"tool_calls","message":{"content":null,"reasoning":"hm","tool_calls":[{"id":"a","function":{"name":"jev_jev_score","arguments":{"state":"s"}}}]}}]}`,
			tool:     "score",
			wantArgs: "state",
		},
		{
			name: "first tool call wins",
			body: `{"choices":[{"message":{"tool_calls":[{"function":{"name":"jev_jev_check","arguments":"{}"}},{"function":{"name":"jev_jev_verify","arguments":"{}"}}]}}]}`,
			tool: "check",
		},
		{
			name: "no tool call",
			body: `{"choices":[{"finish_reason":"stop","message":{"content":"I would pick A."}}],"usage":{"prompt_tokens":3}}`,
			tool: "none",
		},
		{
			name: "truncated by max_tokens",
			body: `{"choices":[{"finish_reason":"length","message":{"content":null,"reasoning":"long..."}}]}`,
			tool: "none",
		},
		{
			name: "malformed arguments keep the tool",
			body: `{"choices":[{"message":{"tool_calls":[{"function":{"name":"jev_jev_ask","arguments":"{not json"}}]}}]}`,
			tool: "ask",
		},
		{name: "api error", body: `{"error":{"message":"bad key"}}`, wantErr: true},
		{name: "no choices", body: `{"choices":[]}`, wantErr: true},
		{name: "not json", body: `<html>`, wantErr: true},
	}
	for _, tt := range tests {
		obs, err := parseResponse([]byte(tt.body), "jev_")
		if (err != nil) != tt.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", tt.name, err, tt.wantErr)
			continue
		}
		if err != nil {
			continue
		}
		if obs.Tool != tt.tool {
			t.Errorf("%s: tool = %q, want %q", tt.name, obs.Tool, tt.tool)
		}
		if tt.wantArgs != "" {
			if _, ok := obs.Args[tt.wantArgs]; !ok {
				t.Errorf("%s: args %v missing %q", tt.name, obs.Args, tt.wantArgs)
			}
		}
		if tt.cost != 0 && obs.Cost != tt.cost {
			t.Errorf("%s: cost = %v, want %v", tt.name, obs.Cost, tt.cost)
		}
	}
}

func TestEndpointRoute(t *testing.T) {
	e := Endpoint{BaseURL: "http://p:1/"}
	cases := map[string]string{
		"~anthropic/claude-haiku-latest": "http://p:1/v1/chat/completions",
		"anthropic/claude-opus-4":        "http://p:1/v1/chat/completions",
		"z-ai/glm-5.3-flash":             "http://p:1/openrouter/api/v1/chat/completions",
		"openai/gpt-5-mini":              "http://p:1/openrouter/api/v1/chat/completions",
	}
	for m, want := range cases {
		if got := e.URL(m); got != want {
			t.Errorf("URL(%s) = %s, want %s", m, got, want)
		}
	}
	d := Endpoint{BaseURL: "https://openrouter.ai/api/v1", Direct: true}
	if got := d.URL("anthropic/x"); got != "https://openrouter.ai/api/v1/chat/completions" {
		t.Errorf("direct URL = %s", got)
	}
}

func TestCallerRetryAndTemperatureFallback(t *testing.T) {
	var hits int
	var sawTemp []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, hasTemp := body["temperature"]
		sawTemp = append(sawTemp, hasTemp)
		switch {
		case hasTemp:
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"message":"Unsupported value: temperature"}}`))
		default:
			_, _ = w.Write([]byte(`{"choices":[{"message":{"tool_calls":[{"function":{"name":"jev_jev_check","arguments":"{}"}}]}}]}`))
		}
	}))
	defer srv.Close()
	c := &Caller{HTTP: srv.Client(), Endpoint: Endpoint{BaseURL: srv.URL, APIKey: "k", Direct: true}, Prefix: "jev_", MaxTokens: 64, Retries: 2}
	obs, err := c.Call(context.Background(), "m", "sys", "p", []Function{{Name: "jev_jev_check", Parameters: map[string]any{"type": "object"}}})
	if err != nil {
		t.Fatal(err)
	}
	if obs.Tool != "check" || hits != 2 || !sawTemp[0] || sawTemp[1] {
		t.Errorf("tool=%s hits=%d sawTemp=%v", obs.Tool, hits, sawTemp)
	}
}

func TestSummarize(t *testing.T) {
	yes, no := true, false
	trials := []Trial{
		{CaseID: "a", Model: "m", Chosen: "decide", Ideal: "decide", Verdict: VerdictIdeal, SchemaValid: &yes, LatencyMs: 100},
		{CaseID: "b", Model: "m", Chosen: "ask", Ideal: "decide", Verdict: VerdictAcceptable, SchemaValid: &no, LatencyMs: 200},
		{CaseID: "c", Model: "m", Chosen: "ask", Ideal: "check", Verdict: VerdictEscape, SchemaValid: &yes},
		{CaseID: "d", Model: "m", Chosen: "none", Ideal: "score", Verdict: VerdictNone},
		{CaseID: "e", Model: "m", Chosen: "none", Verdict: VerdictError, Error: "boom"},
	}
	rep := summarize(trials)
	if len(rep.Models) != 1 {
		t.Fatalf("models = %d", len(rep.Models))
	}
	s := rep.Models[0]
	if s.Trials != 4 || s.Errors != 1 {
		t.Errorf("trials=%d errors=%d", s.Trials, s.Errors)
	}
	check := func(name string, got, want float64) {
		if got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	check("ideal", s.IdealPct, 25)
	check("accept", s.AcceptPct, 50)
	check("escape", s.EscapePct, 25)
	check("none", s.NonePct, 25)
	check("schema", s.SchemaPct, 100*2.0/3.0)
	if len(rep.Confusion) != 2 { // escape + none
		t.Errorf("confusion = %+v", rep.Confusion)
	}
}

// TestShippedCasesAreValid guards the committed case set against the live
// tool list: known tools, unique ids, no tool names in prompts, and every
// tool covered at least twice as the ideal pick.
func TestShippedCasesAreValid(t *testing.T) {
	fns, err := loadTools(context.Background(), "jev_")
	if err != nil {
		t.Fatal(err)
	}
	var known []string
	for _, f := range fns {
		known = append(known, bareName(f.Name, "jev_"))
	}
	cf, err := loadCases("cases.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCases(cf, known); err != nil {
		t.Fatal(err)
	}
	count := map[string]int{}
	for _, c := range cf.Cases {
		count[c.Ideal]++
	}
	for _, k := range known {
		if count[k] < 2 {
			t.Errorf("tool %s is the ideal pick of only %d cases, want >= 2", k, count[k])
		}
	}
	if count["decide"] < 8 {
		t.Errorf("decide has %d cases, want >= 8", count["decide"])
	}
}

func TestEstimateUSD(t *testing.T) {
	fns := []Function{{Name: "a", Description: strings.Repeat("x", 4000)}}
	cases := []Case{{Prompt: "hello"}}
	if got := estimateUSD([]string{"~anthropic/claude-opus-latest"}, fns, "sys", cases, 1); got <= 0 {
		t.Errorf("estimate = %v", got)
	}
	a := estimateUSD([]string{"x/flash"}, fns, "sys", cases, 1)
	b := estimateUSD([]string{"x/flash"}, fns, "sys", cases, 2)
	if b < 1.99*a || b > 2.01*a {
		t.Errorf("reps should scale linearly: %v vs %v", a, b)
	}
}
