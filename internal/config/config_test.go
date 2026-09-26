package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPath_EnvOverrideWins(t *testing.T) {
	t.Setenv("JEV_MCP_CONFIG_PATH", "/tmp/custom/jev-mcp-config.json")
	p, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if p != "/tmp/custom/jev-mcp-config.json" {
		t.Errorf("Path() = %q, want the JEV_MCP_CONFIG_PATH value", p)
	}
}

func TestLoad_MissingFile_ReturnsDefaults(t *testing.T) {
	t.Setenv("JEV_MCP_CONFIG_PATH", filepath.Join(t.TempDir(), "does-not-exist.json"))
	t.Setenv("JEV_MCP_MODEL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Default()
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("Load() with missing file = %+v, want defaults %+v", cfg, want)
	}
}

func TestLoad_MalformedFile_IsAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	t.Setenv("JEV_MCP_CONFIG_PATH", path)

	if _, err := Load(); err == nil {
		t.Fatal("expected an error for a malformed config file (fail fast), got nil")
	}
}

func TestLoad_PartialFileMergesOntoDefaults(t *testing.T) {
	// Only overrides one nested field (budget.max_usd_per_call); everything
	// else, including budget.max_usd_per_session, must retain its Default()
	// value because json.Unmarshal only overwrites keys present in the
	// input.
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"budget": {"max_usd_per_call": 0.05}}`), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	t.Setenv("JEV_MCP_CONFIG_PATH", path)
	t.Setenv("JEV_MCP_MODEL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Budget.MaxUSDPerCall != 0.05 {
		t.Errorf("Budget.MaxUSDPerCall = %v, want 0.05 (from file)", cfg.Budget.MaxUSDPerCall)
	}
	def := Default()
	if cfg.Budget.MaxUSDPerSession != def.Budget.MaxUSDPerSession {
		t.Errorf("Budget.MaxUSDPerSession = %v, want default %v (omitted from file)", cfg.Budget.MaxUSDPerSession, def.Budget.MaxUSDPerSession)
	}
	if cfg.DefaultModel != def.DefaultModel {
		t.Errorf("DefaultModel = %q, want default %q (omitted from file)", cfg.DefaultModel, def.DefaultModel)
	}
	if cfg.Retry != def.Retry {
		t.Errorf("Retry = %+v, want default %+v (omitted from file)", cfg.Retry, def.Retry)
	}
}

func TestLoad_EnvModelOverridesDefaultModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"default_model": "typesafe/jev-1.13"}`), 0o600); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	t.Setenv("JEV_MCP_CONFIG_PATH", path)
	t.Setenv("JEV_MCP_MODEL", "typesafe/jev-experimental")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DefaultModel != "typesafe/jev-experimental" {
		t.Errorf("DefaultModel = %q, want env override %q", cfg.DefaultModel, "typesafe/jev-experimental")
	}
}

func TestModelForTool_ToolOverrideWinsOverDefault(t *testing.T) {
	cfg := Default()
	cfg.DefaultModel = "typesafe/jev-experimental" // simulating a JEV_MCP_MODEL override
	cfg.ToolModelOverrides = map[string]string{"jev_score": "typesafe/jev-1.13"}

	if got := cfg.ModelForTool("jev_score"); got != "typesafe/jev-1.13" {
		t.Errorf(`ModelForTool("jev_score") = %q, want the tool-specific override "typesafe/jev-1.13" to win over DefaultModel`, got)
	}
	if got := cfg.ModelForTool("some_other_tool"); got != "typesafe/jev-experimental" {
		t.Errorf(`ModelForTool("some_other_tool") = %q, want DefaultModel %q (no override present)`, got, cfg.DefaultModel)
	}
}
