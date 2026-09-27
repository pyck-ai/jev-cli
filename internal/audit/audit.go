// Package audit implements the append-only JSONL audit log written for
// every jev_score tool call.
//
// The judged `state` text itself is never written to the log verbatim: only
// its SHA-256 hex digest is recorded (see Entry.InputStateSHA256), both to
// avoid unbounded log growth from arbitrary-length judged content and to
// avoid leaking potentially sensitive judged text into a plaintext file.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pyck-ai/jev-cli/internal/xdg"
)

// Usage mirrors the token accounting reported by OpenRouter for a call.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Entry is one audit log line. Status is one of "ok", "invalid_response",
// or "error":
//   - "ok": a well-formed score was returned.
//   - "invalid_response": the call completed but the model's answer was
//     malformed, missing, or its probabilities did not sum to ~1; no score
//     is trustworthy.
//   - "error": the call itself failed (network error, non-retryable or
//     retry-exhausted HTTP error, pre-call session budget refusal, etc.)
//     and no model response was obtained at all.
//
// Score, Confidence, and Usage are omitted (via omitempty) when not
// meaningful for the given Status, rather than written as fabricated
// zeroes, so a log reader can distinguish "genuinely scored 0" from
// "no score available".
type Entry struct {
	Timestamp        string   `json:"timestamp"`
	Tool             string   `json:"tool"`
	Model            string   `json:"model"`
	InputStateSHA256 string   `json:"input_state_sha256"`
	ScaleMin         int      `json:"scale_min"`
	ScaleMax         int      `json:"scale_max"`
	Score            *float64 `json:"score,omitempty"`
	Confidence       *float64 `json:"confidence,omitempty"`
	Status           string   `json:"status"`
	Usage            *Usage   `json:"usage,omitempty"`
	CostUSD          *float64 `json:"cost_usd,omitempty"`
	LatencyMs        int64    `json:"latency_ms"`
	// BudgetExceeded is an addition beyond the audit line shape sketched in
	// the project brief, mirroring tools.ScoreOutput.BudgetExceeded (see
	// that type's doc comment for the full rationale). Omitted when false
	// so existing log readers that don't know about it see no change for
	// the common case.
	BudgetExceeded bool   `json:"budget_exceeded,omitempty"`
	Error          string `json:"error,omitempty"`
	// ItemCount and InvalidCount are additions for tools added after the
	// plugin-architecture refactor that batch more than one question into
	// a single call (jev_verify's claims, jev_check's propositions,
	// jev_rerank/jev_classify's candidates/items, etc.): ItemCount is how
	// many sub-items/questions this one call carried, and InvalidCount is
	// how many of them came back invalid_response (fail-closed at the
	// item level) even though the call itself (Status) succeeded. Both
	// are omitted (as 0) for single-question tools/calls where they are
	// not meaningful -- including every jev_score audit entry, which
	// never sets either field and therefore serializes identically to
	// before this addition.
	ItemCount    int `json:"item_count,omitempty"`
	InvalidCount int `json:"invalid_count,omitempty"`
}

// Logger appends Entry records to a JSONL file, creating parent
// directories on first use.
type Logger struct {
	path string
	mu   sync.Mutex
}

// DefaultPath returns "<data home>/jev-cli/audit.jsonl", where <data home>
// is $XDG_DATA_HOME or, when unset, $HOME/.local/share (see internal/xdg,
// also used by internal/credentials to locate opencode's auth store).
//
// Rename fallback: if that file does not exist yet but the pre-rename
// "<data home>/jev-mcp/audit.jsonl" does, the legacy path is returned
// instead, so an existing audit history keeps growing in one file rather
// than being split across two.
func DefaultPath() (string, error) {
	dataHome, err := xdg.DataHome()
	if err != nil {
		return "", err
	}
	current := filepath.Join(dataHome, "jev-cli", "audit.jsonl")
	if _, err := os.Stat(current); err == nil {
		return current, nil
	}
	legacy := filepath.Join(dataHome, "jev-mcp", "audit.jsonl")
	if _, err := os.Stat(legacy); err == nil {
		return legacy, nil
	}
	return current, nil
}

// NewLogger creates a Logger writing to path. It does not touch the
// filesystem until the first Log call.
func NewLogger(path string) *Logger {
	return &Logger{path: path}
}

// Log appends entry as one JSON line, filling in Timestamp if empty.
//
// Log write failures (directory creation, open, write) never fail the tool
// call that triggered them: this method has no error return, and instead
// prints a warning to stderr on failure and continues. stdout is reserved
// exclusively for the MCP JSON-RPC stream on the stdio transport, so
// diagnostics never go there.
func (l *Logger) Log(entry Entry) {
	if entry.Timestamp == "" {
		entry.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}

	line, err := json.Marshal(entry)
	if err != nil {
		fmt.Fprintf(os.Stderr, "jev: audit: failed to marshal entry: %v\n", err)
		return
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		fmt.Fprintf(os.Stderr, "jev: audit: failed to create log directory: %v\n", err)
		return
	}

	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "jev: audit: failed to open log file %s: %v\n", l.path, err)
		return
	}
	defer f.Close()

	if _, err := f.Write(line); err != nil {
		fmt.Fprintf(os.Stderr, "jev: audit: failed to write log entry: %v\n", err)
	}
}

// HashState returns the lowercase hex-encoded SHA-256 digest of state, for
// use as Entry.InputStateSHA256.
func HashState(state string) string {
	sum := sha256.Sum256([]byte(state))
	return hex.EncodeToString(sum[:])
}

// HashValue is a convenience wrapper for tools whose sensitive input is a
// structured value rather than a single string: it JSON-marshals v and
// returns HashState of that JSON text. Added for the batch of tools built
// on top of the self-registering plugin architecture (see
// internal/registry), each of which has a multi-field input struct rather
// than jev_score's single State string; jev_score itself continues to
// call HashState(in.State) directly (a different, pre-existing digest)
// and is unaffected by this addition.
//
// If v fails to marshal (not expected for the plain data structs this is
// used with -- every tool Input type in this codebase is built entirely
// from strings, slices, and other Inputs, none of which json.Marshal
// rejects), the error text itself is hashed instead of panicking or
// silently hashing nothing: still never the raw input, and still a
// deterministic value usable for "was this the same call" debugging.
func HashValue(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return HashState(fmt.Sprintf("!MARSHAL_ERROR!%v", err))
	}
	return HashState(string(b))
}
