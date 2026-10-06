// Package record implements jev's opt-in recording: an append-only JSONL log
// of FULL tool calls and SystemOne wire exchanges, for later analysis of
// model accuracy and of which tools agents pick.
//
// It is the opposite of internal/audit, which deliberately stores only a
// SHA-256 of the input state. Recording keeps full request and response
// content, so it is off unless enabled with `--record <dir>` or
// JEV_CLI_RECORD=<dir> (the flag wins; empty means off), and its files are
// created 0600 in a 0700 directory. Nothing is created until the first
// record is written, so `jev --help` and every disabled run touch no disk.
//
// One file per process: <dir>/<UTC yyyymmdd-hhmmss>-<pid>.jsonl, one JSON
// object per line, each with ts, kind, session and (where applicable)
// call_id. Kinds:
//
//	session    once per process: version, mode (mcp|cli), model and its source, route, tools, argv
//	client     MCP only: the connecting client's name/version, after initialize
//	tool_call  one per tool invocation (MCP: full arguments and result; CLI: flags only, see below)
//	systemone  one per Ask that reached the network or was aborted by the preflight guard
//
// Correlation: the MCP middleware (Recorder.MCPMiddleware) or the CLI
// wrapper puts a call_id into the context (WithCallID); the systemone hook
// reads it back, so one tool_call maps to its N wire calls via call_id.
//
// Wire capture is the Hook returned by Recorder.WrapHook, which wraps the
// models.Guard rather than being ordered next to it: the wrapper sees the
// guard's BeforeAsk error (and records it as preflight_error), which a
// separately registered hook would never see because a BeforeAsk error
// stops the hook chain. The recorder never aborts a call.
//
// Keys: the API key lives in the route and in HTTP headers only; neither is
// ever passed to this package, so no key or Authorization header is recorded.
package record

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// EnvDir is the environment variable enabling recording (the --record flag
// wins over it).
const EnvDir = "JEV_CLI_RECORD"

// Dir resolves the recording directory: the flag value if non-empty, else
// the environment value. Empty means recording is off.
func Dir(flag string) string {
	if flag != "" {
		return flag
	}
	return os.Getenv(EnvDir)
}

// Kinds of record.
const (
	KindSession   = "session"
	KindClient    = "client"
	KindToolCall  = "tool_call"
	KindSystemOne = "systemone"
)

// ClientInfo is an MCP client's self-reported identity.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ToolResult is a recorded MCP tool result.
type ToolResult struct {
	Structured any      `json:"structured,omitempty"`
	Text       []string `json:"text,omitempty"`
}

// Record is one JSONL line. Fields not relevant to a kind are omitted.
type Record struct {
	TS      string `json:"ts"`
	Kind    string `json:"kind"`
	Session string `json:"session"`
	CallID  string `json:"call_id,omitempty"`

	// session
	Version     string            `json:"version,omitempty"`
	Mode        string            `json:"mode,omitempty"`
	Model       string            `json:"model,omitempty"`
	ModelSource string            `json:"model_source,omitempty"`
	ToolModels  map[string]string `json:"tool_model_overrides,omitempty"`
	Route       string            `json:"route,omitempty"`
	RouteWhy    string            `json:"route_why,omitempty"`
	Tools       []string          `json:"tools,omitempty"`
	Argv        []string          `json:"argv,omitempty"`

	// client (also set on tool_call when known)
	Client *ClientInfo `json:"client,omitempty"`

	// tool_call
	Transport string          `json:"transport,omitempty"` // mcp | cli
	Tool      string          `json:"tool,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Result    *ToolResult     `json:"result,omitempty"`
	IsError   *bool           `json:"is_error,omitempty"`

	// systemone
	Request        json.RawMessage `json:"request,omitempty"`
	HTTPStatus     *int            `json:"http_status,omitempty"`
	Response       json.RawMessage `json:"response,omitempty"` // raw body as JSON, or a JSON string when not parseable
	ResponseModel  string          `json:"response_model,omitempty"`
	Provider       string          `json:"provider,omitempty"`
	Usage          any             `json:"usage,omitempty"`
	PreflightError string          `json:"preflight_error,omitempty"`

	// tool_call and systemone
	Error     string   `json:"error,omitempty"`
	LatencyMS *float64 `json:"latency_ms,omitempty"`
}

// Recorder appends Records to one per-process file. A nil *Recorder is
// valid and records nothing, so call sites need no enabled-check.
type Recorder struct {
	dir     string
	session string
	stderr  func(string)

	mu          sync.Mutex
	f           *os.File
	failed      bool
	warned      bool
	sessionOnce sync.Once
	clientOnce  sync.Once
}

// New returns a Recorder writing under dir, or nil when dir is empty. It
// does not touch the filesystem.
func New(dir string, now time.Time, pid int) *Recorder {
	if dir == "" {
		return nil
	}
	return &Recorder{
		dir:     dir,
		session: fmt.Sprintf("%s-%d", now.UTC().Format("20060102-150405"), pid),
		stderr:  func(s string) { fmt.Fprintln(os.Stderr, s) },
	}
}

// SessionID is the per-process session id (also the file's base name).
func (r *Recorder) SessionID() string {
	if r == nil {
		return ""
	}
	return r.session
}

// Path is the file this recorder writes (it may not exist yet).
func (r *Recorder) Path() string {
	if r == nil {
		return ""
	}
	return filepath.Join(r.dir, r.session+".jsonl")
}

// Write appends rec (filling ts, kind defaults and session). It never
// fails the caller: the first write error is warned about once on stderr
// and all later records are dropped.
func (r *Recorder) Write(rec Record) {
	if r == nil {
		return
	}
	rec.TS = time.Now().UTC().Format(time.RFC3339Nano)
	rec.Session = r.session
	line, err := json.Marshal(rec)
	if err != nil {
		r.warn(fmt.Errorf("marshaling record: %w", err))
		return
	}
	line = append(line, '\n')

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failed {
		return
	}
	if r.f == nil {
		if err := os.MkdirAll(r.dir, 0o700); err != nil {
			r.failed = true
			r.warnLocked(err)
			return
		}
		f, err := os.OpenFile(r.Path(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			r.failed = true
			r.warnLocked(err)
			return
		}
		r.f = f
	}
	if _, err := r.f.Write(line); err != nil {
		r.failed = true
		r.warnLocked(err)
	}
}

func (r *Recorder) warn(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.warnLocked(err)
}

func (r *Recorder) warnLocked(err error) {
	if r.warned {
		return
	}
	r.warned = true
	r.stderr(fmt.Sprintf("jev: warning: recording disabled: %v", err))
}

// Close closes the file, if one was opened.
func (r *Recorder) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f != nil {
		_ = r.f.Close()
		r.f = nil
	}
}

// Session writes the session record, at most once per process; later calls
// are ignored. rec.Kind and session are filled in here.
func (r *Recorder) Session(rec Record) {
	if r == nil {
		return
	}
	r.sessionOnce.Do(func() {
		rec.Kind = KindSession
		r.Write(rec)
	})
}

type ctxKey struct{}

// WithCallID returns ctx carrying id as the current tool call's id.
func WithCallID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// CallID returns the call id in ctx, or "" when none.
func CallID(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// NewCallID returns a fresh random call id.
func NewCallID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func ms(d time.Duration) *float64 {
	v := float64(d.Microseconds()) / 1000
	return &v
}
