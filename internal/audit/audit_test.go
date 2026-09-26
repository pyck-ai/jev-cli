package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHashState(t *testing.T) {
	got := HashState("hello")
	want := sha256.Sum256([]byte("hello"))
	if got != hex.EncodeToString(want[:]) {
		t.Errorf("HashState(%q) = %q, want %q", "hello", got, hex.EncodeToString(want[:]))
	}
	if HashState("hello") == HashState("hello2") {
		t.Error("expected different inputs to hash differently")
	}
}

func TestLogger_Log_AppendsJSONLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "audit.jsonl")
	l := NewLogger(path)

	l.Log(Entry{Tool: "jev_score", Model: "m1", InputStateSHA256: HashState("a"), ScaleMin: 0, ScaleMax: 2, Status: "ok"})
	l.Log(Entry{Tool: "jev_score", Model: "m1", InputStateSHA256: HashState("b"), ScaleMin: 0, ScaleMax: 2, Status: "ok"})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading log file (parent dirs should have been created): %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %q", len(lines), string(data))
	}
	for i, line := range lines {
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d is not valid JSON: %v\nline: %s", i, err, line)
		}
		if e.Timestamp == "" {
			t.Errorf("line %d: expected Timestamp to be auto-filled", i)
		}
	}
}

func TestLogger_Log_NeverPanicsOnUnwritableDirectory(t *testing.T) {
	// Point the log at a path whose parent is actually a regular file, so
	// MkdirAll must fail. Log must swallow the error, not panic or block.
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("setting up blocker file: %v", err)
	}
	l := NewLogger(filepath.Join(blocker, "audit.jsonl"))

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Log panicked on a write failure: %v", r)
		}
	}()
	l.Log(Entry{Tool: "jev_score", Status: "ok"})
}

func TestDefaultPath_HonorsXDGDataHome(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/tmp/xdgdata")
	p, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	want := filepath.Join("/tmp/xdgdata", "jev-mcp", "audit.jsonl")
	if p != want {
		t.Errorf("DefaultPath() = %q, want %q", p, want)
	}
}

func TestDefaultPath_FallsBackToHomeLocalShare(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "/tmp/fakehome")
	p, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	want := filepath.Join("/tmp/fakehome", ".local", "share", "jev-mcp", "audit.jsonl")
	if p != want {
		t.Errorf("DefaultPath() = %q, want %q", p, want)
	}
}
