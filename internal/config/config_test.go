package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPath_EnvOverrideWins(t *testing.T) {
	t.Setenv("JEV_CLI_CONFIG_PATH", "/tmp/custom/jev-cli-config.json")
	p, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if p != "/tmp/custom/jev-cli-config.json" {
		t.Errorf("Path() = %q, want the JEV_CLI_CONFIG_PATH value", p)
	}
}

func TestLoad_MissingFile_ReturnsDefaults(t *testing.T) {
	t.Setenv("JEV_CLI_CONFIG_PATH", filepath.Join(t.TempDir(), "does-not-exist.json"))
	t.Setenv("JEV_CLI_MODEL", "")

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
	t.Setenv("JEV_CLI_CONFIG_PATH", path)

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
	t.Setenv("JEV_CLI_CONFIG_PATH", path)
	t.Setenv("JEV_CLI_MODEL", "")

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
	t.Setenv("JEV_CLI_CONFIG_PATH", path)
	t.Setenv("JEV_CLI_MODEL", "typesafe/jev-experimental")

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
	cfg.DefaultModel = "typesafe/jev-experimental" // simulating a JEV_CLI_MODEL override
	cfg.ToolModelOverrides = map[string]string{"jev_score": "typesafe/jev-1.13"}

	if got := cfg.ModelForTool("jev_score"); got != "typesafe/jev-1.13" {
		t.Errorf(`ModelForTool("jev_score") = %q, want the tool-specific override "typesafe/jev-1.13" to win over DefaultModel`, got)
	}
	if got := cfg.ModelForTool("some_other_tool"); got != "typesafe/jev-experimental" {
		t.Errorf(`ModelForTool("some_other_tool") = %q, want DefaultModel %q (no override present)`, got, cfg.DefaultModel)
	}
}

func TestPath_LegacyEnvStillHonored(t *testing.T) {
	t.Setenv("JEV_CLI_CONFIG_PATH", "")
	t.Setenv("JEV_MCP_CONFIG_PATH", "/tmp/legacy/config.json")
	p, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if p != "/tmp/legacy/config.json" {
		t.Errorf("Path() = %q, want the legacy JEV_MCP_CONFIG_PATH value", p)
	}
}

func TestPath_NewEnvWinsOverLegacy(t *testing.T) {
	t.Setenv("JEV_CLI_CONFIG_PATH", "/tmp/new/config.json")
	t.Setenv("JEV_MCP_CONFIG_PATH", "/tmp/legacy/config.json")
	p, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if p != "/tmp/new/config.json" {
		t.Errorf("Path() = %q, want the JEV_CLI_CONFIG_PATH value", p)
	}
}

// clearPathEnv unsets both config-path env vars and points the user
// config dir at a fresh temp dir, returning it.
func clearPathEnv(t *testing.T) string {
	t.Helper()
	t.Setenv("JEV_CLI_CONFIG_PATH", "")
	t.Setenv("JEV_MCP_CONFIG_PATH", "")
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	return dir
}

func writeConfig(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPath_DefaultIsJevCli(t *testing.T) {
	dir := clearPathEnv(t)
	p, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if want := filepath.Join(dir, "jev-cli", "config.json"); p != want {
		t.Errorf("Path() = %q, want %q", p, want)
	}
}

func TestPath_FallsBackToLegacyFile(t *testing.T) {
	dir := clearPathEnv(t)
	legacy := filepath.Join(dir, "jev-mcp", "config.json")
	writeConfig(t, legacy)
	p, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if p != legacy {
		t.Errorf("Path() = %q, want legacy %q", p, legacy)
	}
}

func TestPath_PrefersNewFileWhenBothExist(t *testing.T) {
	dir := clearPathEnv(t)
	current := filepath.Join(dir, "jev-cli", "config.json")
	writeConfig(t, current)
	writeConfig(t, filepath.Join(dir, "jev-mcp", "config.json"))
	p, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if p != current {
		t.Errorf("Path() = %q, want %q", p, current)
	}
}

func TestLoad_LegacyModelEnvStillHonored(t *testing.T) {
	t.Setenv("JEV_CLI_CONFIG_PATH", filepath.Join(t.TempDir(), "missing.json"))
	t.Setenv("JEV_CLI_MODEL", "")
	t.Setenv("JEV_MCP_MODEL", "legacy/model")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DefaultModel != "legacy/model" {
		t.Errorf("DefaultModel = %q, want legacy/model", cfg.DefaultModel)
	}
}

func TestLoad_NewModelEnvWinsOverLegacy(t *testing.T) {
	t.Setenv("JEV_CLI_CONFIG_PATH", filepath.Join(t.TempDir(), "missing.json"))
	t.Setenv("JEV_CLI_MODEL", "new/model")
	t.Setenv("JEV_MCP_MODEL", "legacy/model")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DefaultModel != "new/model" {
		t.Errorf("DefaultModel = %q, want new/model", cfg.DefaultModel)
	}
}
