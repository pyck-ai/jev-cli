// Package credentials resolves the OpenRouter API key jev-mcp uses to
// authenticate to OpenRouter.
//
// jev-mcp keeps stdio-only, single-user, locally-invoked operation: this
// package only ever reads local files/env vars already on disk/in the
// process environment. It does not start any service, does not accept
// network connections, and does not perform any bearer-token HTTP auth of
// its own -- it simply decides which string to put in the Authorization
// header jev-mcp itself sends to OpenRouter (see internal/openrouter).
package credentials

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/pyck-ai/jev-mcp/internal/xdg"
)

// EnvVar is the environment variable checked first.
const EnvVar = "OPENROUTER_API_KEY"

// Result is a successfully resolved API key, plus a short, human-readable
// description of where it came from, for startup logging. Source never
// contains the key value itself, and neither field of Result should ever
// be logged except via the Source string.
type Result struct {
	Key    string
	Source string
}

// Resolve finds an OpenRouter API key using, in order:
//
//  1. The OPENROUTER_API_KEY environment variable, if set and non-empty --
//     used as-is.
//  2. Otherwise, opencode's own credential store: as an existing user of
//     opencode may already have logged into OpenRouter there, jev-mcp
//     falls back to reusing that credential rather than requiring a
//     separate login. The store is a static JSON file at
//     "$XDG_DATA_HOME/opencode/auth.json" (falling back to
//     "~/.local/share/opencode/auth.json" when XDG_DATA_HOME is unset --
//     see internal/xdg for the shared resolution logic, also used for
//     jev-mcp's own audit log path). It is only ever read, never written,
//     and opencode need not be running. If the file exists, parses as
//     JSON, and has an "openrouter" entry with "type":"api" and a
//     non-empty "key", that key is used. An "openrouter" entry with any
//     other "type" (e.g. "oauth") is not usable this way and is skipped.
//
// ok is false if neither source yields a usable key. This is a normal,
// expected outcome (e.g. a fresh machine with neither configured) -- not
// an error -- and callers should turn it into their own fail-fast startup
// error exactly as an unset OPENROUTER_API_KEY was already handled before
// this fallback existed. Every failure mode reading/parsing opencode's
// auth store (missing file, permission denied, malformed JSON, unexpected
// shape, oauth-typed entry, empty key) is likewise folded into ok=false:
// from a caller's perspective these all mean the same thing, "no key from
// this source", never a crash and never a partial/garbage key.
func Resolve() (Result, bool) {
	if key := os.Getenv(EnvVar); key != "" {
		return Result{Key: key, Source: "env"}, true
	}

	path, err := opencodeAuthPath()
	if err != nil {
		return Result{}, false
	}
	if key, ok := readOpenRouterKey(path); ok {
		return Result{Key: key, Source: "opencode auth store at " + path}, true
	}
	return Result{}, false
}

// opencodeAuthPath resolves opencode's credential store path.
func opencodeAuthPath() (string, error) {
	dataHome, err := xdg.DataHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dataHome, "opencode", "auth.json"), nil
}

// opencodeAuthEntry is the subset of an opencode auth.json provider entry
// this package cares about. opencode's real entries carry more fields for
// oauth-typed credentials (refresh/access/expires); those are irrelevant
// here and are simply ignored by json.Unmarshal.
type opencodeAuthEntry struct {
	Type string `json:"type"`
	Key  string `json:"key"`
}

// readOpenRouterKey reads path as an opencode auth.json file and extracts
// its "openrouter" entry's key, if usable. Every failure mode (missing
// file, unreadable/permission denied, malformed top-level JSON, no
// "openrouter" entry, an "openrouter" entry that doesn't parse into
// opencodeAuthEntry, a non-"api" type, or an empty key) returns ok=false
// rather than an error, and never panics: none of these are exceptional
// from this package's point of view, they're all just "no key here".
//
// The top level is decoded into a map of json.RawMessage rather than a
// full struct modeling every known provider/credential shape, since only
// the "openrouter" entry is ever inspected: this also means a malformed
// entry for an unrelated provider (e.g. a botched "anthropic" entry)
// cannot break resolution of the "openrouter" one.
func readOpenRouterKey(path string) (key string, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}

	var auth map[string]json.RawMessage
	if err := json.Unmarshal(data, &auth); err != nil {
		return "", false
	}

	raw, present := auth["openrouter"]
	if !present {
		return "", false
	}

	var entry opencodeAuthEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return "", false
	}

	if entry.Type != "api" || entry.Key == "" {
		return "", false
	}
	return entry.Key, true
}
