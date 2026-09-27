// Package config loads jev-cli's JSON configuration file and applies the
// small set of environment-variable overrides documented in the project
// README.
//
// The OpenRouter API key is deliberately NOT part of this package: per the
// project's security requirements it is supplied exclusively via the
// OPENROUTER_API_KEY environment variable, is never read from the config
// file, and must never be logged or embedded in an error message. Callers
// read it directly from the environment (see main.go) and pass it around
// out-of-band from *Config.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultModel is the OpenRouter model slug used when neither the config
// file nor JEV_CLI_MODEL specify one.
//
// Plain "typesafe/jev-latest" (no tilde) is NOT valid: a live call returns
// HTTP 400 "Model typesafe/jev-latest does not exist". The correct
// always-latest alias carries a leading tilde, "~typesafe/jev-latest";
// confirmed with a live POST /api/v1/systemone call on 2026-09-26, which
// resolved it to the pinned model "typesafe/jev-1.13-20260917" and returned
// a valid score answer end-to-end. OpenRouter's own model listing
// (https://openrouter.ai/typesafe) also lists "typesafe/jev-1.13" (pinned)
// and "typesafe/jev-router" (untested here, semantics unconfirmed) as
// separate, non-tilde-prefixed slugs.
const DefaultModel = "~typesafe/jev-latest"

// Budget holds spend-limiting thresholds, in US dollars.
//
// A value <= 0 for either field means "no limit" for that field. This is a
// convention chosen by this implementation (not documented by OpenRouter or
// specified verbatim in the project brief) so that a user can explicitly
// disable a cap by setting it to 0 without needing a separate "enabled"
// flag. The shipped defaults (see Default()) match the brief's example and
// are both strictly positive, so this convention only takes effect when a
// user opts into it explicitly in their own config file.
type Budget struct {
	MaxUSDPerCall    float64 `json:"max_usd_per_call"`
	MaxUSDPerSession float64 `json:"max_usd_per_session"`
}

// Retry holds HTTP retry/backoff tuning for the OpenRouter client.
type Retry struct {
	MaxAttempts   int `json:"max_attempts"`
	BaseBackoffMs int `json:"base_backoff_ms"`
	MaxBackoffMs  int `json:"max_backoff_ms"`
}

// Config is the parsed shape of ~/.config/jev-cli/config.json (or the path
// named by JEV_CLI_CONFIG_PATH).
type Config struct {
	DefaultModel       string            `json:"default_model"`
	ToolModelOverrides map[string]string `json:"tool_model_overrides"`
	Budget             Budget            `json:"budget"`
	Retry              Retry             `json:"retry"`
	RequestTimeoutMs   int               `json:"request_timeout_ms"`
}

// Default returns the built-in configuration used when no config file is
// present, and as the base that a present config file's fields are merged
// onto (json.Unmarshal only overwrites keys that are actually present in
// the input, so any field a user's file omits keeps its Default() value).
func Default() Config {
	return Config{
		DefaultModel:       DefaultModel,
		ToolModelOverrides: map[string]string{"jev_score": DefaultModel},
		Budget: Budget{
			MaxUSDPerCall:    0.01,
			MaxUSDPerSession: 1.0,
		},
		Retry: Retry{
			MaxAttempts:   3,
			BaseBackoffMs: 500,
			MaxBackoffMs:  8000,
		},
		RequestTimeoutMs: 30000,
	}
}

// Environment variables. Each has a legacy JEV_MCP_* spelling from before
// the project was renamed from jev-mcp to jev-cli; the new name wins when
// both are set, and the legacy name is still honored when only it is.
const (
	EnvConfigPath       = "JEV_CLI_CONFIG_PATH"
	EnvModel            = "JEV_CLI_MODEL"
	legacyEnvConfigPath = "JEV_MCP_CONFIG_PATH"
	legacyEnvModel      = "JEV_MCP_MODEL"
)

// appDirName is this project's directory name under the user config dir
// (and, in internal/audit, under the user data dir). legacyAppDirName is
// the pre-rename name, still read as a fallback.
const (
	appDirName       = "jev-cli"
	legacyAppDirName = "jev-mcp"
)

// getenvFirst returns the first non-empty value among the named
// environment variables.
func getenvFirst(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// Path resolves the config file path, in order:
//
//  1. JEV_CLI_CONFIG_PATH, or the legacy JEV_MCP_CONFIG_PATH, if set.
//  2. "<user config dir>/jev-cli/config.json", if that file exists.
//  3. "<user config dir>/jev-mcp/config.json" (the pre-rename location),
//     if that file exists and the new one doesn't, so an existing config
//     keeps working without being moved.
//  4. Otherwise "<user config dir>/jev-cli/config.json" (it need not
//     exist; Load treats a missing file as "use defaults").
//
// On Linux, os.UserConfigDir() is $XDG_CONFIG_HOME, or $HOME/.config when
// that is unset.
func Path() (string, error) {
	if p := getenvFirst(EnvConfigPath, legacyEnvConfigPath); p != "" {
		return p, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolving default config directory: %w", err)
	}
	current := filepath.Join(dir, appDirName, "config.json")
	if _, err := os.Stat(current); err == nil {
		return current, nil
	}
	legacy := filepath.Join(dir, legacyAppDirName, "config.json")
	if _, err := os.Stat(legacy); err == nil {
		return legacy, nil
	}
	return current, nil
}

// Load reads and parses the config file, merges it onto Default(), and
// applies the JEV_CLI_MODEL environment override to DefaultModel.
//
// A missing config file is not an error: Default() (plus any env override)
// is returned as-is. A config file that exists but fails to parse IS
// treated as an error (fail fast) so that a typo in the file is surfaced
// immediately at startup rather than silently ignored.
func Load() (Config, error) {
	cfg := Default()

	path, err := Path()
	if err != nil {
		return Config{}, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			applyEnvOverrides(&cfg)
			return cfg, nil
		}
		return Config{}, fmt.Errorf("reading config file %s: %w", path, err)
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parsing config file %s: %w", path, err)
	}

	applyEnvOverrides(&cfg)
	return cfg, nil
}

// applyEnvOverrides applies JEV_CLI_MODEL (or the legacy JEV_MCP_MODEL),
// which overrides the default_model field specifically.
//
// Precedence: the env var overrides cfg.DefaultModel, but an explicit
// per-tool entry in tool_model_overrides in the config file still wins over
// the (possibly env-overridden) default for that specific tool, since a
// tool-specific setting is more specific than a blanket default override.
// If you want the env var to force every tool regardless of
// tool_model_overrides, remove the relevant entry from your config file.
func applyEnvOverrides(cfg *Config) {
	if m := getenvFirst(EnvModel, legacyEnvModel); m != "" {
		cfg.DefaultModel = m
	}
}

// ModelForTool resolves the effective model slug for the named tool:
// tool_model_overrides[tool] if present and non-empty, else DefaultModel.
func (c Config) ModelForTool(tool string) string {
	if m, ok := c.ToolModelOverrides[tool]; ok && m != "" {
		return m
	}
	return c.DefaultModel
}
