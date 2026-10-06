package record

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNilRecorderIsNoop(t *testing.T) {
	var r *Recorder
	r.Write(Record{Kind: KindToolCall})
	r.Session(Record{})
	r.Close()
	if New("", time.Now(), 1) != nil {
		t.Error("empty dir must give a nil recorder")
	}
}

func TestLazyAndWriteErrorWarnsOnce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d")
	_ = New(dir, time.Now(), 7)
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("New must not touch disk")
	}
	// A file where the directory should be makes MkdirAll fail.
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, nil, 0o600)
	bad := New(filepath.Join(blocker, "sub"), time.Now(), 7)
	var warns []string
	bad.stderr = func(s string) { warns = append(warns, s) }
	bad.Write(Record{Kind: KindToolCall})
	bad.Write(Record{Kind: KindToolCall})
	if len(warns) != 1 {
		t.Errorf("warnings = %v, want exactly 1", warns)
	}
}

func TestSummarize(t *testing.T) {
	dir := t.TempDir()
	r := New(dir, time.Now(), 9)
	yes, no := true, false
	lat := 100.0
	r.Session(Record{Mode: "mcp"})
	r.Write(Record{Kind: KindToolCall, Tool: "jev_ask", IsError: &no, LatencyMS: &lat})
	r.Write(Record{Kind: KindToolCall, Tool: "jev_ask", IsError: &yes, LatencyMS: &lat})
	r.Write(Record{Kind: KindSystemOne, Model: "m", LatencyMS: &lat})
	r.Close()
	s, err := Summarize(dir)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	s.Render(&buf)
	out := buf.String()
	for _, want := range []string{"tool calls: 2", "jev_ask", "50%", "100 ms", "model"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
}
