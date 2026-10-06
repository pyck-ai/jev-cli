package doctor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pyck-ai/jev-cli/internal/audit"
	"github.com/pyck-ai/jev-cli/internal/budget"
	"github.com/pyck-ai/jev-cli/internal/config"
	"github.com/pyck-ai/jev-cli/internal/openrouter"
)

func newTestHandler(t *testing.T, serverURL string, cfg config.Config) (*DoctorHandler, *budget.Tracker) {
	t.Helper()
	client := openrouter.NewClientWithEndpoint("test-key", serverURL, openrouter.RetryPolicy{
		MaxAttempts: cfg.Retry.MaxAttempts, BaseBackoffMs: 1, MaxBackoffMs: 5,
	})
	tracker := budget.NewTracker(cfg.Budget.MaxUSDPerSession)
	auditLog := audit.NewLogger(filepath.Join(t.TempDir(), "audit.jsonl"))
	return NewDoctorHandler(client, cfg, tracker, auditLog), tracker
}

func TestDoctorHandler_Handle_Reachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers: map[string]json.RawMessage{"ping": json.RawMessage(`{"type":"noul","noul":0.99}`)},
			Model:   "typesafe/jev-1.13-20260917",
			Usage:   &openrouter.Usage{Cost: 0.000001, InputTokens: 5, OutputTokens: 2},
		})
	}))
	defer srv.Close()

	cfg := config.Default()
	t.Setenv("OPENROUTER_API_KEY", "test-key-for-doctor")
	h, tracker := newTestHandler(t, srv.URL, cfg)
	_, out, err := h.Handle(context.Background(), nil, DoctorInput{})
	if err != nil {
		t.Fatalf("expected doctor to never return a Go error, got %v", err)
	}
	if !out.Reachable {
		t.Errorf("Reachable = false, want true; Error = %v", out.Error)
	}
	if out.Error != nil {
		t.Errorf("Error = %q, want nil", *out.Error)
	}
	if out.Model != cfg.DefaultModel {
		t.Errorf("Model = %q, want the resolved default %q", out.Model, cfg.DefaultModel)
	}
	if out.Config.ResolvedModel != cfg.DefaultModel {
		t.Errorf("Config.ResolvedModel = %q, want %q", out.Config.ResolvedModel, cfg.DefaultModel)
	}
	if out.Config.CredentialSource != "explicit" {
		t.Errorf("Config.CredentialSource = %q, want \"explicit\"", out.Config.CredentialSource)
	}
	if out.Config.BudgetMaxUSDPerCall != cfg.Budget.MaxUSDPerCall {
		t.Errorf("Config.BudgetMaxUSDPerCall = %v, want %v", out.Config.BudgetMaxUSDPerCall, cfg.Budget.MaxUSDPerCall)
	}
	if tracker.Total() != 0.000001 {
		t.Errorf("tracked spend = %v, want 0.000001", tracker.Total())
	}
	if out.Config.SessionSpendUSD != tracker.Total() {
		t.Errorf("Config.SessionSpendUSD = %v, want %v", out.Config.SessionSpendUSD, tracker.Total())
	}
}

func TestDoctorHandler_Handle_ProbeModelOverride(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req openrouter.Request
		json.NewDecoder(r.Body).Decode(&req)
		gotModel = req.Model
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers: map[string]json.RawMessage{"ping": json.RawMessage(`{"type":"noul","noul":0.9}`)},
			Model:   "custom/probe-model",
			Usage:   &openrouter.Usage{Cost: 0},
		})
	}))
	defer srv.Close()

	h, _ := newTestHandler(t, srv.URL, config.Default())
	_, out, err := h.Handle(context.Background(), nil, DoctorInput{ProbeModel: "custom/probe-model"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotModel != "custom/probe-model" {
		t.Errorf("model sent to SystemOne = %q, want the probe_model override", gotModel)
	}
	if out.Model != "custom/probe-model" {
		t.Errorf("Model = %q, want custom/probe-model", out.Model)
	}
}

func TestDoctorHandler_Handle_Unreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"code":401,"message":"Missing Authentication header"}}`))
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.Retry.MaxAttempts = 1
	h, _ := newTestHandler(t, srv.URL, cfg)
	_, out, err := h.Handle(context.Background(), nil, DoctorInput{})
	if err != nil {
		t.Fatalf("expected doctor to never return a Go error even when unreachable, got %v", err)
	}
	if out.Reachable {
		t.Error("Reachable = true, want false for a 401 response")
	}
	if out.Error == nil || *out.Error == "" {
		t.Error("expected a non-empty Error message when unreachable")
	}
}

func TestDoctorHandler_Handle_SessionBudgetExhaustedIsReportedNotErrored(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	cfg := config.Default()
	cfg.Budget.MaxUSDPerSession = 1.0
	h, tracker := newTestHandler(t, srv.URL, cfg)
	tracker.Add(1.0)

	_, out, err := h.Handle(context.Background(), nil, DoctorInput{})
	if err != nil {
		t.Fatalf("expected doctor to never return a Go error, got %v", err)
	}
	if called {
		t.Error("expected no API call once the session budget is exhausted")
	}
	if out.Reachable {
		t.Error("Reachable = true, want false when the probe was refused")
	}
	if out.Error == nil {
		t.Error("expected a non-nil Error explaining the refusal")
	}
}

func TestDoctor_ReportsRouteAndNeverTheKey(t *testing.T) {
	const secret = "virtual-key-SECRET-do-not-print"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(openrouter.Response{
			Answers: map[string]json.RawMessage{"ping": json.RawMessage(`{"type":"noul","noul":0.99}`)},
			Usage:   &openrouter.Usage{Cost: 0.000001},
		})
	}))
	defer srv.Close()

	cfg := config.Default()
	client := openrouter.NewClientWithRoutes(openrouter.Route{
		Name: openrouter.RouteProxy, Endpoint: srv.URL, APIKey: secret, BaseURL: srv.URL,
		CredentialSource: "env PYCKLLM_API_KEY", Why: "proxy probe ok (GET /key 200)",
	}, nil, openrouter.RetryPolicy{MaxAttempts: 1})
	h := NewDoctorHandler(client, cfg, budget.NewTracker(1), audit.NewLogger(filepath.Join(t.TempDir(), "a.jsonl")))
	_, out, err := h.Handle(context.Background(), nil, DoctorInput{})
	if err != nil {
		t.Fatal(err)
	}
	c := out.Config
	if c.Route != "proxy" || c.BaseURL != srv.URL || c.CredentialSource != "env PYCKLLM_API_KEY" || c.RouteWhy == "" {
		t.Errorf("config snapshot = %+v", c)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), secret) {
		t.Errorf("doctor output contains the key: %s", raw)
	}
}
